package wafcaptcha

import (
	"SamWaf/cache"
	"SamWaf/enums"
	"SamWaf/model"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Embedding only CacheStore intentionally hides its optional capabilities.
type legacyPassStore struct{ cache.CacheStore }

type failingPassStore struct {
	cache.CacheStore
	cache.AtomicByteStore
	putErr, readErr, takeErr error
}

func (s failingPassStore) PutBytes(ctx context.Context, key string, data []byte, expires time.Time) error {
	if s.putErr != nil {
		return s.putErr
	}
	return s.AtomicByteStore.PutBytes(ctx, key, data, expires)
}
func (s failingPassStore) ReadBytes(ctx context.Context, key string) ([]byte, bool, error) {
	if s.readErr != nil {
		return nil, false, s.readErr
	}
	return s.AtomicByteStore.ReadBytes(ctx, key)
}
func (s failingPassStore) TakeBytes(ctx context.Context, key string) ([]byte, bool, error) {
	if s.takeErr != nil {
		return nil, false, s.takeErr
	}
	return s.AtomicByteStore.TakeBytes(ctx, key)
}

func TestPassScopeBindsSitePolicyAndCanonicalIP(t *testing.T) {
	cfg := model.DefaultCaptchaConfig()
	scope := Scope("site-a", "192.0.2.1", cfg)
	if scope == "" || scope != Scope("site-a", "::ffff:192.0.2.1", cfg) {
		t.Fatal("IPv4 mapped canonicalization failed")
	}
	if Scope("site-a", "2001:db8::1", cfg) != Scope("site-a", "2001:0db8:0000::1", cfg) {
		t.Fatal("IPv6 canonicalization failed")
	}
	if scope == Scope("site-b", "192.0.2.1", cfg) || scope == Scope("site-a", "192.0.2.2", cfg) {
		t.Fatal("scope missing site/IP binding")
	}
	changed := cfg
	changed.CapJsConfig.ChallengeDifficulty++
	if scope == Scope("site-a", "192.0.2.1", changed) {
		t.Fatal("policy change must invalidate pass")
	}
	for _, ip := range []string{"", "not-ip", "192.0.2.1:123", "fe80::1%eth0"} {
		if Scope("site-a", ip, cfg) != "" {
			t.Fatalf("accepted invalid identity %q", ip)
		}
	}
	if Scope("", "192.0.2.1", cfg) != "" {
		t.Fatal("empty site accepted")
	}
}

func TestPassIssueReadAndRevoke(t *testing.T) {
	store := cache.InitWafCache()
	cfg := model.DefaultCaptchaConfig()
	cfg.ExpireTime = 2
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, scheme+"://example.test/verify", nil)
			w := httptest.NewRecorder()
			before := time.Now()
			token, err := IssuePass(store, w, r, "site-a", "192.0.2.1", cfg)
			if err != nil {
				t.Fatal(err)
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatal("missing cookie")
			}
			cookie := cookies[0]
			if cookie.Name != PassCookieName || cookie.Value != token || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" || cookie.Secure != (scheme == "https") || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 7200 {
				t.Fatalf("bad cookie: %+v", cookie)
			}
			if w.Header().Get(PassHeaderName) != token || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing token header or cache control")
			}
			key := PassKey(Scope("site-a", "192.0.2.1", cfg), token)
			if !strings.HasPrefix(key, passNamespace) || strings.Contains(key, token) {
				t.Fatal("cache key leaks token or wrong namespace")
			}
			data, found, err := store.ReadBytes(r.Context(), key)
			if err != nil || !found || strings.Contains(string(data), token) {
				t.Fatal("cache data invalid or leaks bearer token")
			}
			var meta passMetadata
			if err := json.Unmarshal(data, &meta); err != nil {
				t.Fatal(err)
			}
			if delta := time.UnixMilli(meta.ExpiresAt).Sub(before); delta < 2*time.Hour-time.Second || delta > 2*time.Hour+time.Second {
				t.Fatal("wrong pass TTL")
			}
			request := httptest.NewRequest(http.MethodGet, scheme+"://example.test/page?scope=attacker", nil)
			request.AddCookie(cookie)
			if !HasPass(store, request, "site-a", "::ffff:192.0.2.1", cfg) {
				t.Fatal("valid cookie rejected")
			}
			if HasPass(store, request, "site-b", "192.0.2.1", cfg) || HasPass(store, request, "site-a", "192.0.2.2", cfg) {
				t.Fatal("cross-site/IP pass accepted")
			}
			changed := cfg
			changed.EngineType = model.CaptchaEngineCapJs
			if HasPass(store, request, "site-a", "192.0.2.1", changed) {
				t.Fatal("old policy pass accepted")
			}
			request.Header.Del("Cookie")
			request.Header.Set(PassHeaderName, token)
			if !HasPass(store, request, "site-a", "192.0.2.1", cfg) {
				t.Fatal("valid header rejected")
			}
			if err := RevokePass(store, request, "site-b", "192.0.2.1", cfg); err != nil {
				t.Fatal(err)
			}
			if !HasPass(store, request, "site-a", "192.0.2.1", cfg) {
				t.Fatal("cross-site revocation revoked another site")
			}
			if err := RevokePass(store, request, "site-a", "192.0.2.1", cfg); err != nil {
				t.Fatal(err)
			}
			if HasPass(store, request, "site-a", "192.0.2.1", cfg) {
				t.Fatal("revoked pass accepted")
			}
		})
	}
}

