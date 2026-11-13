# ZenSys Gateway — Implementation Plan

**Target:** V1 — Go runtime, single shared gateway process
**Source of truth for architecture:** [`docs/architecture/ZenSys_Gateway_V1_Architecture.md`](docs/architecture/ZenSys_Gateway_V1_Architecture.md)
**Decision log:** [`docs/adr/0001-v1-open-decisions.md`](docs/adr/0001-v1-open-decisions.md)

---

## 1. What this document is

The architecture document answers *what the Gateway is*. This plan answers *how it gets built, in what order, and how we know each step is done*.

It covers:

- the eight §94 decisions that are now **closed**, and the ones that stay **open but non-blocking**;
- the concrete technology baseline and dependency set;
- the repository layout, config contract, and database schema;
- the exact request pipeline, routing rules, and header contract;
- the internal lifecycle API surface;
- a milestone sequence (M0–M10) with explicit exit criteria;
- traceability from every §91/§92 acceptance criterion back to the milestone that satisfies it;
- the risks that can actually derail this.

Anything marked **[OPEN]** is an owner decision that this plan deliberately does not make. Anything marked **[PROPOSED]** is an addition this plan recommends that is *not* in the architecture document — treat it as a proposal, not a settled requirement.

---

## 2. Scope boundary

**In scope:** a standalone Go service in this repository that terminates HTTP for `{tenant}.zensys.lk`, resolves `tenant → product` from its own registry, and reverse-proxies to product frontends and backends.

**Explicitly out of scope** (per §3, §5, §66, §67):

- any change to POS or GMS source code, config, or databases;
- POS/GMS tenant provisioning and their internal `tenant → database` resolution;
- user login, JWT issuance, JWT validation, roles, permissions;
- product business logic of any kind.

The Gateway's entire job is to change **where** a request goes. It never decides **which database** a request belongs to.

---

## 3. Decision register

### 3.1 Closed — §94 decisions

| # | Decision | Answer | Consequence |
|---|---|---|---|
| 1 | Registry database engine | **MySQL 8** | `database/sql` + `go-sql-driver/mysql`; migrations are plain SQL; matches existing POS/GMS operational tooling |
| 2 | Frontend serving model | **Gateway proxies to product frontend servers** | Gateway stays a pure reverse proxy — no static file serving, no SPA fallback logic in Go. Products (or their web servers) own history fallback |
| 3 | Internal API security | **Private bind + per-service bearer tokens, structured mTLS-ready** | Lifecycle API on a separate listener bound to loopback/private IP; token → service identity → product authorization. Auth middleware sits behind an interface so mTLS can be layered on without rework |
| 4 | Platform host behavior | **Apex (`zensys.lk`) serves a minimal static landing page; every other reserved host returns 404** | `www.zensys.lk` is treated as reserved → 404 in V1 (it is not the apex). `admin`/`gateway`/`status` stay unbuilt and unresolvable |
| 5 | Disabled tenant response | **404** | `DISABLED` is indistinguishable from unknown. Uniform error contract, zero information leakage |
| 6 | V1 availability | **HA-ready design, one instance deployed** | Shared registry, per-instance cache, no sticky local state. A second node needs no code change |
| 7 | Deployment topology | **Undecided — stay portable** | Everything is containerized and fully config-driven. No loopback co-location assumptions |
| 8 | V1 tenant model | **One tenant slug → exactly one product** | `tenant_slug` is the primary key. §12 confirmed; `mangala → POS` and `mangala → GMS` cannot coexist |

Two further §94 items were closed by direct implication of the architecture and are recorded for completeness:

| # | Decision | Answer | Basis |
|---|---|---|---|
| 9 | Tenant lifecycle authority | Gateway reserves the slug; the product provisions its own tenant; the product requests activation; Gateway activates the route | §34, §62 |
| 10 | Slug policy | `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$` | §9's format with the length bound corrected — see the note below |

> **Slug policy note — §9 contains a bug, and it has been corrected.** §9 gives the regex as `^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`. With `{1,62}` between two mandatory anchors that permits 3–**64** characters, but a tenant slug *is* the leftmost DNS label of `{tenant}.zensys.lk`, and DNS labels are capped at **63** octets (RFC 1035 §2.3.4). A 64-character slug would be reservable through the lifecycle API, storable, and permanently unresolvable — a tenant that exists in the registry and can never be reached, with nothing reporting it as broken. The bound is **63**, enforced identically in `SlugMaxLength`/`SlugPattern`, the `chk_slug_format` CHECK, and the `VARCHAR(63)` column. Do not restore the documented value.
>
> Two-character slugs such as `ab` are still rejected, and that part is intentional. If 1–2 character slugs are ever wanted, the regex must change *and* the POS/GMS shared policy must change with it.

### 3.2 Open — non-blocking owner items

These do not prevent M0–M9 from being built and proven against mocks. They block production cutover only.

| ID | Item | Needed by | Notes |
|---|---|---|---|
| O1 | Registry MySQL DSN + credentials | M1 integration tests | Dev machine uses `root:root@127.0.0.1:3306/zensys_gateway`; production credentials still needed. Supplied via env var, never committed |
| O2 | Go module path / repo host | M0 | **Decided for now:** `github.com/zensys/gateway`, as `go.mod` declares. Revisit only if the repository host differs at publishing time |
| O3 | Real POS and GMS upstream addresses (§94.2–3) | M10 / §84 | Config-only change; mocks cover until then |
| O4 | POS-side `/api/...` API surface (§66, §20) | POS pilot only | **Separate project.** The Gateway can be fully proven on GMS + mocks first |
| O5 | Audit/event table adoption | M1 | **Decided: included.** `tenant_route_events` is live and written in the same transaction as each mutation |
| O6 | Rate limit thresholds | M7 | Plan ships configurable defaults |
| O7 | Route + negative cache TTLs | M3 | Shipped with defaults (60s / 30s), verified end to end at a 2s override. Production values still to be chosen |
| O8 | Apex landing page content | M2 | Placeholder page ships and serves; real content still needed |

---

## 4. Technology baseline

### 4.1 Language and toolchain

- **Go** — latest stable, installed via `winget install GoLang.Go`. Minimum **1.22**, because the plan uses `http.ServeMux` method-and-pattern routing (`POST /internal/v1/tenant-routes/{tenant}`) and the `httputil.ProxyRequest` rewrite hook, both of which are 1.22+ features.
- **MySQL 8.0.45** — already present locally.

### 4.2 Dependency set

Deliberately small, per §3 ("the Gateway should remain intentionally small").

| Dependency | Purpose | Why not stdlib |
|---|---|---|
| `github.com/go-sql-driver/mysql` | MySQL driver | Only real option |
| `gopkg.in/yaml.v3` | Config parsing | stdlib has no YAML |
| `github.com/prometheus/client_golang` | §52 metrics | stdlib has no Prometheus exposition format |
| `golang.org/x/time/rate` | §81 rate limiting | Token-bucket limiter |
| `golang.org/x/sync/singleflight` | Cache stampede collapse | Prevents N concurrent DB reads for one cold slug |

Everything else is stdlib:

- **Router:** `net/http` + `http.ServeMux`. No chi, no gin, no echo.
- **Logging:** `log/slog` with a JSON handler — covers §51 exactly.
- **Proxy:** `net/http/httputil.ReverseProxy`.
- **DB access:** `database/sql` with hand-written queries. No ORM.
- **Request IDs:** `crypto/rand`, 16 bytes, hex-encoded. No UUID library needed.
- **Testing:** `testing` + `net/http/httptest`.

**No ORM. No framework. No dependency injection container.** If a decision here is later reversed, the blast radius is small by design.

### 4.3 Why stdlib `ServeMux` is sufficient

The Gateway has exactly two routing surfaces:

- **Public:** one catch-all handler. There is no path-based route table — host resolution decides the destination, and path only picks frontend vs backend. A router library would add nothing.
- **Internal:** six fixed patterns. `ServeMux` handles all of them natively in 1.22+.

---

## 5. Repository layout

Refines §70 with concrete file responsibilities.

```text
zensys-gateway/
├── cmd/
│   └── gateway/
│       └── main.go                 # wiring only: config → deps → listeners → shutdown
│
├── internal/
│   ├── config/
│   │   ├── config.go               # config structs
│   │   ├── load.go                 # YAML load + env overlay for secrets
│   │   └── validate.go             # fail-fast validation, run before listeners open
│   │
│   ├── tenant/
│   │   ├── model.go                # Route, Product, Status, transition table
│   │   ├── slug.go                 # slug policy + reserved-host registry (§9, §10)
│   │   ├── repository.go           # Repository interface
│   │   ├── mysql_repository.go     # MySQL implementation
│   │   ├── service.go              # Reserve/Activate/Disable/Release + cache invalidation
│   │   └── cache.go                # TTL cache, negative cache, singleflight
│   │
│   ├── routing/
│   │   ├── host.go                 # parse, normalize, validate, extract tenant (§8)
│   │   ├── resolver.go             # host → tenant → product (§17 Stage A)
│   │   ├── upstream.go             # product + path → upstream (§17 Stage B)
│   │   ├── headers.go              # header sanitization + injection (§24, §43)
│   │   └── proxy.go                # ReverseProxy construction + error mapping (§47)
│   │
│   ├── security/
│   │   ├── serviceauth.go          # token → service identity (interface: mTLS-ready)
│   │   ├── authz.go                # service → permitted products (§42)
│   │   └── ratelimit.go            # two-tier limiter (§81)
│   │
│   ├── httpapi/
│   │   ├── public.go               # public catch-all handler
│   │   ├── internal.go             # lifecycle API handlers (§39)
│   │   ├── platform.go             # apex landing + reserved-host responses
│   │   ├── health.go               # /healthz, /readyz (§49)
│   │   ├── middleware.go           # request ID, recovery, size limit, logging
│   │   └── responses.go            # error contract (§69)
│   │
│   └── observability/
│       ├── logging.go              # slog JSON, redaction rules (§51)
│       └── metrics.go              # Prometheus collectors (§52)
│
├── migrations/
│   └── 0001_init.sql
│
├── configs/
│   ├── gateway.example.yaml        # committed, no secrets
│   ├── gateway.dev.yaml            # local dev
│   └── landing/index.html          # apex placeholder
│
├── deploy/
│   ├── Dockerfile                  # multi-stage, static binary, distroless/scratch
│   ├── docker-compose.dev.yml      # MySQL + mocks + gateway
│   ├── systemd/zensys-gateway.service
│   └── edge/nginx.conf.example     # reference edge config (§85)
│
├── cmd/mockproduct/                # §83 mock POS/GMS frontends + backends
│   └── main.go
│
├── tests/
│   ├── integration/                # end-to-end against mocks + real MySQL
│   └── testdata/
│
├── docs/
│   ├── architecture/ZenSys_Gateway_V1_Architecture.md
│   ├── adr/
│   └── runbooks/
│
├── go.mod
├── go.sum
├── Makefile
├── .gitignore
└── README.md
```

**Where the tree has since diverged.** This is the plan's intended layout; the implementation has moved in three places, each recorded under the relevant milestone in §17:

- `routing/upstream.go` and `routing/proxy.go` became **`internal/proxy/`** (`proxy.go`, `transport.go`). `routing` must not import `config` or `tenant`, and a proxy that builds per-product transports from configuration needs both.
- **`internal/archtest/`** was added: a source-tree guard that fails the build on any package-level variable that could hold per-request routing state (§31).
- `internal/tenant/` gained `errors.go`, `event.go`, `reserved.go`, and `dsn.go`, and `cmd/migrate/` was added alongside `cmd/gateway/`.

Still absent, as planned: `internal/security/ratelimit.go` (M7), `internal/observability/metrics.go` (M8), `cmd/mockproduct/` and `tests/integration/` (M9), and `deploy/` (M9–M10). The evidence harness lives at `configs/.probe/` rather than `tests/` — it is committed so its claims can be re-checked, and `cmd/mockproduct` should replace its upstream stand-in.

---

## 6. Configuration contract

### 6.1 Shape

