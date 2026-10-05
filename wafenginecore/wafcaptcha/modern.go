package wafcaptcha

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"SamWaf/cache"
	"SamWaf/common/zlog"
	"SamWaf/innerbean"
	"SamWaf/model"
	"SamWaf/wafenginecore/wafcaptcha/modernassets"
	capserver "github.com/samwafgo/cap_go_server"
)

// Modern endpoints and assets are versioned and embedded together. Legacy disk
// templates (including user-locked templates) cannot silently downgrade them.
const modernBodyLimit = 2 << 20
const modernStorePrefix = "CAP_V2_ENGINE:"

// Trace is deliberately opt-in: the disabled path only checks the environment,
// without a clock read, allocation, response interception, or payload inspection.
// Never add request headers/body, engine tokens, scope, or raw errors here: cache
// and decoder errors can contain credentials or attacker-controlled input.
type modernTraceEvent struct {
	RequestID       string   `json:"request_id"`
	HostCode        string   `json:"host_code"`
	IP              string   `json:"ip"`
	Method          string   `json:"method"`
	Path            string   `json:"path"`
	Stage           string   `json:"stage"`
	Status          int      `json:"status"`
	Bytes           int      `json:"bytes"`
	Success         bool     `json:"success"`
	DurationMS      float64  `json:"duration_ms"`
	Reason          string   `json:"reason,omitempty"`
	ErrorType       string   `json:"error_type,omitempty"`
	EngineReason    string   `json:"engine_reason,omitempty"`
	InstrError      bool     `json:"instr_error"`
	BlockedBy       []string `json:"blocked_by,omitempty"`
	RiskFlags       []string `json:"risk_flags,omitempty"`
	Format          int      `json:"format"`
	Protocols       []string `json:"protocols"`
	Instrumentation bool     `json:"instrumentation"`
}

var modernTraceEmit = func(event modernTraceEvent) { zlog.Info("cap_trace", event) }

type modernTraceWriter struct {
	http.ResponseWriter
	event   modernTraceEvent
	started time.Time
}

