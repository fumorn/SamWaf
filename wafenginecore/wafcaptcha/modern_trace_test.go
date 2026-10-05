package wafcaptcha

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"SamWaf/innerbean"
	"SamWaf/model"
	capserver "github.com/samwafgo/cap_go_server"
)

func captureModernTrace(t *testing.T) *[]modernTraceEvent {
	t.Helper()
	events := []modernTraceEvent{}
	old := modernTraceEmit
	modernTraceEmit = func(event modernTraceEvent) { events = append(events, event) }
	t.Cleanup(func() { modernTraceEmit = old })
	return &events
}

func TestModernTraceStagesAndPrivacy(t *testing.T) {
	t.Setenv("SAMWAF_CAP_TRACE", "1")
	events := captureModernTrace(t)
	s, cfg, log := modernFixture()
	log.REQ_UUID = "trace-request-id"
	log.SRC_IP = "198.51.100.99"
	token := modernSolve(t, s, cfg, log)
	body, _ := json.Marshal(map[string]string{"token": token})
	if w := modernRequest(t, s, cfg, log, "validate", string(body)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	r := httptest.NewRequest("GET", "https://example.test/private/page?query-secret=hidden", nil)
	r.Header.Set("Cookie", "cookie-secret=hidden")
	r.Header.Set("X-Forwarded-For", "203.0.113.123")
	s.handleModernCaptcha(httptest.NewRecorder(), r, log, cfg, "nic")
	if len(*events) != 4 {
		t.Fatalf("events: %+v", *events)
	}
	for i, stage := range []string{"challenge", "redeem", "validate", "page"} {
		e := (*events)[i]
		if e.Stage != stage || !e.Success || e.Status != 200 || e.RequestID != log.REQ_UUID || e.HostCode != log.HOST_CODE || e.IP != log.NetSrcIp || e.DurationMS < 0 || e.Format != int(cfg.CapJsConfig.Format) || !reflect.DeepEqual(e.Protocols, cfg.CapJsConfig.Protocols) {
			t.Fatalf("incomplete %s: %+v", stage, e)
		}
	}
	if e := (*events)[3]; e.Path != "/private/page" || e.Method != "GET" {
		t.Fatalf("page request: %+v", e)
	}
	encoded, _ := json.Marshal(events)
	for _, secret := range []string{token, "query-secret", "cookie-secret", "203.0.113.123", "hidden", "solutions", "scope"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("trace leaked %q: %s", secret, encoded)
		}
	}
}

func TestModernTraceFailuresPreserveResponses(t *testing.T) {
	events := captureModernTrace(t)
	tests := []struct {
		name, method, path, body, origin, reason, engineReason string
		status                                                 int
		missing, invalid, unavailable                          bool
	}{
		{name: "method", method: "GET", path: "/v2/challenge", reason: "method_not_allowed", status: 405},
		{name: "site", method: "POST", path: "/v2/challenge", reason: "missing_site_context", status: 503, missing: true},
		{name: "config", method: "POST", path: "/v2/challenge", reason: "invalid_configuration", status: 503, invalid: true},
		{name: "origin", method: "POST", path: "/v2/challenge", origin: "https://other.test", reason: "origin_not_allowed", status: 403},
		{name: "engine", method: "POST", path: "/v2/challenge", reason: "verification_unavailable", status: 503, unavailable: true},
		{name: "large", method: "POST", path: "/v2/redeem", body: strings.Repeat("x", modernBodyLimit+1), reason: "request_too_large", status: 413},
		{name: "redeem", method: "POST", path: "/v2/redeem", body: `{"token":"private-token","solutions":[]}`, reason: "verification_failed", engineReason: "invalid_token", status: 403},
		{name: "empty", method: "POST", path: "/v2/redeem", body: `{}`, reason: "verification_failed", engineReason: "missing_token", status: 403},
		{name: "body", method: "POST", path: "/v2/validate", body: `{"private-unknown":"hidden"}`, reason: "invalid_body", status: 400},
		{name: "trailing", method: "POST", path: "/v2/validate", body: `{"token":"private-token"} {}`, reason: "invalid_body", status: 400},
		{name: "validate", method: "POST", path: "/v2/validate", body: `{"token":"private-token"}`, reason: "verification_failed", status: 403},
		{name: "legacy", method: "POST", path: "/redeem", reason: "verification_upgrade_reload", status: 409},
		{name: "unknown", method: "GET", path: "/unknown", reason: "not_found", status: 404},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var previous *httptest.ResponseRecorder
			for _, mode := range []string{"0", "1"} {
				t.Setenv("SAMWAF_CAP_TRACE", mode)
				s, cfg, log := modernFixture()
				log.REQ_UUID = "failure-request"
				if tc.missing {
					log = nil
				}
				if tc.invalid {
					cfg.CapJsConfig.Format = 99
				}
				if tc.unavailable {
					s.cache = legacyPassStore{s.cache}
				}
				r := httptest.NewRequest(tc.method, "https://example.test"+captchaPrefix(cfg)+tc.path+"?query-secret=hidden", strings.NewReader(tc.body))
				r.Header.Set("Origin", tc.origin)
				w := httptest.NewRecorder()
				count := len(*events)
				s.handleModernCaptcha(w, r, log, cfg, "nic")
				if w.Code != tc.status {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				if mode == "0" {
					if len(*events) != count {
						t.Fatal("disabled trace emitted")
					}
					previous = w
					continue
				}
				if previous.Code != w.Code || previous.Body.String() != w.Body.String() || !reflect.DeepEqual(previous.Header(), w.Header()) {
					t.Fatal("trace changed response")
				}
				if len(*events) != count+1 {
					t.Fatalf("expected one event: %+v", *events)
				}
				e := (*events)[count]
				if e.Success || e.Status != tc.status || e.Reason != tc.reason || e.EngineReason != tc.engineReason {
					t.Fatalf("failure details: %+v", e)
				}
				raw, _ := json.Marshal(e)
				for _, secret := range []string{"private-token", "query-secret", "private-unknown", "hidden"} {
					if strings.Contains(string(raw), secret) {
						t.Fatalf("trace leaked %q", secret)
					}
				}
				if tc.engineReason != "" && strings.Contains(w.Body.String(), tc.engineReason) {
					t.Fatal("internal reason exposed to visitor")
				}
			}
		})
	}
}

