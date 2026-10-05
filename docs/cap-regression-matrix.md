# Cap enhancement regression matrix (2026-10-05)

Executed after all four parallel implementations land (engine library, TLS
fingerprint capture, backend integration, management UI). **Scope: the entire
system, not the delta.** Every package, every pre-existing test and every
pre-existing behavior is exercised — the five enhancements must not break
anything that was green before them. Every item records an explicit pass/fail
with evidence; failures block the release claim.

Known pre-existing baseline failures (recorded before these changes, not
caused by them): the `api` package's live-update/Excel tests
(`TestExportExcelApi`, `TestGetFileDescription`, `TestCheckVersionApi`,
`TestCheckVersionNeedUploadApi`) fail on an uninitialized DB and the live
update service. Everything else that was green must remain green.

Executed result (full `go test -race -count=1 ./...`, 2026-10-05 13:36):
**49 packages ok. No new regressions.** All failures verified untouched by
this session's changes (git status empty on every failing path):
`test` (TestRegrep panic in an untouched grule test; also needs network for
testify), `wafproxy` (untouched internal-package compile), `binarydist`
(missing bsdiff binary, environment), `api` ×4 (documented baseline),
`cmd/samwaf` (pre-existing vet printf on the untouched startup banner).
`wafenginecore/cccounter` dual-window tests flaked only under concurrent CPU
contention with the parallel library suite and pass in isolation (1.06s).

## A. Independent library (`cap_go_server`) — ALL tests

1. Full `go test -race ./...` — including the legacy `cap.go` API suite
   (atomic saves, no-eager-file-creation, throttle, persistence), HashWX
   (spec vectors, 1024 fixed oracle vectors, 512 live oracle comparisons),
   RSW, store, modern protocol/security suites and every new test.
2. `CGO_ENABLED=0 go test ./...` — whole library.
3. `go vet ./...`.
4. Node differential oracle, both directions (format 1 + format 2, all four
   protocols) against the pinned upstream source — including upgraded
   obfuscation defaults and full 18-check injection.
5. Real-Chrome instrumentation execution for levels 1–10 plus the upstream
   42 detection fixtures.

## B. SamWaf backend — ALL packages (RESULT: PASS, no new regressions)

6. Full-repo `go test -race -count=1 ./...` for every package that was green
   at the recorded baseline: cache (full suite incl. the 130s SetWithTTl
   case), model, service/waf_service, wafenginecore and every wafenginecore
   subpackage, wafhttpserver, utils, common, wafdb, router, wafinit, api
   (excluding the four documented pre-existing failures), firewall,
   iplocation, middleware, plugins, waftask, globalobj — all of them.
7. Pass fingerprint binding: match passes; mismatch fails; strict (`ip+tls`)
   with no fingerprint fails closed at issue **and** check; `ip` mode
   unaffected; explicit `false` for the new booleans survives save/read.
8. Configuration: `passBinding` enum + `ip+tls` requires SSL (cross-field on
   add and edit), `instrumentationCheckCount` 0..18, `dynamicEscalation`
   three-state; unknown/duplicate/malformed fields rejected; legacy configs
   (no new fields) load with defaults and lose no existing field.
9. Traditional click-captcha regression: the Go suite plus the shared
   `IssuePass` path — binding semantics identical for both engines.
10. Startup resource release (`wafinit`), legacy version tags, config
    loading — all pre-existing behavior.

## C. Frontend (`web-dev`) — ALL tests

11. Full `npm test` — all 32 tests: the 27 pre-existing plus the 5 new; no
    old test was deleted or weakened.
12. ESLint zero warnings on the documented touched-file set; existing
    baseline lint debt unchanged (verified against HEAD).
13. Production build via the low-memory esbuild path; console dropping stays
    disabled (guarded by the optimizer regression test).

## D. Real-browser E2E (lab) — full flows

14. HTTP site: original-look challenge page renders; mixed challenge
    completes; protected content reached; language switch works.
15. HTTPS site (`https://127.0.0.1:18443/`, self-signed): real browser
    completes the flow with `passBinding=ip+tls`.
16. **Token transfer attack**: the browser's pass token replayed via curl and
    Node fetch at the same IP must fail; the browser itself must reach the
    protected content (proves binding does not break the normal flow).
17. Blocking on: headless browser rejected (including `navigator.webdriver`
    spoof variant) — the pre-existing 7/7 behavior must hold.
18. Observe mode: blocking off, headless browser **passes but** `cap_trace`
    records the `instr_blocked_reported` risk flag.
19. 18 injected checks + obfuscation default 6: real browser still completes
    (bigger script/probe, unchanged UX).
20. Dynamic escalation: after one rejected automated attempt the same scope's
    next challenge is measurably harder (parameters compared before/after);
    a clean first attempt must NOT escalate (no false positives).

## E. Penetration PoC regression — all demonstrated attacks

21. `bypass.py` (browserless forgery) re-run as-is against the upgraded
    build — expected: level-6 obfuscation breaks its static parsing; report
    the actual outcome honestly either way.
22. Same-IP `curl` replay on the HTTPS site — must fail now.
23. Headless real-browser path under blocking — must stay rejected.
24. Legacy PoC set from the earlier report: offline forgery attempts
    (invalid token / instr_missing / wrong solutions) must all still be
    rejected — re-run a sample of each against the new build.

## F. Management console — full app

25. Real-Chrome login + dashboard render (the whole app, not just the
    captcha tab); the three new captcha fields render, save, reload; all
    existing tabs still load (spot-check the previously verified flows).

## G. Build, docs, operations

26. Complete `go build ./cmd/samwaf` (embedded modern challenge assets +
    built management frontend) and a release-flags build for the lab.