// ResponseController must reach the actual server writer's read deadline.
func (w *modernTraceWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *modernTraceWriter) WriteHeader(status int) {
	if w.event.Status == 0 && (status >= 200 || status == http.StatusSwitchingProtocols) {
		w.event.Status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *modernTraceWriter) Write(p []byte) (int, error) {
	if w.event.Status == 0 {
		w.event.Status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.event.Bytes += n
	if err != nil {
		modernTraceFailure(w, "response_write_failed", err)
	}
	return n, err
}
func (w *modernTraceWriter) finish() {
	if w.event.Status == 0 {
		w.event.Status = http.StatusOK
	}
	// Asset ServeContent errors (method/range/precondition) bypass modernError.
	if w.event.Status >= 400 && w.event.Reason == "" {
		w.event.Reason = http.StatusText(w.event.Status)
	}
	w.event.Success = w.event.Status < 400 && w.event.Reason == ""
	w.event.DurationMS = float64(time.Since(w.started)) / float64(time.Millisecond)
	modernTraceEmit(w.event)
}
func beginModernTrace(w http.ResponseWriter, r *http.Request, cfg model.CaptchaConfig, log *innerbean.WebLog, ipMode, stage string) (http.ResponseWriter, *modernTraceWriter) {
	if os.Getenv("SAMWAF_CAP_TRACE") != "1" {
		return w, nil
	}
	if _, exists := w.(*modernTraceWriter); exists {
		return w, nil
	}
	event := modernTraceEvent{Method: r.Method, Path: r.URL.Path, Stage: stage,
		Format: int(cfg.CapJsConfig.Format), Protocols: cfg.CapJsConfig.Protocols,
		Instrumentation: cfg.CapJsConfig.Instrumentation}
	if log != nil {
		event.RequestID, event.HostCode = log.REQ_UUID, log.HOST_CODE
		event.IP = model.GetClientIPByMode(ipMode, log.NetSrcIp, log.SRC_IP)
	} else {
		// The standalone page API has no site context. Never trust IP headers.
		event.IP, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	trace := &modernTraceWriter{ResponseWriter: w, event: event, started: time.Now()}
	return trace, trace
}
func modernTraceFailure(w http.ResponseWriter, reason string, err error) {
	if trace, ok := w.(*modernTraceWriter); ok {
		trace.event.Reason = reason
		if err != nil {
			trace.event.ErrorType = fmt.Sprintf("%T", err)
		}
	}
}
func modernTraceResult(w http.ResponseWriter, result *capserver.ModernRedeem) {
	if trace, ok := w.(*modernTraceWriter); ok && result != nil {
		trace.event.EngineReason = result.Reason
		trace.event.InstrError = result.InstrError
		trace.event.BlockedBy = result.BlockedBy
		trace.event.RiskFlags = result.RiskFlags
	}
}

type capRateWindow struct {
	Until time.Time
	Count int
}
type capStoreAdapter struct{ store cache.AtomicByteStore }

func (a capStoreAdapter) Put(ctx context.Context, k string, v []byte, e time.Time) error {
	return a.store.PutBytes(ctx, modernStorePrefix+k, v, e)
}
func (a capStoreAdapter) PutIfAbsent(ctx context.Context, k string, v []byte, e time.Time) (bool, error) {
	return a.store.PutBytesIfAbsent(ctx, modernStorePrefix+k, v, e)
}
func (a capStoreAdapter) Get(ctx context.Context, k string) ([]byte, bool, error) {
	return a.store.ReadBytes(ctx, modernStorePrefix+k)
}
func (a capStoreAdapter) Take(ctx context.Context, k string) ([]byte, bool, error) {
	return a.store.TakeBytes(ctx, modernStorePrefix+k)
}

func (s *CaptchaService) modernEngine() (*capserver.Engine, error) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if s.capJsCapt != nil {
		return s.capJsCapt, nil
	}
	store, ok := s.cache.(cache.AtomicByteStore)
	if !ok {
		return nil, errors.New("captcha cache lacks atomic operations")
	}
	// An explicit shared secret supports multiple WAF processes. Without it an
	// ephemeral process-local key intentionally expires challenges on restart.
	var secret []byte
	if raw, present := os.LookupEnv("SAMWAF_CAP_SECRET"); present {
		var err error
		secret, err = base64.StdEncoding.DecodeString(raw)
		if err != nil || len(secret) < 32 {
			return nil, errors.New("SAMWAF_CAP_SECRET must be base64 of at least 32 random bytes")
		}
	} else {
		if _, shared := s.cache.(*cache.RedisCache); shared {
			return nil, errors.New("Redis-backed Cap requires a shared SAMWAF_CAP_SECRET")
		}
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
	}
	var key *capserver.RSWKeypair
	if raw, present := os.LookupEnv("SAMWAF_CAP_RSW_KEY"); present {
		if len(raw) > 8192 {
			return nil, errors.New("SAMWAF_CAP_RSW_KEY exceeds limit")
		}
		key = &capserver.RSWKeypair{}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(key); err != nil {
			return nil, errors.New("invalid SAMWAF_CAP_RSW_KEY")
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			return nil, errors.New("invalid SAMWAF_CAP_RSW_KEY")
		}
	}
	engine, err := capserver.NewEngine(capserver.EngineConfig{Secret: secret, Store: capStoreAdapter{store}, RSWKey: key})
	if err != nil {
		return nil, err
	}
	s.capJsCapt = engine
	return engine, nil
}

func captchaPrefix(cfg model.CaptchaConfig) string {
	if cfg.PathPrefix == "" {
		return "/samwaf_captcha"
	}
	return strings.TrimSuffix(cfg.PathPrefix, "/")
}

// IsCaptchaEndpoint never admits an arbitrary user-controlled path prefix.
func (s *CaptchaService) IsCaptchaEndpoint(r *http.Request, cfg model.CaptchaConfig) bool {
	prefix := captchaPrefix(cfg)
	p := r.URL.Path
	if cfg.EngineType == model.CaptchaEngineCapJs {
		switch p {
		case prefix + "/v2/challenge", prefix + "/v2/redeem", prefix + "/v2/validate", prefix + "/challenge", prefix + "/redeem", prefix + "/validate":
			return true
		}
		base := prefix + "/v2/assets/"
		return strings.HasPrefix(p, base) && modernassets.Has(strings.TrimPrefix(p, base))
	}
	switch p {
	case prefix + "/click_basic", prefix + "/verify", prefix + "/favicon.ico", prefix + "/js/gocaptcha.global.js", prefix + "/css/gocaptcha.global.css", prefix + "/css/toastify.css", prefix + "/js/axios.js", prefix + "/js/qs.js", prefix + "/js/toastify.js":
		return true
	}
	return false
}

func (s *CaptchaService) handleModernCaptcha(w http.ResponseWriter, r *http.Request, log *innerbean.WebLog, cfg model.CaptchaConfig, ipMode string) {
	prefix := captchaPrefix(cfg)
	base := prefix + "/v2/assets/"
	if strings.HasPrefix(r.URL.Path, base) {
		w, trace := beginModernTrace(w, r, cfg, log, ipMode, "asset")
		if trace != nil {
			defer trace.finish()
		}
		if !modernassets.Serve(w, r, strings.TrimPrefix(r.URL.Path, base)) {
			modernTraceFailure(w, "asset_not_found", nil)
			http.NotFound(w, r)
		}
		return
	}
	switch r.URL.Path {
	case prefix + "/v2/challenge":
		s.GetCapJsChallenge(w, r, cfg, log, ipMode)
	case prefix + "/v2/redeem":
		s.VerifyCapJsCaptcha(w, r, cfg, log, ipMode)
	case prefix + "/v2/validate":
		s.ValidateCapJsCaptcha(w, r, cfg, log, ipMode)
	case prefix + "/challenge", prefix + "/redeem", prefix + "/validate":
		w, trace := beginModernTrace(w, r, cfg, log, ipMode, strings.TrimPrefix(r.URL.Path, prefix+"/"))
		if trace != nil {
			defer trace.finish()
		}
		modernError(w, http.StatusConflict, "verification_upgrade_reload")
	default:
		w, trace := beginModernTrace(w, r, cfg, log, ipMode, "page")
		if trace != nil {
			defer trace.finish()
		}
		if strings.HasPrefix(r.URL.Path, prefix+"/") {
			modernTraceFailure(w, "not_found", nil)
			http.NotFound(w, r)
			return
		}
		s.showModernCaptcha(w, r, cfg, prefix, log.REQ_UUID)
	}
}

func (s *CaptchaService) showModernCaptcha(w http.ResponseWriter, r *http.Request, cfg model.CaptchaConfig, prefix, requestID string) {
	w, trace := beginModernTrace(w, r, cfg, nil, "", "page")
	if trace != nil {
		trace.event.RequestID = requestID
		defer trace.finish()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	c := cfg.CapJsConfig
	if err := modernassets.Render(w, modernassets.Data{Prefix: prefix, TitleZh: c.InfoTitle.Zh, TitleEn: c.InfoTitle.En, TextZh: c.InfoText.Zh, TextEn: c.InfoText.En, ContactInfo: cfg.ContactInfo, RequestID: requestID, RequestMethod: r.Method}); err != nil {
		modernTraceFailure(w, "page_render_failed", err)
		http.Error(w, "Unable to render verification", http.StatusInternalServerError)
	}
}

func modernError(w http.ResponseWriter, status int, reason string) {
	modernTraceFailure(w, reason, nil)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "reason": reason})
}

// bounded per-process limits supplement (not replace) the site's CC rules.
func (s *CaptchaService) admitModern(scope string) (func(), bool) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if s.capSlots == nil {
		s.capSlots = make(chan struct{}, 16)
	}
	if s.capLimits == nil {
		s.capLimits = make(map[string]capRateWindow)
	}
	now := time.Now()
	if len(s.capLimits) >= 4096 {
		for k, v := range s.capLimits {
			if !now.Before(v.Until) {
				delete(s.capLimits, k)
			}
		}
	}
	v, exists := s.capLimits[scope]
	if !exists && len(s.capLimits) >= 4096 {
		return nil, false
	}
	if !now.Before(v.Until) {
		v = capRateWindow{Until: now.Add(time.Minute)}
	}
	if v.Count >= 120 {
		return nil, false
	}
	select {
	case s.capSlots <- struct{}{}:
	default:
		return nil, false
	}
	v.Count++
	s.capLimits[scope] = v
	return func() { <-s.capSlots }, true
}

