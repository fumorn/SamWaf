# Modern Cap: embedded, local-only frontend

This directory is independent of SamWaf's legacy `exedata` extraction, `.lock` and `.new` files. The Go executable embeds the frontend directly; no runtime Node/Bun, network download, directory creation, or extraction is required.

## Integration API

```go
import "SamWaf/wafenginecore/wafcaptcha/modernassets"

// Route this namespace BEFORE legacy asset handling. Do not clean/decode name.
const suffix = "/v2/assets/"
name := strings.TrimPrefix(r.URL.Path, prefix+suffix)
if !modernassets.Serve(w, r, name) {
    http.NotFound(w, r) // NEVER fall back to legacy exedata for this namespace
}

// Has(name string) bool is the same exact public whitelist, useful for routing.
// FS is embed.FS: static assets, template, manifest, third-party licenses.
// Render(w io.Writer, data Data) error does not choose HTTP status or set cookies.
w.Header().Set("Content-Type", "text/html; charset=utf-8")
w.Header().Set("Cache-Control", "no-store")
err := modernassets.Render(w, modernassets.Data{
    Prefix: prefix,
    TitleZh: "安全验证", TitleEn: "Security Verification",
    TextZh: "为了确保您的访问安全，请完成以下验证", TextEn: "To ensure the security of your access, please complete the following verification",
    ContactInfo: contact, RequestID: requestID,
    RequestMethod: r.Method,
})
```

Every `Data` field is `string`. Values remain plain text and pass through Go `html/template` contextual escaping; no `template.HTML`, inline event handler, inline script, or script-body interpolation is used. Prefix must be empty/root or a clean local absolute path made of ASCII letters, digits, `_`, `-`, `.`, `~` and `/`; query strings, encoded separators, dot path segments, backslashes, double slashes, and remote URLs are rejected before output. A trailing slash is removed. Blank titles/text receive bilingual defaults.

Public names are exactly `app.js`, `cap.js`, `worker.js`, `cap_wasm_bg.wasm`, `hashwx.wasm`, and `pako_inflate.min.js`. Unknown names return false without writing; known names permit GET/HEAD only. WASM has `application/wasm`. Responses use `nosniff`, same-origin CORP, content-hash ETags and mandatory revalidation (not immutable URLs). Do not expose all of FS with an unfiltered file server.

## Protocol and navigation

- `app.js` sets **all three** `CAP_CUSTOM_WASM_URL`, `CAP_CUSTOM_HASHWX_URL`, `CAP_PAKO_URL` overrides before loading Cap; all point into `Prefix + '/v2/assets/'`.
- The widget endpoint is `Prefix + '/v2/'`: upstream sends POST `challenge` and POST `redeem` automatically, including format-2 SHA256/HashWX/RSW solutions and instrumentation output.
- The solve event's token is POSTed as JSON `{ "token": "..." }` to `Prefix + '/v2/validate'` with same-origin credentials. The service must issue its SamWaf cookie and return a 2xx JSON `{ "success": true }`. Tokens are not saved to storage, URLs, or logs.
- The visible page replicates the legacy SamWaf Cap challenge page (same background, language button, title/instruction, widget, success message, toast and request-reference/contact lines). No progress bar, status line, retry button, GET link or method note is rendered; the upstream widget labels its own state.
- Only `RequestMethod == "GET"` automatically navigates after the original 2-second delay, using `location.replace(currentURL)` to issue a fresh GET. It does **not** use `location.reload`, which may replay a POST. Other/unknown methods complete without automatic navigation; visitors return and retry their original request manually. There is no original-body storage or automatic POST replay.
- Network requests reject redirects; challenge/redeem URLs are restricted to the two expected same-origin endpoints, and challenge/redeem fetches have 30-second timeouts. Validate is a single one-use POST per solve event.
- Language selection keeps the original behavior: the top-right button toggles simplified Chinese/English, persists `samwaf_lang` for 30 days, and reloads via fresh GET. `CAP_DISABLE_WIDGET_REF` and `CAP_DISABLE_HAPTICS` suppress upstream credit/haptics additions.

No font, stylesheet, image, worker, WASM or inflater is fetched from a CDN. The local patched Cap defaults are also same-directory URLs, so omitting the overrides cannot silently reinstate a CDN. The upstream instrumentation payload is received from the local challenge endpoint, raw-deflate decompressed locally, and executed in an `allow-scripts` sandboxed iframe as upstream intends.

The parent controls response CSP. It must permit this same-origin script/worker/resource namespace, WASM compilation, Cap's shadow-root style, and the upstream sandboxed `srcdoc` instrumentation script. A strict policy forbidding inline styles/srcdoc scripts without coordinating upstream nonce plumbing will break Cap; do not claim this frontend works under arbitrary preexisting CSP. `CAP_SCRIPT_NONCE` / `CAP_CSS_NONCE` are upstream hooks, not Data fields here.

## Pinned inputs and generation