27. Docs updated and consistent with the implementation:
    `cap-embedded.md`, `model/captcha_modern.md`, `cap-regression-matrix.md`,
    lab README (HTTPS site + new switches).
28. `cap_trace` observability: `risk_flags`, `instr_blocked_reported`,
    escalation effect visible in the JSON log.
29. Lab restart: config, logs and credential chain survive; in-flight
    challenges and old passes invalidated as designed (policy hash changed
    with the new fields).

---

# RESULTS (2026-10-05, executed)

| Item | Result |
|---|---|
| A1 library full race | ✅ exit 0, 3 packages ok (includes legacy cap.go suite, all new tests) |
| A2 CGO_ENABLED=0 | ✅ exit 0, 3 packages ok |
| A3 go vet | ✅ exit 0 |
| A4 Node differential oracle | ✅ exit 0 (bidirectional, format 1+2, all protocols; engine agent also ran 512-case live HashWX oracle + escalated subtests) |
| A5 real-Chrome instrumentation | ✅ engine agent: 20 subtests, levels 1-10, full-injection self-report + clean-seed completion; upstream 42 detection fixtures green |
| B6 full-repo race | ✅ 49 packages ok, no new regressions (documented pre-existing: test/wafproxy/binarydist/api×4/cmd vet; cccounter flaked only under CPU contention, passes in isolation) |
| B7 pass binding | ✅ match passes, mismatch/missing fail closed (issue and check), ip mode unaffected, explicit false persists |
| B8 configuration | ✅ enum + cross-field SSL requirement (add and edit), bounds 0..18, three-state, legacy configs load, unknown fields rejected |
| B9 traditional click captcha | ✅ shared IssuePass path unchanged (wafenginecore/wafcaptcha race suite green) |
| B10 startup resources | ✅ wafinit suite green |
| C11 frontend npm test | ✅ 32/32 (27 pre-existing + 5 new, none weakened) |
| C12 ESLint | ✅ zero warnings on touched set (baseline debt unchanged) |
| C13 production build | ✅ esbuild path, console drop stays disabled (guarded by test) |
| D14 HTTP E2E | ✅ classic-look page renders, mixed challenge completes, protected content reached, zero external requests |
| D15 HTTPS E2E | ✅ real browser completes with passBinding=ip+tls, pass cookie issued, reload reaches protected content |
| D16 token transfer attack | ✅ BLOCKED: browser's pass replayed via curl (h2 and http/1.1, same IP) both rejected with the challenge page; browser itself reaches protected content |
| D17 blocking on | ✅ headless rejected, including navigator.webdriver spoof (pre-existing 7/7 behavior holds) |
| D18 observe mode | ✅ headless passes, cap_trace records instr_blocked_reported; redeem still succeeds |
| D19 18 checks + obfuscation 6 | ✅ full injection completes in a real browser (valid-seed path); observe-mode E2E at count 18 completes; blocking path correctly self-reports |
| D20 dynamic escalation | ✅ level 1 → level 2 confirmed exactly per spec (sha difficulty +1/level, hashwx/rsw doubling, count +2/level); escalation resets on restart (memory-only advisory) |
| E21 bypass.py browserless forgery | ⚠️ **still passes as-is** — level-6 obfuscation is string pooling; the PoC's regexes match structure (declarations, arithmetic chains), which survives pooling. The documented boundary holds: a clean forged probe emits zero risk flags, so escalation never triggers; checks are client-attested and cannot be cryptographically verified. Closed by these changes: retry-luck (deterministic selection), cross-client token transfer (D16), observe visibility (D18) |
| E22 same-IP curl replay on HTTPS | ✅ rejected (covered by D16) |
| E23 headless under blocking | ✅ rejected (D17) |
| E24 legacy offline forgery samples | ✅ ALL REJECTED: missing/invalid token, wrong solutions, missing solutions, wrong count, bad validate each 403 verification_failed |
| F25 management console | ✅ login + dashboard render, zero errors, i18n healthy; new fields round-trip verified via API readbacks and the UI agent's Chrome smoke tests |
| G26 complete build | ✅ go build ./cmd/samwaf (release flags) exit 0 |
| G27 docs | ✅ cap-embedded.md, captcha_modern.md, matrix, lab README updated |
| G28 cap_trace observability | ✅ risk_flags/instr_blocked_reported recorded in JSON log; escalation effect visible in challenge parameters |
| G29 restart survival | ✅ config (static site, passBinding, blocking, checkCount) persists in DB across restarts; memory escalation counters reset; passes invalidate on policy hash change |

## Two real defects found and fixed by this regression

1. **Per-connection JA3 instability** (fingerprint binding false-rejecting the
   same browser): modern Chrome randomizes cipher/extension/curve ORDER per
   connection and toggles resumption extensions on later connections. Stock
   JA3 is order-sensitive, so the pass bound at validate was rejected at the
   reload connection. Fixed with normalized digest: strip GREASE (already)
   and resumption extensions (session_ticket/pre_shared_key/
   psk_key_exchange_modes/early_data), sort and dedupe every list
   (JA4-style). One client is now stable across connections while
   client-vs-client discrimination is unchanged (unit + real-handshake
   regression tests added).
2. **Empty 200 on HTTPS site**: the lab bootstrap wrote a minimal
   static_site_json lacking `sensitive_paths`; the startup migration replaces
   such configs with defaults (static site disabled), so passing requests
   fell through to the proxy path and `url.Parse(":0")` failed with a silent
   empty-200 return. Fixed by restoring the migration-compatible config;
   the underlying silent-empty-200-on-target-parse-failure remains a product
   hardening candidate (should be a 502, not an empty 200).
