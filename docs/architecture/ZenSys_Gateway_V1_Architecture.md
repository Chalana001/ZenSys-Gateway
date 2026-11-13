# ZenSys Gateway — Full Architecture & Implementation Plan
## V1 — Go Runtime, Single Shared Gateway Process

> **Status:** Architecture specification with explicit open decisions.
>
> **Important:** This document does not invent deployment details that have not been decided. Items marked **TBD / OWNER DECISION** must be confirmed before implementation where they materially affect the architecture.

---

# 1. Purpose

ZenSys has multiple products that maintain their own tenancy and databases:

- ZenSys POS
- ZenSys GMS

The goal is to provide one consistent public hostname model:

```text
https://{tenant}.zensys.lk/
https://{tenant}.zensys.lk/api/...
```

The ZenSys Gateway is a new platform-level service in front of those products.

Its job is:

```text
hostname
    ↓
tenant
    ↓
product
    ↓
upstream
```

The Gateway does **not** replace POS or GMS tenancy.

The Gateway decides:

```text
tenant → product
```

POS/GMS continue deciding:

```text
tenant → database
```

---

# 2. Confirmed Architecture Principles

## 2.1 One shared Gateway process

There is **not** one Gateway process per tenant.

The model is:

```text
                    One Go Gateway Process
                             │
           ┌─────────────────┼─────────────────┐
           │                 │                 │
        Tenant A          Tenant B          Tenant C
           │                 │                 │
          POS               GMS               POS
```

All tenants use the same Gateway application/process.

The Gateway keeps tenant/product information request-scoped.

A tenant does not receive its own:

- Go process
- JVM
- server
- application instance

This is the same general shared-service model used by normal Spring Boot/Laravel applications: one running application serves many requests concurrently.

---

# 3. Why Go

Go is the selected Gateway runtime.

The motivation is:

- low runtime memory overhead;
- fast startup;
- simple deployment;
- simple HTTP/reverse-proxy implementation;
- no additional JVM;
- appropriate for a small edge service.

The Gateway should remain intentionally small.

It should not contain:

- Spring Boot
- Hibernate
- product repositories
- POS business logic
- GMS business logic
- product user management
- product database access

---

# 4. High-Level Architecture

```text
                              INTERNET
                                  │
                                  ▼
                             Cloudflare
                        DNS / TLS / Edge Security
                                  │
                                  ▼
                               Nginx
                         Edge / Proxy Layer
                                  │
                                  ▼
                     ┌────────────────────────┐
                     │      ZenSys Gateway    │
                     │         Go             │
                     │                        │
                     │ Host → Tenant          │
                     │ Tenant → Product       │
                     │ Product → Upstream     │
                     │                        │
                     │ Central route registry │
                     └────────────┬───────────┘
                                  │
                     ┌────────────┴────────────┐
                     │                         │
                     ▼                         ▼
                    POS                       GMS
                     │                         │
                     ▼                         ▼
               POS application           GMS application
                     │                         │
                     ▼                         ▼
               POS tenant DB             GMS tenant DB
```

Actual product server locations, ports, and network addresses are **TBD**.

---

# 5. Gateway Responsibility Boundary

## Gateway owns

```text
Hostname validation
Tenant extraction
Tenant slug validation
Reserved-host handling
Tenant existence
Tenant status
Tenant → product mapping
Product → upstream mapping
Reverse proxying
Gateway-level rate limiting
Gateway logging
Gateway metrics
Gateway health/readiness
Internal tenant lifecycle API
Route cache
```

## Gateway does not own

```text
User login
User passwords
JWT issuance
JWT authorization
Roles
Permissions
Orders
Customers
Vehicles
Jobs
Inventory
Payments
POS tenancy
GMS tenancy
POS database resolution
GMS database resolution
Product business rules
Product subscriptions/business rules
```

---

# 6. Two Separate Tenancy Layers

This distinction is the core design.

```text
                    public request
                         │
                         ▼
                 ZenSys Gateway
                         │
                 tenant → product
                         │
              ┌──────────┴──────────┐
              ▼                     ▼
             POS                   GMS
              │                     │
       tenant → database      tenant → database
              │                     │
              ▼                     ▼
        pos_<tenant>          gms_<tenant>
```

The Gateway must never turn:

```text
mangala → pos_mangala
```

or:

```text
garage1 → gms_garage1
```

Its routing responsibility is only:

```text
mangala → POS
garage1 → GMS
```

---

# 7. Public Hostname Contract

Canonical tenant hostname:

```text
{tenant}.zensys.lk
```

Examples:

```text
mangala.zensys.lk
garage1.zensys.lk
abc-motors.zensys.lk
```

The Gateway extracts the first label as the tenant.

Example:

```text
Host:
mangala.zensys.lk

Tenant:
mangala
```

---

# 8. Hostname Validation

The Gateway should:

1. Read the request Host.
2. Strip the port.
3. Normalize to lowercase.
4. Validate the configured root domain.
5. Reject the bare apex as a tenant.
6. Reject unsupported multi-label tenant hosts.
7. Reject reserved platform subdomains.
8. Validate the tenant slug.
9. Look up the tenant in the platform registry.
10. Check tenant status.

---

# 9. Tenant Slug Rules

Recommended platform format:

```text
^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$
```

Allowed examples:

```text
mangala
garage1
abc-motors
garage-01
```

Invalid examples:

```text
-abc
abc-
ABC
abc_def
my shop
```

The exact final slug policy must be confirmed and then shared consistently with POS and GMS.

---

# 10. Reserved Platform Hosts

These must never become customer tenants:

```text
www
admin
api
gateway
status
app
console
support
help
docs
mail
email
smtp
imap
ftp
ssh
static
assets
cdn
media
files
download
uploads
auth
login
register
account
accounts
billing
payments
dashboard
portal
dev
test
staging
demo
internal
blog
ns
ns1
ns2
```

The final reserved list should have one authoritative definition in the Gateway.

---

# 11. Apex Host

The Gateway must not interpret:

```text
zensys.lk
```

as:

```text
tenant = zensys
```

The apex is a platform host.

Its V1 behavior is **TBD / OWNER DECISION**.

Possible platform-level roles include:

```text
landing page
marketing site
registration
documentation
```

---

# 12. Platform Tenant Registry

The Gateway requires a central platform-level registry.

Logical V1 fields:

```text
tenant_slug
product
status
created_at
updated_at
```

Example:

```text
tenant_slug      product    status
----------------------------------------
mangala          POS        ACTIVE
shop123          POS        ACTIVE
garage1          GMS        ACTIVE
garage2          GMS        DISABLED
```

`tenant_slug` must be globally unique.

Therefore:

```text
mangala → POS
mangala → GMS
```

cannot both exist in V1.

---

# 13. Registry Database

**TBD / OWNER DECISION**

The Gateway requires a persistent registry database.

MySQL is a candidate because existing POS and GMS infrastructure already uses MySQL, but this document does not treat MySQL as a final decision until confirmed.

The Gateway registry database must be separate from:

```text
POS master DB
GMS master DB
```

The Gateway must not use either product's internal registry as its authoritative runtime product-routing database.

---

# 14. Registry Source of Truth

For platform routing:

```text
Gateway registry = source of truth
```

For internal product tenancy:

```text
POS registry → POS tenancy
GMS registry → GMS tenancy
```

Runtime routing must not become:

```text
Gateway → POS DB → decide whether tenant exists
```

or:

```text
Gateway → GMS DB → decide product
```

---

# 15. Product Upstream Model

The Gateway uses:

```text
product → upstream
```

not:

```text
tenant → upstream
```

Conceptually:

```text
POS → POS frontend/backend
GMS → GMS frontend/backend
```

Then:

```text
mangala → POS → POS upstream
garage1 → GMS → GMS upstream
```

This prevents tenant-specific infrastructure configuration from becoming embedded in the routing core.

---

# 16. Product Upstream Configuration

The Gateway must support at least:

```text
POS frontend upstream
POS backend upstream

GMS frontend upstream
GMS backend upstream
```

Conceptual configuration:

```yaml
products:
  POS:
    frontend_upstream: "<TBD>"
    backend_upstream: "<TBD>"

  GMS:
    frontend_upstream: "<TBD>"
    backend_upstream: "<TBD>"
```

Actual addresses and ports are **TBD**.

---

# 17. Public Request Routing

Routing has two stages.

## Stage A — Tenant/product

```text
Host
 ↓
tenant
 ↓
registry
 ↓
product
```

## Stage B — Frontend/backend

```text
Path
 ├── /api/* → backend
 └── everything else → frontend
```

---

# 18. POS Request Example

```text
https://mangala.zensys.lk/
```

resolves to:

```text
tenant = mangala
product = POS
path = /
```

Result:

```text
POS frontend
```

For:

```text
https://mangala.zensys.lk/api/orders
```

result:

```text
POS backend
```

---

# 19. GMS Request Example