Cap source: <https://github.com/tiagorangel1/cap/tree/af624b9f15cb03f1f8194fb4c9f262ac34984923>.
Versions: **core 0.1.3 / widget 0.1.58 / WASM 0.0.8**.

The generator hashes every consumed Cap input against hardcoded SHA-256 pins before assembly. It does **not** trust the checked-in `cap.min.js` to contain the newest worker protocols, and does not call the upstream build script (which would launch its demo server). It reads `widget/src/src/cap.js`, CSS, translations and the complete worker at that commit, substitutes CSS and all upstream shipped i18n rows, externalizes the worker to local `worker.js`, and replaces the three CDN fallbacks. The result is intentionally readable rather than minified; no npm installation or floating build dependency is required.

The two WASM binaries are copied **byte-for-byte** from `wasm/src/browser/` of the pinned Cap source; Rust/emcc compilation is not performed by this assembly script. `cap_wasm_bg.wasm` is SHA-256 `e4f3c00246a775193661f9277ca1288cd310a6514de166ecc2176ccd26fb06a9`. `hashwx.wasm` is SHA-256 `b1a0dbb3ef444d3c7069e0a5e0a0273ffa4cf8fef62cbbe43761c02f7cd6aff5`. The included worker has the matching wasm-bindgen imports and HashWX compiled/interpreter ABI.

**Compatibility fix to the pinned worker:** WASM 0.0.8's `solve_pow` only hashes a single SHA-256 block (UTF-8 salt + decimal nonce must be under 56 bytes). A valid format-2 64-character salt otherwise loops forever. The generator adds a guard routing salts over 35 UTF-8 bytes to the existing general SHA256 fallback, reserving 20 digits for the nonce. It uses Web Crypto when available, otherwise the worker's existing multi-block `sha256Into`, so plain HTTP does not introduce a hidden dependency. The WASM binary remains unchanged. Tests cover 36/55/64/512-byte and multibyte salts with and without Web Crypto. Opt-in `CAP_DEBUG` messages also identify per-protocol completion and instrumentation decompression phases without logging challenge values or tokens. A real Chrome mixed-protocol integration test verified challenge → SHA256/HashWX/RSW/instrumentation → redeem → validate → protected GET with no external requests after this fix.

Pako **2.1.0** is copied from `package/dist/pako_inflate.min.js` in the retained npm tarball. Its fixed archive URL and SHA-256 are in the manifest and generator. It supports browsers lacking `DecompressionStream('deflate-raw')`; it is not an optional CDN fallback.

```sh
# From this directory, with Node available only at build/test time:
node tools/generate.mjs /path/to/cap-af624b9f15cb03f1f8194fb4c9f262ac34984923
node tools/verify.mjs
node tools/bridge-test.mjs
node tools/mixed-test.mjs
# From the SamWaf Go module:
go test ./wafenginecore/wafcaptcha/modernassets
```

`manifest.json` records upstream commit/versions, source pins, pako provenance, HashWX provenance and SHA-256 for every embedded resource/template/license. After deliberately editing the local template/bridge/CSS only, `node tools/generate.mjs --manifest-only` refreshes output hashes. Review manifest changes in the same commit. Go tests reject changed assets without updated hashes, check path whitelisting, escaping, GET/HEAD/405/ETag and WASM MIME; the offline Node test verifies actual SHA256 WASM/JS fallback, RSW, HashWX compiled-worker proofs against an independent interpreter, and pako decompression. It starts no server. Browser/end-to-end tests against the real parent service are separate from these offline checks.

## Licenses and redistribution

- Cap: Apache-2.0, Copyright 2025 Tiago, retained in `licenses/CAP.txt`. Generated Cap/worker files identify origin and modifications.
- Pako: MIT, Copyright 2014–2017 Vitaly Puzrin and Andrei Tuputcyn, retained in `licenses/PAKO.txt`; the distributed minified header also preserves its zlib-related attribution.
- **HashWX is LGPL-3.0, not Apache-2.0.** Its upstream source is <https://github.com/tevador/hashwx/tree/74b567a31276a4c5d7cb232a5b7639467f49f961>. The complete unmodified source archive is retained under `vendor/`, SHA-256 `14f9b95399abae1c4a3d2d1b92e1402c0cdaa870cc4f337fe960b93ce6416e99`; `licenses/HASHWX-LGPL-3.txt` and `licenses/GPL-3.txt` retain the license texts. Cap's `core/src/hashwx-wasm.js` records that this exact binary was built with emcc and lowered to WASM MVP; this task reuses that known-compatible artifact rather than asserting a new reproducible compiler build.

When redistributing the binary, include the corresponding source archives, licenses and source/build materials necessary for your distribution's LGPL obligations. HashWX remains a separate WASM resource with a stable ABI: it can be replaced with an interface-compatible modified build, then the manifest refreshed and the Go executable rebuilt. SamWaf's Go embed is packaging, not a claim that HashWX changed license. Exact compiler-environment reproducibility of Cap's prebuilt HashWX artifact was not established here.
