# Project Memory — ZenSys Gateway

Curated long-term notes for `D:\My Projects\Gateway`. Append-only daily logs live alongside this file.

## What this project is

A Go edge service in front of ZenSys POS and ZenSys GMS. Public contract `https://{tenant}.zensys.lk/`. Its only routing job:

```text
hostname → tenant → product → stage → upstream
```

POS and GMS keep owning `tenant → database`. The Gateway never resolves a tenant to a database, never touches product DBs, never handles auth/JWT. Drift from this boundary is a bug, not a feature request.

Spec: `docs/architecture/ZenSys_Gateway_V1_Architecture.md` (source of truth; §93 = 20 invariants, §91/§92 = acceptance criteria). `IMPLEMENTATION_PLAN.md` cites its section numbers, so keep them in sync.

## Settled decisions (`docs/adr/0001-v1-open-decisions.md`)

MySQL 8 registry · proxy-to-frontend · private bind + service tokens (mTLS-ready) · apex landing page, other reserved hosts 404 · disabled tenant 404s byte-identically to unknown · HA-ready design, one instance · topology portable · one slug → one product.

**Slug policy `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$` — 3 to 63 chars.** The spec's §9 regex says `{1,62}`, permitting 64. That is a spec bug: a slug *is* the leftmost DNS label, and labels cap at 63, so a 64-char slug would be reservable but permanently unresolvable. Corrected in three places that must agree — `internal/tenant/slug.go`, the `chk_slug_format` CHECK, and `VARCHAR(63)`. Do not "restore" the documented value.

The **apex label is not a reserved host**. The apex is the bare root domain (`zensys.lk`); `zensys.zensys.lk` is a legitimate tenant host whose slug is `zensys`. Do not add the apex label to `reservedHosts` — it would make the published `platform-policy` disagree with `reserve`.

## Conventions

- **Go stdlib first.** `net/http` + `ServeMux`, `log/slog`, `httputil.ReverseProxy`, `database/sql`. Five external deps max (mysql driver, yaml.v3, prometheus, x/time/rate, x/sync). No framework, no ORM, no DI. `make test` runs `go test ./... -race`; the race detector is not optional.
- **No secrets in config.** Config names env vars; the loader resolves them at startup and fails loudly. **Fail-fast validation** before any listener opens — except the registry, deliberately optional so a DB outage degrades routing instead of crash-looping.
- **Single authoritative definition, and a rule gets exactly one home.** Reserved hosts are compiled into `internal/tenant/reserved.go` so a bad deploy cannot empty it; config may only *add*. `ParseHost` does structure only; reserved-host rejection is a pipeline step (§65); slug shape is `tenant.ValidateSlugFormat`. Never duplicate a rule into another layer.
- **`internal/proxy` owns the terminal step, and `routing` must not import `config` or `tenant`.** A proxy that builds per-product transports from configuration needs both, which is why it is a separate package. Keep that direction.
- **Each route owns its transport and its `ReverseProxy`; nothing is shared between routes.** Makes §48's "no cross-product fallback" structural: there is no object holding two products' upstreams, so there is no place to write one. `internal/proxy/structure_test.go` (AST guard with its own negative controls) fails if `Proxy` gains a second `*route` field or `route` gains any field reaching another `route`.
- **Error mapping in `proxy.handleError`:** client-cancel → log only, no response · `*http.MaxBytesError` → 413 · timeout → 503 + `Retry-After: 5` · everything else → 502. A product's own status is never reinterpreted. **A timeout is 503, an unreachable product is 502** — one is worth retrying, the other may not be (§47).
- **`FlushInterval: -1`, not 0.** Go flushes immediately for `text/event-stream` or an unknown `Content-Length`, but that is a heuristic over headers a product controls. It works only because `statusRecorder.Unwrap` exists — `ReverseProxy` flushes via `http.NewResponseController`, which unwraps. `TestProxy_StandardChainExposesFlush` pins that dependency.
- **The body limit is enforced twice, and both halves are load-bearing.** `MaxBodyBytes` refuses a declared `Content-Length` over `server.max_body_bytes` *before* routing or dialling; `proxy.handleError` maps `*http.MaxBytesError` to the same 413 for a chunked body, which declares no length and is only caught mid-read.
- **The header contract is one policy, applied to both listeners.** A forwarded header is believed only when the connection came from `proxy.trusted_proxies`; otherwise it is ignored, never "cleaned up". `X-Tenant-ID` is deleted unconditionally — routing is host-derived, always. **The client address is the rightmost *untrusted* entry of `X-Forwarded-For`**; outbound is the resolved client alone. **Every hop must be listed**, and an omission fails silently: routing is unaffected, because routing never reads a forwarded header.
- **`RequestState` must stay the outermost middleware in `httpapi.StandardChain`.** It installs **two** per-request slots — the routing decision and the authenticated service identity. Both are written by a handler and read by the access log *outside* it, and a derived context travels only inward. This bit twice: M4 (routing) and M6 (identity). Anything listed before it installs nothing.
- **`tenant` in a log line means a slug that resolved to a route, and nothing else.** A hostname label that has not resolved is `host_label`.
- **Tokens are compared as SHA-256 digests via `crypto/subtle`, and the loop never exits early.** `ConstantTimeCompare` returns 0 immediately on a length mismatch, leaking the stored token's *length*; hashing makes every comparison a fixed 32 bytes and keeps the raw secret off the long-lived heap. Never `==`, `bytes.Equal`, or `reflect.DeepEqual` on a token.
- **`internal/archtest` guards the source tree**, failing on any package-level variable that could hold per-request routing state (§31). Its detector has its own tests, so a typo that made it match nothing fails instead of silently disabling the guard. **A guard must be observed to fail before it is trusted.**
- **The route cache is never authoritative.** Deleting every entry must leave the Gateway fully correct (§92). A registry *failure* is never cached (only `ErrNotFound`); loads run on a context detached from the caller's; the cache is re-checked *inside* the singleflight call; a write is discarded if an invalidation landed while its read was in flight.
- **Public path and lifecycle path have different consistency needs.** The resolver reads through the cache; the lifecycle `Service` reads the repository directly and drops the cached route after every mutation (§33). Reserving invalidates even though the slug is new, because a prior probe cached its *absence*.
- **Never put a tenant slug on a Prometheus label.** Unbounded cardinality. The same rule governs any keyed structure whose keys the *caller* chooses — a rate-limiter's key table is the second instance of this class.
- **MySQL integration tests are gated on `ZENSYS_TEST_DSN` and skip without it.** A green `go test ./...` says nothing about them; check the count (16 passes for `-run TestMySQL`). They `DELETE FROM tenant_routes` in setup, so running them against the dev DB wipes the probe harness's fixtures.
- **`configs/.probe/` is the evidence, not the deliverable.** `run.sh` asserts every result and exits non-zero on failure; `upstream/` is a minimal stand-in for the M9 `cmd/mockproduct`. Generated configs, logs, and bodies are gitignored; the harness is committed so the claims can be re-checked.

