# ZenSys Gateway

Platform-level edge service that routes `{tenant}.zensys.lk` traffic to the correct product.

```text
hostname  →  tenant  →  product  →  upstream
```

The Gateway decides **which product** a request belongs to. POS and GMS continue deciding **which tenant database** it belongs to. That separation is the permanent architectural boundary.

---

## Status

**M7 complete** — the Gateway is hardened. Requests are rate limited in two tiers, oversized requests are refused before they cost anything, and every refusal is answered in the same error contract. The trust policy for forwarded headers is settled and documented.

M5 made it proxy; M4 made the request-scoped context and header contract live; M6 made the registry mutable through a controlled door; M7 makes it survive being talked to by someone who is not a customer.

The Gateway **starts even when the registry is unreachable**. That is deliberate, not an oversight: it logs a warning, reports `registry: failed` on `/readyz`, and stays up so that a registry outage degrades routing rather than causing a crash-loop. See [Registry behaviour under failure](#registry-behaviour-under-failure).

| Milestone | State |
|---|---|
| M0 — Bootstrap and environment | Done |
| M1 — Registry layer | Done |
| M2 — Host resolution | Done |
| M3 — Route cache | Done |
| M4 — Request context and headers | Done |
| M5 — Reverse proxy | Done |
| M6 — Internal lifecycle API | Done |
| M7 — Security hardening | Done |
| M8 — Observability | Next |
| M9 — Mock integration environment | — |
| M10 — Pilot preparation | — |

For the evidence behind each "Done", and for every place the implementation diverges from the plan and why, see **Implementation status** in [`IMPLEMENTATION_PLAN.md` §17](IMPLEMENTATION_PLAN.md).

| Document | Purpose |
|---|---|
| [`IMPLEMENTATION_PLAN.md`](IMPLEMENTATION_PLAN.md) | How this gets built, in what order, with exit criteria |
| [`docs/adr/0001-v1-open-decisions.md`](docs/adr/0001-v1-open-decisions.md) | The §94 decisions, with rationale |
| [`docs/architecture/ZenSys_Gateway_V1_Architecture.md`](docs/architecture/ZenSys_Gateway_V1_Architecture.md) | Architecture specification (source of truth) |

---

## What this is not

Not a replacement for POS or GMS tenancy. Not an authentication service. Not a business logic layer.

The Gateway does **not**: authenticate product users, issue or validate JWTs, store roles or permissions, access product business databases, hold product database credentials, or contain POS/GMS code.

---

## Decisions already made

| Area | Decision |
|---|---|
| Registry database | MySQL 8 |
| Frontend serving | Gateway proxies to product frontend servers |
| Internal API security | Private bind + per-service tokens, structured mTLS-ready |
| Platform hosts | Apex serves a landing page; other reserved hosts return 404 |
| Disabled tenant | 404, byte-identical to unknown tenant |
| V1 availability | HA-ready design, one instance |
| Deployment topology | Undecided — containerized and portable |
| Tenant model | One slug → exactly one product |

---

## Key routing rules

| Request | Destination |
|---|---|
| `mangala.zensys.lk/` | POS frontend |
| `mangala.zensys.lk/api/orders` | POS backend |
| `garage1.zensys.lk/` | GMS frontend |
| `garage1.zensys.lk/api/v1/jobs` | GMS backend |
| `random.zensys.lk/` | 404 — no default tenant, no fallback |
| `a.b.zensys.lk/` | 404 — multi-label rejected |
| `admin.zensys.lk/` | 404 — reserved |
| `zensys.lk/` | Platform landing page |

`/api` is forwarded **unchanged**. No prefix stripping, ever.

---

## Host resolution

Every request's `Host` header passes through one ordered sequence. Order matters, because each step closes a specific hole:

| # | Step | Why it is in this position |
|---|---|---|
| 1 | Trim whitespace, reject empty | Go's parser already strips it; a direct caller needs the guard. |
| 2 | Reject IP literals (`127.0.0.1`, `[::1]`) | A plausible-looking hostname that must never be a tenant. Checked before *and* after port stripping, because `[::1]` makes the port split fail and would otherwise be reported as malformed rather than as an address. |
| 3 | Strip the port | The port is discarded, but a non-numeric port is rejected — `SplitHostPort` does not check that, and accepting `host:http` would make this parser's strictness inconsistent. |
| 4 | Trim one trailing dot, lowercase | `mangala.zensys.lk.` is legal FQDN notation. Lowercasing is a *correction*, not a leniency: DNS is case-insensitive, so `MANGALA.zensys.lk` and `mangala.zensys.lk` are the same host. |
| 5 | Character allow-list `[a-z0-9.-]` | One rule rejects percent-encoding, underscores, whitespace, non-ASCII bytes, and CR/LF injection. |
| 6 | Reject `xn--` tenant labels | Punycode is spelled entirely in `[a-z0-9-]`, so the allow-list does **not** catch it. The slug policy is ASCII-only, so an IDN host can never be a real tenant. |
| 7 | Length limits: host ≤ 253, label ≤ 63, no empty labels | Empty labels matter — without the check, `.zensys.lk` matches the root-domain suffix and yields an *empty* tenant slug. |
| 8 | Root-domain validation | `zensys.lk` is the apex; exactly one label below it is a tenant; anything else is rejected. |

Two rules are deliberately **not** in the parser, because each already has exactly one home:

- **Reserved hosts** are checked in the pipeline, *after* the multi-label check, because §65 fixes that ordering. `ParseHost` classifies `admin` as an ordinary tenant label so the log can distinguish "reserved" from "malformed".
- **Slug policy** (length 3–63, no leading or trailing hyphen) is enforced by `tenant.ValidateSlugFormat` at the next step. `ab.zensys.lk` and `-abc.zensys.lk` are legal *DNS labels*, so the parser accepts them and the slug policy rejects them.

### Every rejection is the same 404

Unknown tenant, disabled tenant, still-provisioning tenant, reserved host, multi-label host, wrong domain, IP literal, malformed host, punycode, invalid slug — all produce a **byte-identical** `404` body and `Content-Type`. If they differed, an anonymous prober could enumerate which hostnames belong to real businesses.

The distinction is preserved in the **logs**, where it is useful and not attacker-visible:

```json
{"msg":"host rejected","host":"admin.zensys.lk","reason":"reserved platform host \"admin\""}
{"msg":"host rejected","host":"a.b.zensys.lk","reason":"routing: multiple labels below the root domain: ..."}
```

### The one exception: 503

A registry that cannot answer says nothing about whether the tenant exists. The Gateway must not guess, so a resolution failure returns `503 registry_unavailable`, never a `404` — a `404` would break a working tenant during a database blip and would make the Gateway's own outage invisible.

---

## Header contract

A header is believed only when the connection demonstrably came from an address in `proxy.trusted_proxies`. There is no middle ground and no attempt to clean up an untrusted value — a client that can set `X-Forwarded-Proto` can set it to anything, so the only safe response is to ignore it.

| Header | Client → Gateway | Gateway → upstream |
|---|---|---|
| `Host` | read, normalized | preserved as the public host |
| `X-Tenant-ID` | **always deleted**, trusted proxy or not | set to the canonical tenant, only if `inject_tenant_header: true` |
| `X-Request-ID` | accepted only from a trusted proxy | the canonical request ID |
| `X-Forwarded-For` | discarded unless from a trusted proxy | the resolved client address |
| `X-Forwarded-Host` | discarded unless from a trusted proxy | the public host |
| `X-Forwarded-Proto` | discarded unless from a trusted proxy | the scheme as seen at the edge |
| `Authorization`, `Cookie` | untouched | untouched |

Three things about this are less obvious than they look.

**Routing never reads a forwarded host.** `X-Forwarded-Host` is believed from a trusted proxy, but only for what the Gateway *advertises* downstream. Which tenant a request reaches is decided by the request's own `Host` header, always. Otherwise anyone who could forge a request from a trusted address could serve one tenant's traffic under another's hostname.

**The client address is the rightmost untrusted entry of `X-Forwarded-For`.** Every hop appends, so the leftmost entry is whatever the client chose to send. Reading it — the obvious implementation — would let any client pick its own apparent address, and once rate limiting lands, its own rate limit.

**`X-Forwarded-For` is set, not appended.** `httputil.ProxyRequest` strips inbound forwarded headers before `Rewrite` runs, so appending would mean re-introducing a chain the Gateway never verified. The outbound value is the single address it resolved.

### The trap this design exists to avoid

The Gateway sits behind Cloudflare → Nginx, so `r.TLS` is `nil` and the only truthful source for the scheme is the trusted proxy's header. A naive `SetXForwarded()` would advertise `X-Forwarded-Proto: http` to every product while the browser used HTTPS — products building absolute URLs or setting `Secure` cookies would work correctly in development and wrongly in production, which is the worst way for a bug to behave.

### Fail closed

`routing.EdgeFrom` reports `false` for a context with no edge, and the request-ID policy treats that as untrusted. A chain that somehow lost the trusted-proxy middleware generates IDs rather than believing the client's — a lost correlation ID instead of a forged one.

---

## Reverse proxy

```text
Host ──► tenant ──► product ──► stage ──► upstream
                       │          │
                       │          └─ /api... → backend, everything else → frontend
                       └─ POS or GMS, from the registry record
```

That is the whole of V1's routing job. The Gateway does not resolve a tenant to a database, does not touch a product's data, and does not rewrite the path or query — they reach the product exactly as the client sent them, so the products' own route tables are the only ones that matter.

**The stage split is one rule.** A path that *is* `routing.backend_path_prefix` or is under it goes to the backend; everything else goes to the frontend. The match is on whole path segments, so `/apix` and `/api-v2` are frontend traffic — a plain prefix match would send them to a backend that has no such route, trading a clear 404 for a confusing one. Matching is case-sensitive, because HTTP paths are.

**Each route owns its transport and its reverse proxy.** Nothing is shared between routes, which is what makes §48's "no cross-product fallback, ever" structural rather than a promise: there is no object holding two products' upstreams, so there is no place a fallback could be written. The error handler for a POS route has no reference to GMS — an absent path, not a disabled one. `internal/proxy/structure_test.go` fails the build if that shape changes, and the guard has its own negative controls so it cannot quietly stop matching.

| Condition | Response |
|---|---|
| Product unreachable | `502 upstream_unavailable` |
| Product did not answer in time | `503 upstream_unavailable` + `Retry-After: 5` |
| Body over `server.max_body_bytes` | `413 payload_too_large`, before any upstream is contacted |
| Product returns 500, 404, 302, … | passed through **unmodified** |

A timeout is a 503 and an unreachable product is a 502 because they are different situations: one is worth retrying and the other may not be. Both are the Gateway's own errors, and neither ever falls through to another product.

Streaming responses are flushed immediately (`FlushInterval: -1`) rather than relying on Go's heuristic, which only recognises `text/event-stream` or an unknown `Content-Length`. That heuristic is a decision made from headers a product controls, and a server-sent-events endpoint is not the only thing that streams.

`proxy.preserve_host` (default `true`) sends the product the hostname the client asked for, not the upstream's internal address. This is not cosmetic: GMS resolves its own tenant from the `Host` header, so an internal address here would break tenant resolution inside the product.

---

## Route cache

```text
request → cache ── HIT  → route
                 └ MISS → registry → cache → route
```

| Property | Behaviour |
|---|---|
| Positive TTL | `cache.route_ttl` (default 60s) |
| Negative TTL | `cache.negative_ttl` (default 30s) — short, so a newly registered tenant becomes routable quickly (§32) |
| Stampede control | `singleflight` keyed by **slug**, so N concurrent misses for one cold tenant produce one registry read |
| Invalidation | Every successful lifecycle mutation drops the affected slug (§33) |
| Capacity | `cache.max_entries`. On overflow the cache **refuses to evict a live entry** and serves the new slug uncached |
| Authority | **Never authoritative.** `Flush()` at any moment must leave the Gateway fully correct (§92) |

Measured against a real MySQL, with a 60s TTL and bursts completing in under a second:

| Burst | Registry reads |
|---|---|
| 200 requests, one cached tenant | ~0 |
| 100 requests, one unregistered tenant | 1 (then the negative cache) |

### Three non-obvious decisions

**A registry failure is never cached.** "I could not ask" is not an answer. Caching it would turn a transient outage into a negative TTL of wrong answers, and would keep serving them after the registry recovered. An *absence* is an answer and is cached; a *failure to answer* is not.

**The load runs with a context detached from the caller's.** `singleflight.Do` runs the load in the first caller's goroutine using that caller's context, so if that client disconnects, every request waiting on the same slug inherits its cancellation — one dropped connection fails a whole cohort of unrelated requests. The load is detached and bounded by its own timeout; each waiter still gives up when its own request goes away.

**The cache is re-checked inside the singleflight call.** `singleflight` deletes a key the moment its function returns, so the window between a cache miss and arrival at `DoChan` is not atomic with the store. A caller that missed just before the previous call finished would otherwise start a *second* registry read for the same slug. Re-checking inside the call closes that gap; without it, a burst for one cold tenant produces two reads instead of one.

**A stale write is discarded.** A request that misses reads the registry, and the route can change and be invalidated while that read is in flight. Writing the answer anyway would serve a state that no longer exists for a full TTL — a newly activated tenant would stay unroutable for up to `route_ttl`, which looks exactly like a cache bug and is miserable to reproduce. A single global invalidation counter, rather than one per slug, keeps memory bounded as unknown hostnames are probed.

### Where the cache is *not* used

The lifecycle API reads through the repository, never the cache. A lifecycle decision must be made on the truth, not on a copy that may be up to `route_ttl` old. The two consumers deliberately have different consistency needs.

---

## Registry

MySQL 8. Two tables:

| Table | Purpose |
|---|---|
| `tenant_routes` | The routing decision. `tenant_slug` is the primary key, so one slug maps to exactly one product — globally, enforced by the storage layer. |
| `tenant_route_events` | Append-only audit trail. Written in the **same transaction** as the mutation it describes, so it can neither record a rolled-back change nor miss a committed one. |

Status transitions use compare-and-swap:

```sql
UPDATE tenant_routes SET status = ? WHERE tenant_slug = ? AND status = ?
```

The `AND status = ?` clause is load-bearing. Without it, two concurrent callers could each read `PROVISIONING` and each write a different target status, and the last writer would silently win. With it, the loser gets a conflict and retries against fresh state. `TestMySQL_Transition_ConcurrentOnlyOneWins` races eight goroutines at one transition and asserts that exactly one succeeds.

Apply migrations with:

```bash
go run ./cmd/migrate -config configs/gateway.dev.yaml -dry-run   # print, change nothing
go run ./cmd/migrate -config configs/gateway.dev.yaml            # apply
go run ./cmd/migrate -config configs/gateway.dev.yaml            # no-op; already applied
```

The runner tracks applied files in a `schema_migrations` table, so re-running is safe. It requires `parseTime=true` in the DSN and refuses to start without it — without that flag the driver hands back `[]byte` for timestamps and every scan fails at request time instead of at startup.

### Registry behaviour under failure

| Situation | Behaviour |
|---|---|
| DSN malformed | Process exits with a configuration error. A malformed DSN is a bug, not an outage. |
| Registry unreachable at startup | Process **starts anyway**. Warns, and `/readyz` reports `registry: failed`. |
| Registry unreachable during operation | `/readyz` reports `registry: failed`; liveness stays `200`. |

Liveness deliberately performs **no** dependency checks. A liveness probe that failed because the registry was down would make an orchestrator restart a healthy process and turn a registry outage into a restart loop.

Readiness failure details are logged but never written to the response body — the endpoint can be exposed publicly via `health_on_public`, and a raw driver error would leak internal addresses.

---

## Internal lifecycle API

The registry is mutable, and this is the only door. It is a separate `http.Server` on `internal_addr` (loopback by default), not a route on the public mux — there is no public address at which it can be reached, which is a structural fact rather than a configuration choice.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/internal/v1/tenant-routes/reserve` | create as `PROVISIONING` |
| `POST` | `/internal/v1/tenant-routes/activate` | `→ ACTIVE` |
| `POST` | `/internal/v1/tenant-routes/disable` | `→ DISABLED` |
| `POST` | `/internal/v1/tenant-routes/release` | delete the row |
| `GET` | `/internal/v1/tenant-routes/{tenant}` | one route |
| `GET` | `/internal/v1/tenant-routes` | list, filterable by `product`, `status`, `after`, `limit` |
| `GET` | `/internal/v1/platform-policy` | the slug pattern, lengths, and reserved hosts |

```bash
export ZENSYS_TOKEN_POS="$(printf 'a%.0s' $(seq 1 32))"

# Reserve, then activate. The tenant is not routable until it is ACTIVE.
curl -s -X POST -H "Authorization: Bearer $ZENSYS_TOKEN_POS" \
  -H 'Content-Type: application/json' \
  -d '{"tenant":"garage1","product":"POS"}' \
  http://127.0.0.1:8081/internal/v1/tenant-routes/reserve

curl -s -X POST -H "Authorization: Bearer $ZENSYS_TOKEN_POS" \
  -H 'Content-Type: application/json' \
  -d '{"tenant":"garage1","product":"POS"}' \
  http://127.0.0.1:8081/internal/v1/tenant-routes/activate

# Release needs the slug repeated. There is no way to do it by accident.
curl -s -X POST -H "Authorization: Bearer $ZENSYS_TOKEN_POS" \
  -H 'Content-Type: application/json' \
  -d '{"tenant":"garage1","confirm":"garage1"}' \
  http://127.0.0.1:8081/internal/v1/tenant-routes/release
```

### Status codes

| Code | Meaning |
|---|---|
| `200` / `201` | success / reserved |
| `400` | malformed body, unknown field, invalid slug, bad product, missing or mismatched `confirm` |
| `401` | no token, unknown token, wrong scheme, or two `Authorization` headers |
| `403` | authenticated, but the product is not in the caller's `may_manage_products` |
| `404` | no such route, or no such endpoint under the prefix |
| `405` | right path, wrong method; the `Allow` header names what it accepts |
| `409` | slug taken, product mismatch, or a concurrent modification |
| `503` | registry unavailable |

### Five decisions worth knowing

**A mutation is visible on the very next public request.** The lifecycle service reads through the repository — never the cache — and drops the cached route after every successful mutation. Without that, a newly activated tenant would keep returning 404 for up to `route_ttl`, and a suspended one would keep serving. The probe proves it with both TTLs at 300 s: nothing can converge on its own, so the next request either reflects the change or it does not.

**Reserving invalidates even though the slug is new.** Anyone who probed `{slug}.zensys.lk` before the reservation cached its *absence*, so a route cache alone would leave the tenant returning 404 for up to `negative_ttl` after it was reserved.

**`disable` and `release` take no product.** The product is read from the registry and authorization is checked against the **stored** value, which removes the whole class of bug where a caller names a product that does not match the route it is acting on. The consequence, documented rather than hidden: a service that manages one product can distinguish "no such slug" from "exists but is another product's", and so can enumerate the other product's tenants by guessing slugs. Both callers are the platform's own services on a loopback listener. **M7 considered tightening this and deliberately did not**: the listener is loopback-only, both callers are the platform's own services, and the status codes are the ones the plan specifies. It becomes a real finding the moment that listener is reachable from anywhere else, and it is recorded in `IMPLEMENTATION_PLAN.md` §13.3 as such.

**`release` checks its confirmation before reading the registry.** That ordering is a disclosure property, not an optimisation: reading first would let a caller that forgot the field use 400-versus-404 to probe which slugs exist.

**Tokens are compared as SHA-256 digests, and the loop never exits early.** `subtle.ConstantTimeCompare` returns 0 immediately when lengths differ, which leaks the stored token's *length*; hashing both sides first makes every comparison a fixed 32 bytes. Comparing every token without returning early means the response time does not depend on the position of the match. Both properties are guarded by tests, and the guard was observed to fail when an early `return` was added. A short-circuiting `==` on a bearer token is a remote timing oracle.

### The mTLS seam

Authentication sits behind a one-method interface, and authorization is separate from it:

```go
type Authenticator interface {
    Identify(*http.Request) (ServiceIdentity, error)
}
```

`tokenAuth` implements it for V1. Adding mTLS means adding a second implementation, not touching a handler or a single authorization check. That is what "mTLS-ready" means here — not a stub, just a boundary that is already load-bearing.

---

## Rate limiting

Two tiers, because one limiter cannot do both jobs.

| Tier | Key | Runs | Why there |
|---|---|---|---|
| 1 | Resolved client address | Middleware chain, before any lookup | An attacker hammering unknown hostnames must not reach the registry at all |
| 2 | Tenant slug | Inside the routing handler, after resolution | One noisy tenant behind shared NAT must not starve its neighbours |

The tier-1 key is the address resolved through `proxy.trusted_proxies`, **not** the socket address — keying on the socket would put every client on the internet into one bucket, and trusting a raw header would let anyone mint a fresh bucket per request. Requests to `healthz`, `readyz`, and `metrics` are exempt: a probe refused for being too frequent reports the service as dead, and an orchestrator that restarts on a failed probe turns a load spike into an outage.

A refusal is `429` with `Retry-After`, in the same error contract as every other refusal:

```bash
curl -si -H "Host: mangala.zensys.lk" http://127.0.0.1:8080/ | head -1
# HTTP/1.1 429 Too Many Requests
# Retry-After: 1
```

`Retry-After` carries the limiter's own estimate of when the bucket refills, floored at one second — a refusal that reports `0` is an instruction to retry immediately, which is the opposite of what a rate limit is for. The response deliberately does not say which tier refused or what the limit is: a caller that can read the limit can time its traffic to sit just under it.

**The tier is still recoverable, from the log.** Tier 1 refuses before the hostname is parsed, so its log line cannot carry a `tenant`; tier 2 refuses after resolution, so its log line does. That distinction is structural — it holds because the routing decision is recorded *before* the tier-2 check — and a test pins it.

**The key table is bounded.** Its keys are chosen by whoever is sending requests, so an unbounded map would be a memory-exhaustion vector — the same class of defect as an unbounded metric label. `rate_limit.max_entries` caps it, and eviction sweeps entries that have not been seen for a full refill period first, since such an entry has certainly regained every token and is indistinguishable from one that never existed. If a shard is still full, the least-recently-seen entry is dropped; that discards a real client's state, which is the honest cost of a bound. **Raise `max_entries` above the number of distinct clients you expect within one refill period** if you see clients being reset.

**Rate limiting fails open.** A nil or disabled limiter allows everything, and the Gateway logs a warning at startup when rate limiting is off — because a disabled limiter and a limiter that is silently failing open look identical from the outside. This is an availability control, not an authorization one.

## Size limits

Oversized requests are refused before they cost anything, and the refusal is in the same error contract as everything else.

| Condition | Status | Error code | Where |
|---|---|---|---|
| Declared `Content-Length` over the limit | `413` | `payload_too_large` | Before routing or dialling |
| Chunked body over the limit | `413` | `payload_too_large` | Mid-read, by the proxy |
| Header block over the limit | `431` | `headers_too_large` | Before routing |
| Request URI long enough to blow the header budget | `431` | `headers_too_large` | Shares the header budget |

The body limit is enforced twice and both halves are load-bearing: a declared `Content-Length` is a cheap comparison, while a chunked body declares no length and can only be caught as it is read.

**There is a band the Gateway cannot close.** `net/http` reads up to `MaxHeaderBytes + 4096` bytes before refusing (the 4096 is deliberate slack in `net/http/server.go`), and above that it answers a bare text `431` written straight to the connection — before any handler or middleware runs, and **without logging anything**, even with `ErrorLog` configured. `HeaderSizeLimit` closes the band the Gateway can see; the remainder is not fixable from inside the process. An over-long request URI shares that same budget, so it is a `431` and not a `414`.

The access log's `path` field is truncated at 512 bytes, because it is the one log field whose length the caller chooses.

---

## Local development

`*.zensys.lk` does not resolve locally, so every check uses an explicit `Host` header.

**Note:** the recipes that expect a *product* response need something listening on the configured upstream ports (POS frontend `3001`, POS backend `8082`, GMS frontend `3002`, GMS backend `8083`). Without one, a tenant that resolves correctly answers `502 upstream_unavailable` — which is itself the proof that resolution and proxying both worked. The apex, 404, and rejection recipes are final behaviour.

```bash
# Apex landing page
curl -s -H "Host: zensys.lk" http://127.0.0.1:8080/

# POS frontend (502 means the tenant resolved and the upstream is down;
# 404 means the tenant is not registered)
curl -s -H "Host: mangala.zensys.lk" http://127.0.0.1:8080/

# POS backend
curl -s -H "Host: mangala.zensys.lk" http://127.0.0.1:8080/api/orders

# GMS backend
curl -s -H "Host: garage1.zensys.lk" http://127.0.0.1:8080/api/v1/jobs

# Tenant header spoofing must be ignored.
#
# The strongest form of this check needs an upstream that reports what it
# received, which is what configs/.probe/upstream/ provides. Without one, the
# observable is the status code: the spoofed slug does not exist, so a 404
# would mean the header was believed, while a 502 means the host decided and
# the host is a real tenant.
curl -si -H "Host: garage1.zensys.lk" -H "X-Tenant-ID: nonexistent" \
     http://127.0.0.1:8080/

# An inbound request ID is adopted from a trusted proxy, and echoed back
curl -si -H "Host: garage1.zensys.lk" -H "X-Request-ID: trace-abc-123" \
     http://127.0.0.1:8080/ | grep -i x-request-id

# Unknown tenant — expect 404
curl -si -H "Host: random.zensys.lk" http://127.0.0.1:8080/

# Reserved host — expect 404
curl -si -H "Host: admin.zensys.lk" http://127.0.0.1:8080/

# Multi-label — expect 404
curl -si -H "Host: a.b.zensys.lk" http://127.0.0.1:8080/

# Internal API is not reachable on the public port — expect 404
curl -si -H "Host: mangala.zensys.lk" http://127.0.0.1:8080/internal/v1/tenant-routes
```

The access log carries the routing decision:

```json
{"level":"INFO","msg":"request","request_id":"trace-abc-123","host":"garage1.zensys.lk",
 "tenant":"garage1","product":"GMS","method":"GET","path":"/","status":200}
```

A rejected host has no `tenant` field at all — an empty one would be indistinguishable from a tenant whose slug is empty. Its rejection log uses `host_label`, because at that point the label is not yet a tenant.

The full end-to-end check — four upstreams, both trust configurations, the routing matrix, host preservation, the header contract, streaming, request bodies, and a product outage — is `configs/.probe/run.sh`. It asserts every result, exits non-zero on failure, and takes the registry DSN from `ZENSYS_REGISTRY_DSN`:

```bash
export ZENSYS_REGISTRY_DSN='root:secret@tcp(127.0.0.1:3306)/zensys_gateway?parseTime=true'
bash configs/.probe/run.sh
```

It needs two ACTIVE tenants — `mangala` (POS) and `garage1` (GMS) — and says so, with the SQL to fix it, if either is missing.

Health and readiness live on the **internal** listener only:

```bash
curl -s http://127.0.0.1:8081/healthz
curl -s http://127.0.0.1:8081/readyz
```

---

## Layout

```text
cmd/gateway/          entrypoint — wiring only
cmd/migrate/          SQL migration runner (idempotent, tracked)
cmd/mockproduct/      mock POS/GMS frontends and backends      [M9]
internal/config/      configuration loading and validation
internal/tenant/      registry model, repository, service, route cache, reserved hosts
internal/routing/     host parsing, resolver, stage selector, header contract, trust set
internal/proxy/       reverse proxy: per-product transports, rewrite, error mapping
internal/security/    service auth (bearer tokens), product authorization, two-tier rate limiting
internal/httpapi/     public pipeline, lifecycle API, apex, health, middleware
internal/observability/  logging                                   [metrics M8]
internal/archtest/    source-tree guards (no package-level request state)
migrations/           SQL migrations
configs/              example and dev configuration, landing page
configs/.probe/       end-to-end probe harness and its upstream stand-in
deploy/               Dockerfile, compose, systemd, edge reference config
tests/integration/    end-to-end tests
```

Packages marked with a milestone do not exist yet.

**`internal/tenant` never imports `internal/config`.** The domain model is deliberately independent of how a deployment is configured, so the registry, cache, and resolver can all be tested without a config file. `internal/routing` follows the same rule; only `internal/httpapi` and `cmd/` know about configuration.

**`internal/archtest` asserts properties of the source tree.** It fails the build if a package-level variable could hold per-request routing state — the `global currentTenant` anti-pattern that §31 names explicitly, and the one way this design could leak one tenant's route to another. Its detector has its own tests, so a typo that made the pattern match nothing would fail rather than silently disable the guard.

---

## Build

Make is not installed on Windows by default, so the raw commands are the primary path:

```bash
# Build a static binary (pure Go, no cgo — this is what the deployment image uses)
CGO_ENABLED=0 go build -o bin/gateway ./cmd/gateway

# Unit tests with the race detector
CGO_ENABLED=1 go test ./... -race

# Static checks
go vet ./...

# Run
export ZENSYS_REGISTRY_DSN='user:pass@tcp(127.0.0.1:3306)/zensys_gateway?parseTime=true'
export ZENSYS_TOKEN_POS="$(openssl rand -hex 32)"
export ZENSYS_TOKEN_GMS="$(openssl rand -hex 32)"
go run ./cmd/gateway -config configs/gateway.dev.yaml
```

On Linux and macOS the same targets are available through `make`:

```bash
make build      # static binary
make test       # go test ./... -race
make test-int   # integration tests against the dev compose stack
make vet
make run
```

### Verifying against the running binary

A green test suite is not evidence that the program works — running the binary has found a real defect at every milestone so far, including two in M5 and two in M7. `configs/.probe/run.sh` builds the gateway **and** four upstream stand-ins, starts the Gateway ten times over eight configurations, prints **98 assertions**, and exits non-zero if any of them fails:

```bash
export ZENSYS_REGISTRY_DSN='...'   # needs a registry with mangala (POS) and garage1 (GMS) ACTIVE
bash configs/.probe/run.sh
```

It builds the gateway itself rather than running whatever `bin/gateway.exe` happens to be on disk, because the version of this harness that assumed a pre-built binary silently measured one that was 20 minutes older than the source and reported a fixed defect as still present. **Existence is not freshness.**

| Section | What it asserts |
|---|---|
| A — trusted proxy | Inbound request ID adopted from loopback; spoofed tenant header ignored; forwarded headers believed; unknown and reserved hosts 404; the access log's routing fields; `host_label` never confused with `tenant`; the configured upstreams logged at startup |
| B — nothing trusted | The same spoofs with `trusted_proxies: []`: forwarded host and tenant headers ignored, and the canonical tenant still reaching the product |
| C — the proxy | The four §90 routing rows, path and query unrewritten, `Host` at the product, the header contract as the *upstream* saw it, and streaming measured by `time_starttransfer` |
| E — request bodies | A body within the limit arriving intact (length and digest), a body over it refused with `413 payload_too_large` by the Gateway itself |
| F — the lifecycle API | Every endpoint 401 without a token, an unknown token, a wrong scheme, or two `Authorization` headers; a POS token 403 on every GMS operation with the GMS tenant verified untouched; reserve → duplicate 409 → activate → disable → release, each taking effect on the *very next* public request; release refused without a matching `confirm`; `/internal/v1/...` proxied to the product on the public port; no token in the log; the registry left exactly as found |
| G — tier 2, per tenant | The tenant's third request refused with `429` and a non-zero `Retry-After`; the refusal in the error contract; the refused request never reaching a product; a neighbouring tenant unaffected by another's exhausted bucket; and the refusal's log line carrying the tenant, which is how tier 2 is told from tier 1 |
| H — tier 1, per client | Five health checks in a row all served, so the exemption is real and does not consume the allowance; two different forwarded clients getting two buckets; a third request from one of them refused; and that refusal naming no tenant, because tier 1 refuses before the hostname is parsed |
| I — a rotated `X-Forwarded-For` | With nothing trusted, three requests carrying three different forwarded addresses still share one bucket, so spoofing the header cannot mint a fresh allowance |
| J — the header limit | An oversized header block refused with `431 headers_too_large` by the Gateway's own check — in the error contract, and logged, which `net/http`'s own `431` would not be — while an ordinary request to the same process is still served |
| D — product outage | POS stopped: POS tenant 502, GMS tenant 200 in the same process, and no GMS identity in the POS response |

It generates its configs from `configs/gateway.dev.yaml` (asserting each substitution, so a renamed key cannot turn it into a silent copy of the dev config) and reads the DSN from the environment rather than carrying one. What it generates is gitignored; the harness itself is committed so the claims in the plan can be re-checked rather than taken on trust.

Four details are load-bearing rather than cosmetic:

- **Section F runs with `route_ttl` and `negative_ttl` at 300 s.** With the dev config's 60 s, "the next request reflects the change" would pass whether or not the mutation invalidated anything — the cache would simply have expired. Removing `.WithInvalidator(routeCache)` from the wiring makes the assertion fail with 404, which is how the check was shown to discriminate.
- **Section D runs last**, because it deliberately stops the POS upstreams and section F needs them.
- **It waits for the ports with a raw TCP connect, not an HTTP request.** A liveness probe written as an HTTP request *is* a request: on the public listener it is logged, and it spends a rate-limit token. The tier-1 sections measure a bucket, so a probe that spends from it makes the harness a client of the thing it is measuring — which is exactly how "a rotated `X-Forwarded-For` buys nothing" first failed with a 429 where it wanted a 200.
- **Every config substitution is asserted, and a burst value is chosen so it cannot be confused with another.** The tier-2 burst in section G is `99999` rather than `100000` precisely because `100000` contains `100`, which is one of the shipped values the stale-substitution check greps for.

Section F mutates the registry: it reserves, activates, disables, and releases a tenant of its own (`probe-lc-1`), and releases it again at the start of the next run if a previous one was interrupted. It never mutates the two fixtures.

### The race detector on Windows

`-race` requires cgo, and cgo on Windows requires a C compiler. Install one with
`winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e`, then point Go at it once:

```bash
go env -w CC="C:/Users/<you>/AppData/Local/Microsoft/WinGet/Packages/BrechtSanders.WinLibs.POSIX.UCRT_Microsoft.Winget.Source_8wekyb3d8bbwe/mingw64/bin/gcc.exe"
```

After that, `CGO_ENABLED=1 go test ./... -race` works with no further setup. Note that
this sets only `CC`, which Go consults only when cgo is enabled — so normal builds stay
pure Go and keep producing a static binary.

`-race` is not optional on this project. Request-scoped tenant isolation is an explicit
acceptance criterion, and the race detector is the only thing that actually proves it.

### Registry integration tests

The MySQL tests skip unless `ZENSYS_TEST_DSN` is set, so `go test ./...` stays green on a
machine with no database. To run them:

```bash
export ZENSYS_TEST_DSN='root:root@tcp(127.0.0.1:3306)/zensys_gateway?parseTime=true&loc=UTC&time_zone=%27%2B00%3A00%27'
CGO_ENABLED=1 go test ./internal/tenant/ -race -run TestMySQL -v
```

They skip by default rather than fail, because a missing database is not a code defect —
but that also means a green `go test ./...` says nothing about them. Check the count.

### Windows/Git Bash traps

Each of these costs real time to diagnose, and none produces an error message that points
at the cause.

**1. `curl -o /dev/null` always fails.** Git Bash resolves `curl` to the Windows binary
(`C:/Windows/system32/curl.exe`), and MSYS rewrites the `/dev/null` argument into a
Windows path curl cannot write to. curl then exits **23** (`CURLE_WRITE_ERROR`)
*regardless of the HTTP status* — so a readiness poll written as
`if curl -s -o /dev/null URL; then break; fi` never breaks, even when the server is
answering `200`. Use shell redirection instead, which bash handles rather than curl:

```bash
# wrong on Git Bash — exits 23 even on HTTP 200
curl -s -o /dev/null http://127.0.0.1:8081/readyz

# right — redirection is performed by the shell, not passed to curl
curl -sf http://127.0.0.1:8081/readyz > /dev/null
```

**2. `go env -w` can write to the wrong drive.** If `APPDATA` is Unix-style
(`/c/Users/...`) in the shell, Go resolves its env file against the *current drive* and
creates a stray `D:\c\Users\...` tree. Export a Windows-style path first:

```bash
export APPDATA='C:\Users\<you>\AppData\Roaming'
go env -w CC="..."
```

**3. A Windows binary cannot read Git Bash's `/tmp`.** `bin/gateway.exe -config
/tmp/x.yaml` fails with *"The system cannot find the path specified"* even though the
file plainly exists in bash. Put test configs somewhere the process can resolve — a path
inside the project, or `$(cygpath -w /tmp)`.

**4. Never `kill` a PID you have not confirmed is alive.** `kill $PID` on a process that
already exited can hit a *reused* PID. Combined with trap 3 — where the gateway exits
immediately — this has already killed the calling shell once. Guard it:

```bash
if ! kill -0 "$GW_PID" 2>/dev/null; then
  echo "the process died:"; cat ./gw.log; exit 1
fi
```

**5. Readiness does not prove *which* process is ready.** `kill` on a Windows-native
process from Git Bash is a hard termination, not a graceful stop — the gateway's
`shutdown_grace` drain never runs, and the port is not released until the process is
actually gone. A harness that starts a new gateway and waits for `/readyz` can therefore
be answered by the *previous* one, while the new process dies on
`bind: Only one usage of each socket address` and writes that failure to its own log. The
section then measures a configuration it never loaded. Wait for the ports to be free
*before* starting, and confirm the process you launched is still alive while you wait —
and wait on the **socket**, not on an endpoint:

```bash
# wait for the previous gateway to actually release its sockets.
# A raw connect, not an HTTP request: this asks the kernel a question about a
# socket, and on the public listener an HTTP probe is a request like any other
# — logged, and counted against a rate limit.
port_listening() { (exec 3<>"/dev/tcp/$1/$2") >/dev/null 2>&1; }
until ! port_listening 127.0.0.1 8081; do sleep 0.25; done

./bin/gateway.exe -config "$cfg" >"$log" 2>&1 &
GW_PID=$!
# liveness first, then readiness — a stale process answers the same endpoint
kill -0 "$GW_PID" 2>/dev/null || { cat "$log"; exit 1; }
```

**6. A liveness probe that is an HTTP request is a request.** It appears in the access
log, and on a rate-limited listener it spends a token — so a test that asserts on the
bucket is asserting about a bucket the harness has already spent from. This is not
hypothetical: it made a probe assertion fail with a `429` where it wanted a `200`, and the
fix was to ask the kernel (`/dev/tcp`, above) rather than the service. The same rule
applies to a readiness gate that *retries*: if the first attempt is refused or fails, the
retry is a second request.

**7. A signal-handling gateway looks healthy when it was never signalled.** Related to the
above: because `kill` hard-terminates, the graceful-shutdown path is *not* exercised by a
Git Bash harness. Do not read a clean-looking probe run as evidence that draining works.

---

## Prerequisites

- Go 1.26 or newer. The Gateway's own code only needs 1.22 (for `ServeMux` method-and-pattern routing and the `httputil.ProxyRequest` rewrite hook), but `golang.org/x/sync` raises the floor — and `x/sync` is worth keeping, since `singleflight` is what stops N concurrent requests for one cold tenant from producing N registry queries.
- MySQL 8.0+ (registry)
- A C compiler, for `-race` only (see above)
- Docker (for the dev integration environment)
