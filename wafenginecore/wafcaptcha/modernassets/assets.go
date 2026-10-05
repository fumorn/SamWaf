// Package modernassets serves a self-contained, versioned Cap frontend directly
// from the executable. It never reads or extracts legacy exedata resources.
package modernassets

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

// FS includes the audited frontend, template, provenance manifest and licenses.
// Use Serve for HTTP access: the template and metadata are intentionally private.
//
//go:embed static/* challenge.html manifest.json licenses/*
var FS embed.FS

// Data contains plain text, never HTML. RequestMethod must be the original
// intercepted request method; only an explicit GET permits automatic navigation.
type Data struct {
	Prefix        string
	TitleZh       string
	TitleEn       string
	TextZh        string
	TextEn        string
	ContactInfo   string
	RequestID     string
	RequestMethod string
}

var page = template.Must(template.ParseFS(FS, "challenge.html"))
var safePrefix = regexp.MustCompile(`^/[A-Za-z0-9_./~-]*$`)
var public = map[string]string{
	"app.js":              "text/javascript; charset=utf-8",
	"cap.js":              "text/javascript; charset=utf-8",
	"worker.js":           "text/javascript; charset=utf-8",
	"pako_inflate.min.js": "text/javascript; charset=utf-8",
	"cap_wasm_bg.wasm":    "application/wasm",
	"hashwx.wasm":         "application/wasm",
}

// Has accepts only exact public asset names, with no leading slash, URL
// decoding or path cleaning. Unknown names must not fall through to old assets.
func Has(name string) bool {
	_, ok := public[name]
	return ok
}

// Serve writes a known asset and returns true, including a 405 for a known
// asset requested with an unsupported method. A false return writes nothing.
// Mount at Prefix + "/v2/assets/" and pass only the remaining asset name.
func Serve(w http.ResponseWriter, r *http.Request, name string) bool {
	contentType, ok := public[name]
	if !ok {
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	body, err := FS.ReadFile("static/" + name)
	if err != nil {
		http.Error(w, "embedded asset unavailable", http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", contentType)
	// Stable paths may change on a binary upgrade: revalidate, never immutable.
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(body)))
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
	return true
}

// Render renders escaped plain-text configuration into data attributes. It does
// not set an HTTP status, cookie or CSP: those belong to the enclosing service.
// Invalid/non-local prefixes are rejected before any bytes are written.
func Render(w io.Writer, data Data) error {
	if strings.Contains(data.Prefix, "//") {
		return fmt.Errorf("modernassets: prefix must not contain double slashes")
	}
	data.Prefix = strings.TrimSuffix(data.Prefix, "/")
	if data.Prefix != "" && (!safePrefix.MatchString(data.Prefix) || path.Clean(data.Prefix) != data.Prefix || strings.Contains(data.Prefix, "//")) {
		return fmt.Errorf("modernassets: prefix must be a clean local absolute path")
	}
	if data.TitleZh == "" {
		data.TitleZh = "安全验证"
	}
	if data.TitleEn == "" {
		data.TitleEn = "Security Verification"
	}
	if data.TextZh == "" {
		data.TextZh = "为了确保您的访问安全，请完成以下验证"
	}
	if data.TextEn == "" {
		data.TextEn = "To ensure the security of your access, please complete the following verification"
	}
	var rendered bytes.Buffer
	if err := page.ExecuteTemplate(&rendered, "challenge.html", data); err != nil {
		return err
	}
	_, err := w.Write(rendered.Bytes())
	return err
}
