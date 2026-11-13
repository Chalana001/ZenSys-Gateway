# ADR 0001 — ZenSys Gateway V1 Open Decisions

**Status:** Accepted
**Date:** 2026-09-19
**Deciders:** Project owner
**Source:** `docs/architecture/ZenSys_Gateway_V1_Architecture.md` §94

---

## Context

The architecture document is deliberately explicit that it "does not invent deployment details that have not been decided", and §94 lists ten decisions that must be answered before the architecture can be considered implementation-final. §71 forbids guessing them.

This ADR records the answers, the reasoning, and the consequences. Where an answer closes one decision and constrains another, that is noted.

---

## Decision 1 — Registry database engine

**Chosen: MySQL 8**

The existing POS and GMS infrastructure already runs MySQL (8.0.45 is present on the development machine), so this adds no new engine to the operational surface: one backup strategy, one monitoring story, one set of failure modes the team already knows.

**Consequences**
- `database/sql` + `github.com/go-sql-driver/mysql`; hand-written SQL, no ORM.
- Migrations are plain `.sql` files.
- MySQL 8 `CHECK` constraints are used to enforce slug format and case at the storage layer, which a pre-8.0 MySQL could not do.
- Column collation `utf8mb4_0900_as_cs` is chosen deliberately so slug uniqueness and equality are case-sensitive.

**Rejected:** PostgreSQL (stronger constraints and partial indexes, but a second engine to operate for no V1 benefit); SQLite-for-dev (splits the SQL dialect surface across dev and production, which is exactly where routing bugs would hide).

---

## Decision 2 — Frontend serving model

**Chosen: Gateway proxies to product frontend servers**

The Gateway stays a pure reverse proxy. Products — or their existing web servers — own static assets and SPA history fallback.

**Consequences**
- No filesystem access in the Gateway for frontend content.
- No SPA fallback logic in Go; §22's requirement is satisfied by whatever serves the product frontend.
- `products.<P>.frontend_upstream` points at a real HTTP origin, not a directory.
- The Gateway remains stateless and container-friendly, which supports decision 7.

**Rejected:** Gateway-served static files (puts filesystem layout, cache headers, and build-artifact deployment inside an edge service that §3 says should stay small); per-product Nginx (an extra hop whose only job would be static serving, when the product frontend already exists).

---

## Decision 3 — Internal API security

**Chosen: Private bind + per-service bearer tokens, structured to be mTLS-ready**

The lifecycle API listens on a separate listener bound to a private address, and authenticates callers with per-service bearer tokens resolved from environment variables. Tokens map to a service identity, which maps to a set of products that service may manage.

"mTLS-ready" is a concrete commitment, not a hedge: the authenticator sits behind a single-method interface, so adding an mTLS implementation in V1.1 requires no change to handlers, authorization, or the API surface.

**Consequences**
- `internal_auth.mode` accepts `token` and `mtls`; `mtls` fails startup with "not implemented" rather than silently degrading to no authentication.
- All token comparisons use `crypto/subtle.ConstantTimeCompare`. A short-circuiting `==` on a bearer token is a remote timing oracle.
- Startup fails if any referenced token env var is missing or shorter than 32 characters.
- §91's "internal lifecycle APIs are authenticated" is satisfied by application logic, not solely by the network boundary.