```text
https://garage1.zensys.lk/
```

resolves to:

```text
tenant = garage1
product = GMS
path = /
```

Result:

```text
GMS frontend
```

For:

```text
https://garage1.zensys.lk/api/v1/jobs
```

result:

```text
GMS backend
```

The `/api/v1/jobs` path is forwarded unchanged.

---

# 20. API Prefix Contract

The platform-level edge rule is:

```text
/api/*
```

means backend traffic.

GMS already uses:

```text
/api/v1/*
```

and therefore fits this rule directly.

POS is being prepared separately so its APIs will be available under `/api/...`.

The Gateway does not need product-specific controller knowledge.

---

# 21. No Automatic API Prefix Rewrite

The Gateway should not strip `/api` by default.

Example:

```text
Incoming:
/api/v1/jobs

Forward:
/api/v1/jobs
```

Example:

```text
Incoming:
/api/orders

Forward:
/api/orders
```

This prevents the Gateway from becoming coupled to downstream route internals.

---

# 22. Frontend Routing

Non-API paths are frontend requests.

Examples:

```text
/
/dashboard
/customers/123
/jobs/456
/settings
```

These should reach the correct product frontend.

SPA history fallback must be supported by the chosen frontend serving layer.

API requests must never be returned as `index.html`.

---

# 23. Required SPA/API Distinction

This must be true:

```text
GET /dashboard
    → frontend HTML

GET /api/dashboard
    → backend JSON
```

Similarly:

```text
GET /jobs/123
    → frontend

GET /api/jobs/123
    → backend
```

---

# 24. Tenant Header Policy

The Host-derived tenant is canonical.

Example:

```http
Host: mangala.zensys.lk
X-Tenant-ID: another-shop
```

The Gateway must not allow the incoming header to override the hostname-derived tenant.

Recommended behavior:

```text
Host → mangala

remove incoming X-Tenant-ID

inject:
X-Tenant-ID: mangala
```

only where the downstream product uses that header.

---

# 25. Public Host Preservation

The Gateway should preserve the public Host when downstream tenancy uses it.

Example:

```http
Host: garage1.zensys.lk
```

must remain usable by GMS for host-bound tenant resolution.

The internal upstream hostname must not accidentally replace the tenant-bearing host where this would break tenant identity.

---

# 26. Authentication Boundary

The Gateway does not authenticate application users.

Flow:

```text
Browser
   ↓
Gateway
   ↓
POS/GMS
   ↓
product authentication
```

The Gateway does not:

```text
issue JWT
validate product JWT
load product users
check product roles
check product permissions
```

These remain downstream.

---

# 27. Login Before JWT

The Gateway must support login requests even though no JWT exists yet.

Example:

```text
POST /api/auth/login
Host: mangala.zensys.lk
```

Gateway resolves:

```text
mangala → POS
```

and forwards the request.

POS then performs:

```text
tenant resolution
user lookup
password verification
JWT creation
```

The Gateway remains unaware of the credential process.

---

# 28. JWT Forwarding

The Gateway should simply forward:

```text
Authorization: Bearer ...
```

to the selected product.

It does not need access to POS/GMS JWT signing secrets.

---

# 29. Database Security Boundary

The Gateway must never connect to:

```text
pos_<tenant>
gms_<tenant>
POS master DB
GMS master DB
```

for normal routing.

The Gateway's database contains only platform routing metadata.

---

# 30. Route Cache

The Gateway should keep route metadata in an in-memory cache.

Example:

```text
cache["mangala"]
    = POS / ACTIVE

cache["garage1"]
    = GMS / ACTIVE
```

Request flow:

```text
request
   ↓
cache
   ├── HIT  → route
   └── MISS → registry DB → cache → route
```

The cache is an optimization, not the source of truth.

---

# 31. Cache Concurrency

Because one Go process serves many tenants, tenant state must be request-scoped.

Unsafe design:

```text
global currentTenant = "mangala"
```

Safe conceptual design:

```text
Request A:
tenant = mangala
product = POS

Request B:
tenant = garage1
product = GMS
```

Both requests can execute concurrently without shared mutable tenant state.

---

# 32. Negative Cache

Unknown-tenant lookups may use a short-lived negative cache:

```text
unknown123
    ↓
not found
    ↓
short negative cache
```

The TTL must be short enough that a newly registered tenant becomes routable quickly.

Exact TTL is **TBD / implementation configuration**.

---

# 33. Cache Invalidation

Lifecycle changes should immediately invalidate the affected route.

Example:

```text
PROVISIONING → ACTIVE
```

Flow:

```text
registry update
    ↓
cache invalidation
    ↓
new request
    ↓
ACTIVE route
```

---

# 34. Tenant Lifecycle

Recommended logical lifecycle:

```text
                 reserve
                    │
                    ▼
             PROVISIONING
                    │
             product ready
                    │
                    ▼
                  ACTIVE
                    │
                    ▼
                DISABLED
```

The Gateway controls platform routing state.

The product controls its own tenant/database provisioning.

---

# 35. Reservation API

Future product onboarding needs a platform reservation operation.

Concept:

```http
POST /internal/v1/tenant-routes/reserve
```

```json
{
  "tenant": "garage1",
  "product": "GMS"
}
```

Gateway:

```text
validate slug
    ↓
check reserved hosts
    ↓
check global uniqueness
    ↓
create PROVISIONING
```

The actual POS/GMS integration is handled by their separate plans.

---

# 36. Activation API

Concept:

```http
POST /internal/v1/tenant-routes/activate
```

```json
{
  "tenant": "garage1",
  "product": "GMS"
}
```

Result:

```text
garage1 → GMS → ACTIVE
```

---

# 37. Disable API

Concept:

```http
POST /internal/v1/tenant-routes/disable
```

Result:

```text
garage1 → GMS → DISABLED
```

Public behavior for disabled tenants is **TBD / OWNER DECISION**.

---

# 38. Release API

Concept:

```http
POST /internal/v1/tenant-routes/release
```

A release should be deliberately protected.

Exact historical retention/audit behavior is **TBD**.

---

# 39. Internal API

Recommended V1 operations:

```text
POST /internal/v1/tenant-routes/reserve
POST /internal/v1/tenant-routes/activate
POST /internal/v1/tenant-routes/disable
POST /internal/v1/tenant-routes/release

GET  /internal/v1/tenant-routes/{tenant}
GET  /internal/v1/tenant-routes
```

These are platform APIs, not public customer APIs.

---

# 40. Internal Network Boundary

The internal API must not be directly reachable from the public Internet.

Possible designs:

```text
private IP
private port
VPN
Unix socket
private service network
```

The exact mechanism is **TBD / OWNER DECISION**.

---

# 41. Internal Authentication

The Gateway must authenticate callers of its lifecycle APIs.

Possible V1 mechanisms:

```text
service tokens
mTLS
private network + service tokens
```

The final choice is **TBD / OWNER DECISION**.

---

# 42. Internal Authorization

The Gateway should know which service is making a lifecycle request.

Example policy:

```text
POS service
    → may manage POS tenants

GMS service
    → may manage GMS tenants
```

A product must not be able to claim ownership of another product's tenant type unless explicitly authorized.

---

# 43. Forwarded Headers

The edge chain should control:

```text
Host
X-Forwarded-For
X-Forwarded-Host
X-Forwarded-Proto
X-Request-ID
```

Untrusted client-provided forwarded headers must not be blindly trusted.

---

# 44. Product Host Expectations

Because GMS uses host-based tenant resolution, the Gateway must preserve the tenant-bearing host correctly.

Because POS uses `X-Tenant-ID` plus host fallback and JWT tenant validation, the Gateway must provide the canonical tenant consistently.

The Gateway therefore adapts to product tenancy; it does not replace it.

---

# 45. Unknown Tenant Behavior

Example:

```text
random.zensys.lk
```

with no registry record:

```text
404
```

No default product.

No default tenant.

No legacy fallback.

---

# 46. Registry Failure

For an uncached tenant:

```text
registry unavailable
        ↓
503
```

The Gateway must not guess.

---

# 47. Upstream Failure

If:

```text
tenant → POS
```

and POS is unavailable:

```text
502/503
```

The request must not be redirected to GMS.

The same applies in reverse.

---

# 48. No Cross-Product Fallback

Never:

```text
POS unavailable → GMS
```

Never:

```text
GMS unavailable → POS
```

Never:

```text
unknown tenant → default product
```

Product ownership must be explicit.

---

# 49. Health Endpoints

Recommended:

```text
GET /healthz
```

Checks process health.

```text
GET /readyz
```

Checks:

```text
configuration valid
registry reachable
required upstream configuration available
```

It should not require checking every tenant.

---

# 50. Request ID

Every request should have a request identifier.

Behavior:

```text
incoming request
    ↓
Gateway generates/accepts trusted request ID
    ↓
forward request ID downstream
    ↓
log request ID
```

This allows a single request to be traced through:

```text
Gateway
POS/GMS
```

---

# 51. Structured Logging

Example:

```json
{
  "request_id": "abc123",
  "tenant": "mangala",
  "product": "POS",
  "method": "POST",
  "path": "/api/orders",
  "status": 200,
  "duration_ms": 42
}
```

Never log:

```text
password
JWT
refresh token
service secret
database password
database URL
```

---

# 52. Metrics

Recommended metrics:

```text
gateway_requests_total
gateway_request_duration
gateway_registry_lookup_total
gateway_registry_errors_total
gateway_unknown_tenant_total
gateway_disabled_tenant_total
gateway_upstream_errors_total
```

Avoid uncontrolled high-cardinality labels such as arbitrary full request URLs.

---

# 53. Public Backend Exposure

Preferred final network boundary:

```text
Internet
    ↓
Cloudflare
    ↓
Nginx
    ↓
Gateway
    ↓
private product services
```

Product backend ports should not normally be directly exposed to the Internet.

Exact firewall/network implementation is **TBD**.

---

# 54. Frontend Serving Model

The Gateway requires a destination for product frontend traffic.

Possible models:

```text
Gateway → existing frontend web server
Gateway → Nginx/static server
Gateway serves static files
Gateway → product frontend service
```

The exact model is **TBD / OWNER DECISION**.

The logical contract remains:

```text
tenant → product → frontend destination
```

---

# 55. Backend Serving Model

Likewise, each product needs an upstream backend destination.

Logical configuration:

```text
POS → POS backend
GMS → GMS backend
```

Actual locations and ports are **TBD**.

---

# 56. Single-Process Memory Model

The Gateway should operate as:

```text
1 Go process
```

serving many tenants.

Example:

```text
             Go Gateway
                  │
       ┌──────────┼──────────┐
       │          │          │
   Request A  Request B  Request C
   mangala    garage1    shop123
       │          │          │
      POS        GMS        POS
```

There is no:

```text
1 process per tenant
1 JVM per tenant
1 Go server per tenant
```

This is a shared application/server model.

---

# 57. Statelessness

The authoritative tenant/product state lives in the registry.

The Gateway should not depend on local process memory for correctness.

Local memory may contain:

```text
route cache
metrics
temporary request context
```

but not the only copy of tenant ownership.

This enables future horizontal scaling.

---

# 58. Horizontal Scaling

V1 may use one Gateway process.

The architecture should still allow:

```text
Gateway A
Gateway B
Gateway C
```

later.

All instances use:

```text
same platform registry
```

Each instance may have its own local route cache.

---

# 59. High Availability

**TBD / OWNER DECISION**

Possible V1 choices:

```text
A. one Gateway instance
B. multiple Gateway instances
C. design for HA, deploy one initially
```

The route registry must remain shared regardless.

---

# 60. Security Model

The trust boundary is:

```text
Internet
   ↓
Cloudflare
   ↓
Nginx
   ↓
Gateway
   ↓
private product services
```

The Gateway is the platform routing authority.

POS/GMS remain application security authorities.

---

# 61. Data Model Rule

Gateway records should contain only information needed to answer:

```text
Who owns this tenant hostname?
Which product should receive it?
Is routing currently enabled?
```

Gateway should not store:

```text
customer data
sales
jobs
passwords
JWT secrets
product DB credentials
```

---

# 62. Tenant Registration Flow

Future complete flow:

```text
New business
      ↓
Request tenant slug
      ↓
Gateway reserves slug
      ↓
PROVISIONING
      ↓
POS/GMS provisions its own tenant
      ↓
product confirms ready
      ↓
Gateway activates route
      ↓
tenant.zensys.lk becomes ACTIVE
```

The exact product-side API calls belong to the separate POS/GMS plans.

---

# 63. Existing Tenant Migration

Initial migration should:

1. Enumerate existing POS tenants.
2. Enumerate existing GMS tenants.
3. Normalize/validate slugs.
4. Check reserved-name conflicts.
5. Detect duplicates across products.
6. Insert platform registry records.
7. Activate only verified tenants.

Do not copy product database credentials into the Gateway.

---

# 64. Duplicate Slug Handling

If migration discovers:

```text
POS:
mangala

GMS:
mangala
```

the migration must stop/report the conflict.

It must not silently rename a business.

Example error:

```text
Duplicate global tenant slug: mangala
Products: POS, GMS
```

The conflict must be manually resolved before activation.

---

# 65. Platform Host Handling

Reserved hosts should be processed before tenant lookup.

Example:

```text
admin.zensys.lk
```

should not become:

```text
tenant = admin
```

Possible future:

```text
admin → Super Admin
```

Exact V1 host-to-application mapping remains **TBD / OWNER DECISION**.

---

# 66. POS Interaction Contract

The Gateway implementation must assume only:

```text
Host contains tenant
X-Tenant-ID can carry canonical tenant
/api/* is backend traffic
POS has its own tenant/database resolution
```

The POS code changes required to support `/api/...` are handled separately.

---

# 67. GMS Interaction Contract

The Gateway implementation must assume:

```text
Host contains tenant
/api/v1/* is backend traffic
GMS has its own host-bound tenant/database resolution
```

GMS code/config changes are handled separately.

---

# 68. No Direct Product Registry Dependency

The Gateway should not require live access to:

```text
POS master database
GMS master database
```

just to route a normal request.

Its runtime routing dependency is:

```text
Gateway registry
```

The product registries remain independent.

---

# 69. Error Contract

Gateway errors should be platform-level and intentionally generic.

Examples:

```text
404 unknown tenant
403/404 disabled tenant (final choice TBD)
503 registry unavailable
502/503 upstream unavailable
```

Do not expose internal:

```text
database errors
JDBC URLs
stack traces
service credentials
internal network addresses
```

---

# 70. Gateway Repository Structure

Recommended structure:

```text
zensys-gateway/
│
├── cmd/
│   └── gateway/
│       └── main.go
│
├── internal/
│   ├── config/
│   │
│   ├── tenant/
│   │   ├── model.go
│   │   ├── repository.go
│   │   ├── service.go
│   │   └── cache.go
│   │
│   ├── routing/
│   │   ├── resolver.go
│   │   ├── proxy.go
│   │   └── headers.go
│   │
│   ├── security/
│   │   ├── internal_auth.go
│   │   └── rate_limit.go
│   │
│   ├── http/
│   │   ├── handlers.go
│   │   ├── middleware.go
│   │   └── responses.go
│   │
│   └── observability/
│       ├── logging.go
│       └── metrics.go
│
├── migrations/
├── configs/
├── deploy/
├── tests/
├── go.mod
└── README.md
```

Exact Go libraries/frameworks are intentionally not prescribed by this architecture document.

---

# 71. Implementation Plan

## Phase 0 — Owner Decisions

Before production implementation, decide:

1. Registry database engine.
2. Gateway hosting location.
3. POS upstream location.
4. GMS upstream location.
5. Frontend serving model.
6. Internal API network boundary.
7. Internal authentication mechanism.
8. V1 HA/single-instance approach.
9. Platform-host behavior.
10. Disabled-tenant response.
11. Exact slug policy.
12. Tenant lifecycle authority.

Do not guess these.

---

# 72. Phase 1 — Repository Bootstrap

Create the Go project.

Implement:

```text
project structure
configuration loader
configuration validation
logging
health endpoint
readiness endpoint
```

No POS/GMS code changes.

No product database access.

---

# 73. Phase 2 — Registry Layer

Implement:

```text
registry database connection
tenant route model
repository
service
migrations
```

Operations:

```text
GetBySlug
List
Reserve
Activate
Disable
Release
```

Tests:

```text
unknown tenant
duplicate slug
invalid slug
invalid product
invalid status
state transitions
```

---

# 74. Phase 3 — Host Resolver

Implement:

```text
parseHost
validateRootDomain
extractTenant
validateSlug
reservedHostCheck
```

Tests:

```text
tenant.zensys.lk
uppercase
port
apex
multi-label
reserved host
wrong domain
invalid slug
```

---

# 75. Phase 4 — Product Resolver

Implement:

```text
tenant → registry → product
```

Tests:

```text
POS tenant
GMS tenant
unknown tenant
disabled tenant
registry failure
```

---

# 76. Phase 5 — Route Cache

Implement:

```text
cache get
cache set
cache invalidation
negative cache
TTL
```

Tests must prove concurrent tenants remain isolated in cache state.

---

# 77. Phase 6 — Request Context

Implement request-scoped state:

```text
request_id
host
tenant
product
path
routing result
```

Never store current tenant in process-global mutable state.

---

# 78. Phase 7 — Header Middleware

Implement:

```text
canonical tenant header
request ID
forwarded-header policy
```

Tests:

```text
correct header
conflicting header
missing header
untrusted forwarded host
```

---

# 79. Phase 8 — Reverse Proxy

Implement:

```text
backend proxy
frontend proxy
product upstream selection
```