## Security hardening (M7, §81)

- **Stateless checks run before stateful ones (§8.1).** The body-size limit is step 3 and the tier-1 limiter is step 4, deliberately: the limit is a comparison against a declared `Content-Length`, while the limiter consults a bucket and spends a token. A request that cannot be accepted on its own terms is refused on its own terms. For a chunked body the limiter still refuses before a byte is read, in either order.
- **Two rate-limit tiers, because one cannot do both jobs.** Tier 1 is keyed by the resolved client address and runs in the middleware chain; tier 2 is keyed by tenant slug and runs inside the routing handler after resolution. An IP-only limiter lets one noisy tenant behind shared NAT starve neighbours; a tenant-only limiter makes an attacker hammering unknown hostnames reach the registry first. **Rate limiting is an availability control, so it fails open** — a nil or disabled limiter allows everything, and the Gateway logs a Warn when it is disabled, because a disabled limiter and a silently failing-open one look identical from outside.
- **The limiter's key table is bounded, and eviction is two rules in order:** sweep entries unseen for one refill period (`burst/rps` seconds — an entry that stale has certainly regained every token, so it is indistinguishable from an absent one), then, if still full, drop the least-recently-seen. The second rule discards a real client's state, which is the honest consequence of a bound: a caller presenting enough distinct keys can reset others' buckets. A bound that degrades under key churn beats a bound that does not exist. Eviction is **per shard**, so its cost is bounded by the shard rather than the tier — which makes reclaim lazy, and a test asserting a tier-wide sweep is asserting the wrong thing.
- **A 429's tier is recoverable from the access log, and it rests on one line.** Tier 1 runs before the hostname is parsed so it can never carry a `tenant`; tier 2 runs after resolution so it can. That holds only while `PublicHandler.ServeHTTP` calls `routing.SetInfo` **before** its tier-2 check. Moving it below makes a tier-2 refusal an anonymous 429. `TestTier2RateLimit_RefusalIsDistinguishableInTheLog` pins it.
- **`Retry-After` is floored at 1 for a refusal, and 0 is reserved for the allowed case.** A refusal that becomes admissible within the current second would otherwise report 0, which is an instruction to spin.
- **net/http's header limit is `MaxHeaderBytes + 4096`** ("bufio slop", `net/http/server.go`), and above it the server answers a **bare text 431 straight to the connection, before any handler or middleware, logging nothing even with `ErrorLog` set**. `HeaderSizeLimit` closes the band the Gateway can see; the remainder is not fixable from inside the process. An over-long request URI shares the header budget, so it is 431 and not 414.
- **Bound the client-chosen log field.** The path is the one log field whose length the caller picks, and size-refused requests run *before* the limiter, so nothing else throttles them. `maxLoggedPathLength` (512) truncates it with an ASCII `...`.