func TestPassFailClosed(t *testing.T) {
	store := cache.InitWafCache()
	cfg := model.DefaultCaptchaConfig()
	r := httptest.NewRequest(http.MethodPost, "https://example.test/verify", nil)
	token, err := IssuePass(store, httptest.NewRecorder(), r, "site", "192.0.2.1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(PassHeaderName, token)
	backendErr := errors.New("backend unavailable")
	broken := failingPassStore{CacheStore: store, AtomicByteStore: store, putErr: backendErr, readErr: backendErr, takeErr: backendErr}
	for name, candidate := range map[string]cache.CacheStore{"nil": nil, "legacy": legacyPassStore{store}, "backend": broken} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if token, err := IssuePass(candidate, w, r, "site", "192.0.2.1", cfg); err == nil || token != "" {
				t.Fatal("failed write issued a pass")
			}
			if len(w.Result().Cookies()) != 0 || w.Header().Get(PassHeaderName) != "" {
				t.Fatal("failed write emitted credentials")
			}
			if HasPass(candidate, r, "site", "192.0.2.1", cfg) {
				t.Fatal("cache failure accepted a pass")
			}
			if RevokePass(candidate, r, "site", "192.0.2.1", cfg) == nil {
				t.Fatal("cache failure hidden on revoke")
			}
		})
	}
	key := PassKey(Scope("site", "192.0.2.1", cfg), token)
	for name, data := range map[string][]byte{
		"malformed":   []byte("garbage"),
		"expired":     []byte(`{"version":2,"scope":"` + Scope("site", "192.0.2.1", cfg) + `","expires_at":1}`),
		"wrong_scope": []byte(`{"version":2,"scope":"other","expires_at":9223372036854775807}`),
		"old_version": []byte(`{"version":1,"scope":"` + Scope("site", "192.0.2.1", cfg) + `","expires_at":9223372036854775807}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.PutBytes(r.Context(), key, data, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if HasPass(store, r, "site", "192.0.2.1", cfg) {
				t.Fatal("corrupt/expired metadata accepted")
			}
		})
	}
	cfg.ExpireTime = 0
	if _, err := IssuePass(store, httptest.NewRecorder(), r, "site", "192.0.2.1", cfg); err == nil {
		t.Fatal("zero lifetime accepted")
	}
}

func TestPassLegacyTicketsRejectedAndDualCredentialsRevoked(t *testing.T) {
	store := cache.InitWafCache()
	cfg := model.DefaultCaptchaConfig()
	r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	store.SetWithTTl(enums.CACHE_CAPTCHA_PASS+"old-token"+"192.0.2.1", "ok", time.Hour)
	r.AddCookie(&http.Cookie{Name: PassCookieName, Value: "old-token"})
	r.Header.Set(PassHeaderName, "old-token")
	if HasPass(store, r, "site", "192.0.2.1", cfg) {
		t.Fatal("legacy token accepted")
	}
	if PassKey(Scope("site", "192.0.2.1", cfg), "old-token") != "" {
		t.Fatal("legacy token creates v2 key")
	}
	a, err := IssuePass(store, httptest.NewRecorder(), r, "site", "192.0.2.1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := IssuePass(store, httptest.NewRecorder(), r, "site", "192.0.2.1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("token entropy collision")
	}
	r.Header.Del("Cookie")
	r.AddCookie(&http.Cookie{Name: PassCookieName, Value: a})
	r.Header.Set(PassHeaderName, b)
	if err := RevokePass(store, r, "site", "192.0.2.1", cfg); err != nil {
		t.Fatal(err)
	}
	if HasPass(store, r, "site", "192.0.2.1", cfg) {
		t.Fatal("dual credential revocation incomplete")
	}
}
