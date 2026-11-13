-- 0001_init.sql
--
-- Platform tenant registry. This database is the Gateway's source of truth for
-- routing only: it answers "who owns this hostname, which product should
-- receive it, and is routing enabled right now".
--
-- It deliberately contains no customer data, no sales, no jobs, no passwords,
-- no JWT secrets, and no product database credentials (architecture doc §61).
-- It is also completely separate from the POS and GMS master databases: the
-- Gateway must never need live access to a product database in order to route
-- a normal request (§29, §68).

CREATE TABLE IF NOT EXISTS tenant_routes (
  -- Case-sensitive collation is load-bearing, not incidental.
  --
  -- It makes both uniqueness and equality case-sensitive, so 'Mangala' can
  -- never collide with 'mangala' and can never silently match it. Combined
  -- with application-side lowercasing of the host, an uppercase slug that
  -- somehow reaches the database misses rather than matching the wrong
  -- tenant — it fails closed.
  tenant_slug VARCHAR(63) COLLATE utf8mb4_0900_as_cs NOT NULL,

  product     VARCHAR(16) NOT NULL,
  status      VARCHAR(16) NOT NULL,
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
                ON UPDATE CURRENT_TIMESTAMP(3),

  -- The primary key IS the "one tenant slug maps to exactly one product" rule
  -- (architecture doc §12). Global uniqueness is a storage guarantee here, not
  -- application logic that a future code path could bypass.
  PRIMARY KEY (tenant_slug),

  KEY idx_status_product (status, product),

  -- Slug format, mirroring the validator in internal/tenant/slug.go.
  -- Defence in depth: the application validates first and produces the good
  -- error message; this constraint is the backstop that makes it impossible
  -- to store a malformed slug even if a future code path forgets to check.
  --
  -- Length is 3-63, NOT the 3-64 that the architecture document's §9 regex
  -- implies. A tenant slug IS a DNS label — it is the leftmost label of
  -- {tenant}.zensys.lk — and DNS labels are capped at 63 characters. A
  -- 64-character slug would be reservable but could never resolve, creating a
  -- tenant that exists in the registry and is permanently unreachable.
  --
  -- The column width above matches this limit exactly, so an over-long slug is
  -- rejected twice over: by the declared width, and by this format check for
  -- anything that fits the width but is malformed.
  CONSTRAINT chk_slug_format
    CHECK (tenant_slug REGEXP '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'),

  -- Belt and braces alongside the case-sensitive collation above.
  CONSTRAINT chk_slug_lowercase
    CHECK (tenant_slug = LOWER(tenant_slug)),

  -- Product and status are a closed set in V1.
  --
  -- Adding a product is deliberately a migration rather than a runtime
  -- decision: which products exist is a platform-level commitment, and
  -- configuration already requires POS and GMS to be present. A free-text
  -- product column would let a typo at provisioning time create a route that
  -- no upstream configuration can ever serve.
  CONSTRAINT chk_product CHECK (product IN ('POS', 'GMS')),
  CONSTRAINT chk_status  CHECK (status IN ('PROVISIONING', 'ACTIVE', 'DISABLED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;


-- Append-only audit trail for tenant lifecycle mutations.
--
-- Every state change writes one row in the same transaction as the mutation,
-- so the trail cannot drift from reality. This exists because `release`
-- deletes a routing record outright: without an event log there is no way to
-- answer "who disabled this tenant, when, and in response to which request".
--
-- Like tenant_routes, this holds platform routing metadata only — no customer
-- data, no credentials, no PII.
CREATE TABLE IF NOT EXISTS tenant_route_events (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_slug      VARCHAR(63) COLLATE utf8mb4_0900_as_cs NOT NULL,
  action           VARCHAR(16) NOT NULL,
  from_status      VARCHAR(16) NULL,
  to_status        VARCHAR(16) NULL,
  product          VARCHAR(16) NOT NULL,

  -- Who made the change. actor_request_id ties the event back to a single
  -- request across the Gateway and the calling product's logs (§50).
  actor_service    VARCHAR(32) NOT NULL,
  actor_request_id VARCHAR(64) NOT NULL,

  occurred_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),

  PRIMARY KEY (id),
  KEY idx_events_slug_time (tenant_slug, occurred_at),
  KEY idx_events_time (occurred_at),

  CONSTRAINT chk_event_action
    CHECK (action IN ('RESERVE', 'ACTIVATE', 'DISABLE', 'RELEASE'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
