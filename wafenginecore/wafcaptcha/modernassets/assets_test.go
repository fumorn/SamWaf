package modernassets

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestRenderEscapesEveryTextField(t *testing.T) {
	attack := `</script><script>alert("x")</script><img src=x onerror=alert(1)>"'&` + "\u2028\u2029"
	var out bytes.Buffer
	err := Render(&out, Data{Prefix: "/custom/captcha", TitleZh: attack, TitleEn: attack, TextZh: attack, TextEn: attack, ContactInfo: attack, RequestID: attack, RequestMethod: attack})
	if err != nil {
		t.Fatal(err)
	}
	body := out.String()
	for _, unsafe := range []string{attack, "<img", "<script>alert", `data-request-method="</script>`} {
		if strings.Contains(body, unsafe) {
			t.Fatalf("unsafe unescaped value: %q", unsafe)
		}
	}
	if !strings.Contains(body, "&lt;img") || !strings.Contains(body, "&#34;") {
		t.Fatal("missing contextual escaping")
	}
	if n := strings.Count(body, "<script"); n != 1 {
		t.Fatalf("expected sole external app script, got %d", n)
	}
	if !strings.Contains(body, `<script defer src="/custom/captcha/v2/assets/app.js"></script>`) {
		t.Fatal("script is not a local external script")
	}
	// The page must keep the original SamWaf challenge-page look, not a redesign.
	for _, signature := range []string{"background: #EBF3FB", `id="lang-switch" class="lang-switch"`, `id="info-title"`, `id="info-text"`, "cap-widget", "success-message", "toast", "识别码 / Ref:"} {
		if !strings.Contains(body, signature) {
			t.Fatalf("original challenge-page signature missing: %q", signature)
		}
	}
	// Parse the DOM: regex would mistake escaped attack text INSIDE a quoted
	// data-* value for an executable event attribute.
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	scripts := 0
	var inspect func(*html.Node)
	inspect = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "img" {
				t.Fatal("attack created an image element")
			}
			attrs := make(map[string]string)
			for _, attr := range n.Attr {
				attrs[attr.Key] = attr.Val
				if strings.HasPrefix(strings.ToLower(attr.Key), "on") {
					t.Fatalf("executable event attribute: %s", attr.Key)
				}
			}
			if n.Data == "script" {
				scripts++
				if attrs["src"] != "/custom/captcha/v2/assets/app.js" || n.FirstChild != nil {
					t.Fatal("unexpected script content or source")
				}
			}
			if n.Data == "cap-widget" && attrs["data-cap-api-endpoint"] != "/custom/captcha/v2/" {
				t.Fatalf("cap widget endpoint = %q", attrs["data-cap-api-endpoint"])
			}
			if attrs["id"] == "challenge" {
				for _, key := range []string{"data-title-zh", "data-title-en", "data-request-method"} {
					if attrs[key] != attack {
						t.Fatalf("attribute %s did not safely round-trip", key)
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			inspect(child)
		}
	}
	inspect(doc)
	if scripts != 1 {
		t.Fatalf("parsed %d script elements, expected one", scripts)
	}
}

func TestRenderPrefixesAndMethod(t *testing.T) {
	for _, prefix := range []string{"https://evil.example", "//evil.example", `/a\b`, "/a/../b", "/a/./b", "/..", "/.", "/a//b", "//", "/a//", "/a?b", "/a#b", "/%2fexample", "/a\n", `/x\" onload=\"alert(1)`} {
		var out bytes.Buffer
		if err := Render(&out, Data{Prefix: prefix}); err == nil || out.Len() != 0 {
			t.Errorf("accepted prefix %q", prefix)
		}
	}
	for _, prefix := range []string{"", "/", "/captcha", "/custom/cap_2-/", "/captcha.v2", "/~captcha"} {
		var out bytes.Buffer
		if err := Render(&out, Data{Prefix: prefix, RequestMethod: "POST"}); err != nil {
			t.Errorf("valid prefix %q: %v", prefix, err)
		}
		if !strings.Contains(out.String(), `data-request-method="POST"`) {
			t.Fatal("original method was lost")
		}
	}
}

func TestServeWhitelist(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../assets.go", "static/cap.js", "/cap.js", "CAP.js", "cap.js/", "cap.js?x=1", "%63ap.js", "a/../cap.js", `a\cap.js`, "challenge.html", "manifest.json", "licenses/CAP.txt", "cap.js\x00"} {
		w := httptest.NewRecorder()
		if Has(name) || Serve(w, httptest.NewRequest("GET", "/", nil), name) {
			t.Errorf("not rejected: %q", name)
		}
		if w.Body.Len() != 0 || len(w.Header()) != 0 {
			t.Errorf("unknown asset wrote response: %q", name)
		}
	}
	for name, mime := range public {
		w := httptest.NewRecorder()
		if !Has(name) || !Serve(w, httptest.NewRequest("GET", "/v2/assets/"+name, nil), name) {
			t.Fatalf("missing asset %s", name)
		}
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("asset %s returned %d", name, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != mime {
			t.Errorf("%s MIME=%q, want %q", name, got, mime)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("missing nosniff")
		}
		etag := w.Header().Get("ETag")
		if etag == "" {
			t.Fatal("missing content hash ETag")
		}
		r := httptest.NewRequest("GET", "/v2/assets/"+name, nil)
		r.Header.Set("If-None-Match", etag)
		cached := httptest.NewRecorder()
		Serve(cached, r, name)
		if cached.Code != 304 || cached.Body.Len() != 0 {
			t.Fatal("conditional GET failed")
		}
		if strings.HasSuffix(name, ".wasm") && !bytes.HasPrefix(w.Body.Bytes(), []byte{0, 'a', 's', 'm'}) {
			t.Fatal("invalid WASM header")
		}
		head := httptest.NewRecorder()
		Serve(head, httptest.NewRequest("HEAD", "/", nil), name)
		if head.Code != 200 || head.Body.Len() != 0 {
			t.Fatal("HEAD emitted body")
		}
		post := httptest.NewRecorder()
		Serve(post, httptest.NewRequest("POST", "/", nil), name)
		if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
			t.Fatal("POST should be rejected")
		}
	}
}