**Rejected:** Private network only (fastest to build, but it fails §91's authentication criterion and makes a single network misconfiguration catastrophic); immediate mTLS (strongest, but certificate lifecycle work is disproportionate for a V1 with exactly two known callers).

---

## Decision 4 — Platform host behavior

**Chosen: Apex serves a static landing page; all other reserved hosts return 404**

- `zensys.lk` → minimal static landing page
- `www.zensys.lk` → 404 (it is reserved, not the apex)
- `admin.zensys.lk`, `gateway.zensys.lk`, `status.zensys.lk` → 404

**Consequences**
- The apex is matched by exact string equality against `root_domain`, never by label extraction — so `zensys` can never be interpreted as a tenant (§11).
- §65's future `admin → Super Admin` mapping stays fully open; nothing is built that would need unpicking.
- The Gateway serves exactly one static file. This is a deliberate, bounded exception to decision 2's "no static serving" rule, scoped to the apex only.

**Rejected:** All reserved hosts 404 including the apex (leaves the platform's own hostname dead, which looks broken); apex 301 to www (bakes in a canonicalization decision that may not survive contact with the marketing site); 503 for reserved hosts (signals "temporarily unavailable" for things that are simply not built).

---

## Decision 5 — Disabled tenant response

**Chosen: 404**

**Consequences**
- `DISABLED` and `unknown` produce a **byte-identical** response. The distinction exists only in logs and metrics.
- This is a genuine constraint, not a preference: if `disabled_tenant` returned a different body, it would confirm tenant existence to anyone probing hostnames, which defeats the purpose of choosing 404 in the first place.
- §91's "disabled tenants do not reach products" is satisfied at the resolver, before any upstream is contacted.

**Rejected:** 403 (confirms the hostname exists); 503 (implies retry is worthwhile and the tenant may return); custom branded page (best UX, but adds a frontend asset and content pipeline to the Gateway and requires content negotiation that a pure API client would not benefit from).

---

## Decision 6 — V1 availability

**Chosen: HA-ready design, one instance deployed**

**Consequences**
- The registry is shared and authoritative; the route cache is per-instance and explicitly non-authoritative (§57).
- No sticky sessions, no local-only state, no in-process coordination.
- A second instance requires no code change — only deployment and a shared registry, which already exists.
- **Consequence for the cache:** because two instances may hold independent caches, a lifecycle mutation invalidates only the cache of the instance that served the request. Other instances converge within `cache.route_ttl` (default 60s). This is acceptable for V1 because tenant lifecycle changes are rare and not latency-critical, but it must be understood before scaling out. If instant global invalidation is ever required, that is a new decision (pub/sub or registry-driven invalidation), not a config change.

**Rejected:** single instance with no HA accommodation (rework later for no present saving); multiple instances from day one (forces the cross-instance invalidation question to be answered now, for a system with no production tenants).

---

## Decision 7 — Deployment topology

**Chosen: Undecided — stay portable**

The Gateway is containerized, and every bind address, upstream, and trust boundary comes from configuration. No assumptions are baked in about co-location with POS or GMS.

**Consequences**
- `deploy/` ships a Dockerfile, a dev compose file, and a systemd unit — all three, because all three remain viable.
- No loopback shortcuts in config; the example config uses `127.0.0.1` for *local development only* and is explicitly marked as such.
- The internal listener's bind address is configurable, so "private" can mean loopback, a private interface, or a private network depending on the eventual answer.
- This decision **must** be revisited before production cutover (M10), because it determines the internal API's actual network boundary (§40).

---

## Decision 8 — V1 tenant model

**Chosen: One tenant slug → exactly one product**

Already implied by §12, confirmed explicitly here. `mangala → POS` and `mangala → GMS` cannot both exist in V1.

**Consequences**
- `tenant_slug` is the `PRIMARY KEY` of `tenant_routes`. Uniqueness is a storage guarantee, not application logic.
- The §64 migration conflict (`mangala` present in both POS and GMS) is a **hard stop** requiring manual resolution — never a silent rename.
- The internal API's `reserve` returns 409 on any existing slug, regardless of product.

**Revisit if:** the business ever wants one business entity to hold both a POS and a GMS presence under one hostname. That would require a `(tenant_slug, product)` composite key and a hostname scheme that disambiguates product — an architecture revision, not a V1 configuration change.

---

## Decision 9 — Tenant lifecycle authority

**Chosen: Gateway reserves, product provisions, product requests activation, Gateway activates**

Directly as specified by §34 and §62.

**Consequences**
- The Gateway owns *platform routing state* only. It never provisions a product tenant and never touches a product database (§68).
- The `PROVISIONING` state exists precisely to represent "slug reserved, product not ready" — a tenant in this state is not routable and returns 404.
- Two transitions beyond §34's linear diagram are proposed: `DISABLED → ACTIVE` (re-enable without re-provisioning) and `PROVISIONING → DISABLED` (clean provisioning failure). Both are cheap now and awkward to add once products depend on the API's refusal behaviour.

---

## Decision 10 — Slug policy

**Chosen: `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`** — §9's recommended format, with one bound corrected.

**Amendment (M2 implementation).** §9 gives the regex as `^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`, and this ADR originally adopted it verbatim. That is wrong, and the error is worth recording rather than quietly fixing.

`{1,62}` between two mandatory anchors permits 3–**64** characters. But a tenant slug *is* the leftmost DNS label of `{tenant}.zensys.lk`, and DNS labels are capped at **63** octets (RFC 1035 §2.3.4). A 64-character slug would therefore be reservable through the lifecycle API, storable, and **permanently unresolvable** — a tenant that exists in the registry and can never be reached. That is worse than an outright rejection, because nothing would report it as broken: the tenant would appear correctly provisioned, and the failure would only surface as an unexplained 404 in production.

The bound is now 63, enforced identically in three places that must agree:

| Place | Value |
|---|---|
| `internal/tenant/slug.go` — `SlugMaxLength`, `SlugPattern` | `{1,61}` |
| `migrations/0001_init.sql` — `chk_slug_format` | `{1,61}` |
| `migrations/0001_init.sql` — `tenant_slug` column | `VARCHAR(63)` |

`TestParseHost_LabelLengthBoundary` pins the boundary from both sides, and `TestMySQL_DatabaseAccepts63CharacterSlug` does the same at the storage layer.

**Consequences**
- The shortest legal slug is 3 characters (`abc`); two-character slugs such as `ab` are rejected.
- Uppercase, underscores, spaces, and leading/trailing hyphens are all invalid.
- Enforced in three places: the Go validator, the `reserve` API, and a MySQL `CHECK` constraint. Defence in depth, single definition — the regex literal lives in exactly one Go constant, and the SQL constraint mirrors it.
- Must be shared with POS and GMS so their onboarding forms validate identically (§9). The proposed `GET /internal/v1/platform-policy` endpoint exists so products can fetch the authoritative policy rather than reimplement it.
- **Do not restore `{1,62}`.** It is the documented value, but the document is wrong here.

---

## Summary

| # | Decision | Answer |
|---|---|---|
| 1 | Registry database | MySQL 8 |
| 2 | Frontend serving | Proxy to product frontend servers |
| 3 | Internal API security | Private bind + service tokens, mTLS-ready |
| 4 | Platform hosts | Apex landing page; other reserved hosts 404 |
| 5 | Disabled tenant | 404 (byte-identical to unknown) |
| 6 | V1 availability | HA-ready design, one instance |
| 7 | Deployment topology | Undecided — portable and containerized |
| 8 | Tenant model | One slug → one product |
| 9 | Lifecycle authority | Gateway reserves, product provisions, product activates |
| 10 | Slug policy | `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$` (3–63 chars; §9's `{1,62}` was a 64-char bug — see decision 10) |

## Consequences carried forward

Three items are **not** closed by this ADR and must be answered before production cutover:

1. **Deployment topology (decision 7)** determines the internal API's real network boundary (§40). Portable now, must be concrete before the internal API is exposed to real callers.
2. **Cross-instance cache invalidation (decision 6)** is a known, accepted 60-second convergence window. It becomes a blocking question the moment a second instance is deployed.
3. **Slug policy minimum length (decision 10)** requires confirmation if two-character slugs are desired — changing it later means changing the POS/GMS shared policy too.