func (s *CaptchaService) prepareModern(w http.ResponseWriter, r *http.Request, cfg model.CaptchaConfig, log *innerbean.WebLog, ipMode string) (*capserver.Engine, string, func(), bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Bound chunked/slow uploads before reserving any scarce verification slot.
	// ResponseController traverses supported middleware via Unwrap.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Second))
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		modernError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return nil, "", nil, false
	}
	if log == nil || log.HOST_CODE == "" {
		modernError(w, http.StatusServiceUnavailable, "missing_site_context")
		return nil, "", nil, false
	}
	if err := model.ValidateCaptchaConfig(cfg); err != nil {
		modernTraceFailure(w, "invalid_configuration", err)
		modernError(w, http.StatusServiceUnavailable, "invalid_configuration")
		return nil, "", nil, false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if err != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, r.Host) || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			modernError(w, http.StatusForbidden, "origin_not_allowed")
			return nil, "", nil, false
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		modernError(w, http.StatusForbidden, "origin_not_allowed")
		return nil, "", nil, false
	}
	ip := model.GetClientIPByMode(ipMode, log.NetSrcIp, log.SRC_IP)
	scope := Scope(log.HOST_CODE, ip, cfg)
	if scope == "" {
		modernError(w, http.StatusServiceUnavailable, "missing_client_context")
		return nil, "", nil, false
	}
	done, ok := s.admitModern(scope)
	if !ok {
		w.Header().Set("Retry-After", "60")
		modernError(w, http.StatusTooManyRequests, "verification_rate_limited")
		return nil, "", nil, false
	}
	e, err := s.modernEngine()
	if err != nil {
		modernTraceFailure(w, "verification_unavailable", err)
		s.capWarn.Do(func() {
			zlog.Error("Cap initialization failed; check atomic cache and SAMWAF_CAP_SECRET/SAMWAF_CAP_RSW_KEY configuration")
		})
		done()
		modernError(w, http.StatusServiceUnavailable, "verification_unavailable")
		return nil, "", nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, modernBodyLimit)
	return e, scope, done, true
}

