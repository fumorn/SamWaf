package waf_service

import (
	"SamWaf/model"
	"strings"
	"testing"
)

// TestCheckCaptchaPassBindingSSL 验证 passBinding 的保存期跨字段约束：
// "ip+tls" 把通行凭证绑到 TLS 指纹上，站点必须启用 SSL（Ssl==1），
// 否则拒绝保存并给出明确错误；""/"ip"（默认）不受影响。
func TestCheckCaptchaPassBindingSSL(t *testing.T) {
	strict, err := model.NormalizeCaptchaJSON(`{"passBinding":"ip+tls"}`)
	if err != nil {
		t.Fatal(err)
	}
	loose, err := model.NormalizeCaptchaJSON(`{"passBinding":"ip"}`)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := model.NormalizeCaptchaJSON(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		captchaJSON string
		ssl         int
		wantErr     string
	}{
		{"strict+ssl on", strict, 1, ""},
		{"strict+ssl off", strict, 0, "TLS 指纹绑定需要站点启用 SSL"},
		{"strict+ssl weird", strict, 2, "TLS 指纹绑定需要站点启用 SSL"},
		{"strict+ssl negative", strict, -1, "TLS 指纹绑定需要站点启用 SSL"},
		{"loose+ssl off", loose, 0, ""},
		{"default+ssl off", empty, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCaptchaPassBindingSSL(tc.captchaJSON, tc.ssl)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected rejection: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("want error %q, got %v", tc.wantErr, err)
			}
		})
	}
	// 绑定枚举坏值在 Normalize 层就被拒绝，轮不到跨字段校验兜底。
	for _, raw := range []string{`{"passBinding":"ip+tls "}`, `{"passBinding":"IP"}`, `{"passBinding":"mac"}`} {
		if _, err := model.NormalizeCaptchaJSON(raw); err == nil || !strings.Contains(err.Error(), "passBinding") {
			t.Fatalf("invalid passBinding must be rejected explicitly: %s => %v", raw, err)
		}
	}
}
