package tenant

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestRegistry returns a repository bound to a real MySQL instance, or
// skips the test when ZENSYS_TEST_DSN is not set.
//
// The schema is applied from the migration file so the test is self-contained,
// and both tables are cleared so each test starts from a known state.
func newTestRegistry(t *testing.T) (*MySQLRepository, *sql.DB) {
	t.Helper()

	raw := os.Getenv("ZENSYS_TEST_DSN")
	if strings.TrimSpace(raw) == "" {
		t.Skip("ZENSYS_TEST_DSN is not set; skipping registry integration tests")
	}

	dsn, err := NormalizeDSN(raw, WithMultiStatements())
	if err != nil {
		t.Fatalf("NormalizeDSN: %v", err)
	}

	db, err := OpenMySQL(dsn, MySQLOptions{MaxOpenConns: 8, MaxIdleConns: 4, QueryTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("OpenMySQL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("registry is not reachable at the configured DSN: %v", err)
	}

	schema, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0001_init.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	for _, table := range []string{"tenant_route_events", "tenant_routes"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}

	return NewMySQLRepository(db, MySQLOptions{QueryTimeout: 5 * time.Second}), db
}

func eventFor(slug string, action Action, from, to Status, product Product) Event {
	return Event{
		TenantSlug: slug,
		Action:     action,
		From:       from,
		To:         to,
		Product:    product,
		Actor:      Actor{Service: "pos", RequestID: "test-request-id"},
	}
}

func TestMySQL_ReserveAndGet(t *testing.T) {
	repo, _ := newTestRegistry(t)
	ctx := context.Background()

	stored, err := repo.Reserve(ctx,
		Route{TenantSlug: "garage1", Product: ProductGMS, Status: StatusProvisioning},
		eventFor("garage1", ActionReserve, "", StatusProvisioning, ProductGMS))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if stored.Status != StatusProvisioning {
		t.Errorf("status = %s, want PROVISIONING", stored.Status)
	}
	if stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
		t.Fatal("Reserve did not return database-assigned timestamps")
	}
	if stored.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at location = %s, want UTC", stored.CreatedAt.Location())
	}

	fetched, err := repo.GetBySlug(ctx, "garage1")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if fetched.Product != ProductGMS || fetched.Status != StatusProvisioning {
		t.Fatalf("fetched = %+v", fetched)
	}
}

func TestMySQL_GetBySlug_Unknown(t *testing.T) {
	repo, _ := newTestRegistry(t)

	_, err := repo.GetBySlug(context.Background(), "nosuchtenant")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBySlug(unknown) = %v, want ErrNotFound", err)
	}
}

func TestMySQL_Reserve_DuplicateSlug(t *testing.T) {
	repo, _ := newTestRegistry(t)
	ctx := context.Background()

	if _, err := repo.Reserve(ctx,
		Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusProvisioning},
		eventFor("mangala", ActionReserve, "", StatusProvisioning, ProductPOS)); err != nil {
		t.Fatalf("first Reserve: %v", err)
	}

	// Different product, same slug: §12 makes the slug globally unique, and
	// the primary key enforces it at the storage layer.
	_, err := repo.Reserve(ctx,
		Route{TenantSlug: "mangala", Product: ProductGMS, Status: StatusProvisioning},
		eventFor("mangala", ActionReserve, "", StatusProvisioning, ProductGMS))

	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate Reserve = %v, want ErrAlreadyExists", err)
	}
}

// TestMySQL_DatabaseRejectsInvalidSlugs proves the CHECK constraints are a real
// backstop, not decoration. The service validates first, so reaching the
// database with a malformed slug means a validation path was bypassed.
func TestMySQL_DatabaseRejectsInvalidSlugs(t *testing.T) {
	repo, _ := newTestRegistry(t)
	ctx := context.Background()

	tests := []struct {
		name string
		slug string
	}{
		{"uppercase", "Mangala"},
		{"leading hyphen", "-abc"},
		{"underscore", "abc_def"},
		{"two characters", "ab"},
		// 64 characters: permitted by the architecture document's §9 regex,
		// rejected here because a slug is a DNS label and labels cap at 63.
		{"64 characters", strings.Repeat("a", 64)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := repo.Reserve(ctx,
				Route{TenantSlug: tt.slug, Product: ProductPOS, Status: StatusProvisioning},
				eventFor(tt.slug, ActionReserve, "", StatusProvisioning, ProductPOS))

			if err == nil {
				t.Fatalf("the database accepted slug %q", tt.slug)
			}
			// It must not masquerade as a duplicate: that would tell the API
			// layer to return 409 instead of surfacing a validation bug.
			if errors.Is(err, ErrAlreadyExists) {
				t.Fatalf("slug %q was reported as a duplicate rather than as malformed", tt.slug)
			}
		})
	}
}