func modernOptions(c model.CaptchaConfig) capserver.Options {
	v := c.CapJsConfig
	protocols := v.Protocols
	if v.Format != 2 {
		protocols = nil
	} // format 1 implicitly selects SHA-256
	// DynamicEscalation 三态映射：nil=默认 true；显式 false 只在指针非 nil 且
	// 解引用为 false 时才传 false——压缩成普通 bool 会让"显式关闭"与"未设置"
	// 无法区分，这正是模型侧用 *bool 的原因。
	// CheckCount 0 交给引擎按默认解析（阻断开=18/关=8；混淆 0→阻断开=6/关=3）。
	dynamicEscalation := v.DynamicEscalation == nil || *v.DynamicEscalation
	return capserver.Options{Format: int(v.Format), Protocols: protocols, ChallengeCount: int(v.ChallengeCount), ChallengeSize: int(v.ChallengeSize), ChallengeDifficulty: int(v.ChallengeDifficulty), ExpiresMs: int64(v.ExpiresMs), HashWXDifficulty: int(v.HashWXDifficulty), HashWXNoncesPerHash: int(v.HashWXNoncesPerHash), HashWXChallengeCount: int(v.HashWXChallengeCount), RSWT: int(v.RSWT), Instrumentation: v.Instrumentation, DynamicEscalation: dynamicEscalation, InstrumentationOptions: capserver.InstrumentationOptions{BlockAutomatedBrowsers: v.InstrumentationOptions.BlockAutomatedBrowsers, ObfuscationLevel: int(v.InstrumentationOptions.ObfuscationLevel), CheckCount: int(v.InstrumentationCheckCount)}}
}

