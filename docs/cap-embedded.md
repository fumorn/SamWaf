# Embedded modern Cap

## Source layout and build

This workspace has two independent Git repositories: `SamWaf/` (application) and
`cap_go_server/` (Go library). The parent `go.work` selects both local modules;
no library release has been published. Build from this workspace with Go 1.25.9:

```sh
cd SamWaf
go build -o SamWaf ./cmd/samwaf
```

For CI outside this layout, publish/pin the reviewed library version or reproduce
the two-module workspace. Do not accidentally build against the released
`cap_go_server v1.0.1`: it does not contain the new Engine API. Management UI
sources are a separate nested development checkout in `web-dev/`; its built
release assets belong in `public/dist/` before building the application.

## Runtime and migration

The server runs Cap inside the Go process. No Node/Bun process, sidecar, Cgo
algorithm, or server-side WASM runtime is required by Cap. SamWaf itself retains
its unrelated pre-existing dependencies (including SQLite). Browser JavaScript
and browser WASM remain necessary. All challenge assets, including decompression
fallback, are embedded and served locally; no CDN is needed.

The pinned browser WASM 0.0.8 SHA solver only handles single-block inputs. The
embedded worker deliberately routes long UTF-8 salts to the local multi-block
SHA256 solver (Web Crypto or bundled JavaScript fallback); without this fix,
format-2's default 64-character salt never finishes. WASM binary hashes remain
unchanged and regression tests cover long salts and mixed protocols.

The Cap reference is commit `af624b9f15cb03f1f8194fb4c9f262ac34984923`.
Supported challenge formats are compact SHA256 format 1 (with optional browser
instrumentation) and format 2 with SHA256, HashWX, RSW, instrumentation, or mixed
protocols. Keep the original engine name `capJs`. Existing Cap numeric options
are preserved; modern configuration is described in `model/captcha_modern.md`.

- Browser API: `<path_prefix>/v2/challenge`, `/v2/redeem`, `/v2/validate`.
- Assets: `<path_prefix>/v2/assets/<exact embedded filename>`.
- Old unversioned Cap POST endpoints return 409 and require reloading the page.
- Old unscoped pass cookies are invalid; visitors complete one fresh challenge.
- A pass and CC grace bind the matched server-side site, canonical client IP and
  complete normalized policy. Policy edits invalidate earlier passes/grace.
- The new embedded Cap page does not execute disk-based legacy Cap templates.
  Existing custom/locked files are preserved but no longer control this engine;
  use configured titles, descriptions and contact information for customization.
- Only an intercepted GET is automatically reloaded after success. POST/PUT/etc.
  are never silently replayed: users must return safely or resubmit explicitly.
- A completed challenge does not bypass SQL/XSS/CC-deny or other WAF protections.

## Secret, restart and multiple instances

For Redis-backed or multiple WAF processes, configure the **same** random secret
on every process. `SAMWAF_CAP_SECRET` is standard Base64 encoding of at least 32
cryptographically random bytes. Supply it using deployment secret management,
not public configuration, source control or logs. Invalid encoding fails closed.
Redis without this variable fails initialization rather than silently creating
incompatible per-node challenge signatures.

A single memory-backed process may omit the variable: an ephemeral cryptographic
key is generated, so pending challenges expire on restart. Fixed secret plus a
volatile store is **not** durable replay prevention: after losing consumed nonce
records, an unexpired old challenge could be redeemed again. For durable/multi-
node operation, retain both the secret and a shared durable atomic store. Configure
Redis persistence appropriately and `maxmemory-policy noeviction`; do not flush or
evict live replay records. Synchronize host clocks. Rotating the secret across all
nodes invalidates in-flight challenges/tokens; rolling mixed keys need a separate
keyring design and are not supported by this release.

Optional `SAMWAF_CAP_RSW_KEY` accepts the JSON representation of the library's
2048-bit RSW keypair for precomputation reuse. It contains secret factors: secure
it like the main secret. Omitting it lazily generates a key. Nodes do not need
the same RSW key to validate challenges when sharing the main secret and store.

## Limits and failure behavior

- Challenge HTTP bodies are bounded to 2 MiB, with a 10-second upload deadline.
- At most 16 concurrent Cap API operations and 120 requests/minute per scoped
  client are admitted per process. This supplements the configured WAF CC rules;
  it is not a cluster-wide edge rate limiter.
- The admission map is capped at 4096 scopes; capacity failures return 429.
- The memory cache caps the three captcha namespaces at 100000 live records
  combined. It reclaims expired records but never evicts a live nonce. Capacity
  or storage errors fail closed. This is an entry budget, not a byte budget.
- Redis operations use atomic Lua, absolute expiry, error propagation and no
  fallback to per-process memory when Redis fails.
- Origin checks reject cross-origin browser submissions. Existing site IP mode
  still decides client identity; configure trusted proxies correctly.