// TestMySQL_DatabaseAccepts63CharacterSlug pins the corrected boundary.
func TestMySQL_DatabaseAccepts63CharacterSlug(t *testing.T) {
	repo, _ := newTestRegistry(t)

	slug := strings.Repeat("a", SlugMaxLength)
	if _, err := repo.Reserve(context.Background(),
		Route{TenantSlug: slug, Product: ProductPOS, Status: StatusProvisioning},
		eventFor(slug, ActionReserve, "", StatusProvisioning, ProductPOS)); err != nil {
		t.Fatalf("a %d-character slug must be storable, got: %v", SlugMaxLength, err)
	}
}

// TestMySQL_ReservedHostsAreNotBlockedByTheDatabase documents a deliberate
// design choice rather than a gap that was overlooked.
//
// §10 requires one authoritative reserved-host definition. That definition
// lives in Go; duplicating 41 literals into a CHECK constraint would create a
// second source of truth that could drift. The database therefore accepts a
// reserved slug, and Service.Reserve is the guard.
//
// The practical consequence: anything writing to this table outside the
// service must check reserved hostnames itself. Today nothing does.
func TestMySQL_ReservedHostsAreNotBlockedByTheDatabase(t *testing.T) {
	repo, _ := newTestRegistry(t)
	ctx := context.Background()

	if _, err := repo.Reserve(ctx,
		Route{TenantSlug: "admin", Product: ProductPOS, Status: StatusProvisioning},
		eventFor("admin", ActionReserve, "", StatusProvisioning, ProductPOS)); err != nil {
		t.Fatalf("the database is not expected to enforce reserved hostnames, but it rejected one: %v", err)
	}

	// The service is the guard, and it rejects the same slug.
	svc := newTestService(repo)
	if _, err := svc.Reserve(ctx, "admin", ProductPOS, testActor()); !errors.Is(err, ErrReservedHost) {
		t.Fatalf("Service.Reserve(admin) = %v, want ErrReservedHost", err)
	}
}

func TestMySQL_Transition(t *testing.T) {
	repo, _ := newTestRegistry(t)
	ctx := context.Background()

	if _, err := repo.Reserve(ctx,
		Route{TenantSlug: "garage1", Product: ProductGMS, Status: StatusProvisioning},
		eventFor("garage1", ActionReserve, "", StatusProvisioning, ProductGMS)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	activated, err := repo.Transition(ctx, "garage1", StatusProvisioning, StatusActive,
		eventFor("garage1", ActionActivate, StatusProvisioning, StatusActive, ProductGMS))
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}

	if activated.Status != StatusActive {
		t.Fatalf("status = %s, want ACTIVE", activated.Status)
	}
	if !activated.UpdatedAt.After(activated.CreatedAt) && !activated.UpdatedAt.Equal(activated.CreatedAt) {
		t.Error("updated_at went backwards")
	}
}

func TestMySQL_Transition_UnknownTenant(t *testing.T) {
	repo, _ := newTestRegistry(t)

	_, err := repo.Transition(context.Background(), "nosuchtenant", StatusProvisioning, StatusActive,
		eventFor("nosuchtenant", ActionActivate, StatusProvisioning, StatusActive, ProductPOS))

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Transition(unknown) = %v, want ErrNotFound", err)
	}
}