func (s *CaptchaService) GetCapJsChallenge(w http.ResponseWriter, r *http.Request, cfg model.CaptchaConfig, log *innerbean.WebLog, ipMode string) {
	w, trace := beginModernTrace(w, r, cfg, log, ipMode, "challenge")
	if trace != nil {
		defer trace.finish()
	}
	e, scope, done, ok := s.prepareModern(w, r, cfg, log, ipMode)
	if !ok {
		return
	}
	defer done()
	// A challenge has no client-controlled options; drain a bounded optional body.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		modernTraceFailure(w, "request_too_large", err)
		modernError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	ch, err := e.GenerateChallenge(r.Context(), scope, modernOptions(cfg))
	if err != nil {
		modernTraceFailure(w, "challenge_unavailable", err)
		modernError(w, http.StatusServiceUnavailable, "challenge_unavailable")
		return
	}
	writeJSONResponse(w, ch)
}

func (s *CaptchaService) VerifyCapJsCaptcha(w http.ResponseWriter, r *http.Request, cfg model.CaptchaConfig, log *innerbean.WebLog, ipMode string) {
	w, trace := beginModernTrace(w, r, cfg, log, ipMode, "redeem")
	if trace != nil {
		defer trace.finish()
	}
	e, scope, done, ok := s.prepareModern(w, r, cfg, log, ipMode)
	if !ok {
		return
	}
	defer done()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		modernTraceFailure(w, "request_too_large", err)
		modernError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	result, err := e.Redeem(r.Context(), scope, json.RawMessage(body))
	modernTraceResult(w, result)
	if err != nil {
		modernTraceFailure(w, "verification_unavailable", err)
		modernError(w, http.StatusServiceUnavailable, "verification_unavailable")
		return
	}
	if !result.Success {
		modernError(w, http.StatusForbidden, "verification_failed")
		return
	}
	// Do not expose scope (which encodes site/policy binding) or probe verdicts.
	writeJSONResponse(w, map[string]any{"success": true, "token": result.Token, "expires": result.Expires})
}

func (s *CaptchaService) ValidateCapJsCaptcha(w http.ResponseWriter, r *http.Request, cfg model.CaptchaConfig, log *innerbean.WebLog, ipMode string) {
	w, trace := beginModernTrace(w, r, cfg, log, ipMode, "validate")
	if trace != nil {
		defer trace.finish()
	}
	e, scope, done, ok := s.prepareModern(w, r, cfg, log, ipMode)
	if !ok {
		return
	}
	defer done()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		modernTraceFailure(w, "request_too_large", err)
		modernError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err = d.Decode(&req); err != nil || req.Token == "" || len(req.Token) > 512 {
		modernError(w, http.StatusBadRequest, "invalid_body")
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		modernError(w, http.StatusBadRequest, "invalid_body")
		return
	}
	valid, err := e.ValidateToken(r.Context(), scope, req.Token)
	if err != nil {
		modernTraceFailure(w, "verification_unavailable", err)
		modernError(w, http.StatusServiceUnavailable, "verification_unavailable")
		return
	}
	if !valid {
		modernError(w, http.StatusForbidden, "verification_failed")
		return
	}
	ip := model.GetClientIPByMode(ipMode, log.NetSrcIp, log.SRC_IP)
	if _, err = IssuePass(s.cache, w, r, log.HOST_CODE, ip, cfg); err != nil {
		modernTraceFailure(w, "verification_storage_unavailable", err)
		modernError(w, http.StatusServiceUnavailable, "verification_storage_unavailable")
		return
	}
	writeJSONResponse(w, map[string]any{"success": true})
}
