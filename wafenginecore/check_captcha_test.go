package wafenginecore

import (
	"SamWaf/cache"
	"SamWaf/global"
	"SamWaf/innerbean"
	"SamWaf/model"
	"SamWaf/wafenginecore/wafcaptcha"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCaptchaGateUsesServerHostAndIPMode(t *testing.T) {
	old := global.GCACHE_WAFCACHE
	global.GCACHE_WAFCACHE = cache.InitWafCache()
	defer func() { global.GCACHE_WAFCACHE = old }()
	cfg := model.DefaultCaptchaConfig()
	cfg.IsEnableCaptcha = 1
	r := httptest.NewRequest(http.MethodGet, "http://attacker.test/private?host_code=site-a&scope=site-a", nil)
	w := httptest.NewRecorder()
	token, err := wafcaptcha.IssuePass(global.GCACHE_WAFCACHE, w, r, "site-a", "192.0.2.1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(wafcaptcha.PassHeaderName, token)
	log := innerbean.WebLog{HOST_CODE: "site-a", NetSrcIp: "192.0.2.1", SRC_IP: "192.0.2.2", URL: "/private"}
	engine := &WafEngine{}
	if !engine.checkCaptchaToken(r, log, cfg, "nic") {
		t.Fatal("valid site and nic IP pass rejected")
	}
	if engine.checkCaptchaToken(r, log, cfg, "proxy") {
		t.Fatal("pass accepted for wrong selected IP")
	}
	log.HOST_CODE = "site-b"
	if engine.checkCaptchaToken(r, log, cfg, "nic") {
		t.Fatal("request-controlled site overrode server-selected site")
	}
}

func TestCaptchaEndpointDispatchWithPassAndGrace(t *testing.T) {
	old := global.GCACHE_WAFCACHE
	global.GCACHE_WAFCACHE = cache.InitWafCache()
	defer func() { global.GCACHE_WAFCACHE = old }()
	cfg := model.DefaultCaptchaConfig()
	cfg.EngineType = model.CaptchaEngineCapJs
	cfg.PathPrefix = "/private-captcha"
	ip, host := "192.0.2.1", "site-a"
	base := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	token, err := wafcaptcha.IssuePass(global.GCACHE_WAFCACHE, httptest.NewRecorder(), base, host, ip, cfg)
	if err != nil {
		t.Fatal(err)
	}
	GrantCCCaptchaGrace(host, ip, cfg)
	log := &innerbean.WebLog{HOST_CODE: host, SRC_IP: ip, NetSrcIp: ip}
	for _, endpoint := range []string{"challenge", "redeem", "validate"} {
		// Legacy exact routes are intentionally served as explicit reload errors,
		// not proxied, even after the challenge-required marker has been cleared.
		r := httptest.NewRequest(http.MethodPost, "http://example.test/private-captcha/"+endpoint, nil)
		r.Header.Set(wafcaptcha.PassHeaderName, token)
		w := httptest.NewRecorder()
		if !hasCaptchaPass(r, host, ip, cfg) || !isCCCaptchaInGrace(host, ip, cfg) {
			t.Fatal("invalid pass/grace fixture")
		}
		if !(&WafEngine{}).handleCaptchaEndpoint(w, r, log, cfg, "nic") {
			t.Fatal("internal endpoint fell through to origin")
		}
		if w.Code != http.StatusConflict {
			t.Fatalf("expected reload error, got %d", w.Code)
		}
	}
	for _, path := range []string{"/private-captcha/challenge-extra", "/private-captcha/v2/validate/extra", "/private-captcha/admin", "/private-captcha/verify"} {
		r := httptest.NewRequest(http.MethodPost, "http://example.test"+path, nil)
		if (&WafEngine{}).handleCaptchaEndpoint(httptest.NewRecorder(), r, log, cfg, "nic") {
			t.Fatalf("prefix or mismatched engine routed internally: %s", path)
		}
	}
}

func TestCCCaptchaGraceBoundToPolicyAndSite(t *testing.T) {
	old := global.GCACHE_WAFCACHE
	global.GCACHE_WAFCACHE = cache.InitWafCache()
	defer func() { global.GCACHE_WAFCACHE = old }()
	cfg := model.DefaultCaptchaConfig()
	GrantCCCaptchaGrace("site-a", "192.0.2.1", cfg)
	if !isCCCaptchaInGrace("site-a", "::ffff:192.0.2.1", cfg) {
		t.Fatal("canonical IP grace missing")
	}
	if isCCCaptchaInGrace("site-b", "192.0.2.1", cfg) {
		t.Fatal("cross-site grace reuse")
	}
	cfg.CapJsConfig.ChallengeDifficulty++
	if isCCCaptchaInGrace("site-a", "192.0.2.1", cfg) {
		t.Fatal("changed policy reused old grace")
	}
}

func TestCaptchaPassDoesNotBypassCCDenial(t *testing.T) {
	old := global.GCACHE_WAFCACHE
	global.GCACHE_WAFCACHE = cache.InitWafCache()
	defer func() { global.GCACHE_WAFCACHE = old }()
	ip := "192.0.2.1"
	r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	r.Header.Set(wafcaptcha.PassHeaderName, issueCCPass(t, "hostcode", ip))
	if !hasCaptchaPass(r, "hostcode", ip, model.DefaultCaptchaConfig()) {
		t.Fatal("invalid pass fixture")
	}
	result, stop := (&WafEngine{}).applyCCAction(r, newActionRule(model.CCActionDeny), &innerbean.WebLog{SRC_IP: ip}, newActionHost(), ip, "hostcode")
	if !stop || !result.IsBlock {
		t.Fatal("human verification skipped independent CC denial")
	}
}

type failingRevokeCache struct{ cache.CacheStore }

type failedAtomicRevokeCache struct {
	cache.CacheStore
	cache.AtomicByteStore
}

func (s failedAtomicRevokeCache) TakeBytes(context.Context, string) ([]byte, bool, error) {
	return nil, false, errors.New("cache unavailable")
}

func TestCCCaptchaRevokeErrorBlocksStalePass(t *testing.T) {
	old := global.GCACHE_WAFCACHE
	store := cache.InitWafCache()
	global.GCACHE_WAFCACHE = store
	defer func() { global.GCACHE_WAFCACHE = old }()
	ip := "192.0.2.1"
	token := issueCCPass(t, "hostcode", ip)
	global.GCACHE_WAFCACHE = failedAtomicRevokeCache{CacheStore: store, AtomicByteStore: store}
	r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	r.Header.Set(wafcaptcha.PassHeaderName, token)
	cfg := model.DefaultCaptchaConfig()
	if !hasCaptchaPass(r, "hostcode", ip, cfg) {
		t.Fatal("invalid stale pass fixture")
	}
	result, _ := (&WafEngine{}).applyCCAction(r, newActionRule(model.CCActionCaptcha), &innerbean.WebLog{SRC_IP: ip}, newActionHost(), ip, "hostcode")
	if !result.IsBlock {
		t.Fatal("revocation failure accepted stale pass")
	}
}

func TestCCCaptchaRejectsInvalidPolicy(t *testing.T) {
	old := global.GCACHE_WAFCACHE
	global.GCACHE_WAFCACHE = cache.InitWafCache()
	defer func() { global.GCACHE_WAFCACHE = old }()
	host := newActionHost()
	host.Host.CaptchaJSON = `{"is_enable_captcha":1,"cap_js_config":{"challengeCount":-1}}`
	r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	log := &innerbean.WebLog{SRC_IP: "192.0.2.1"}
	result, _ := (&WafEngine{}).applyCCAction(r, newActionRule(model.CCActionCaptcha), log, host, log.SRC_IP, host.Host.Code)
	if !result.IsBlock {
		t.Fatal("invalid policy silently disabled CAPTCHA")
	}
	if err := (&WafEngine{}).applyEmergencyCaptcha(r, log, host); err == nil {
		t.Fatal("invalid policy silently disabled emergency CAPTCHA")
	}
}

func TestCCCaptchaFailsClosedWithoutAtomicStore(t *testing.T) {
	old := global.GCACHE_WAFCACHE
	global.GCACHE_WAFCACHE = failingRevokeCache{cache.InitWafCache()}
	defer func() { global.GCACHE_WAFCACHE = old }()
	r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	log := &innerbean.WebLog{SRC_IP: "192.0.2.1"}
	host := newActionHost()
	result, stop := (&WafEngine{}).applyCCAction(r, newActionRule(model.CCActionCaptcha), log, host, log.SRC_IP, host.Host.Code)
	if !stop || !result.IsBlock {
		t.Fatal("CC cache capability failure did not block")
	}
	if err := (&WafEngine{}).applyEmergencyCaptcha(r, log, host); err == nil {
		t.Fatal("emergency cache capability failure hidden")
	}
}