// TestMySQL_Transition_StaleFromIsAConflict checks the compare-and-swap guard:
// a transition decided against stale state must be refused, not applied.
func TestMySQL_Transition_StaleFromIsAConflict(t *testing.T) {
	repo, _ := newTestRegistry(t)
	ctx := context.Background()

	if _, err := repo.Reserve(ctx,
		Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusProvisioning},
		eventFor("mangala", ActionReserve, "", StatusProvisioning, ProductPOS)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err := repo.Transition(ctx, "mangala", StatusProvisioning, StatusActive,
		eventFor("mangala", ActionActivate, StatusProvisioning, StatusActive, ProductPOS)); err != nil {
		t.Fatalf("first Transition: %v", err)
	}

	// A second caller that still believes the route is PROVISIONING.
	_, err := repo.Transition(ctx, "mangala", StatusProvisioning, StatusActive,
		eventFor("mangala", ActionActivate, StatusProvisioning, StatusActive, ProductPOS))

	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Transition = %v, want ErrConflict", err)
	}

	// The stored state must be untouched by the rejected attempt.
	route, err := repo.GetBySlug(ctx, "mangala")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if route.Status != StatusActive {
		t.Fatalf("status = %s, want ACTIVE", route.Status)
	}
}

// TestMySQL_Transition_ConcurrentOnlyOneWins is the concurrency test for the
// compare-and-swap. Without the "AND status = ?" clause, several callers could
// each read PROVISIONING and each write, and the last writer would silently
// win — a lost update that no single-threaded test would catch.
func TestMySQL_Transition_ConcurrentOnlyOneWins(t *testing.T) {
	repo, _ := newTestRegistry(t)
	ctx := context.Background()

	if _, err := repo.Reserve(ctx,
		Route{TenantSlug: "contended", Product: ProductPOS, Status: StatusProvisioning},
		eventFor("contended", ActionReserve, "", StatusProvisioning, ProductPOS)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	const goroutines = 8

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		conflicts int
		other     []error
	)

	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines at once

			_, err := repo.Transition(ctx, "contended", StatusProvisioning, StatusActive,
				eventFor("contended", ActionActivate, StatusProvisioning, StatusActive, ProductPOS))

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				other = append(other, err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if succeeded != 1 {
		t.Fatalf("%d transitions succeeded, want exactly 1 (the rest must conflict)", succeeded)
	}
	if conflicts != goroutines-1 {
		t.Fatalf("%d conflicts, want %d", conflicts, goroutines-1)
	}
}

func TestMySQL_Release(t *testing.T) {
	repo, db := newTestRegistry(t)
	ctx := context.Background()

	if _, err := repo.Reserve(ctx,
		Route{TenantSlug: "garage1", Product: ProductGMS, Status: StatusProvisioning},
		eventFor("garage1", ActionReserve, "", StatusProvisioning, ProductGMS)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := repo.Release(ctx, "garage1",
		eventFor("garage1", ActionRelease, StatusProvisioning, "", ProductGMS)); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if _, err := repo.GetBySlug(ctx, "garage1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBySlug after release = %v, want ErrNotFound", err)
	}

	// The audit trail outlives the routing row. This is the whole reason the
	// event table exists: Release is destructive, and the row it removes is
	// the only other record that the tenant ever existed.
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenant_route_events WHERE tenant_slug = 'garage1' AND action = 'RELEASE'`).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Fatalf("found %d RELEASE events, want 1", count)
	}
}

func TestMySQL_Release_UnknownTenant(t *testing.T) {
	repo, _ := newTestRegistry(t)

	err := repo.Release(context.Background(), "nosuchtenant",
		eventFor("nosuchtenant", ActionRelease, StatusActive, "", ProductPOS))

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Release(unknown) = %v, want ErrNotFound", err)
	}
}

// TestMySQL_EventIsWrittenWithTheMutation checks that the audit record shares
// the transaction. A rolled-back mutation must not leave an event claiming it
// happened.
func TestMySQL_EventIsWrittenWithTheMutation(t *testing.T) {
	repo, db := newTestRegistry(t)
	ctx := context.Background()

	// A reserve that fails on a duplicate must not record a second event.
	if _, err := repo.Reserve(ctx,
		Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusProvisioning},
		eventFor("mangala", ActionReserve, "", StatusProvisioning, ProductPOS)); err != nil {
		t.Fatalf("first Reserve: %v", err)
	}

	if _, err := repo.Reserve(ctx,
		Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusProvisioning},
		eventFor("mangala", ActionReserve, "", StatusProvisioning, ProductPOS)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate Reserve = %v, want ErrAlreadyExists", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenant_route_events WHERE tenant_slug = 'mangala'`).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Fatalf("recorded %d events for a single successful reserve, want 1", count)
	}
}

