package wafcaptcha

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"SamWaf/cache"
	"SamWaf/innerbean"
	"SamWaf/model"
)

func modernFixture() (*CaptchaService, model.CaptchaConfig, *innerbean.WebLog) {
	cfg := model.DefaultCaptchaConfig()
	cfg.EngineType = model.CaptchaEngineCapJs
	cfg.IsEnableCaptcha = 1
	cfg.CapJsConfig.ChallengeCount = 1
	cfg.CapJsConfig.ChallengeDifficulty = 1
	return &CaptchaService{cache: cache.InitWafCache()}, cfg, &innerbean.WebLog{HOST_CODE: "site-a", NetSrcIp: "192.0.2.1", SRC_IP: "192.0.2.1"}
}
func modernRequest(t *testing.T, s *CaptchaService, cfg model.CaptchaConfig, log *innerbean.WebLog, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "https://example.test"+captchaPrefix(cfg)+"/v2/"+path, strings.NewReader(body))
	w := httptest.NewRecorder()
	s.HandleCaptchaRequest(w, r, log, cfg, captchaPrefix(cfg), "nic")
	return w
}
func modernPRNG(seed string, size int) string {
	h := uint32(2166136261)
	for _, b := range []byte(seed) {
		h ^= uint32(b)
		h *= 16777619
	}
	out := ""
	for len(out) < size {
		h ^= h << 13
		h ^= h >> 17
		h ^= h << 5
		v := strconv.FormatUint(uint64(h), 16)
		out += strings.Repeat("0", 8-len(v)) + v
	}
	return out[:size]
}
func modernSolve(t *testing.T, s *CaptchaService, cfg model.CaptchaConfig, log *innerbean.WebLog) string {
	t.Helper()
	w := modernRequest(t, s, cfg, log, "challenge", "")
	if w.Code != 200 {
		t.Fatalf("challenge %d: %s", w.Code, w.Body.String())
	}
	var ch struct {
		Token     string                `json:"token"`
		Challenge struct{ C, S, D int } `json:"challenge"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ch); err != nil {
		t.Fatal(err)
	}
	solutions := make([]int, ch.Challenge.C)
	for i := range solutions {
		salt := modernPRNG(ch.Token+strconv.Itoa(i+1), ch.Challenge.S)
		target := modernPRNG(ch.Token+strconv.Itoa(i+1)+"d", ch.Challenge.D)
		for nonce := 0; nonce < 100000; nonce++ {
			sum := sha256.Sum256([]byte(salt + strconv.Itoa(nonce)))
			if strings.HasPrefix(hex.EncodeToString(sum[:]), target) {
				solutions[i] = nonce
				break
			}
			if nonce == 99999 {
				t.Fatal("solver exhausted")
			}
		}
	}
	body, _ := json.Marshal(map[string]any{"token": ch.Token, "solutions": solutions})
	w = modernRequest(t, s, cfg, log, "redeem", string(body))
	if w.Code != 200 {
		t.Fatalf("redeem %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Token == "" {
		t.Fatalf("missing token: %s", w.Body.String())
	}
	return result.Token
}
func TestModernChallengeRedeemValidatePass(t *testing.T) {
	s, cfg, log := modernFixture()
	token := modernSolve(t, s, cfg, log)
	body, _ := json.Marshal(map[string]string{"token": token})
	other := *log
	other.HOST_CODE = "site-b"
	if w := modernRequest(t, s, cfg, &other, "validate", string(body)); w.Code != 403 {
		t.Fatalf("cross-site token accepted: %d", w.Code)
	}
	w := modernRequest(t, s, cfg, log, "validate", string(body))
	if w.Code != 200 {
		t.Fatalf("validate %d: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie attributes: %+v", cookies)
	}
	r := httptest.NewRequest("GET", "https://example.test/", nil)
	r.AddCookie(cookies[0])
	if !HasPass(s.cache, r, log.HOST_CODE, log.NetSrcIp, cfg) {
		t.Fatal("pass not recognized")
	}
	if HasPass(s.cache, r, "site-b", log.NetSrcIp, cfg) {
		t.Fatal("pass crossed sites")
	}
	if w := modernRequest(t, s, cfg, log, "validate", string(body)); w.Code != 403 {
		t.Fatalf("replay accepted: %d", w.Code)
	}
}
func TestModernHTTPBoundaries(t *testing.T) {
	s, cfg, log := modernFixture()
	for _, path := range []string{"/samwaf_captcha/v2/challenge/extra", "/samwaf_captcha/v2/assets/../secret", "/samwaf_captcha/v2/assets/unknown.js"} {
		r := httptest.NewRequest("POST", path, nil)
		if s.IsCaptchaEndpoint(r, cfg) {
			t.Fatalf("non-endpoint admitted %s", path)
		}
	}
	r := httptest.NewRequest("GET", "https://example.test/samwaf_captcha/v2/challenge", nil)
	w := httptest.NewRecorder()
	s.HandleCaptchaRequest(w, r, log, cfg, "", "nic")
	if w.Code != 405 {
		t.Fatalf("GET accepted %d", w.Code)
	}
	r = httptest.NewRequest("POST", "https://example.test/samwaf_captcha/v2/challenge", nil)
	r.Header.Set("Origin", "https://evil.test")
	w = httptest.NewRecorder()
	s.HandleCaptchaRequest(w, r, log, cfg, "", "nic")
	if w.Code != 403 {
		t.Fatalf("origin accepted %d", w.Code)
	}
	w = modernRequest(t, s, cfg, log, "redeem", string(bytes.Repeat([]byte{'x'}, modernBodyLimit+1)))
	if w.Code != 413 {
		t.Fatalf("large body accepted %d", w.Code)
	}
	r = httptest.NewRequest("POST", "https://example.test/samwaf_captcha/challenge", nil)
	w = httptest.NewRecorder()
	s.HandleCaptchaRequest(w, r, log, cfg, "", "nic")
	if w.Code != 409 {
		t.Fatalf("legacy endpoint silently accepted %d", w.Code)
	}
}
func TestModernUnavailableCacheFailsClosed(t *testing.T) {
	s, cfg, log := modernFixture()
	s.cache = legacyPassStore{s.cache}
	if w := modernRequest(t, s, cfg, log, "challenge", ""); w.Code != 503 {
		t.Fatalf("non-atomic cache accepted %d", w.Code)
	}
}

func TestModernSharedSecretCrossInstance(t *testing.T) {
	t.Setenv("SAMWAF_CAP_SECRET", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)))
	s, cfg, log := modernFixture()
	token := modernSolve(t, s, cfg, log)
	other := &CaptchaService{cache: s.cache}
	body, _ := json.Marshal(map[string]string{"token": token})
	if w := modernRequest(t, other, cfg, log, "validate", string(body)); w.Code != 200 {
		t.Fatalf("cross-instance failed %d %s", w.Code, w.Body.String())
	}
	if w := modernRequest(t, s, cfg, log, "validate", string(body)); w.Code != 403 {
		t.Fatalf("cross-instance replay succeeded %d", w.Code)
	}
}

func TestModernSlowBodyReadDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("10-second real socket read deadline")
	}
	s, cfg, log := modernFixture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local := *log
		s.HandleCaptchaRequest(w, r, &local, cfg, captchaPrefix(cfg), "nic")
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(conn, "POST %s/v2/challenge HTTP/1.1\r\nHost: %s\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n", captchaPrefix(cfg), strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("server did not bound upload: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("slow upload status %d", response.StatusCode)
	}
	deadline := time.Now().Add(time.Second)
	for len(s.capSlots) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.capSlots) != 0 {
		t.Fatal("verification slot leaked after timeout")
	}
}