- Captcha configuration updates reject malformed, duplicate and unknown JSON
  fields. See the configuration document for migration details and bounds.

## Coverage and deliberate differences

Instrumentation includes state/probe verification and server-side automation
verdicts. Obfuscation levels 1–10 use documented Go-native generation, string pools,
encodings and chunking. They do not reproduce every transformation of upstream's
optional `javascript-obfuscator` (e.g. self-defending/debugger injection or its
control-flow flattening). Do not market equal obfuscation strength. PoW and
browser probes increase automation cost; neither proves a visitor is human.

Automation blocking **changes what is enforced, not what is collected**
(observe mode): the 18 environment checks are now always injected whenever the
instrumentation protocol is present, and their verdicts are recorded in
`cap_trace` risk flags either way. `blockAutomatedBrowsers` only decides whether
a failed verdict blocks the redemption. With it enabled, all 18 checks are
injected by default (upstream-compatible 8 is available via
`instrumentationCheckCount`, 0..18; 0 = automatic: 18 with blocking on, 8 off).
Check selection is deterministic per client scope+secret, so retrying challenges
cannot luck into an easier sample. The honest widget self-reports a failure and
the server additionally runs Detect on the submitted probe. Local headless
Chrome was rejected in this mode, including with `navigator.webdriver` spoofed;
client attestation keeps this probabilistic — it raises cost, it does not prove
a human. An automated real browser can also complete PoW itself: a pass only
certifies the cost was paid by that client IP for that site and policy.

Obfuscation level 0 now resolves to 6 when automated-browser blocking is on
(3 when off); explicit 1–10 is respected.

Risk-driven dynamic escalation (`dynamicEscalation`, default on, explicit off
persists) records per-scope suspicious outcomes (rejected automated browser,
risk-flagged redemption) for 24 hours and hardens the next challenge for that
scope: SHA-256 difficulty +level, HashWX difficulty and RSW rounds doubled per
level, obfuscation +level, challenge count +2×level, capped at level 4 and the
hard parameter ceilings. It only engages after a suspicious event, so clean
forged probes (zero risk flags) do not trigger it — it is a cost escalation,
not detection. NAT-shared clients may see harder challenges after one flagged
client behind the same address; disable per site if that trade-off is wrong
for you.

Pass credential binding (`passBinding`, `""`/`"ip"` default, or `"ip+tls"`)
binds the pass additionally to the client's TLS fingerprint (normalized
JA3+ALPN), captured at the site TLS listener. The digest deliberately deviates
from stock JA3: GREASE and session-resumption extensions are stripped and every
list is sorted and deduplicated (JA4-style), because modern browsers randomize
cipher/extension/curve order per connection and toggle resumption extensions on
later connections — an order-sensitive digest would reject the same browser's
own pass on its next connection. `"ip+tls"` requires the site to run with SSL
enabled and is rejected at save time otherwise; with it active, a pass issued
to a browser does not work from curl or any other TLS client at the same IP,
and a missing fingerprint fails closed. Corporate TLS-normalizing middleboxes
can change a client's fingerprint and cause false rejections; a browser
TLS-stack upgrade can also change it mid-TTL, forcing one re-verification. The
setting is therefore opt-in. On plain-HTTP sites there is no fingerprint to
bind, so the setting is unavailable there. The traditional click-captcha
shares the same pass issuance path and the same binding.

Cap Standalone's independent admin dashboard, site-key management, GeoIP download
service and container lifecycle are not embedded as another application. SamWaf
retains its own site management, WAF policy and diagnostics. The independent Go
library supplies verification APIs, not a clone of every Standalone REST route.

## Licensing

The modern Cap port and frontend preserve the upstream Apache-2.0 notices. HashWX
has separate LGPLv3 terms; the Go translation and browser WASM preserve notices,
license texts and the browser WASM's corresponding source archive. Review the
applicable combined-work/relinking and source-distribution obligations before
shipping binaries. A source URL alone is not a blanket determination of compliance.

## Validation

```sh
# independent core, race and optional upstream differential oracle
cd ../cap_go_server
go test -race ./...
CAP_UPSTREAM_SRC=/path/to/pinned/cap/core/src go test -run TestModernNodeOracleBidirectional -v

# WAF/API/policy/cache
cd ../SamWaf
go test -race ./model ./cache ./wafenginecore/...
SAMWAF_CAP_BROWSER_TEST=1 go test ./wafenginecore/wafcaptcha -run TestModernBrowserEndToEnd -v
```

Real-browser tests need Chrome and Node only as development tools and block
external browser requests. Redis contract tests use `SAMWAF_TEST_REDIS_ADDR` with
isolated expiring keys, never FLUSHDB. Chromium results do not establish a complete
Safari/iOS/Firefox compatibility matrix; those require additional device testing.