func TestMySQL_List(t *testing.T) {
	repo, _ := newTestRegistry(t)
	ctx := context.Background()

	seed := []struct {
		slug    string
		product Product
		status  Status
	}{
		{"garage1", ProductGMS, StatusActive},
		{"garage2", ProductGMS, StatusDisabled},
		{"mangala", ProductPOS, StatusActive},
	}

	for _, s := range seed {
		if _, err := repo.Reserve(ctx,
			Route{TenantSlug: s.slug, Product: s.product, Status: s.status},
			eventFor(s.slug, ActionReserve, "", s.status, s.product)); err != nil {
			t.Fatalf("Reserve %s: %v", s.slug, err)
		}
	}

	t.Run("all ordered by slug", func(t *testing.T) {
		routes, err := repo.List(ctx, ListFilter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(routes) != 3 {
			t.Fatalf("got %d routes, want 3", len(routes))
		}
		if routes[0].TenantSlug != "garage1" || routes[1].TenantSlug != "garage2" || routes[2].TenantSlug != "mangala" {
			t.Fatalf("not ordered by slug: %+v", routes)
		}
	})

	t.Run("by product", func(t *testing.T) {
		product := ProductGMS
		routes, err := repo.List(ctx, ListFilter{Product: &product})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(routes) != 2 {
			t.Fatalf("got %d, want 2", len(routes))
		}
	})

	t.Run("by status", func(t *testing.T) {
		status := StatusActive
		routes, err := repo.List(ctx, ListFilter{Status: &status})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(routes) != 2 {
			t.Fatalf("got %d, want 2", len(routes))
		}
	})

	t.Run("keyset pagination covers every row exactly once", func(t *testing.T) {
		var (
			seen   []string
			cursor string
		)

		for {
			page, err := repo.List(ctx, ListFilter{Limit: 1, After: cursor})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(page) == 0 {
				break
			}
			seen = append(seen, page[0].TenantSlug)
			cursor = page[0].TenantSlug

			if len(seen) > 10 {
				t.Fatal("pagination did not terminate")
			}
		}

		want := []string{"garage1", "garage2", "mangala"}
		if len(seen) != len(want) {
			t.Fatalf("paginated over %v, want %v", seen, want)
		}
		for i := range want {
			if seen[i] != want[i] {
				t.Fatalf("paginated over %v, want %v", seen, want)
			}
		}
	})

	t.Run("empty filter result", func(t *testing.T) {
		product := Product("POS")
		status := StatusDisabled
		routes, err := repo.List(ctx, ListFilter{Product: &product, Status: &status})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(routes) != 0 {
			t.Fatalf("got %d, want 0", len(routes))
		}
	})
}

func TestMySQL_Ping(t *testing.T) {
	repo, _ := newTestRegistry(t)

	if err := repo.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestMySQL_FullLifecycleThroughTheService runs the whole documented flow
// against a real database, which is the closest thing to a production path
// that exists before the proxy is wired up.
func TestMySQL_FullLifecycleThroughTheService(t *testing.T) {
	repo, _ := newTestRegistry(t)
	svc := newTestService(repo)
	ctx := context.Background()

	product := ProductGMS

	route, err := svc.Reserve(ctx, "garage1", product, testActor())
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if route.Status != StatusProvisioning {
		t.Fatalf("after reserve: %s", route.Status)
	}

	route, err = svc.Activate(ctx, "garage1", &product, testActor())
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !route.Routable() {
		t.Fatalf("after activate: %s", route.Status)
	}

	route, err = svc.Disable(ctx, "garage1", testActor())
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if route.Routable() {
		t.Fatalf("after disable: %s", route.Status)
	}

	route, err = svc.Activate(ctx, "garage1", &product, testActor())
	if err != nil {
		t.Fatalf("re-Activate: %v", err)
	}
	if !route.Routable() {
		t.Fatalf("after re-activate: %s", route.Status)
	}

	// Re-enabling must not have created a second row.
	routes, err := svc.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
}