Tests:

```text
POS /
POS /api/*
GMS /
GMS /api/*
unknown tenant
upstream unavailable
```

---

# 80. Phase 9 — Internal Lifecycle API

Implement:

```text
reserve
activate
disable
release
get
list
```

Protect it using the selected internal network/authentication mechanism.

Every state mutation should invalidate the relevant cache entry.

---

# 81. Phase 10 — Security Hardening

Implement and test:

```text
reserved hosts
slug validation
host canonicalization
tenant-header spoof protection
internal API authentication
internal authorization
rate limiting
safe error responses
request size limits
```

Do not implement product JWT validation.

---

# 82. Phase 11 — Observability

Implement:

```text
structured logging
request IDs
metrics
health
readiness
routing failure metrics
registry failure metrics
upstream failure metrics
```

---

# 83. Phase 12 — Mock Integration Environment

Before connecting to real POS/GMS, run the Gateway against:

```text
mock POS frontend
mock POS backend
mock GMS frontend
mock GMS backend
test registry
```

Example:

```text
mangala.zensys.lk → mock POS
garage1.zensys.lk → mock GMS
```

Prove the platform routing contract first.

---

# 84. Phase 13 — Real Product Integration

Connect:

```text
Gateway
   ↓
real POS
real GMS
```

Do not modify POS/GMS from the Gateway repository.

Their readiness changes are separate.

---

# 85. Phase 14 — Edge Infrastructure

Configure the decided edge:

```text
Cloudflare
    ↓
Nginx
    ↓
Gateway
```

Verify:

```text
DNS
TLS
Host
X-Forwarded-Host
X-Forwarded-For
X-Forwarded-Proto
```

---

# 86. Phase 15 — Existing Tenant Import

Import:

```text
POS tenant slugs → POS
GMS tenant slugs → GMS
```

into the platform registry.

Only import:

```text
tenant
product
status
```

Never import product DB credentials.

---

# 87. Phase 16 — Pilot

Pilot one POS tenant and one GMS tenant.

Test:

```text
homepage
API
login
JWT
tenant isolation
database routing
SPA deep link
printing
reports
logout
refresh
unknown tenant
disabled tenant
```

---

# 88. Phase 17 — Production Rollout

Roll tenants in batches.

Per tenant:

```text
registry ready
DNS ready
TLS ready
frontend ready
backend ready
routing verified
tenant DB verified
authentication verified
```

Keep rollback available during the migration period.

---

# 89. Phase 18 — Cleanup

After stable production rollout:

```text
remove migration-only routes
remove temporary compatibility configuration
tighten firewall rules
remove obsolete direct backend exposure
finalize documentation
```

POS and GMS route cleanup remains in their own plans.

---

# 90. Gateway Testing Matrix

| Test | Expected |
|---|---|
| Active POS tenant `/` | POS frontend |
| Active POS tenant `/api/...` | POS backend |
| Active GMS tenant `/` | GMS frontend |
| Active GMS tenant `/api/v1/...` | GMS backend |
| Unknown tenant | 404 |
| Disabled tenant | controlled policy |
| Bare apex | platform behavior |
| Reserved host | platform behavior |
| Conflicting `X-Tenant-ID` | Host-derived identity wins |
| Registry unavailable | 503 for uncached route |
| POS unavailable | 502/503 |
| GMS unavailable | 502/503 |
| Public access to internal API | blocked |
| Duplicate tenant reservation | rejected |
| Cache invalidation | immediate route refresh |

---

# 91. Gateway Security Acceptance Criteria

V1 must satisfy:

- [ ] Client cannot override Host-derived tenant.
- [ ] Unknown tenants fail closed.
- [ ] Disabled tenants do not reach products.
- [ ] Product failures do not fall through to another product.
- [ ] Gateway has no product DB credentials.
- [ ] Gateway does not authenticate product users.
- [ ] Gateway does not issue product JWTs.
- [ ] Internal lifecycle APIs are private.
- [ ] Internal lifecycle APIs are authenticated.
- [ ] Internal lifecycle APIs enforce product authorization.
- [ ] Reserved hosts cannot become tenants.
- [ ] Slug rules are enforced.
- [ ] Forwarded headers are trusted only from the intended proxy chain.

---

# 92. Gateway Performance Acceptance Criteria

V1 should:

- run as one shared process;
- handle many tenants concurrently;
- use in-memory route caching;
- avoid product DB lookups for routing;
- avoid per-tenant processes;
- avoid per-tenant JVMs;
- keep the persistent registry separate from product databases;
- remain correct if the local route cache is empty.