## Environment

Go 1.27.0 (`C:\Program Files\Go\bin`, not on the sandbox PATH by default). Git 2.55.0, Docker 29.7.2, MySQL 8.0.45. No PostgreSQL. `make` is **not** installed on Windows — raw `go` commands are the documented path; the Makefile is for CI. Module Go floor is 1.26.0, set by `golang.org/x/sync`.

Local MySQL accepts `root:root` (passwordless `root` fails); client at `C:\Program Files\MySQL\MySQL Server 8.0\bin\mysql.exe`; dev DB `zensys_gateway`. The race detector needs a C compiler — `CC` is set to WinLibs gcc via `go env -w`, so ordinary builds stay pure Go and test targets pass `CGO_ENABLED=1`. **Run `gofmt -l .` explicitly**; `go vet` does not catch formatting, and it has already let one test file through.

## Working notes

- **Run the real binary before declaring a milestone done.** It has found a genuine defect at *every* milestone: M0 (a 405 test passing only because it omitted the production catch-all; a log level hiding 5xx on health paths), M1 (a `Reserve` that lowercased input instead of rejecting it), M2 (IP literals checked only after port stripping), M4 (a `rejectRoute` logging an unvalidated label under the key `tenant`), M5 (an oversized body answering 502 because `ErrPayloadTooLarge` was declared and written by nothing), M6 (a probe config whose substitution silently stopped matching), M7 (a tier-2 refusal with no `tenant` in the log, because `SetInfo` ran after the tier-2 check).
- **Grep for error codes that are declared but never written.** `WritePayloadTooLarge` existed for two milestones while no path could produce a 413. An exported helper nothing calls is a defect class, not dead code — the branch it belongs to was never implemented.
- **Writing the test table before the implementation repeatedly exposes spec bugs** — the 64-char slug and the false punycode claim in §8.3 were both found that way. Treat a failing expectation as a question about the spec.
- **Prove a guard fails before trusting it — and when a break does not fail the test, fix the claim, not the break.** M7 attempted four breaks; three did not discriminate on the first try. A comment that overstates what a test establishes is worse than no comment, because the next reader will trust it. Two of the three were comments corrected to match reality; the third needed a new test written (`TestLimiter_ForcedEvictionDropsTheStalestLiveEntry`) to reach the path the old one never executed.
- **A timing-dependent test must name the cause in its failure message**, or the run reports a bare `EOF` after a long hang and teaches nothing.
- **A readiness check proves *something* is ready, not that the process you started is.** The harness once measured a configuration it had never loaded, because a stale gateway answered `/readyz` while the new one died on a port clash. Wait for the ports free before starting, and check liveness while waiting. Corollary: `kill` from Git Bash is a hard termination, so a clean harness run is *not* evidence that graceful shutdown works.
- **A harness that measures a service must not talk to it in order to find it.** A liveness probe written as an HTTP request *is* a request: on the public listener it is logged and it spends a rate-limit token. M7's tier-1 section failed with a 429 where it wanted a 200 for exactly this reason. `wait_ports_free` now asks the kernel with a raw TCP connect (`/dev/tcp`), which also costs no process.
- **Make a timing assertion unable to pass by accident.** Section F sets both TTLs to 300 s so "the next request reflects the change" cannot be satisfied by cache expiry; the limiter's bound test freezes the clock so the cap is the only thing holding the table down.
- `D:\c` holds the user's pre-existing Code::Blocks projects. Leave it alone.

## Open items

**Closed:** O1 registry DSN (supplied via `ZENSYS_REGISTRY_DSN`) · O2 module path (`github.com/zensys/gateway`) · O5 audit table (`tenant_route_events`, included and live).

**Open:** O3 real POS/GMS upstreams · O4 POS-side `/api/...` work (separate project) · O6/O7 rate-limit and cache TTL values · O8 apex landing content.

**Carried out of M7:** every-hop-must-be-listed is a prerequisite for M10, not a tuning note. Size-refused requests are logged but not rate limited; the bound is `maxLoggedPathLength`. The internal listener's `disable`/`release` still authorize against the *stored* product, so a single-product service can distinguish 404 from 403 and enumerate the other product's tenants by guessing slugs — M7 was named as the place to tighten this and did not, deliberately: both callers are loopback services and the plan specifies those statuses.