```yaml
server:
  public_addr: "0.0.0.0:8080"
  internal_addr: "127.0.0.1:8081"      # §40 — private bind, never public
  read_header_timeout: 5s
  idle_timeout: 60s
  shutdown_grace: 15s
  max_header_bytes: 65536
  max_body_bytes: 10485760             # §81 request size limit

platform:
  root_domain: "zensys.lk"
  apex_behavior: "landing"             # landing | not_found
  landing_dir: "./configs/landing"
  reserved_hosts: [www, admin, api, gateway, status, app, console, support,
                   help, docs, mail, email, smtp, imap, ftp, ssh, static,
                   assets, cdn, media, files, download, uploads, auth, login,
                   register, account, accounts, billing, payments, dashboard,
                   portal, dev, test, staging, demo, internal, blog, ns, ns1, ns2]

registry:
  driver: "mysql"
  dsn_env: "ZENSYS_REGISTRY_DSN"       # secret comes from env, never this file
  max_open_conns: 20
  max_idle_conns: 10
  conn_max_lifetime: 5m
  query_timeout: 2s

cache:
  route_ttl: 60s                       # [OPEN O7]
  negative_ttl: 30s                    # [OPEN O7] §32 — short, so new tenants route quickly
  max_entries: 10000

products:
  POS:
    frontend_upstream: "http://127.0.0.1:3001"
    backend_upstream:  "http://127.0.0.1:8082"
    inject_tenant_header: true
    tenant_header_name: "X-Tenant-ID"
    upstream_timeout: 30s
  GMS:
    frontend_upstream: "http://127.0.0.1:3002"
    backend_upstream:  "http://127.0.0.1:8083"
    inject_tenant_header: true
    tenant_header_name: "X-Tenant-ID"
    upstream_timeout: 30s

routing:
  backend_path_prefix: "/api"          # §20 — /api and /api/* are backend
  path_prefix_case_sensitive: true     # see §9.2

proxy:
  preserve_host: true                  # §25, §44 — GMS host-bound tenancy depends on this
  request_id_header: "X-Request-ID"
  trusted_proxies: ["127.0.0.1/32", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]
  upstream_response_header_timeout: 30s

internal_auth:
  mode: "token"                        # token | mtls  (mtls reserved, not implemented in V1)
  tokens:
    - service: "pos"
      token_env: "ZENSYS_TOKEN_POS"
      may_manage_products: ["POS"]
    - service: "gms"
      token_env: "ZENSYS_TOKEN_GMS"
      may_manage_products: ["GMS"]

rate_limit:
  enabled: true
  ip:     { rps: 200, burst: 400 }     # tier 1 — coarse, protects the Gateway
  tenant: { rps: 50,  burst: 100 }     # tier 2 — per-tenant fairness

observability:
  log_level: "info"
  log_format: "json"
  metrics_enabled: true
  health_on_public: false              # see §11.2 — deviation, explained there
```

### 6.2 Secret handling

Only three secrets exist, and **none of them appear in a config file**:

| Secret | Source |
|---|---|
| Registry DSN (contains DB password) | `ZENSYS_REGISTRY_DSN` env |
| POS service token | `ZENSYS_TOKEN_POS` env |
| GMS service token | `ZENSYS_TOKEN_GMS` env |

Config references them by env var *name*. `config.Load` resolves the name to a value at startup. If a referenced env var is missing or empty, startup fails loudly — no silent empty-token auth (§91).

### 6.3 Startup validation (fail-fast, before any listener opens)

- `root_domain` is a bare lowercase domain — no scheme, no port, no trailing dot.
- Every `reserved_hosts` entry is itself a syntactically valid DNS label.
- `reserved_hosts` does not contain the apex label or any `root_domain` label.
- `public_addr` ≠ `internal_addr` (otherwise the private boundary silently disappears).
- Both POS and GMS exist, and each has parseable `http`/`https` upstream URLs with a host.
- `backend_path_prefix` starts with `/` and has no trailing slash.
- `trusted_proxies` entries all parse as CIDR.
- Cache TTLs are > 0; `max_entries` > 0.
- If `apex_behavior: landing`, `landing_dir` exists and contains `index.html`.
- `internal_auth.mode: token` ⇒ at least one token entry, and every resolved token is ≥ 32 chars.
- If `internal_auth.mode: mtls` ⇒ startup fails with "not implemented in V1" rather than silently running unauthenticated.

---

## 7. Registry schema

### 7.1 `tenant_routes`

```sql
CREATE TABLE IF NOT EXISTS tenant_routes (
  tenant_slug VARCHAR(63) COLLATE utf8mb4_0900_as_cs NOT NULL,
  product     VARCHAR(16) NOT NULL,
  status      VARCHAR(16) NOT NULL,
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
                ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_slug),
  KEY idx_status_product (status, product),
  CONSTRAINT chk_slug_format
    CHECK (tenant_slug REGEXP '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'),
  CONSTRAINT chk_slug_lowercase
    CHECK (tenant_slug = LOWER(tenant_slug)),
  CONSTRAINT chk_product CHECK (product IN ('POS','GMS')),
  CONSTRAINT chk_status  CHECK (status IN ('PROVISIONING','ACTIVE','DISABLED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

Design notes:

- **`PRIMARY KEY (tenant_slug)`** is the §12 global-uniqueness rule expressed as a database constraint. Duplicate reservation across products becomes impossible at the storage layer, not just in application logic.
- **`COLLATE utf8mb4_0900_as_cs`** on the slug column makes both uniqueness *and* equality case-sensitive. Combined with application-side lowercasing, an uppercase lookup misses rather than silently matching — fail-closed behaviour.
- **`VARCHAR` + `CHECK` instead of `ENUM`.** Adding a third product later is a one-line `CHECK` change and a config entry, not an `ALTER TABLE` on an enum.
- **`chk_slug_lowercase`** is defence in depth. The app normalizes; the DB refuses to store anything that slipped through.
- The reserved-host list is **not** duplicated here. §10 requires one authoritative definition; it lives in `internal/tenant/slug.go` and is enforced in the service layer. A `CHECK` against 39 literal strings would be a second source of truth waiting to drift.
- **`{1,61}`, not `{1,62}` — a correction to the architecture document.** §9 gives the regex as `^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`, which permits 3–64 characters. A tenant slug *is* the leftmost DNS label of `{tenant}.zensys.lk`, and DNS labels are capped at 63 octets. A 64-character slug would therefore be reservable, storable, and permanently unresolvable — a tenant that exists in the registry and can never be reached. The bound is fixed at 63 in three places that must agree: this constraint, `SlugMaxLength`/`SlugPattern` in `internal/tenant/slug.go`, and the `VARCHAR(63)` column. `TestMySQL_DatabaseAccepts63CharacterSlug` pins the boundary from both sides.

### 7.2 State machine

```text
                    reserve
                       │
                       ▼
                 PROVISIONING ──────────┐
                       │                │ disable
                  activate              │ (provisioning failed)
                       │                ▼
                       ▼            DISABLED
                     ACTIVE ────────────┘
                       ▲                │
                       └──── activate ──┘
                                        │
                                     release
                                        ▼
                                     (row deleted)