Exact target numbers are **TBD / OWNER DECISION**.

---

# 93. Architecture Invariants

These must remain true throughout implementation:

```text
1. One shared Go Gateway process can serve many tenants.

2. A tenant does not get its own Gateway process.

3. Hostname is the canonical public tenant identity.

4. Gateway resolves:
       tenant → product

5. POS/GMS resolve:
       tenant → database

6. Gateway does not authenticate product users.

7. Gateway does not issue product JWTs.

8. Gateway does not access product business databases.

9. /api/* is backend traffic.

10. Non-/api traffic is frontend traffic.

11. Unknown tenants fail closed.

12. Product failures do not trigger cross-product fallback.

13. Client X-Tenant-ID cannot override Host-derived tenant.

14. Platform registry is the cross-product routing source of truth.

15. Route cache is an optimization, not authoritative state.

16. Tenant state is request-scoped; no global mutable currentTenant.

17. Product upstreams are product-level configuration.

18. POS/GMS tenancy remains independent.

19. Database credentials never enter the Gateway.

20. POS/GMS implementation changes are separate projects.
```

---

# 94. Open Decisions — Must Be Answered

The architecture is intentionally not considered implementation-final until these are answered:

### 1. Registry database

```text
A. MySQL
B. PostgreSQL
C. Other
```

### 2. Gateway hosting

```text
A. Same VPS as POS
B. Same VPS as GMS
C. Dedicated VPS
D. Other
```

### 3. Product hosting

```text
A. POS and GMS on same server
B. Different servers
C. Not decided
```

### 4. Frontend serving

```text
A. Gateway proxies to frontend servers
B. Gateway serves static frontend files
C. Gateway proxies through Nginx/frontend servers
D. Other
```

### 5. Internal API security

```text
A. Private network + service tokens
B. mTLS
C. Private network only
D. Other
```

### 6. V1 availability

```text
A. Single Gateway instance
B. Multiple Gateway instances
C. HA-ready design, one instance initially
```

### 7. Platform host behavior

Define the V1 behavior of:

```text
zensys.lk
www.zensys.lk
admin.zensys.lk
gateway.zensys.lk
status.zensys.lk
```

### 8. V1 tenant model

Confirm:

```text
one tenant slug → one product
```

or:

```text
one tenant slug → multiple products
```

### 9. Disabled tenant response

Choose the final V1 behavior:

```text
403
404
503
custom platform page
```

### 10. Tenant lifecycle authority

Confirm whether the intended flow is:

```text
Gateway reserves slug
    ↓
product provisions tenant
    ↓
product requests activation
    ↓
Gateway activates route
```

---

# 95. Final Architecture Summary

```text
                              INTERNET
                                  │
                                  ▼
                             Cloudflare
                                  │
                                  ▼
                               Nginx
                                  │
                                  ▼
                    ┌───────────────────────────┐
                    │        Go Gateway          │
                    │                           │
                    │ Host → Tenant             │
                    │ Tenant → Product          │
                    │ Product → Upstream        │
                    │                           │
                    │ Shared process            │
                    │ Request-scoped state      │
                    │ Route cache               │
                    │ Central registry client   │
                    │ Reverse proxy             │
                    │ Internal lifecycle API    │
                    └────────────┬──────────────┘
                                 │
                     ┌───────────┴───────────┐
                     │                       │
                     ▼                       ▼
                    POS                     GMS
                     │                       │
               existing tenancy        existing tenancy
                     │                       │
                     ▼                       ▼
                pos_<tenant>            gms_<tenant>
```

---

# 96. Final Mental Model

For:

```text
https://mangala.zensys.lk/api/orders
```

the Gateway performs:

```text
Host
  ↓
mangala
  ↓
central registry
  ↓
POS
  ↓
POS backend
```

POS then performs:

```text
tenant
  ↓
TenantContext
  ↓
TenantDatabaseRegistry
  ↓
pos_mangala
```

For:

```text
https://garage1.zensys.lk/api/v1/jobs
```

the Gateway performs:

```text
Host
  ↓
garage1
  ↓
central registry
  ↓
GMS
  ↓
GMS backend
```

GMS then performs:

```text
tenant
  ↓
TenantContext
  ↓
TenantRoutingDataSource
  ↓
gms_garage1
```

The Gateway therefore changes:

```text
where the request goes
```

while POS/GMS continue deciding:

```text
which tenant database the request belongs to
```

That separation is the permanent architectural boundary.
