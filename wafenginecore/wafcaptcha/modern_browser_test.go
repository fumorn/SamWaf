package wafcaptcha

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

// Real embedded widget -> Go engine -> scoped WAF cookie -> protected origin.
// External HTTP requests are blocked by the development-only CDP driver.
func TestModernBrowserEndToEnd(t *testing.T) {
	if os.Getenv("SAMWAF_CAP_BROWSER_TEST") != "1" {
		t.Skip("opt-in real Chrome integration")
	}
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"sha256", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			s, cfg, log := modernFixture()
			cfg.PathPrefix = "/~captcha.v2"
			cfg.CapJsConfig.Instrumentation = true
			if kind == "mixed" {
				cfg.CapJsConfig.Format = 2
				cfg.CapJsConfig.Protocols = []string{"sha256-pow", "hashwx", "rsw", "instrumentation"}
				cfg.CapJsConfig.HashWXDifficulty = 8
				cfg.CapJsConfig.HashWXChallengeCount = 2
				cfg.CapJsConfig.RSWT = 10000
			}
			var reached atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				local := *log
				if s.IsCaptchaEndpoint(r, cfg) {
					s.HandleCaptchaRequest(w, r, &local, cfg, captchaPrefix(cfg), "nic")
					return
				}
				if HasPass(s.cache, r, local.HOST_CODE, local.NetSrcIp, cfg) {
					reached.Store(true)
					w.Header().Set("Content-Type", "text/html")
					io.WriteString(w, "<!doctype html><p>E2E_ORIGIN_REACHED</p>")
					return
				}
				s.HandleCaptchaRequest(w, r, &local, cfg, captchaPrefix(cfg), "nic")
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, node, "browser_e2e.mjs", chrome, srv.URL+"/protected?retain=1", t.TempDir())
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("real browser: %v\n%s", err, out)
			}
			if !reached.Load() {
				t.Fatalf("never reached protected origin: %s", out)
			}
			t.Logf("%s", out)
		})
	}
}
