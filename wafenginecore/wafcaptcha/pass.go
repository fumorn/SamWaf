package wafcaptcha

import (
	"SamWaf/cache"
	"SamWaf/model"
	"SamWaf/wafenginecore/wafhttpserver"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

const (
	PassCookieName = "samwaf_captcha_token"
	PassHeaderName = "X-SamWaf-Captcha-Token"
	passNamespace  = "CACHE_CAPTCHA_PASS_v2:"
)

var ErrPassStoreUnavailable = errors.New("captcha pass requires an atomic byte store")

// Scope binds a proof to a server-selected site, the complete effective policy,
// and a canonical client IP. Never supply a host or policy taken from request
// parameters. Empty/invalid identities fail closed with an empty scope.
func Scope(hostCode, clientIP string, cfg model.CaptchaConfig) string {
	if strings.TrimSpace(hostCode) == "" {
		return ""
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(clientIP))
	if err != nil || ip.Zone() != "" {
		return ""
	}
	// ""/"ip" 是同一个绑定语义的两种写法（默认仅绑定客户端 IP）。哈希前先归一，
	// 管理员在等价写法之间来回切换时不应让全站存量凭证集体失效。
	if cfg.PassBinding == "" {
		cfg.PassBinding = model.CaptchaPassBindingIP
	}
	data, err := json.Marshal(struct {
		Version int                 `json:"version"`
		Host    string              `json:"host"`
		IP      string              `json:"ip"`
		Policy  model.CaptchaConfig `json:"policy"`
	}{2, hostCode, ip.Unmap().String(), cfg})
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// PassKey contains only hashes, never the bearer token. Legacy tokens/keys are
// deliberately unsupported; existing clients must complete a fresh challenge.
func PassKey(scope, token string) string {
	if len(scope) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(scope); err != nil {
		return ""
	}
	if !strings.HasPrefix(token, "v2.") || len(token) != 46 {
		return ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token[3:])
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != token[3:] {
		return ""
	}
	hash := sha256.Sum256([]byte(token))
	return passNamespace + scope + ":" + hex.EncodeToString(hash[:])
}

type passMetadata struct {
	Version int    `json:"version"`
	Scope   string `json:"scope"`
	// Fingerprint 存的是 TLS 指纹的 sha256 十六进制哈希，绝不存指纹原文。
	// 仅 passBinding="ip+tls" 时参与校验；其余模式下只是信息性记录。
	Fingerprint string `json:"fingerprint,omitempty"`
	ExpiresAt   int64  `json:"expires_at"`
}

// passBindsFingerprint 报告该策略是否把通行凭证绑定到请求的 TLS 指纹。
func passBindsFingerprint(cfg model.CaptchaConfig) bool {
	return cfg.PassBinding == model.CaptchaPassBindingIPTLS
}

// fingerprintHash 返回当前请求 TLS 指纹的 sha256 哈希（hex）；请求无 TLS 指纹
// （明文 HTTP、采集失败等）返回空串。哈希化避免指纹原文进入缓存后端/日志。
func fingerprintHash(r *http.Request) string {
	fp := wafhttpserver.RequestFingerprint(r)
	if fp == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(fp))
	return hex.EncodeToString(sum[:])
}

// IssuePass only exposes a token after a successful error-returning cache write.
// A proof of humanity is not authorization to skip any other WAF protection.
func IssuePass(store cache.CacheStore, w http.ResponseWriter, r *http.Request, hostCode, clientIP string, cfg model.CaptchaConfig) (string, error) {
	bytes, ok := store.(cache.AtomicByteStore)
	if !ok || bytes == nil {
		return "", ErrPassStoreUnavailable
	}
	scope := Scope(hostCode, clientIP, cfg)
	if scope == "" || w == nil || r == nil {
		return "", errors.New("invalid captcha pass identity or request")
	}
	// Check before multiplying to prevent duration/MaxAge integer overflow.
	hours := int64(cfg.ExpireTime)
	if hours <= 0 || hours > int64((1<<63-1)/int64(time.Hour)) {
		return "", errors.New("invalid captcha pass lifetime")
	}
	ttl := time.Duration(hours) * time.Hour
	seconds := int64(ttl / time.Second)
	if int64(int(seconds)) != seconds {
		return "", errors.New("captcha pass lifetime exceeds cookie limit")
	}
	// TLS 绑定 fail closed：策略要求把凭证绑到 TLS 指纹，而当前请求没有指纹
	// 可绑（站点实际跑在明文 HTTP 上、采集失败等）时，宁可签发失败，也不发
	// 一张必然验不过或绑定维度缺失的凭证——错误会一路返回到管理端/挑战端。
	fingerprint := fingerprintHash(r)
	if passBindsFingerprint(cfg) && fingerprint == "" {
		return "", errors.New("captcha pass binding requires a TLS fingerprint")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate captcha pass: %w", err)
	}
	token := "v2." + base64.RawURLEncoding.EncodeToString(random[:])
	expires := time.Now().Add(ttl).Truncate(time.Millisecond)
	// 非绑定模式下指纹存在时也照常入库（信息性记录，校验不使用，见 passMetadata）。
	data, err := json.Marshal(passMetadata{Version: 2, Scope: scope, Fingerprint: fingerprint, ExpiresAt: expires.UnixMilli()})
	if err != nil {
		return "", err
	}
	if err := bytes.PutBytes(r.Context(), PassKey(scope, token), data, expires); err != nil {
		return "", fmt.Errorf("store captcha pass: %w", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name: PassCookieName, Value: token, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
		MaxAge: int(seconds), Expires: expires,
	})
	w.Header().Set(PassHeaderName, token)
	w.Header().Set("Cache-Control", "no-store")
	return token, nil
}

// HasPass accepts cookie or header credentials, bound to the effective site and
// policy. Cache absence, corruption, expiry and backend errors all fail closed.
func HasPass(store cache.CacheStore, r *http.Request, hostCode, clientIP string, cfg model.CaptchaConfig) bool {
	bytes, ok := store.(cache.AtomicByteStore)
	if !ok || bytes == nil || r == nil {
		return false
	}
	scope := Scope(hostCode, clientIP, cfg)
	if scope == "" {
		return false
	}
	// TLS 绑定 fail closed：绑定模式下当前请求没有指纹可比对（明文 HTTP、采集
	// 失败等）时直接判无凭证；存值与当前指纹哈希不一致同样拒绝。cookie 与
	// header token 两条呈现路径走同一份元数据校验，绑定对两者同等生效。
	// ""/"ip" 模式不比对指纹，行为与引入绑定前完全一致。
	bindsFingerprint := passBindsFingerprint(cfg)
	wantFingerprint := ""
	if bindsFingerprint {
		if wantFingerprint = fingerprintHash(r); wantFingerprint == "" {
			return false
		}
	}
	for _, token := range requestPassTokens(r) {
		key := PassKey(scope, token)
		if key == "" {
			continue
		}
		data, found, err := bytes.ReadBytes(r.Context(), key)
		if err != nil {
			return false
		}
		var meta passMetadata
		if found && json.Unmarshal(data, &meta) == nil && meta.Version == 2 && meta.Scope == scope && meta.ExpiresAt > time.Now().UnixMilli() && (!bindsFingerprint || meta.Fingerprint == wantFingerprint) {
			return true
		}
	}
	return false
}

// RevokePass atomically consumes all presented credentials in this scope only.
// Callers must not grant grace or continue on revocation failure.
func RevokePass(store cache.CacheStore, r *http.Request, hostCode, clientIP string, cfg model.CaptchaConfig) error {
	bytes, ok := store.(cache.AtomicByteStore)
	if !ok || bytes == nil {
		return ErrPassStoreUnavailable
	}
	scope := Scope(hostCode, clientIP, cfg)
	if scope == "" || r == nil {
		return errors.New("invalid captcha pass identity or request")
	}
	var errs []error
	for _, token := range requestPassTokens(r) {
		if key := PassKey(scope, token); key != "" {
			if _, _, err := bytes.TakeBytes(r.Context(), key); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func requestPassTokens(r *http.Request) []string {
	var tokens []string
	if cookie, err := r.Cookie(PassCookieName); err == nil && cookie.Value != "" {
		tokens = append(tokens, cookie.Value)
	}
	if token := r.Header.Get(PassHeaderName); token != "" && (len(tokens) == 0 || tokens[0] != token) {
		tokens = append(tokens, token)
	}
	return tokens
}
