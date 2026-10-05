package wafcaptcha

import (
	"SamWaf/cache"
	"SamWaf/model"
	"SamWaf/wafenginecore/wafhttpserver"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bindingRequest 构造带受控 TLS 指纹的请求。契约注入口 wafhttpserver.WithFingerprint
// 仅供测试使用：生产路径的指纹来自真实 TLS 连接（RequestFingerprint 采集）。
// httptest.NewRequest 不设置 r.TLS，未注入指纹即等价于"无指纹"（明文 HTTP 语义）。
func bindingRequest(fp string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://example.test/verify", nil)
	return r.WithContext(wafhttpserver.WithFingerprint(r.Context(), fp))
}

// credentialRequest 把凭证按 cookie 或 header 两条呈现路径之一附到请求上。
func credentialRequest(fp, path, token, presentation string) *http.Request {
	r := bindingRequest(fp)
	r.URL.Path = path
	if presentation == "cookie" {
		r.AddCookie(&http.Cookie{Name: PassCookieName, Value: token})
	} else {
		r.Header.Set(PassHeaderName, token)
	}
	return r
}

func TestPassBindingScopeTreatsEmptyAndIPEquivalent(t *testing.T) {
	cfg := model.DefaultCaptchaConfig()
	loose := Scope("site-a", "192.0.2.1", cfg) // PassBinding ""（默认）
	cfg.PassBinding = model.CaptchaPassBindingIP
	if Scope("site-a", "192.0.2.1", cfg) != loose {
		t.Fatal(`passBinding "" and "ip" must hash identically`)
	}
	cfg.PassBinding = model.CaptchaPassBindingIPTLS
	if Scope("site-a", "192.0.2.1", cfg) == loose {
		t.Fatal(`"ip+tls" must change the policy hash`)
	}
}

func TestPassBindingFingerprintMatchPasses(t *testing.T) {
	store := cache.InitWafCache()
	cfg := model.DefaultCaptchaConfig()
	cfg.ExpireTime = 2
	cfg.PassBinding = model.CaptchaPassBindingIPTLS

	w := httptest.NewRecorder()
	token, err := IssuePass(store, w, bindingRequest("fp-A"), "site-a", "192.0.2.1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w.Header().Get(PassHeaderName) != token {
		t.Fatal("binding issue did not present the token")
	}
	// 元数据只存 sha256 哈希，绝不存指纹原文。
	key := PassKey(Scope("site-a", "192.0.2.1", cfg), token)
	data, found, err := store.ReadBytes(context.Background(), key)
	if err != nil || !found {
		t.Fatalf("pass metadata unreadable: %v (%v)", err, found)
	}
	if strings.Contains(string(data), "fp-A") {
		t.Fatal("metadata leaks raw fingerprint")
	}
	sum := sha256.Sum256([]byte("fp-A"))
	if !strings.Contains(string(data), hex.EncodeToString(sum[:])) {
		t.Fatal("metadata missing fingerprint hash")
	}
	// cookie 与 header 两条呈现路径：指纹匹配时同等通过。
	for _, presentation := range []string{"cookie", "header"} {
		r := credentialRequest("fp-A", "/page", token, presentation)
		if !HasPass(store, r, "site-a", "192.0.2.1", cfg) {
			t.Fatalf("%s presentation rejected with matching fingerprint", presentation)
		}
	}
}

func TestPassBindingFingerprintMismatchRejected(t *testing.T) {
	store := cache.InitWafCache()
	cfg := model.DefaultCaptchaConfig()
	cfg.ExpireTime = 2
	cfg.PassBinding = model.CaptchaPassBindingIPTLS
	token, err := IssuePass(store, httptest.NewRecorder(), bindingRequest("fp-A"), "site-a", "192.0.2.1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, presentation := range []string{"cookie", "header"} {
		// 换 TLS 指纹（换浏览器/TLS 栈复用凭证）：cookie 与 header 两条路径都拒绝。
		if HasPass(store, credentialRequest("fp-B", "/page", token, presentation), "site-a", "192.0.2.1", cfg) {
			t.Fatalf("%s presentation accepted mismatched fingerprint", presentation)
		}
		// 无指纹（明文 HTTP 语义）：两条路径同样拒绝。
		plain := httptest.NewRequest(http.MethodGet, "https://example.test/page", nil)
		if presentation == "cookie" {
			plain.AddCookie(&http.Cookie{Name: PassCookieName, Value: token})
		} else {
			plain.Header.Set(PassHeaderName, token)
		}
		if HasPass(store, plain, "site-a", "192.0.2.1", cfg) {
			t.Fatalf("%s presentation accepted a fingerprint-less request", presentation)
		}
	}
	// 指纹相同但站点/IP 换了：仍然拒绝（原绑定语义不变）。
	if HasPass(store, credentialRequest("fp-A", "/page", token, "header"), "site-b", "192.0.2.1", cfg) ||
		HasPass(store, credentialRequest("fp-A", "/page", token, "header"), "site-a", "192.0.2.2", cfg) {
		t.Fatal("cross-site/IP pass accepted under binding")
	}
}

func TestPassBindingFailClosedWithoutFingerprint(t *testing.T) {
	store := cache.InitWafCache()
	cfg := model.DefaultCaptchaConfig()
	cfg.ExpireTime = 2
	cfg.PassBinding = model.CaptchaPassBindingIPTLS
	// 签发侧 fail closed：绑定模式但请求无指纹 → 返回错误、不发 cookie/token。
	r := httptest.NewRequest(http.MethodPost, "http://example.test/verify", nil)
	w := httptest.NewRecorder()
	token, err := IssuePass(store, w, r, "site-a", "192.0.2.1", cfg)
	if err == nil || token != "" {
		t.Fatal("binding mode issued a pass without a fingerprint")
	}
	if len(w.Result().Cookies()) != 0 || w.Header().Get(PassHeaderName) != "" {
		t.Fatal("binding mode emitted credentials without a fingerprint")
	}
	// 校验侧 fail closed：有指纹签发的凭证，拿到无指纹请求上必须失败。
	token, err = IssuePass(store, httptest.NewRecorder(), bindingRequest("fp-A"), "site-a", "192.0.2.1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest(http.MethodGet, "http://example.test/page", nil)
	r.Header.Set(PassHeaderName, token)
	if HasPass(store, r, "site-a", "192.0.2.1", cfg) {
		t.Fatal("binding mode accepted a pass on a fingerprint-less request")
	}
	// 旧形态元数据（无 fingerprint 字段）在绑定模式下同样拒绝，不当"无绑定"放行。
	key := PassKey(Scope("site-a", "192.0.2.1", cfg), token)
	legacy := []byte(`{"version":2,"scope":"` + Scope("site-a", "192.0.2.1", cfg) + `","expires_at":` +
		strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10) + `}`)
	if err := store.PutBytes(context.Background(), key, legacy, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if HasPass(store, credentialRequest("fp-A", "/page", token, "cookie"), "site-a", "192.0.2.1", cfg) {
		t.Fatal("legacy metadata accepted under binding mode")
	}
}

func TestPassBindingLooseModeUnaffected(t *testing.T) {
	store := cache.InitWafCache()
	for _, binding := range []string{"", model.CaptchaPassBindingIP} {
		cfg := model.DefaultCaptchaConfig()
		cfg.ExpireTime = 2
		cfg.PassBinding = binding
		// 无指纹（明文 HTTP）照常签发与验证——""/"ip" 现行为完全不变。
		r := httptest.NewRequest(http.MethodPost, "http://example.test/verify", nil)
		token, err := IssuePass(store, httptest.NewRecorder(), r, "site-a", "192.0.2.1", cfg)
		if err != nil {
			t.Fatalf("binding=%q issue failed: %v", binding, err)
		}
		r.Header.Set(PassHeaderName, token)
		if !HasPass(store, r, "site-a", "192.0.2.1", cfg) {
			t.Fatalf("binding=%q rejected its own pass", binding)
		}
		// 宽松模式不比对指纹：换指纹的请求同样通过。
		if !HasPass(store, credentialRequest("fp-Z", "/page", token, "cookie"), "site-a", "192.0.2.1", cfg) {
			t.Fatalf("binding=%q must not compare fingerprints", binding)
		}
	}
	// ""/"ip" 等价：用 "" 签发的凭证在 "ip" 下同样有效（等价写法不互相作废）。
	loose := model.DefaultCaptchaConfig()
	loose.ExpireTime = 2
	token, err := IssuePass(store, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://example.test/verify", nil), "site-a", "192.0.2.1", loose)
	if err != nil {
		t.Fatal(err)
	}
	plainCookie := httptest.NewRequest(http.MethodGet, "http://example.test/page", nil)
	plainCookie.AddCookie(&http.Cookie{Name: PassCookieName, Value: token})
	loose.PassBinding = model.CaptchaPassBindingIP
	if !HasPass(store, plainCookie, "site-a", "192.0.2.1", loose) {
		t.Fatal(`pass issued under "" rejected under "ip"`)
	}
	// 绑定收紧后旧凭证立即失效（策略哈希含 passBinding）。
	loose.PassBinding = model.CaptchaPassBindingIPTLS
	if HasPass(store, plainCookie, "site-a", "192.0.2.1", loose) {
		t.Fatal("loose pass survived tightening to ip+tls")
	}
}