func TestModernTraceEngineDiagnosticsAreAllowlisted(t *testing.T) {
	t.Setenv("SAMWAF_CAP_TRACE", "1")
	events := captureModernTrace(t)
	_, cfg, log := modernFixture()
	r := httptest.NewRequest("POST", "/redeem", nil)
	w, trace := beginModernTrace(httptest.NewRecorder(), r, cfg, log, "nic", "redeem")
	modernTraceResult(w, &capserver.ModernRedeem{Reason: "instr_failed", InstrError: true, BlockedBy: []string{"webdriver"}, RiskFlags: []string{"headless"}, Token: "token-secret", Scope: "scope-secret"})
	modernTraceFailure(w, "verification_unavailable", errors.New("body-cookie-secret"))
	modernError(w, 503, "verification_unavailable")
	trace.finish()
	e := (*events)[0]
	if e.EngineReason != "instr_failed" || !e.InstrError || !reflect.DeepEqual(e.BlockedBy, []string{"webdriver"}) || !reflect.DeepEqual(e.RiskFlags, []string{"headless"}) || e.ErrorType != "*errors.errorString" {
		t.Fatalf("diagnostics missing: %+v", e)
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("private fields leaked: %s", raw)
	}
}

type modernDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *modernDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestModernTracePreservesResponseControllerDeadline(t *testing.T) {
	t.Setenv("SAMWAF_CAP_TRACE", "1")
	captureModernTrace(t)
	s, cfg, log := modernFixture()
	r := httptest.NewRequest("GET", "/samwaf_captcha/v2/challenge", nil)
	underlying := &modernDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	w, trace := beginModernTrace(underlying, r, cfg, log, "nic", "challenge")
	if trace.Unwrap() != underlying {
		t.Fatal("incorrect unwrap")
	}
	s.GetCapJsChallenge(w, r, cfg, log, "nic")
	if underlying.deadline.Before(time.Now().Add(9 * time.Second)) {
		t.Fatalf("read deadline not forwarded: %v", underlying.deadline)
	}
	if underlying.Code != 405 {
		t.Fatal("response changed")
	}
	trace.finish()
}

func TestModernTraceDisabledDoesNotWrapOrAllocate(t *testing.T) {
	for _, mode := range []string{"", "0", "true"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SAMWAF_CAP_TRACE", mode)
			r := httptest.NewRequest("GET", "/", nil)
			w := httptest.NewRecorder()
			cfg := model.DefaultCaptchaConfig()
			log := &innerbean.WebLog{}
			if got, trace := beginModernTrace(w, r, cfg, log, "nic", "page"); got != w || trace != nil {
				t.Fatal("disabled trace wraps writer")
			}
			if n := testing.AllocsPerRun(100, func() { beginModernTrace(w, r, cfg, log, "nic", "page") }); n != 0 {
				t.Fatalf("disabled trace allocations: %v", n)
			}
		})
	}
}

func TestModernTraceAssetsMetadataOnly(t *testing.T) {
	t.Setenv("SAMWAF_CAP_TRACE", "1")
	events := captureModernTrace(t)
	s, cfg, log := modernFixture()
	r := httptest.NewRequest("GET", "/samwaf_captcha/v2/assets/unknown.wasm", nil)
	s.handleModernCaptcha(httptest.NewRecorder(), r, log, cfg, "nic")
	if len(*events) != 1 {
		t.Fatal("asset request not traced")
	}
	e := (*events)[0]
	if e.Stage != "asset" || e.Status != 404 || e.Success || e.Reason != "asset_not_found" || e.Bytes == 0 {
		t.Fatalf("asset metadata: %+v", e)
	}
}