func TestPinnedManifest(t *testing.T) {
	manifest, err := FS.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Upstream struct{ Commit, Core, Widget, Wasm string }
		Files    map[string]string
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.Upstream.Commit != "af624b9f15cb03f1f8194fb4c9f262ac34984923" || m.Upstream.Core != "0.1.3" || m.Upstream.Widget != "0.1.58" || m.Upstream.Wasm != "0.0.8" {
		t.Fatal("unexpected upstream versions")
	}
	for name := range public {
		if _, ok := m.Files["static/"+name]; !ok {
			t.Errorf("public asset has no hash: %s", name)
		}
	}
	for name, expected := range m.Files {
		data, err := FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if actual := fmt.Sprintf("%x", sha256.Sum256(data)); actual != expected {
			t.Errorf("hash mismatch for %s", name)
		}
	}
	for _, name := range []string{"static/cap.js", "static/worker.js", "static/app.js", "challenge.html"} {
		data, _ := FS.ReadFile(name)
		for _, forbidden := range []string{"cdn.jsdelivr.net", "unpkg.com", "fonts.googleapis.com", "%%workerScript%%", "%%capCSS%%"} {
			if bytes.Contains(data, []byte(forbidden)) {
				t.Errorf("%s contains %s", name, forbidden)
			}
		}
	}
}

func TestNavigationSafetyContract(t *testing.T) {
	app, _ := FS.ReadFile("static/app.js")
	for _, required := range []string{"dataset.requestMethod === 'GET'", "window.location.replace(", "result.success !== true", "credentials: 'same-origin'", "redirect: 'error'", "CAP_CUSTOM_WASM_URL", "CAP_CUSTOM_HASHWX_URL", "CAP_PAKO_URL", "samwaf_lang"} {
		if !bytes.Contains(app, []byte(required)) {
			t.Errorf("missing frontend safety contract %q", required)
		}
	}
	if bytes.Contains(app, []byte("window.location.reload(")) {
		t.Fatal("reload can replay POST")
	}
}
