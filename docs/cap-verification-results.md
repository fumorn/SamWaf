# Cap implementation verification (2026-10-05)

## Verified

- Independent `cap_go_server`: complete `go test -race ./...`, `CGO_ENABLED=0
  go test ./...`, and `go vet ./...` passed.
- Fixed upstream Node differential oracle passed in both directions for format 1
  and format 2 with all four protocols. Node is only a development oracle.
- HashWX: four specification vectors, 1024 saved oracle vectors and 512 live
  random oracle comparisons; amd64/386 pure-Go tests, race and primitive coverage.
- Instrumentation: 42 upstream detector fixtures, 100 arithmetic comparisons,
  concurrency tests and real Chrome sandbox execution for every level 1–10.
- Real embedded challenge page: format 1 SHA256+instrumentation and mixed format 2
  SHA256+HashWX+RSW+instrumentation completed challenge, redeem, validate, scoped
  cookie and protected GET in Chrome. External browser requests were blocked;
  both reported no external requests and no runtime exceptions.
- Cache: complete race suite passed after atomic byte/capacity/expiry changes;
  real Redis 7 two-client concurrent NX/Take contract tests passed. Dedicated
  temporary Redis container was stopped; existing databases were not touched.
- WAF: complete `go test -race ./wafenginecore/...` passed, including CC grace,
  emergency mode, site/policy/IP isolation, old credential rejection, cache failure,
  bounded slow-body uploads and cross-instance validation.
- Configuration and host service race tests passed.
- Startup resource-release tests passed with a restored tracked fixture.
- Management UI: 24 helper/real Vue component contract tests passed; scoped
  ESLint passed. Real Chrome Vue/TDesign fixture verified format/protocol changes,
  all parameter serialization/readback, empty-protocol errors and desktop/mobile
  layouts (1280/390px), with no runtime exceptions or failed requests.
- Full management release build passed using the explicit low-memory esbuild
  option; default Terser exceeded this environment's memory budget. Subsequent
  full-app diagnosis found esbuild 0.14.54's console-drop branch miscompilation:
  the original component-only verification did not catch the blank login page.
  Console dropping is now disabled, debugger dropping remains, and release
  explicitly defines production NODE_ENV. Three new optimizer/i18n regressions
  pass (27 total frontend tests). Corrected production assets were verified in
  real Chrome against the live backend: login page and authenticated dashboard
  render without uncaught runtime exceptions.
- Complete application `go build ./cmd/samwaf` passed, including embedded modern
  challenge resources and the built management frontend.

## Pre-existing build/test issues addressed during verification

- Pin the root `google.golang.org/genproto` to the same commit as the already-used
  split RPC module to remove an ambiguous import in the complete application build.
- Legacy Cap's invalid-nonce test used a fixed nonce with a 1/16 probability of
  being valid. It now finds a definitely invalid nonce; runtime code was unchanged.
- WAF cache tests were missing the newly required size argument.
- Parallel WAF tests repeatedly replaced a shared logger; reuse the existing
  TestMain initialization without disabling parallel or race checking.
- Resource-release tests referred to an absent gitignored fixture; use a small
  tracked fixture instead. Challenge template tests now exercise shipped source
  files and their supported fallback behavior, not missing runtime data files.

## Known unrelated baseline failures

An exploratory `go test ./api ./router ./wafinit` found unrelated API test failures:

- `TestExportExcelApi` uses an uninitialized global database and receives 500.
- `TestGetFileDescription` expects an outdated log-file description.
- `TestCheckVersionApi` and `TestCheckVersionNeedUploadApi` depend on the live
  update service and outdated expected version results.

These API tests were not weakened or silently skipped. Router compilation passed;
startup fixture tests were corrected separately. This work does **not** claim
that every test in the entire SamWaf repository is green.

## Remaining release validation boundaries

Real browser coverage here is Chromium, not a complete Safari/Firefox/iOS device
matrix. Management UI component tests and browser fixture validation do not by
themselves prove every production login/database deployment path. Deployment must
supply shared durable secret/store configuration for multiple nodes. HashWX LGPLv3
combined-work/source/relinking obligations require release review. High-level
Go obfuscation is a documented native alternative, not an identical implementation
of the upstream optional JavaScript obfuscator.