```

| Transition | Trigger | Allowed from |
|---|---|---|
| → PROVISIONING | `reserve` | (no row) |
| → ACTIVE | `activate` | PROVISIONING, DISABLED |
| → DISABLED | `disable` | PROVISIONING, ACTIVE |
| → deleted | `release` | any |

**Public routing is permitted only for `ACTIVE`.** `PROVISIONING` and `DISABLED` both return 404 (§45, decision 5).

Two extensions beyond §34's linear diagram are **[PROPOSED]**: `DISABLED → ACTIVE` (re-enable a suspended tenant without re-provisioning) and `PROVISIONING → DISABLED` (provisioning failed cleanly). Both are cheap now and painful to retrofit once POS/GMS depend on the API's refusal behaviour.

Idempotency: `activate` on an already-`ACTIVE` route returns `200` with current state rather than an error — retries from a product's provisioning job should not require special-casing.

### 7.3 `tenant_route_events` — **[PROPOSED]**

§38 requires `release` to be "deliberately protected" and notes retention is TBD. This plan recommends an append-only event table, because `release` is otherwise an unaudited destructive operation and there is no way to answer "who disabled this tenant and when".

```sql
CREATE TABLE IF NOT EXISTS tenant_route_events (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_slug     VARCHAR(63) COLLATE utf8mb4_0900_as_cs NOT NULL,
  action          VARCHAR(16) NOT NULL,   -- RESERVE|ACTIVATE|DISABLE|RELEASE
  from_status     VARCHAR(16) NULL,
  to_status       VARCHAR(16) NULL,
  product         VARCHAR(16) NOT NULL,
  actor_service   VARCHAR(32) NOT NULL,
  actor_request_id VARCHAR(64) NOT NULL,
  occurred_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_events_slug_time (tenant_slug, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

Written in the same transaction as the mutation. Contains no customer data, no credentials, no PII — consistent with §61.

**Not adopted until you confirm O5.** If rejected, the table and its migration are simply dropped; nothing else depends on it.

---

## 8. Public request pipeline

### 8.1 Ordered pipeline

```text
PUBLIC LISTENER (:8080)
  │
  1.  recover panic                    → 500 generic, logged with stack, never leaked
  2.  request ID                       → accept from trusted proxy, else generate
  3.  body size limit                  → 413
  4.  tier-1 rate limit (by client IP) → 429
  5.  parse + normalize Host           → strip port, lowercase, trim trailing dot
  6.  validate root domain             → wrong domain ⇒ 404
  7.  apex check                       → apex ⇒ platform handler (landing | 404)
  8.  multi-label check                → >1 label below root ⇒ 404
  9.  reserved host check              → reserved ⇒ 404   (§65: before tenant lookup)
  10. slug validation                  → invalid ⇒ 404
  11. tenant resolution                → cache → registry
  │                                      miss  ⇒ 404
  │                                      error ⇒ 503   (§46)
  12. status check                     → PROVISIONING | DISABLED ⇒ 404
  13. tier-2 rate limit (by tenant)    → 429
  14. upstream selection               → §17 Stage B
  15. header sanitize + inject         → §10 below
  16. reverse proxy                    → upstream
  17. log + metrics
```

Steps 1–4 and 17 wrap everything. Steps 5–12 are the §8 hostname validation sequence verbatim, plus the §65 reserved-host ordering rule.

### 8.2 Why two rate-limit tiers

A single tier keyed by tenant would sit *after* tenant resolution, which means an attacker hammering unknown hostnames reaches the database on every request (negative cache notwithstanding). A single tier keyed by IP would let one noisy tenant behind shared NAT starve its neighbours.

Tier 1 (IP) absorbs abuse before any lookup. Tier 2 (tenant) enforces per-tenant fairness. **[PROPOSED]** — §81 requires rate limiting but does not specify tiers.

### 8.3 Host normalization details

Order matters, and each step closes a specific hole:

1. Read `r.Host`. Reject empty.
2. Strip the port using `net.SplitHostPort` with a fallback for the no-port case.
3. **Reject if the host is a bare IP literal.** `127.0.0.1` and `[::1]` must not be treated as tenants.
4. Trim a single trailing dot (`mangala.zensys.lk.` is legal FQDN notation).
5. Lowercase.
6. Reject any host containing characters outside `[a-z0-9.-]` after normalization — this rejects percent-encoding, underscores, whitespace, non-ASCII bytes, and CR/LF injection attempts in one rule.
7. Reject a tenant label beginning with `xn--` separately. **This rule is not covered by step 6, contrary to an earlier draft of this plan.** Punycode is spelled entirely in `[a-z0-9-]`, so `xn--mangala-xyz.zensys.lk` passes the character allow-list untouched. The platform slug policy is ASCII-only (§9), so an IDN host can never name a real tenant; rejecting it explicitly means a future decision to support Unicode has to be deliberate rather than arriving by accident.
8. Reject hosts longer than 253 characters, and any label longer than 63, and any empty label (`mangala..zensys.lk`, `.zensys.lk`). The empty-label case matters: without it, the suffix match against the root domain succeeds and yields an empty tenant slug.

Only then does root-domain validation run.

### 8.4 Apex and reserved-host behavior

| Host | V1 behavior |
|---|---|
| `zensys.lk` | Static landing page from `landing_dir` |
| `www.zensys.lk` | 404 (reserved) |
| `admin.zensys.lk` | 404 (reserved) — §65's future admin mapping stays open |
| `gateway.zensys.lk` | 404 (reserved) |
| `status.zensys.lk` | 404 (reserved) |
| `mangala.zensys.lk` | tenant routing |
| `a.b.zensys.lk` | 404 — multi-label rejected |
| `zensys.lk.evil.com` | 404 — wrong root domain |
| `mangala.other.lk` | 404 — wrong root domain |

The apex is matched by **exact equality against `root_domain`**, never by label extraction. `zensys` can therefore never become a tenant (§11).

---

## 9. Routing rules

### 9.1 Stage B — frontend vs backend (§17, §20, §21, §22)

| Path | Destination |
|---|---|
| `/api` | backend |
| `/api/...` | backend |
| `/api/v1/jobs` | backend, forwarded **unchanged** |
| `/api/orders` | backend, forwarded **unchanged** |
| `/` | frontend |
| `/dashboard` | frontend |
| `/customers/123` | frontend |
| `/jobs/456` | frontend |
| `/settings` | frontend |

No prefix stripping, no prefix rewriting, no product-specific path knowledge (§21). The Gateway is deliberately ignorant of what lives under `/api`.

### 9.2 Path prefix matching — **[DECISION]**

Matching is **case-sensitive**: `/API/orders` is treated as frontend traffic.

Rationale: HTTP paths are case-sensitive per RFC 9110, and both POS and GMS declare lowercase `/api` routes. Making the prefix case-insensitive would mean `/API/orders` reaches a backend that has no such route — producing a confusing 404 from the product instead of a clear frontend 404 from the Gateway.

This is configurable via `routing.path_prefix_case_sensitive` in case a product turns out to need otherwise. Flagging it because it is the kind of detail that silently causes "the SPA returned index.html for an API call" bugs.

### 9.3 Upstream selection is product-level, never tenant-level (§15)

```text
tenant  → product   (registry)
product → upstream  (config)
```

There is no code path anywhere that maps a tenant to an upstream. Adding one would violate §15 and §93.15.

---

## 10. Header contract

### 10.1 Public traffic

| Header | Client → Gateway | Gateway → upstream |
|---|---|---|
| `Host` | read, normalized | **preserved as the public host** (`preserve_host: true`) |
| `X-Tenant-ID` | **always deleted**, trusted proxy or not | set to canonical tenant, only if `inject_tenant_header: true` |
| `X-Request-ID` | accepted only from a trusted proxy; otherwise discarded | canonical request ID |
| `X-Forwarded-For` | discarded unless from trusted proxy | **set to the resolved client address** — see below |
| `X-Forwarded-Host` | discarded unless from trusted proxy | public host |
| `X-Forwarded-Proto` | discarded unless from trusted proxy | `https` as seen at the edge |
| `Authorization` | untouched | untouched (§28) |
| `Cookie`, `Set-Cookie` | untouched | untouched |
| Hop-by-hop headers | — | stripped by `ReverseProxy` |

**`X-Forwarded-For` is set, not appended.** This plan originally said "client IP appended", which the implementation does not do. `ProxyRequest` strips the inbound header before `Rewrite` runs, so appending would mean re-introducing a chain the Gateway never verified — including entries the client itself wrote. The outbound value is the single address the Gateway resolved, and nothing else. Products that want the intermediate hops can get them from the edge's own logs, where they were actually observed.

**The client address is the rightmost untrusted entry.** Every hop appends the address it received from, so the leftmost entry is whatever the client chose to send. Walking right to left and skipping addresses in `trusted_proxies` yields the last value a trusted hop actually recorded. Reading the leftmost — the obvious implementation — would let any client pick its own apparent address, and from M7 its own rate limit.

§24's rule is absolute: the host-derived tenant is canonical, and a client-supplied `X-Tenant-ID` can never influence routing. The delete-then-set ordering means there is no code path where a client value survives.

### 10.2 The trust policy, finalised (M7)

The policy is one sentence: **a forwarded header is believed only when the connection came from an address in `trusted_proxies`, and otherwise the Gateway asserts what it observed itself.** There is no heuristic, no partial trust, and no attempt to "clean up" an untrusted value into a usable one — a client that can set `X-Forwarded-Proto` can set it to anything, so the only safe response is to ignore it. A header that is not believed is **deleted**, not corrected, which is why `SanitizeInbound` and `ResolveEdge` are one decision split across two halves rather than two rules.

Four consequences, all of which are load-bearing and none of which is obvious:

1. **Routing never reads a forwarded header.** `ParseHost` reads `r.Host`. A trusted proxy that sends `X-Forwarded-Host` for a different tenant changes only what the Gateway *advertises downstream*, never where the request goes. This is what makes §91's "client cannot override the Host-derived tenant" structural rather than a matter of careful coding.
2. **The trust decision is per connection, and made once.** `ResolveEdge` runs in one middleware, stores an `Edge` in the request context, and every later consumer reads that value. Nothing re-derives it, so nothing can disagree with it — including the rate limiter, which keys on `Edge.ClientIP`.
3. **Every hop between the client and the Gateway must be listed.** The resolved client address is the rightmost `X-Forwarded-For` entry that is *not* itself trusted, so a hop missing from the list becomes the client address. With the §85 topology — Cloudflare → Nginx → Gateway — omitting Cloudflare's published ranges makes every visitor key on the Cloudflare edge that served them, and those visitors then share one tier-1 rate-limit bucket. **This failure is silent**: routing is unaffected, because routing never reads a forwarded header, so the only symptom is a rate limit that fires earlier than it should. It is a prerequisite for M10, not a tuning note.
4. **An empty list is valid and means "trust nothing".** Every request is attributed to its immediate peer. A warning is logged at startup, because a typo in the list looks exactly like a deliberate choice, and the consequence — every request reported as coming from the proxy — is invisible until someone reads a product's log.

**Failure direction, stated once.** When the trust decision cannot be made, the policy fails closed: an unparseable `RemoteAddr` is not trusted, a context with no `Edge` reports untrusted, and an unparseable `X-Forwarded-For` entry is skipped rather than believed. The one place this is deliberately *not* fail-closed is the rate limiter's key when no `Edge` exists at all: it falls back to the connection address, which is what the policy would have produced anyway. Exempting the request would be a bypass; sharing one bucket across every request would be a self-inflicted outage.

### 10.3 The `X-Forwarded-Proto` trap — implementation critical

Go's `httputil.ProxyRequest` **strips inbound forwarded headers before `Rewrite` runs**, by design. That is the correct security default, but it creates a real problem here: the Gateway sits behind Cloudflare → Nginx, so `r.TLS` is `nil` at the Gateway. A naive `pr.SetXForwarded()` would therefore advertise `X-Forwarded-Proto: http` downstream, and products generating absolute URLs or enforcing secure cookies would misbehave in production while working fine locally.

The fix, and it must be in M4, not retrofitted:

1. A middleware runs **before** proxying. If `RemoteAddr` is within `trusted_proxies`, it reads `X-Forwarded-Proto`, `X-Forwarded-Host`, and the client IP out of `X-Forwarded-For`, and stores them in the request context.
2. `Rewrite` calls `pr.SetXForwarded()`, then **overrides** `X-Forwarded-Proto` and `X-Forwarded-Host` from the context values.
3. If `RemoteAddr` is **not** trusted, the inbound values are ignored entirely and the Gateway asserts its own.

Without step 1, §85's verification list (`X-Forwarded-Host`, `X-Forwarded-For`, `X-Forwarded-Proto`) cannot pass in the real edge chain.

**Implemented in M4** as: step 1 is `httpapi.TrustedProxy` → `routing.ResolveEdge`, step 3 is the same function's early return, and step 2 is `routing.ApplyForwarded`, which M5's `Rewrite` calls immediately after `SetXForwarded`.

Steps 1 and 3 were exercised end to end in M4. Step 2 could only be exercised in M4 against the function M5 would call, so what remained unproven was that `Rewrite` actually calls it — the same gap the M4 notes flagged, and the one M5 closed. It is now proven three ways: `TestProxy_UpstreamSeesThePublicHostname` and the header-contract tests drive the real `Rewrite` through the assembled pipeline, and the binary probe asserts `X-Forwarded-Proto: https` and the canonical `X-Tenant-ID` **as the upstream received them**.

### 10.4 Host preservation

`pr.Out.Host = pr.In.Host` — the public hostname is what the product sees. This is non-negotiable for GMS, whose tenant resolution is host-bound (§44, §67), and it is why `preserve_host` defaults to `true` and is not something a per-product config can silently disable without a deliberate edit.

---

## 11. Reverse proxy and failure mapping

### 11.1 Error mapping

| Condition | Gateway response |
|---|---|
| Upstream returns any status (200, 404, 500…) | **passed through unchanged** |
| Upstream connection refused / DNS failure | `502` |
| Upstream timeout | `503` with `Retry-After: 5` |
| Upstream protocol error | `502` |
| Client cancelled mid-flight | logged, no response attempted |
| Registry unreachable, route not cached | `503` (§46) |
| Registry unreachable, route cached | serve from cache (fail-open on registry only) |
| Unknown tenant | `404` |
| `PROVISIONING` / `DISABLED` tenant | `404` |
| Rate limited | `429` with `Retry-After` |
| Body too large | `413` |
| Panic | `500` |

Two rules that matter:

- **Product errors are never masked.** If POS returns 500, the Gateway returns 500. It does not reinterpret product responses.
- **No cross-product fallback, ever** (§48). The failure handler for a POS tenant has no reference to GMS at all — not a disabled code path, an absent one.

> `504` would be the more precise code for upstream timeouts. §47 permits `502/503`, so this plan uses `503` to stay within the documented contract. Change it if you prefer precision over fidelity to the spec.

### 11.2 Health and readiness placement — **deviation, explained**

§49 suggests `GET /healthz` and `GET /readyz`. This plan serves them on the **internal listener only** (`127.0.0.1:8081`), not the public one.

Reason: the public listener is a host-based catch-all. Serving `/healthz` there means `mangala.zensys.lk/healthz` would return *Gateway* health instead of reaching POS — silently shadowing a path a product may legitimately own. Reserving specific paths on tenant hostnames would also erode the "Gateway knows nothing about product routes" boundary.

`observability.health_on_public: false` is the default; setting it to `true` restores §49's literal suggestion for the apex host only, if external monitoring requires it.

- `GET /healthz` → `200` whenever the process is alive. No dependency checks.
- `GET /readyz` → checks config validity, registry reachability (`PingContext`, 1s timeout), and upstream configuration presence. Returns `200` or `503` with a per-check breakdown. It does **not** check individual tenants (§49).

---

## 12. Route cache (§30–33, §76)

```go
type entry struct {
    route     tenant.Route
    expiresAt time.Time
    negative  bool
}
```

| Property | Design |
|---|---|
| Structure | `map[string]entry` guarded by `sync.RWMutex` |
| Positive TTL | `cache.route_ttl` (default 60s) |
| Negative TTL | `cache.negative_ttl` (default 30s) — §32 requires short, so a new tenant routes quickly |
| Stampede control | `singleflight` keyed by slug — N concurrent misses for one cold slug produce **one** DB query |
| Invalidation | `Invalidate(slug)` on every successful lifecycle mutation (§33) |
| Capacity | `max_entries`; on overflow, evict expired entries, then fail open to the registry rather than evicting live routes |
| Authority | Never authoritative. A `Flush()` must leave the Gateway fully correct (§92) |

**Refinements found during implementation** — each closes a defect that the design above permits:

| Refinement | Why |
|---|---|
| **A registry failure is never cached** | "I could not ask" is not an answer. Caching it turns a transient outage into a negative TTL of wrong answers and keeps serving them after recovery. An absence is an answer and is cached; a failure to answer is not. |
| **The load runs with a context detached from the caller's** (`context.WithoutCancel` + an explicit load timeout) | `singleflight.Do` runs the load in the first caller's goroutine with that caller's context. One client disconnecting would then fail an entire cohort of unrelated requests waiting on the same slug. |
| **The cache is re-checked inside the singleflight call** | `singleflight` deletes a key the moment its function returns, so the window between a miss and arrival at `DoChan` is not atomic with the store. Without the re-check a burst for one cold slug produces **two** registry reads. Found by a test that asserted exactly one and got two. |
| **A stale write is discarded** | A request that misses reads the registry; the route can change and be invalidated while that read is in flight. Writing the answer anyway serves a state that no longer exists for a full TTL. A single *global* invalidation counter (not one per slug) keeps memory bounded as unknown hostnames are probed; the cost is that an unrelated invalidation discards an in-flight write, which is a wasted fill, never a wrong answer. |
| **The lifecycle Service reads through the repository, not the cache** | A lifecycle decision must be made on the truth, not on a copy up to `route_ttl` old. The public path and the lifecycle path deliberately have different consistency needs. |

**Concurrency is a correctness requirement, not an optimization** (§31). There is no package-level mutable tenant state anywhere in the design — no `currentTenant`, no `sync.Map` of "the tenant for this process". Tenant identity travels in the `context.Context` created per request.

M3's exit criterion is a `-race` test that drives concurrent requests across mixed tenants and asserts each response carries its own tenant, with the race detector enabled.

---

## 13. Internal lifecycle API

### 13.1 Surface (§39)

Separate listener, bound to `internal_addr`. Never routed from the public listener.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/internal/v1/tenant-routes/reserve` | create as `PROVISIONING` |
| `POST` | `/internal/v1/tenant-routes/activate` | `→ ACTIVE` |
| `POST` | `/internal/v1/tenant-routes/disable` | `→ DISABLED` |
| `POST` | `/internal/v1/tenant-routes/release` | delete row |
| `GET` | `/internal/v1/tenant-routes/{tenant}` | single route |
| `GET` | `/internal/v1/tenant-routes` | list, filterable |
| `GET` | `/internal/v1/platform-policy` | **[PROPOSED]** slug regex + reserved hosts |

`platform-policy` is proposed because §10 demands a single authoritative reserved-host definition. Exposing it lets POS and GMS validate a slug *before* calling `reserve`, turning a 409 conflict at provisioning time into a validation error at form-submission time. It returns static config and leaks nothing.

### 13.2 Request and response shapes

```http
POST /internal/v1/tenant-routes/reserve
Authorization: Bearer <pos-service-token>
Content-Type: application/json

{ "tenant": "garage1", "product": "GMS" }
```
```json
201 Created
{
  "tenant": "garage1",
  "product": "GMS",
  "status": "PROVISIONING",
  "created_at": "2026-09-19T09:51:00.000Z",
  "updated_at": "2026-09-19T09:51:00.000Z"
}
```

```http
POST /internal/v1/tenant-routes/activate
{ "tenant": "garage1", "product": "GMS" }
```
```json
200 OK
{ "tenant": "garage1", "product": "GMS", "status": "ACTIVE", ... }
```

`disable` and `release` take `{ "tenant": ... }` only — the product is read from the registry, and authorization checks the caller's `may_manage_products` against the **stored** product. This removes a whole class of bug where a caller claims a product that does not match the route.

`release` requires deliberate confirmation (§38):

```http
POST /internal/v1/tenant-routes/release
{ "tenant": "garage1", "confirm": "garage1" }
```

Mismatched or missing `confirm` ⇒ `400`. There is no way to release a tenant by accident.

The confirmation is checked **before the registry is read**. That ordering is a disclosure property rather than an optimisation: if the lookup came first, a caller that cannot manage the route could still tell "no such slug" (404) from "exists but not yours" (403), and the second answer confirms the slug exists. A request that cannot possibly succeed never reaches the lookup.

`GET /tenant-routes` accepts `product`, `status`, `after`, and `limit`. The result is narrowed to what the caller may manage: a service that manages exactly one product sees only that product's routes without having to ask, and naming a product it does not manage is a `403`. A service that manages every product is not narrowed. This is stricter than §42 requires — §42 governs management, and a list is a read — but it costs nothing and errs toward disclosing less.

A body with an unknown field is a `400`. This is an API between services of one platform, where a misspelled field is far more likely to be a caller's bug than a forward-compatibility requirement; silently ignoring `"prodcut"` would turn a typo into a confusing complaint about a missing product.

### 13.3 Status codes

| Code | Meaning |
|---|---|
| `200` | success |
| `201` | reserved |
| `400` | malformed body, unknown field, invalid slug, bad product value, missing `confirm` |
| `401` | missing or unrecognized service token |
| `403` | token valid, but product not in the caller's `may_manage_products` |
| `404` | tenant not found, or no such endpoint under the API prefix |
| `405` | the path names a real endpoint but the method is wrong; the `Allow` header names what it accepts |
| `409` | slug already exists, product mismatch, or illegal state transition |
| `503` | registry unavailable |

`405` is an addition to §69's error-code set, and the only one. It is needed because the catch-all handler that gives an unknown path the JSON error contract suppresses `net/http`'s own 405 response: without it, a wrong method would be reported as an unknown endpoint, which says the endpoint does not exist when it does and the caller used the wrong verb. The endpoints and their `Allow` values are derived from a single table, so the header cannot drift from what is mounted.

### 13.4 Authentication and authorization

`Authorization: Bearer <token>` → constant-time comparison (`crypto/subtle`) against configured tokens → service identity → `may_manage_products` check (§41, §42).

The comparison is over **SHA-256 digests**, not the raw tokens. `subtle.ConstantTimeCompare` returns 0 immediately when the lengths differ, which is constant-time with respect to contents but leaks the length of the stored token; hashing both sides first makes every comparison a fixed 32 bytes. It also keeps the raw secret out of the long-lived heap in a form a core dump would expose directly.

Every configured token is compared and the loop is never left early, so the response time does not depend on the position of the matching token. Both properties are guarded: a structural test over the source fails if the loop gains a `return` or if the digest is compared with `==`, `bytes.Equal`, or `reflect.DeepEqual`, and a behavioural test constructs a duplicate and asserts the *last* entry wins — which an early-returning implementation could not do.

Exactly one `Authorization` header is accepted. `Header.Get` returns the first and silently drops the rest, so a request carrying two would be authenticated against one while another reader of the same request — a log shipper, a proxy, a WAF — might act on the other.

The authenticator sits behind an interface:

```go
type Authenticator interface {
    Identify(*http.Request) (ServiceIdentity, error)
}
```

`tokenAuth` implements it for V1; `mtlsAuth` can be added in V1.1 without touching handlers or authorization logic. That is what "mTLS-ready" means concretely — not a stub, just a seam.

Timing note: `crypto/subtle.ConstantTimeCompare` is used for all token comparisons. A short-circuiting `==` on a bearer token is a remote timing oracle.

**A known, accepted consequence of authorizing against the stored product.** For `disable` and `release` the product is read from the registry before authorization, so a service that manages one product can distinguish "no such slug" (404) from "exists but is another product's" (403), and can therefore enumerate the other product's tenants by guessing slugs. Both callers are the platform's own services on a loopback-only listener, and the statuses above are what this plan specifies, so it is documented rather than deviated from.

**M7 considered tightening this and deliberately did not.** M7 was named above as the place to do it, and the honest record is that it did not, for three reasons that were checked rather than assumed: the listener binds `127.0.0.1` and is not reachable from the public port (section F asserts that `/internal/v1/...` on the public port is proxied to the product instead); both callers are the platform's own services; and the status codes are the ones §13.3 specifies, so changing them is a specification change and not a hardening task. The finding is real the moment that listener becomes reachable from anywhere else, and it should be reopened at that point rather than rediscovered.

---

## 14. Error contract (§69)

Uniform JSON body on both listeners:

```json
{ "error": "unknown_tenant", "message": "The requested host is not registered." }
```

| `error` | Status | Notes |
|---|---|---|
| `unknown_tenant` | 404 | also used for reserved hosts and multi-label hosts |
| `disabled_tenant` | 404 | distinct code internally, **identical** response shape to `unknown_tenant` |
| `registry_unavailable` | 503 | |
| `upstream_unavailable` | 502 / 503 | |
| `rate_limited` | 429 | |
| `payload_too_large` | 413 | |
| `bad_request` | 400 | internal API only |
| `unauthorized` | 401 | internal API only |
| `forbidden` | 403 | internal API only |
| `conflict` | 409 | internal API only |
| `not_found` | 404 | internal API only |
| `method_not_allowed` | 405 | internal API only; the one addition to §69's set, and the `Allow` header is set from the same table the routes are mounted from |
| `internal_error` | 500 | |

`disabled_tenant` maps to the same status *and* the same message as `unknown_tenant` so the response is byte-identical. The distinction exists only in logs and metrics — otherwise §94's "disabled → 404" choice would leak tenant existence through a differing body.

**Content negotiation [PROPOSED]:** on the public listener, if `Accept` contains `text/html`, return a minimal HTML error page; otherwise JSON. Browsers get something readable, API clients get JSON. No stack traces, no internal addresses, no DB errors, ever (§69).

---

## 15. Observability (§49–52)

### 15.1 Logging

`log/slog` JSON handler, one line per request, exactly the §51 shape:

```json
{"time":"2026-09-19T09:51:00Z","level":"INFO","request_id":"a1b2c3","tenant":"mangala",
 "product":"POS","route":"backend","method":"POST","path":"/api/orders","status":200,
 "duration_ms":42,"upstream":"http://127.0.0.1:8082"}
```

**Never logged** (§51): `password`, `Authorization`, any JWT, refresh tokens, service secrets, the DSN, database credentials, or raw request bodies.

A redaction helper is the *only* sanctioned way to log headers, and it works by allow-list, not deny-list. A deny-list would eventually miss something.

### 15.2 Metrics (§52)

```text
gateway_requests_total{product,route,method,status}
gateway_request_duration_seconds{product,route}
gateway_registry_lookup_total{result}        # cache_hit | cache_miss | db_hit | db_error
gateway_registry_errors_total
gateway_unknown_tenant_total
gateway_disabled_tenant_total
gateway_upstream_errors_total{product,kind}
gateway_rate_limited_total{tier}
gateway_cache_entries
```

**Cardinality is controlled deliberately.** No label carries a full URL, a raw path, a tenant slug, or a request ID. The tenant slug in particular is unbounded — putting it on a metric label is how a Prometheus instance dies at 10k tenants. Per-tenant visibility comes from logs, which are indexed and sampled differently.

`route` is a closed set: `frontend | backend | platform`.

### 15.3 Request ID (§50)

Accepted from a trusted proxy, otherwise generated from `crypto/rand` (16 bytes, hex). Always echoed in the response and always forwarded downstream, so one ID traces Gateway → POS/GMS.

---

## 16. Test strategy (§90)

### 16.1 Layers

| Layer | Scope | Location |
|---|---|---|
| Unit | host parsing, slug rules, reserved hosts, state machine, header policy, cache, auth | `internal/**/*_test.go` |
| Integration | full pipeline against mock products + real MySQL | `tests/integration/` |
| Concurrency | `-race` across mixed tenants | `internal/tenant/cache_test.go` |
| Contract | §90 matrix as table-driven end-to-end cases | `tests/integration/matrix_test.go` |

### 16.2 Mock products (§83)

`cmd/mockproduct` runs four listeners, each echoing the headers it received:

| Port | Role |
|---|---|
| `:3001` | mock POS frontend |
| `:8082` | mock POS backend |
| `:3002` | mock GMS frontend |
| `:8083` | mock GMS backend |

Each response includes the received `Host`, `X-Tenant-ID`, `X-Request-ID`, `X-Forwarded-*`, and its own identity — so a test can *prove* the header contract rather than infer it from a 200. The backend returns JSON; the frontend returns HTML including a history-fallback body for unknown paths.

### 16.3 §90 matrix

| Test | Expected | Milestone |
|---|---|---|
| Active POS tenant `/` | POS frontend | M5 |
| Active POS tenant `/api/...` | POS backend | M5 |
| Active GMS tenant `/` | GMS frontend | M5 |
| Active GMS tenant `/api/v1/...` | GMS backend | M5 |
| Unknown tenant | 404 | M2 |
| Disabled tenant | 404 (byte-identical to unknown) | M2 |
| Bare apex | landing page | M2 |
| Reserved host | 404 | M2 |
| Conflicting `X-Tenant-ID` | host-derived identity wins | M4 |
| Registry unavailable | 503 for uncached route | M3 |
| POS unavailable | 502/503 | M5 |
| GMS unavailable | 502/503 | M5 |
| Public access to internal API | blocked | M6 |
| Duplicate tenant reservation | rejected | M1 |
| Cache invalidation | immediate route refresh | M6 |

**Status:** every row up to M5 is green. The M5 rows run against in-process `httptest` upstreams in `internal/proxy` and against `configs/.probe/upstream/` in the binary probe; both are stand-ins for the `cmd/mockproduct` this table names, which is an M9 deliverable.

### 16.4 Local testing without DNS

`*.zensys.lk` does not resolve on a dev machine. Every test and manual check uses an explicit Host header:

```bash
curl -s -H "Host: mangala.zensys.lk" http://127.0.0.1:8080/api/orders
curl -s -H "Host: garage1.zensys.lk" http://127.0.0.1:8080/
curl -s -H "Host: mangala.zensys.lk" -H "X-Tenant-ID: spoofed" http://127.0.0.1:8080/api/orders
curl -si -H "Host: random.zensys.lk" http://127.0.0.1:8080/        # expect 404
```

This recipe goes in the README. It is the single most useful piece of developer documentation this project will have.

### 16.5 Commands

```bash
make build      # go build ./...
make test       # go test ./... -race
make test-int   # integration tests against docker-compose.dev.yml
make lint       # go vet ./...  (+ golangci-lint if adopted)
make run-dev    # docker compose -f deploy/docker-compose.dev.yml up
```

---

## 17. Milestones

Each milestone is independently verifiable. **Do not start M(n+1) until M(n)'s exit criteria pass** — the ordering is chosen so that each layer is provable before anything depends on it.

### Implementation status

Last updated 2026-09-22, after M6. **267 top-level tests and 513 subtests pass under `-race`, zero failures** (798 `=== RUN` lines; 16 of them skip without `ZENSYS_TEST_DSN`, and all 16 pass when it is set). M4, M5, and M6 were additionally verified against the running binary — see `configs/.probe/run.sh`, which asserts its own results, prints 74 assertions, and exits non-zero on failure.

| Milestone | Status | Evidence |
|---|---|---|
| M0 — Bootstrap and environment | **Delivered** | Binary starts from the dev config; `/healthz` and `/readyz` on the internal port; refuses to start on missing env var, wildcard internal bind, scheme-less upstream, unknown config key |
| M1 — Registry layer | **Delivered** | Migration applied and re-applied idempotently; 16 MySQL integration tests green under `-race`, including 8-goroutine CAS race (1 success, 7 conflicts); startup survives an unreachable registry with `/readyz` reporting `registry: failed` |
| M2 — Host resolution | **Delivered** | 56-case host parser table; six distinct rejection reasons observable in the log; apex serves the landing page; verified end to end against live MySQL (ACTIVE → resolves, DISABLED → 404 byte-identical to unknown) |
| M3 — Route cache | **Delivered** | 60 tenants × 8 concurrent requests → every response carries its own tenant, exactly 60 registry reads; against live MySQL, 200 cached requests → ~0 registry reads; `negative_ttl` convergence verified end to end |
| M4 — Request context and headers | **Delivered** | Against the live binary: a spoofed `X-Tenant-ID` never changes routing and never reaches the product; an inbound `X-Request-ID` is adopted from loopback and replaced with a generated ID when nothing is trusted; a trusted `X-Forwarded-Proto: https` survives to the product while the Gateway's own connection is plain HTTP; the access log carries `tenant`/`product` on resolved requests and omits them on rejected ones |
| M5 — Reverse proxy | **Delivered** | Against the live binary: all four §90 routing rows reach the right one of four upstreams; the upstream sees `Host: {tenant}.zensys.lk`; path and query arrive unrewritten; an SSE response's first event arrives in 2.5 ms while the upstream holds for 2 s; POS stopped → POS tenant 502, GMS tenant 200 in the same process, with no GMS identity in the POS response. **Two defects found and fixed** — see the M5 deviations |
| M6 — Internal lifecycle API | **Delivered** | Against the live binary, the probe's whole lifecycle section (50 assertions, of 98 in the harness): every endpoint is 401 without a token, 401 with an unknown token or a wrong scheme, 401 with two `Authorization` headers; a POS token is 403 on reserve/activate/disable/release/read/list of a GMS tenant, with the GMS tenant verified untouched afterwards; reserve → 201 `PROVISIONING`, duplicate → 409, activate → 200, disable → 200, release without a matching `confirm` → 400, release → 200. **With `route_ttl` and `negative_ttl` at 300 s, a newly activated tenant routes on the very next public request and a disabled one stops on the very next** — proven discriminating by removing the invalidator, which makes the same assertion fail with 404. `/internal/v1/...` on the public address is proxied to the product, never served by the API. The token is never written to the log, and the access log names the calling service |
| M7 — Security hardening | **Delivered** | Two-tier rate limiting, header and body size limits, and the trust policy finalised. Against the live binary: tier 2 refuses a tenant's third request while tier 1 still has tokens, and a neighbour tenant is unaffected; tier 1 keys on the address the trusted-proxy policy resolved, so two forwarded clients get separate buckets, while rotating `X-Forwarded-For` from an untrusted source buys nothing; an oversized header block is a 431 in the error contract **and is logged**, which net/http's own refusal is not. The §8.1 step 3/4 ordering is settled and asserted — a request over both limits gets 413 and **leaves the rate-limit bucket untouched** |
| M8 — Observability | Next | Logging only; no metrics endpoint, no Prometheus client |
| M9 — Mock integration environment | Not started | `cmd/mockproduct`, `deploy/`, `tests/integration/` do not exist. The probe harness carries a minimal stand-in at `configs/.probe/upstream/`, which `cmd/mockproduct` should replace |
| M10 — Pilot preparation | Not started | |

### Deviations from this plan, by milestone

The plan is the intent; this records where the implementation diverged and why.

**M0**
- Health endpoints live on the **internal listener only** (plan §11.2, a documented deviation from §49): the public listener is a host-based catch-all, so `/healthz` there would shadow a product-owned path on `{tenant}.zensys.lk`. `health_on_public` re-enables them explicitly.
- `make` is not installed on this Windows machine; raw `go` commands are the primary path and the Makefile is kept for CI/Linux.
- The module's Go floor is **1.26.0**, not 1.22 as planned — `golang.org/x/sync` requires it, and `singleflight` (M3) is worth keeping.

**M1**
- The `Repository` interface differs from this plan's sketch: it exposes one `Transition(slug, from, to)` rather than separate `Activate`/`Disable`. The from/to form is what makes the compare-and-swap possible; `Activate` and `Disable` remain as `Service` methods that read state and then call `Transition`.
- `cmd/migrate` and `internal/tenant/dsn.go` were added — not in the plan's deliverables — because the migration needed a runner and the DSN needed enforced flags (`parseTime=true`, UTC).
- The slug regex was corrected from the architecture document's `{1,62}` to `{1,61}` (63-char DNS label limit). See §3.1 and §7.1.
- O5 was decided: the audit table **is** included.

**M2**
- `internal/httpapi/public.go` (the whole public pipeline) was added — the plan listed only `host.go`, `resolver.go`, and `platform.go`.
- Three rules were added or corrected against §8.3: punycode is rejected explicitly (the allow-list does **not** catch it, contrary to the plan's original claim), empty labels are rejected, and IP literals are checked both before and after port stripping.
- `internal/archtest` was added — a source-tree guard against package-level request state (§31), not in the plan.
- `NotImplementedUpstream` answered **503, not 404**, for a resolved tenant until M5 landed: by that point the tenant is known to exist, so a 404 would claim the hostname is unregistered. The type and its error code were deleted in M5, when the proxy became the terminal step.

**M3**
- Four refinements beyond §12, each closing a defect the plan's design permits — see the table in §12.

**M4**
- The header policy is split across **three** files in `internal/routing`, not the one this plan named: `headers.go` (the §10.1 policy), `trusted.go` (the trust set), `context.go` (request-scoped `Info` and `Edge`). Each has one job, and the trust decision is shared by `config.Validate`, which no longer has its own copy of the parsing rule.
- **`X-Forwarded-For` is set to the resolved client, not appended to the inbound chain.** §10.1 says "appended", but `ProxyRequest` strips the inbound header before `Rewrite` runs, so appending would mean re-introducing values the Gateway never verified. Only the resolved client address is asserted; the chain is not forwarded.
- The client address is read from the **rightmost** entry of `X-Forwarded-For` that is not itself a trusted proxy, not the leftmost. Every hop appends, so the leftmost entry is whatever the client sent — reading it would let any client choose its own apparent address, and after M7 its own rate limit.
- `internal/httpapi/middleware.go` gained **`RequestState`**, a per-request slot for the routing decision, and **`StandardChain`**, which now owns the middleware order for both listeners so tests exercise the order the process runs. The slot exists because a context value written by a handler cannot travel back out to a wrapping middleware, and the access log needs the tenant (§15.1).
- The rejection log's field for an unresolved hostname label was renamed from `tenant` to **`host_label`**. `tenant` now means a slug that resolved to a route, and nothing else, so a log query for `tenant=X` cannot match a hostname that was never a tenant.
- `TrustedProxy` and the header sanitising are applied to **both** listeners. The header contract is one policy, and the internal listener's callers are loopback services trusted by the same rule — which is what will let POS and GMS pass a correlation ID through when they drive the M6 lifecycle API.
- The probe harness in `configs/.probe/` was added. It is not part of the deliverable; it is the evidence, kept so the claims above can be re-checked rather than taken on trust. It reads the DSN from the environment and refuses to run without it.

**M5**
- The proxy lives in **`internal/proxy`**, not `internal/routing/proxy.go` as this plan named it. `routing` must not import `config` or `tenant` (see the package doc on `internal/tenant`), and a proxy that builds per-product transports from configuration has to import both. Keeping it a separate package preserves the dependency direction.
- **Two defects were found by running the binary, not by the tests** — which is the third milestone in a row where that has been true.
  1. **An oversized request body answered `502 upstream_unavailable` instead of `413 payload_too_large`.** `ErrPayloadTooLarge` was declared in `responses.go` and written by nothing; the `MaxBodyBytes` middleware wrapped the body and left detection to handlers, and no handler read a body until the proxy did. The fix is in two halves, and both are load-bearing: `MaxBodyBytes` now refuses a body whose declared `Content-Length` is over the limit *before* routing or dialling anything, and `proxy.handleError` maps `*http.MaxBytesError` to the same 413 for a chunked body, which declares no length and can only be caught while it is being read. Disabling either half fails a test that the other half does not cover.
  2. **The probe harness could silently measure the wrong process.** `start` waited for `/readyz` without checking that the process it had just launched was alive, so when a previous gateway still held the ports the new one died on `bind: Only one usage of each socket address` while `readyz` was answered by the old one. A whole section then reported on a configuration it had not loaded. `start` now waits for both ports to be free first and checks its own process is alive, and `stop` waits for the ports to be released rather than just reaping the PID. This is the same failure shape as the M0 finding: a check that passed because it was not looking at production wiring.
- `FlushInterval` is set to **`-1`** rather than left at zero. `httputil.ReverseProxy` already flushes immediately for `text/event-stream` or an unknown `Content-Length`, but that recognition is a heuristic over headers a product controls, and the exit criterion says streaming responses are not buffered. `-1` makes it unconditional. This depends on `statusRecorder.Unwrap` continuing to exist, since `ReverseProxy` flushes through `http.NewResponseController`; a test pins that separately so the failure is diagnosed in 0.01 s rather than appearing as a slow product.
- §48's "no cross-product fallback" is asserted **structurally** as the exit criteria require, in two places: each `route` owns its own transport and `ReverseProxy` (checked at runtime), and an AST guard over the package source fails if `Proxy` ever gains a second `*route` field or `route` gains any field that can reach another `route`. The guard has its own negative controls, because a source-scanning guard that stops matching reports success.
- The §90 routing rows and the product-outage row are verified against in-process `httptest` servers rather than `cmd/mockproduct`, which is an M9 deliverable and does not exist yet. The probe harness carries a minimal stand-in at `configs/.probe/upstream/` for the same reason; it should be deleted when `cmd/mockproduct` lands.
- `NotImplementedUpstream`, `ErrNotImplemented`, and `WriteNotImplemented` were deleted, along with the M2 deviation that documented them. `WriteUpstreamTimeout` was added: a timeout is a 503 with `Retry-After: 5`, distinct from the 502 for an unreachable product, because §47 documents 502/503 and only one of them is worth retrying.

**M6**
- The security package is split into **`internal/security/serviceauth.go`** (who is calling) and **`internal/security/authz.go`** (what they may do), as this plan's deliverables named. Authentication can be replaced with mTLS without touching authorization, and authorization can gain a rule without touching either transport.
- **Tokens are compared as SHA-256 digests, not as raw strings.** `subtle.ConstantTimeCompare` returns 0 immediately when the lengths differ, which is constant-time with respect to *contents* but leaks the *length* of the stored token to anyone who can measure it. Hashing both sides first makes every comparison a fixed 32 bytes, so neither contents nor length is observable — and it keeps the raw secret out of the long-lived heap in a form a core dump would expose directly.
- **The comparison loop is proven not to short-circuit, behaviourally.** A structural guard over the source (`internal/security/structure_test.go`) checks that `Identify` calls `subtle.ConstantTimeCompare`, that the comparison loop contains no `return`, and that the digest is never compared with `==`, `bytes.Equal`, or `reflect.DeepEqual`. The guard has four negative controls, and it was *observed to fail* by adding an early return to the loop. A second, behavioural test constructs two entries with the same digest and asserts the **last** one wins — which an early-returning implementation could not do. That state is unreachable in production because duplicate tokens are rejected at construction, which is exactly what makes it usable as evidence.
- **A second per-request slot was added**, in `httpapi.RequestState`, alongside the routing one. The authenticated caller is recorded by the authentication wrapper and read by the access log, which sits *outside* it — and a derived context travels only inward. This is the same problem `RequestState` was created for in M4, and it was found the same way: the access-log test failed, the log line had no `service` field, and the cause was the propagation direction rather than the wiring. `authenticate` now writes the slot instead of deriving a context; if the chain omits `RequestState`, every request is refused rather than allowed through unidentified.
- **`ErrMethodNotAllowed` is an addition to §69's closed error-code set**, and the only one. It exists because the catch-all that gives an unknown path the JSON error contract *suppresses* `net/http`'s own 405 — without a replacement, a wrong method would be reported as an unknown endpoint. The routes and the `Allow` header are derived from one endpoint table, so the header cannot drift from what is actually mounted. Two probe assertions and two test cases cover it, including that `Allow` names `GET` for a GET-only endpoint and `POST` for a POST-only one.
- **`platform-policy` is exposed** as this plan proposed, and a test asserts the document is *sufficient* to predict `reserve`: eleven candidates are checked against a validator implemented from the published pattern, lengths, and reserved-host list only — not by calling `tenant.ValidateSlug`, which would compare the Gateway with itself. The apex label is deliberately **not** in the reserved list, and the probe asserts its absence: the apex is the bare root domain, while `zensys.zensys.lk` is a legitimate tenant host whose slug is `zensys`. Listing it would make the document disagree with `reserve`.
- **`GET /tenant-routes` is narrowed to the caller's products.** A caller that manages exactly one product sees only that product; asking for a product it does not manage is a 403. This is stricter than §42 strictly requires — §42 is written about *management*, and a list is a read — but it costs nothing and points the safe way. A service that manages every product is not narrowed.
- **The illegal-transition exit criterion is tested at the mapping, not end to end.** No request can reach it: every lifecycle state can move to both ACTIVE and DISABLED, and the endpoints only ever ask for one of those two, so `ErrInvalidTransition` is unreachable through the API. It is verified by driving `writeLifecycleError` with a stub lifecycle, and this is stated rather than left as a gap. The related 409 that *is* reachable — asserting the wrong product for a stored route — needs a token that manages both products to get past authorization, so the fixture has one.
- `release` checks its confirmation **before** reading the registry, so a caller that forgot the field cannot use the 400-versus-404 distinction to probe which slugs exist. That ordering is a disclosure property, not an optimisation, and a test pins it.
- **A known, accepted consequence:** because authorization is checked against the *stored* product (§42's rule for `disable` and `release`), a service that manages one product can distinguish "no such slug" (404) from "exists but is another product's" (403), and so can enumerate the other product's tenants by guessing slugs. Both callers are the platform's own services on a loopback listener, and this plan specifies exactly these statuses, so it is documented rather than deviated from. **M7 considered tightening this and deliberately did not** — see the M7 deviations below for why, so that the note here does not read as an unkept promise.
- The probe harness gained a **section F** and was reordered so that section D — which deliberately stops the POS upstreams — runs last. Section F drives the whole lifecycle against the real binary with `route_ttl` and `negative_ttl` set to **300 s**, so "the next request reflects the change" cannot be satisfied by the cache expiring on its own. That assertion was verified by removing `.WithInvalidator(routeCache)` from the wiring and watching it fail with 404.
- Two probe defects were found and fixed by the harness itself: the F config was derived from A after A had already rewritten the TTL, so the substitution silently stopped matching (it now derives from `BASE`), and one assertion expected the apex label in the reserved-host list, which was wrong for the reason above.

**M7**
- **The limiter's key table is bounded, which the plan did not ask for.** §8.2 specifies the two tiers and their keys but says nothing about what holds them. A map from key to token bucket grows without limit and its keys are chosen by whoever is sending requests — the same defect class as an unbounded metric label (§20), applied to a different structure. `rate_limit.max_entries` caps it, eviction sweeps entries unseen for a full refill period before falling back to least-recently-seen, and `TestLimiter_KeySpaceIsBounded` pushes 100 000 distinct keys through a frozen clock to prove the cap holds.
- **`headers_too_large` (431) is an addition to §69's closed error-code set**, and `rate_limited` (429) is now actually written. `ErrRateLimited` had been declared since M0 and produced by nothing, which is the same "declared but never written" defect M5 found in `ErrPayloadTooLarge` — found this time by grepping for it rather than by a request misbehaving.
- **The §8.1 step 3/4 ordering is settled as "stateless before stateful".** The body-size limit is step 3 and tier-1 rate limiting is step 4, so a request that cannot be accepted on its own terms is refused on its own terms and does not spend a token. The discriminating assertion is the *second* one in `TestStandardChain_BodyLimitRunsBeforeTheRateLimiter` — that the 413 left the bucket untouched — because on a first request the bucket is full and a 429 is impossible in either order. The comment says so; the obvious assertion proves nothing.
- **A 429's tier is recoverable from the access log, and that rests on one line.** Tier 1 refuses before the hostname is parsed and so cannot carry a `tenant`; tier 2 refuses after resolution and so can. That holds only while `PublicHandler.ServeHTTP` calls `routing.SetInfo` **before** its tier-2 check, which is where it now is. The probe found the original placement: a tier-2 refusal was an anonymous 429 indistinguishable from a tier-1 one, contradicting the comment written next to it. `TestTier2RateLimit_RefusalIsDistinguishableInTheLog` pins the placement, and was observed to fail — with no other test in the package noticing — when the call was moved back.
- **`Retry-After` is floored at 1 for a refusal, and 0 is reserved for the allowed case.** A refusal that becomes admissible within the current second would otherwise carry `Retry-After: 0`, which is an instruction to spin. My own test caught this: the guard was `RetryAfter <= 0` rather than `Allowed`.
- **The internal listener passes a nil limiter, deliberately.** §8.1 places the limiter in the public pipeline, and the internal callers are authenticated loopback services. This is a decision recorded in `chainOptions` rather than an omission.
- **Two probe defects were found by the probe, and one of them was the harness measuring itself.** Both are the same failure shape as M5's "could silently measure the wrong process":
  1. **The harness's own liveness probe was a client.** `wait_ports_free` checked the public port with `curl -s -m 1 "$PUBLIC/"`, which is a request like any other: it was logged, and it spent a tier-1 token. Section I — which asserts that a rotated `X-Forwarded-For` buys no fresh allowance — failed with a 429 where it wanted a 200, because one of its two tokens had already been spent by the script. The fix is not a larger burst but a different primitive: `port_listening` asks the kernel with a raw TCP connect (`/dev/tcp`), which neither logs nor limits and costs no process.
  2. **The harness measured a binary it did not build.** The probe asserted that `bin/gateway.exe` *existed* and otherwise assumed it was current. After the `SetInfo` fix above was committed, a full probe run still reported the tier-2 refusal with no tenant — because the binary was 20 minutes older than the source. The harness now builds the gateway itself, exactly as it builds its upstream stand-in, and additionally fails if any `.go` file is newer than the binary it is about to run. **Existence is not freshness**, and a check that only proves a file is present reads as coverage while providing none.
- **The enumeration consequence above was left alone.** M7 was named as the place to tighten it. Three things were checked rather than assumed before deciding not to: the listener binds `127.0.0.1`; section F asserts that `/internal/v1/...` on the *public* port is proxied to the product and never served by the API; and the status codes are the ones §13.3 specifies, so changing them is a specification change rather than a hardening task. Recorded in §13.4 and reopened if that listener ever becomes reachable from anywhere else.

### M0 — Bootstrap and environment (§72)

> **Status: delivered.** All exit criteria verified. Deviations above.

**Deliverables**
- Go toolchain installed and on PATH.
- `go mod init github.com/zensys/gateway` (declared in `go.mod`; see O2).
- Repository skeleton, `README.md`, `.gitignore`, `Makefile`.
- `internal/config`: structs, YAML loader, env overlay, fail-fast validation.
- `internal/observability/logging.go`: slog JSON handler.
- `cmd/gateway/main.go`: two listeners, graceful shutdown, `/healthz`, `/readyz`.
- `configs/gateway.example.yaml`.

**Exit criteria**
- `go build ./...` and `go vet ./...` clean.
- `go test ./... -race` green.
- Gateway starts with the example config and answers `/healthz` `200` on the internal port.
- Gateway **refuses to start** with: missing env var, invalid root domain, mismatched listener addresses, unparseable upstream URL. (Negative tests, not just a happy path.)
- No POS/GMS code touched. No product database connection exists in the codebase.

---

### M1 — Registry layer (§73)

> **Status: delivered.** All exit criteria verified against live MySQL. Deviations above — note the `Repository` interface shape differs from the deliverable sketch.

**Deliverables**
- `migrations/0001_init.sql` (`tenant_routes`, plus `tenant_route_events` if O5 is accepted).
- `internal/tenant/model.go` — `Route`, `Product`, `Status`, transition table.
- `internal/tenant/slug.go` — slug regex, reserved-host registry (the single authoritative definition, §10).
- `internal/tenant/repository.go` — interface: `GetBySlug`, `List`, `Reserve`, `Activate`, `Disable`, `Release`.
- `internal/tenant/mysql_repository.go`.
- `internal/tenant/service.go` — validation, transitions, reserved-host rejection, event writes.

**Exit criteria**
- Migration applies cleanly and is idempotent.
- Tests cover: unknown tenant, duplicate slug, invalid slug, invalid product, invalid status, every legal transition, every illegal transition.
- Reserved hostname cannot be reserved through the service. It is **not** re-enforced by the database: the reserved list has exactly one authoritative definition, in Go, so a second copy in a `CHECK` constraint could only drift. The database enforces *format* (`chk_slug_format`, `chk_slug_lowercase`), not *reservation*.
- Uppercase slug cannot be stored.
- No product database credentials exist anywhere in the codebase (§91.5).

---

### M2 — Host resolution (§74, §75)

> **Status: delivered.** All exit criteria verified against the running binary and live MySQL. Deviations above.

**Deliverables**
- `internal/routing/host.go` — full §8 sequence, including the normalization rules in §8.3 above.
- `internal/routing/resolver.go` — `host → tenant → product`.
- `internal/httpapi/platform.go` — apex landing, reserved-host 404.

**Exit criteria** — table tests covering every §74 case plus the §8.3 hardening:

`tenant.zensys.lk` · uppercase · with port · trailing dot · apex · multi-label · reserved host · wrong domain · invalid slug · IP literal · overlong host · overlong label · CR/LF in host · punycode

- `zensys.lk` renders the landing page.
- `admin.zensys.lk` → 404.
- `a.b.zensys.lk` → 404.

---

### M3 — Route cache (§76)

> **Status: delivered.** All exit criteria verified under `-race` and against live MySQL. Deviations above.

**Deliverables**
- `internal/tenant/cache.go` — TTL cache, negative cache, `Invalidate`, `Flush`, singleflight.
- Wire cache into the resolver path.
- `Service.WithInvalidator(cache)` so every lifecycle mutation drops the affected slug.

**Exit criteria**
- **`-race` test drives concurrent requests across ≥ 50 interleaved tenants and asserts each response carries its own tenant.** This is the §31 requirement and the single most important test in the project.
- Cold cache → registry read; warm cache → no registry read (asserted with a counting repository fake).
- 50 concurrent requests for one cold slug produce exactly **one** registry read (singleflight).
- Negative cache expires and a newly activated tenant becomes routable within `negative_ttl`.
- `Flush()` then request → still correct (§92).

Verified additionally: against a real MySQL, a burst of 200 requests for a cached tenant produced ~0 registry reads, and with `negative_ttl: 2s` a tenant inserted into the database became routable at t+4s having correctly 404'd at t+1s.

One note for M6: invalidation is wired into the lifecycle Service and tested, but no HTTP endpoint calls the Service yet, so the invalidation path is not reachable end-to-end until the internal API lands. The TTL remains the backstop (§32) in the meantime, which is what the `negative_ttl` convergence test exercises.

---

### M4 — Request context and headers (§77, §78)

> **Status: delivered.** Deviations above. Each exit criterion below names where it is verified; the ones marked *binary* were observed against the running process, not only in tests.

**Deliverables**
- Request-scoped context carrying `request_id`, `host`, `tenant`, `product`, `path`, routing result.
- `internal/routing/headers.go` — sanitization + injection.
- Trusted-proxy handling including the **`X-Forwarded-Proto` preservation described in §10.2**.

**Exit criteria**

| Criterion | Verified by |
|---|---|
| Client-supplied `X-Tenant-ID: spoofed` on a resolved host → the upstream sees the canonical tenant (§91.1) | `TestHeaderContract_SpoofedTenantHeaderNeverReachesTheUpstream` asserts both halves — deleted before the upstream sees the request, and set to the canonical slug after. *Binary*: `X-Tenant-ID: nonexistent-tenant` on `garage1.zensys.lk` answers `503`, not `404`, so the spoof never reached routing |
| Client `X-Forwarded-*` from an **untrusted** source is discarded | `TestResolveEdge_UntrustedSourceIsNotBelieved`, `TestHeaderContract_UntrustedForwardedHeadersAreDiscarded`. *Binary*: with `trusted_proxies: []`, `X-Forwarded-Host: evil.example.com` does not change which tenant resolves |
| `X-Forwarded-Proto: https` from a **trusted** proxy survives downstream even though `r.TLS == nil` | `TestResolveEdge_TrustedSourceIsBelieved`, `TestApplyForwarded_TrustedProxyPreservesTheEdgeScheme`, `TestHeaderContract_TrustedProxySchemeSurvivesToTheUpstream` |
| `X-Request-ID` forwarded and echoed; generated when absent | `TestRequestID_AdoptsInboundIDOnlyFromATrustedProxy`, `TestRequestID_RejectsMalformedInboundIDs`, `TestPublicChain_AdoptsAnInboundRequestIDFromATrustedProxy`. *Binary*: `trace-abc-123` is echoed from loopback and replaced with a generated ID when nothing is trusted |
| No package-level mutable tenant state exists | `internal/archtest` `TestNoPackageLevelRequestState`, whose detector is itself tested against a synthetic positive case |

Two properties worth stating explicitly, because they are choices rather than consequences:

- **Routing never reads a forwarded host.** `X-Forwarded-Host` is believed from a trusted proxy, but only for what the Gateway advertises downstream. Which tenant a request reaches is decided by the request's own `Host` header, always (`TestHeaderContract_ForwardedHostDoesNotChangeRouting`).
- **The trust decision fails closed.** `routing.EdgeFrom` reports `false` for a context with no edge, and `RequestID` treats that as untrusted, so a chain that lost `TrustedProxy` generates IDs rather than believing the client's (`TestRequestID_FailsClosedWithoutTheTrustMiddleware`).

---

### M5 — Reverse proxy (§79)

> **Status: delivered.** All exit criteria verified. Deviations above.

**Deliverables**
- `internal/proxy/` — per-product transports, `Rewrite`, `ErrorHandler`, `FlushInterval: -1` for streaming. (Planned as `internal/routing/proxy.go`; see the deviations.)
- Upstream selection wired to the M2 resolver.

**Exit criteria**

| Criterion | Verified by | Against the binary |
|---|---|---|
| All four §90 routing rows pass against `cmd/mockproduct` | `TestProxy_RoutingMatrix` — each row also asserts the other three upstreams received **nothing**, so "routed to the frontend" cannot pass for an implementation that sends the request everywhere. `cmd/mockproduct` is an M9 deliverable, so the rows run against `httptest` servers; the deviation is recorded above | all four rows, plus path and query arriving unrewritten |
| Upstream `Host` = the public hostname (§25, §44) | `TestProxy_UpstreamSeesThePublicHostname`, and `TestProxy_PreserveHostOffIsWired` so the first cannot pass because Go happened to do the right thing | `Host: garage1.zensys.lk` observed at the product |
| POS returning 500 is passed through as 500 | `TestProxy_ProductResponsesArePassedThroughUnchanged` — 500, the product's own 404, 201, and a 302 with `Location`; the 404 case also asserts the body is not the Gateway's `unknown_tenant` response | — |
| POS stopped → POS 502/503, GMS unaffected in the same process | `TestProxy_ProductOutageIsIsolated` — both products in one `Proxy` in one process | POS 502, GMS 200, and the POS response carries no GMS identity |
| **No cross-product fallback path exists** (§48), structurally | `TestProxy_EachRouteOwnsItsTransport` (each route owns its transport and `ReverseProxy`) and `TestProxy_HasNoCrossProductFallbackPath` — an AST guard failing if `Proxy` gains a second `*route` field or `route` gains any field reaching another `route`, with its own negative controls | — |
| Streaming/SSE responses are not buffered | `TestProxy_StreamingResponseIsNotBuffered` — a `text/plain` response **with** a `Content-Length`, so the test cannot pass on Go's streaming heuristic. `TestProxy_StandardChainExposesFlush` pins the `Unwrap` dependency separately | first event in 2.5 ms while the upstream holds for 2 s |

Two further properties are asserted rather than assumed: routing never reads a forwarded host (`TestHeaderContract_ForwardedHostDoesNotChangeRouting`, and the binary probe sends `X-Forwarded-Host: mangala.zensys.lk` on a `garage1` request and still reaches GMS), and a body over `max_body_bytes` is refused with 413 before any upstream connection is opened (`TestProxy_OversizedBodyIsRejectedBeforeItReachesAProduct`, `TestProxy_ChunkedBodyOverTheLimitIsA413`).

---

### M6 — Internal lifecycle API (§80)

> **Status: delivered.** All exit criteria verified. Deviations above.

**Deliverables**
- `internal/httpapi/internal.go` — six endpoints plus the **[PROPOSED]** `platform-policy`.
- `internal/security/serviceauth.go`, `authz.go`.
- Cache invalidation on every mutation (§33).

**Exit criteria**

| Criterion | Verified by | Binary evidence |
|---|---|---|
| Every mutation invalidates the affected route; the next request reflects it immediately | `TestInternal_MutationsTakeEffectOnTheNextPublicRequest` (reserve caches an absence first, then asserts routing on the *very next* public request), `TestInternal_InvalidationIsNotCarriedByTheRouteCacheItself` (proves the TTL is honoured, so the first test is not passing on an always-miss cache) | With `route_ttl` and `negative_ttl` at 300 s: activate → the next public request is 200 and reaches the POS frontend; disable → the next is 404. **Removing `.WithInvalidator(routeCache)` makes both fail with 404**, so the check discriminates |
| Missing token → 401; wrong token → 401 | `TestInternal_UnauthenticatedRequestsAreRefused` (all seven endpoints), `TestInternal_UnknownAndMalformedTokensAreRefused` | Probe: no token, unknown token, wrong scheme, and two `Authorization` headers are each 401 |
| Valid token, wrong product → 403 | `TestInternal_POSServiceCannotTouchGMSTenants`, `TestInternal_GMSServiceCannotTouchPOSTenants`, `TestInternal_ReadingAnotherProductsRouteIsForbidden` | Probe: 403 on reserve/activate/disable/release/read/list, and the GMS tenant is verified still reaching its own frontend afterwards |
| A POS token cannot reserve, activate, disable, or release a GMS tenant | as above, plus `TestInternal_ErrorMapping` for the 403 mapping | as above |
| Duplicate reserve → 409 | `TestInternal_DuplicateReserveIsAConflict`, `TestInternal_ConcurrentReservesProduceOneWinner` (8 goroutines, exactly 1 created and 7 conflicts) | Probe: 409 |
| Illegal transition → 409 | `TestInternal_ErrorMapping` — driven through `writeLifecycleError` with a stub, because no request can reach it: every state can move to both ACTIVE and DISABLED | n/a — unreachable through the API, stated rather than left as a gap |
| `release` without a matching `confirm` → 400 | `TestInternal_ReleaseRequiresMatchingConfirmation` (5 cases), `TestInternal_ReleaseChecksConfirmationBeforeReadingTheRegistry` | Probe: missing, mismatched, and product-named confirmations are all 400, and the route is verified still usable afterwards |
| The internal listener is unreachable from the public listener's address | `TestInternal_LifecyclePathsAreNotServedOnThePublicListener` — the discriminating observable is the *upstream* receiving the request, since a 200 alone could have come from either handler | Probe: `/internal/v1/tenant-routes` on the public port with a tenant host returns the product's own response; with an unknown host it is the ordinary 404; the same path on the private port is the API (401 without a token) |
| Token comparison is constant-time (`crypto/subtle`) | `TestSecurity_IdentifyComparesInConstantTime` (structural, four negative controls), `TestTokenAuthenticator_DoesNotShortCircuitOnMatch` (behavioural) | Observed to fail when an early `return` was added to the comparison loop |

Two further properties are asserted rather than assumed: the published `platform-policy` document is sufficient to predict what `reserve` accepts (`TestInternal_PlatformPolicyAgreesWithReserve`, eleven candidates, checked by a validator implemented from the document alone), and no service token is ever written to the log at any level (`TestInternal_AccessLogNamesTheCallingService`, plus a probe assertion over `f.log`).

---

### M7 — Security hardening (§81)

> **Status: delivered.** Verified against the running binary — see the probe sections G, H, I, and J in `configs/.probe/run.sh`, which assert their own results and exit non-zero on failure.

**Deliverables**
- Two-tier rate limiting. ✅ `internal/security/ratelimit.go` — tier 1 keyed by resolved client address in the middleware chain, tier 2 keyed by tenant inside the routing handler. Both bounded.
- Request size limits, header size limits. ✅ `max_body_bytes` (M5) plus `httpapi.HeaderSizeLimit`, which makes `max_header_bytes` the authoritative policy limit and puts the refusal in the error contract.
- Safe error responses on both listeners. ✅ One more code added — `headers_too_large` (431) — and `rate_limited` (429) now actually written, having been declared since M0 and written by nothing.
- Forwarded-header trust policy finalised. ✅ §10.2. The code was already right; what was missing was the stated requirement that every hop be listed, and the consequence of it not being.

**Exit criteria** — the §91 checklist, every box ticked. The "Verified by" column names the guard that would fail if the property were lost; the "Binary evidence" column names the probe assertion that observed it on the running process.

| # | Criterion | Verified by | Binary evidence |
|---|---|---|---|
| 1 | Client cannot override the Host-derived tenant | `TestHeaderContract_*` (routing reads `r.Host` only), `routing.ParseHost` tests | A, B — a spoofed `X-Tenant-ID` and `X-Forwarded-Host` change neither the route nor the upstream's view |
| 2 | Unknown tenants fail closed | `TestPublicHandler_UnknownHost*`; negative cache tests | A — `unknown.zensys.lk` is 404 |
| 3 | Disabled tenants do not reach products | `TestPublicHandler_Disabled*`, `TestInternal_MutationsTakeEffectOnTheNextPublicRequest` | F — disable, then the very next request is 404 and byte-identical to an unknown host |
| 4 | Product failures do not fall through to another product | `internal/proxy/structure_test.go` (AST guard), `TestProxy_*Isolation*` | D — POS stopped → 502 with no GMS identity in the body, GMS still 200 |
| 5 | Gateway has no product DB credentials | `internal/archtest`, config validation (one DSN, and it is the registry's) | Not observable from a request; asserted in source |
| 6 | Gateway does not authenticate product users | No auth code in the request path; `Authorization` passes through untouched | C — the upstream sees the client's own `Authorization` |
| 7 | Gateway does not issue product JWTs | No JWT library in `go.mod` — a structural fact, not a behavioural one | — |
| 8 | Internal lifecycle APIs are private | Separate `http.Server` on `internal_addr`; config rejects a non-private bind | F — `/internal/v1/...` on the public address is proxied to the product, never served by the API |
| 9 | Internal lifecycle APIs are authenticated | `TestTokenAuthenticator_*`, `TestInternal_UnauthenticatedRequestsAreRefused` | F — every endpoint is 401 with no token, an unknown token, or two `Authorization` headers |
| 10 | Internal lifecycle APIs enforce product authorization | `TestServiceIdentity_Authorize*`, `TestInternal_POSServiceCannotTouchGMSTenants` | F — a POS token is 403 on a GMS tenant, with the tenant verified untouched |
| 11 | Reserved hosts cannot become tenants | `internal/tenant/reserved.go` + `TestReserved*`; reserve refuses a reserved slug | F — `platform-policy` publishes the list and `reserve` agrees with it |
| 12 | Slug rules are enforced | `tenant.ValidateSlugFormat` + the DB `CHECK`; `TestInternal_PlatformPolicyAgreesWithReserve` | F — `platform-policy` and `reserve` accept and refuse the same set |
| 13 | Forwarded headers are trusted only from the intended proxy chain | `routing.ResolveEdge`/`SanitizeInbound` tests; `TestTier1RateLimit_ASpoofedForwardedForCannotEvadeTheLimit` | A, B — the same headers are honoured from loopback and discarded when nothing is trusted; I — rotating `X-Forwarded-For` from an untrusted source does not buy a fresh rate-limit bucket |

**Where M7 is thinner than it looks.** Two properties hold but are worth stating rather than implying:

- **The header limit has a floor above it that the Gateway cannot shape or log.** net/http reads up to `max_header_bytes + 4096` before refusing, and answers a bare text 431 straight to the connection before any Gateway code runs. Above that cap the refusal is net/http's, is not in the error contract, and produces no access-log line. This is not fixable from inside the process — the block must be read before anything can judge it too large — so `HeaderSizeLimit` closes the band it can see and the remainder is documented. See `httpapi.HeaderSizeLimit`.
- **Requests refused by the two stateless size checks are logged but not rate limited**, because both run before the limiter (§8.1 step 3 before step 4). The log volume this could produce is bounded by `maxLoggedPathLength` rather than by the limiter, which is the correct place for it: the amplifier was a client-chosen path length, not the request rate.

---

### M8 — Observability (§82)

**Deliverables**
- All §15.2 metrics registered and emitted.
- Structured logging matching §15.1.
- `/readyz` per-check breakdown.

**Exit criteria**
- `/metrics` exposes every series with expected labels.
- A single request produces exactly one structured log line with the §51 field set.
- Grep-based test proves no secret-bearing header can reach the logger.
- **Metric label cardinality is bounded** — asserted by a test that runs 1,000 distinct tenants through and confirms the number of distinct label combinations does not grow with tenant count.

---

### M9 — Mock integration environment (§83)

**Deliverables**
- `cmd/mockproduct` (four listeners).
- `deploy/docker-compose.dev.yml` — MySQL + mocks + gateway.
- `deploy/Dockerfile` — multi-stage, static binary.
- README quickstart with the Host-header recipes from §16.4.

**Exit criteria**
- `docker compose up` brings up the whole stack from a clean checkout.
- `mangala.zensys.lk → mock POS`, `garage1.zensys.lk → mock GMS`, proven end-to-end.
- Full §90 matrix green in one command.
- A newcomer can go from clone to a verified routing decision using only the README.

---

### M10 — Pilot preparation and handoff (§84–§89)

**Deliverables**
- `deploy/edge/nginx.conf.example` — reference Cloudflare → Nginx → Gateway config (§85).
- `deploy/systemd/zensys-gateway.service`.
- Tenant import tool (§86) — reads POS and GMS tenant slugs, validates, detects duplicates, **dry-run by default**.
- `docs/runbooks/rollout.md`, `docs/runbooks/rollback.md`.
- Duplicate-slug conflict report format (§64).

**Exit criteria**
- Import tool dry-run against a sample list produces a conflict report for the §64 case (`mangala` in both POS and GMS) and refuses to proceed.
- Import tool never reads product DB credentials into the Gateway (§86).
- Nginx reference config verified to pass `Host`, `X-Forwarded-Host`, `X-Forwarded-For`, `X-Forwarded-Proto` correctly (§85).
- Runbooks reviewed.

**Blocked on owner input:** real upstream addresses (O3), POS `/api` work (O4), edge infrastructure access.

---

## 18. Acceptance traceability

### 18.1 §91 Security criteria

`✅` means verified against the running binary as well as by test; the evidence column names where.

| Criterion | Milestone | Status |
|---|---|---|
| Client cannot override Host-derived tenant | M4 | ✅ probe sections A and B — a spoofed `X-Tenant-ID` and a spoofed `X-Forwarded-Host` change neither the route nor the product |
| Unknown tenants fail closed | M2, M5 | ✅ probe — 404, byte-identical to a disabled tenant's |
| Disabled tenants do not reach products | M2, M5, M6 | ✅ probe section F — disable takes effect on the very next public request |
| Product failures do not fall through | M5 | ✅ probe section D — POS down gives 502 with no GMS identity in the response |
| Gateway has no product DB credentials | M0, M1 | ✅ by construction — the only DSN is the registry's, and no product database is named anywhere in the tree |
| Gateway does not authenticate product users | M4, M5 (structural) | ✅ by construction — the only `Authenticator` in the tree is the internal API's |
| Gateway does not issue product JWTs | M4, M5 (structural) | ✅ by construction — no JWT library is a dependency and no code path mints a token |
| Internal lifecycle APIs are private | M0, M6 | ✅ probe section F — the public port proxies `/internal/v1/...` to the product; the private port is where the API answers |
| Internal lifecycle APIs are authenticated | M6 | ✅ probe section F — 401 with no token, an unknown token, a wrong scheme, or two `Authorization` headers |
| Internal lifecycle APIs enforce product authorization | M6 | ✅ probe section F — a POS token is 403 on every GMS operation, and the GMS tenant is verified untouched |
| Reserved hosts cannot become tenants | M1, M2 | ✅ probe — `www` is refused; `reserve` rejects a reserved slug with 400 |
| Slug rules are enforced | M1, M2 | ✅ probe section F — the published policy predicts `reserve` for eleven candidates |
| Forwarded headers trusted only from intended chain | M4, M7 | ✅ probe sections A, B, and I — the same headers are honoured from loopback, discarded when nothing is trusted, and rotating the client address buys no fresh rate-limit allowance |
| Oversized requests are refused before they cost anything | M5, M7 | ✅ probe sections E and J — a declared `Content-Length` gives 413 before dialling, a chunked body gives 413 mid-read, and an oversized header block gives 431 in the error contract and in the log |
| A caller cannot exhaust the Gateway's memory by choosing keys | M7 | ✅ `TestLimiter_KeySpaceIsBounded` — 100 000 distinct keys against a frozen clock hold the table at its cap; the probe's tier-1 sections exercise the same table through the running binary |
| Rate limiting cannot be evaded by forging a client address | M7 | ✅ probe sections H and I — the key is the address resolved through `trusted_proxies`, and with nothing trusted three different forwarded addresses share one bucket |
| One tenant cannot consume another's allowance | M7 | ✅ probe section G — a neighbouring tenant is served after the first exhausts its bucket |
| A refusal is distinguishable from a routing failure, and the two tiers from each other | M7 | ✅ probe sections G and H — 429 with `Retry-After` in the error contract; the tier-2 line carries a `tenant` and the tier-1 line cannot, which `TestTier2RateLimit_RefusalIsDistinguishableInTheLog` pins |

### 18.2 §92 Performance criteria

| Criterion | Milestone |
|---|---|
| Runs as one shared process | M0 |
| Handles many tenants concurrently | M3 |
| Uses in-memory route caching | M3 |
| Avoids product DB lookups for routing | M1 (structural) |
| Avoids per-tenant processes / JVMs | Architecture |
| Keeps persistent registry separate from product DBs | M1 |
| Remains correct if the local route cache is empty | M3, M5 |

### 18.3 §93 Architecture invariants

Invariants 1–20 are treated as **code-review gates**, not test cases. The three that are easy to violate accidentally get automated enforcement:

| Invariant | Enforcement |
|---|---|
| 13 — Client `X-Tenant-ID` cannot override Host | M4 test |
| 16 — No global mutable `currentTenant` | M3 `-race` test + `internal/archtest` |
| 19 — DB credentials never enter the Gateway | M1 structural review; no CI grep exists |

**Correction (M7).** This table previously said invariant 16 and 19 were enforced by a "CI grep", and no CI configuration exists in this repository — there is no CI runner, no workflow file, and no git repository at all. That was a plan claiming a control it did not have, which is worse than not claiming it: a grep that never runs reads as coverage. Invariant 16 is in fact enforced, by `internal/archtest`, which fails `go test ./...` on a package-level variable that could hold per-request routing state — and whose detector has its own tests, so a typo that made it match nothing fails instead of silently disabling the guard. Invariant 19 has no automated enforcement today and is a review gate; it is listed as such rather than as something that is checked. Wiring these into CI is an M10 item, because M10 is the first milestone that assumes a pipeline exists.

---

## 19. Risks

| # | Risk | Impact | Mitigation |
|---|---|---|---|
| R1 | Go not installed | Blocks everything | Being installed now via winget; M0 exit criterion |
| R2 | **POS has no `/api/...` surface yet (§66, §20)** | POS cannot be piloted end-to-end until that separate project lands | Prove the Gateway on GMS + mocks first. M9 does not depend on POS changes. Sequence POS pilot after O4 |
| R3 | Real upstream addresses unknown | Blocks M10 / production only | Mocks cover M0–M9 entirely |
| R4 | `X-Forwarded-Proto` misconfiguration | HTTPS-asserting products break in prod while working locally — a classic late-discovery bug | Explicitly handled in M4 (§10.2), verified in M10 (§85) |
| R5 | Reserved-host list drift between Gateway and products | A reserved name becomes a tenant, or a valid tenant is rejected | One authoritative definition in Go (§10), exposed via `platform-policy`, enforced by DB `CHECK` for format |
| R6 | Slug policy requires 3–64 chars (§9) | Two-character tenant slugs silently rejected | Flagged in §3.1; confirm before POS/GMS onboarding code is written |
| R7 | Metric cardinality explosion | Prometheus instability at scale | Tenant slug deliberately excluded from all labels; enforced by M8 test |
| R8 | Cache stampede on cold start | Thundering herd on the registry after restart | `singleflight` in M3 |
| R9 | Multi-label subdomain assumption | Future `api.mangala.zensys.lk` needs a routing change | §8 explicitly rejects multi-label in V1. If sub-subdomains are ever wanted, that is an architecture revision, not a config flag |
| R10 | `release` is destructive and unaudited | Irreversible tenant routing loss with no trail | `confirm` field (M6) + proposed event table (O5) |
| R11 | Header-based attacks (smuggling, CRLF injection) | Request routing confusion | Strict host character allow-list (§8.3), hop-by-hop stripping, no client forwarded-header trust |

---

## 20. Explicit non-goals

Restating these because scope creep is the main threat to a small edge service:

- **No static file serving** — decision 2. Products own their frontends and SPA fallback.
- **No JWT handling** of any kind — not issuance, not validation, not decoding.
- **No product database access** — not for routing, not for health, not for convenience.
- **No per-tenant processes, JVMs, or server instances.**
- **No POS or GMS code changes from this repository** (§84).
- **No tenant-specific upstream configuration** — upstreams are product-level (§15).
- **No admin UI in V1** — `admin.zensys.lk` is 404 by decision.
- **No request/response body inspection or transformation.**

---

## 21. Immediate next step

**M8 — Observability (§82).** M0–M7 are delivered and verified; see §17 for the evidence and the deviations.

M8's deliverables, from the milestone table: every §15.2 metric registered and emitted; structured logging matching §15.1; `/readyz` reporting its per-check breakdown; and a test that a tenant slug never becomes a label value. The last one is the only one that is a *guard* rather than a feature, and it is the one worth writing first — the codebase already treats unbounded cardinality as a defect class (§20's `MaxEntries` bound is the same rule applied to a different structure), so the metric layer should be born with the check rather than acquire it after the first incident.

**Carried out of M7**, none of which blocks M8:

1. **Every hop must be listed in `proxy.trusted_proxies`, and omitting one fails silently.** Routing is unaffected, because routing never reads a forwarded header — but the *rate limiter's key* becomes the last proxy instead of the client, so every client behind that hop shares one bucket. §10.2 records this, and it is a **prerequisite for M10** rather than a tuning note: M10 is where the real edge configuration (§85) is written down, and a trust list that is missing a hop is a silent shared-bucket bug that no test in this repository can catch.
2. **Size-refused requests are logged but not rate limited.** They are refused before the limiter by design (§8.1 step 3 before step 4), so the log line they produce is the only thing an attacker controls the cost of. The bound is `maxLoggedPathLength` (512 bytes). Worth revisiting only if the log becomes the bottleneck.
3. **The band above `max_header_bytes + 4096` is outside the error contract.** `net/http` answers a bare text 431 before any Gateway code runs, and logs nothing. Not fixable from inside the process; documented in §17's M7 section and in the README so it is not rediscovered as a bug.
4. **No CI pipeline exists**, so §18.3's automated enforcement is `internal/archtest` running under `go test`, and invariant 19 is a review gate. There is also no git repository. Both are M10 items.

The three original owner items are all closed: **[O2]** the module path is `github.com/zensys/gateway`, **[O1]** the registry DSN is supplied through `ZENSYS_REGISTRY_DSN` and M1's integration tests are green, and **[O5]** the audit table `tenant_route_events` is included — the probe's lifecycle section writes 19 events through it.
