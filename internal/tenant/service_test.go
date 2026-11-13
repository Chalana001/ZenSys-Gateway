package tenant

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testActor() Actor {
	return Actor{Service: "pos", RequestID: "req-abc123"}
}

func newTestService(repo Repository) *Service {
	return NewService(repo, testLogger())
}

// --- Reserve ---

func TestReserve_CreatesProvisioningRoute(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo)

	route, err := svc.Reserve(context.Background(), "garage1", ProductGMS, testActor())
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// A reserved slug must not be routable until the product confirms its own
	// tenant is ready and activation is requested (§34, §62).
	if route.Status != StatusProvisioning {
		t.Fatalf("status = %s, want PROVISIONING", route.Status)
	}
	if route.Routable() {
		t.Fatal("a freshly reserved route must not be routable")
	}
	if route.Product != ProductGMS {
		t.Fatalf("product = %s, want GMS", route.Product)
	}
	if route.CreatedAt.IsZero() || route.UpdatedAt.IsZero() {
		t.Fatal("the stored route should carry database-assigned timestamps")
	}

	events := repo.recordedEvents()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].Action != ActionReserve {
		t.Errorf("event action = %s, want RESERVE", events[0].Action)
	}
	if events[0].From != "" {
		t.Errorf("a RESERVE event should have no previous status, got %q", events[0].From)
	}
	if events[0].To != StatusProvisioning {
		t.Errorf("event to_status = %s, want PROVISIONING", events[0].To)
	}
	if events[0].Actor != testActor() {
		t.Errorf("event actor = %+v, want %+v", events[0].Actor, testActor())
	}
}

// TestReserve_TrimsWhitespaceButRejectsCaseChange pins the boundary between
// input hygiene and a semantic change.
//
// Trimming is hygiene. Lowercasing is not: a product that sends "Garage1" has
// a bug in its own slug handling, and silently storing "garage1" would leave
// the product and the Gateway disagreeing about what the tenant is called.
func TestReserve_TrimsWhitespaceButRejectsCaseChange(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo)

	route, err := svc.Reserve(context.Background(), "  garage1  ", ProductGMS, testActor())
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if route.TenantSlug != "garage1" {
		t.Fatalf("stored slug = %q, want garage1", route.TenantSlug)
	}

	if _, err := svc.Reserve(context.Background(), "  Garage2  ", ProductGMS, testActor()); !errors.Is(err, ErrInvalidSlug) {
		t.Fatalf("Reserve(Garage2) = %v, want ErrInvalidSlug", err)
	}
	if repo.exists("garage2") {
		t.Fatal("a case-variant slug was stored")
	}
}

func TestReserve_RejectsDuplicate(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo)

	if _, err := svc.Reserve(context.Background(), "mangala", ProductPOS, testActor()); err != nil {
		t.Fatalf("first Reserve: %v", err)
	}

	_, err := svc.Reserve(context.Background(), "mangala", ProductPOS, testActor())
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Reserve error = %v, want ErrAlreadyExists", err)
	}
}

// TestReserve_RejectsCrossProductDuplicate covers §12: tenant_slug is globally
// unique, so the same slug cannot be claimed by both products.
func TestReserve_RejectsCrossProductDuplicate(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo)

	if _, err := svc.Reserve(context.Background(), "mangala", ProductPOS, testActor()); err != nil {
		t.Fatalf("POS Reserve: %v", err)
	}

	_, err := svc.Reserve(context.Background(), "mangala", ProductGMS, testActor())
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("GMS Reserve of the same slug = %v, want ErrAlreadyExists", err)
	}
}

// TestReserve_RejectsReservedHosts is the §91 criterion "reserved hosts cannot
// become tenants", enforced at the service layer.
func TestReserve_RejectsReservedHosts(t *testing.T) {
	for _, reserved := range []string{"admin", "api", "www", "gateway", "status"} {
		t.Run(reserved, func(t *testing.T) {
			repo := newFakeRepository()
			svc := newTestService(repo)

			_, err := svc.Reserve(context.Background(), reserved, ProductPOS, testActor())
			if !errors.Is(err, ErrReservedHost) {
				t.Fatalf("Reserve(%q) error = %v, want ErrReservedHost", reserved, err)
			}
			if repo.exists(reserved) {
				t.Fatalf("%q was stored despite being reserved", reserved)
			}
		})
	}
}

func TestReserve_RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		slug    string
		product Product
		wantErr error
	}{
		{"uppercase", "MANGALA", ProductPOS, ErrInvalidSlug},
		{"too short", "ab", ProductPOS, ErrInvalidSlug},
		{"leading hyphen", "-abc", ProductPOS, ErrInvalidSlug},
		{"underscore", "abc_def", ProductPOS, ErrInvalidSlug},
		{"empty slug", "", ProductPOS, ErrInvalidSlug},
		{"unknown product", "mangala", "ERP", ErrInvalidProduct},
		{"lowercase product", "mangala", "pos", ErrInvalidProduct},
		{"empty product", "mangala", "", ErrInvalidProduct},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeRepository()
			svc := newTestService(repo)

			_, err := svc.Reserve(context.Background(), tt.slug, tt.product, testActor())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Reserve(%q, %q) error = %v, want %v", tt.slug, tt.product, err, tt.wantErr)
			}
			if repo.reserveCalls != 0 {
				t.Fatal("invalid input reached the repository")
			}
		})
	}
}

// --- Activate ---

func TestActivate_FromProvisioning(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo)

	if _, err := svc.Reserve(context.Background(), "garage1", ProductGMS, testActor()); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	product := ProductGMS
	route, err := svc.Activate(context.Background(), "garage1", &product, testActor())
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if route.Status != StatusActive {
		t.Fatalf("status = %s, want ACTIVE", route.Status)
	}
	if !route.Routable() {
		t.Fatal("an ACTIVE route must be routable")
	}

	events := repo.recordedEvents()
	if len(events) != 2 {
		t.Fatalf("recorded %d events, want 2 (RESERVE then ACTIVATE)", len(events))
	}
	activation := events[1]
	if activation.Action != ActionActivate || activation.From != StatusProvisioning || activation.To != StatusActive {
		t.Fatalf("activation event = %+v, want ACTIVATE PROVISIONING -> ACTIVE", activation)
	}
}

func TestActivate_FromDisabled(t *testing.T) {
	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "garage1", Product: ProductGMS, Status: StatusDisabled})
	svc := newTestService(repo)

	route, err := svc.Activate(context.Background(), "garage1", nil, testActor())
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if route.Status != StatusActive {
		t.Fatalf("status = %s, want ACTIVE", route.Status)
	}
}

// TestActivate_AlreadyActiveIsANoOp checks idempotency: a provisioning job that
// retries after a lost response must not have to special-case this.
func TestActivate_AlreadyActiveIsANoOp(t *testing.T) {
	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusActive})
	svc := newTestService(repo)

	route, err := svc.Activate(context.Background(), "mangala", nil, testActor())
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if route.Status != StatusActive {
		t.Fatalf("status = %s, want ACTIVE", route.Status)
	}

	if repo.transitionCalls != 0 {
		t.Fatal("an already-ACTIVE route should not have been written")
	}
	if len(repo.recordedEvents()) != 0 {
		t.Fatal("a no-op activation should not be recorded as an event")
	}
}

// TestActivate_RejectsProductMismatch covers §42: a product must not be able to
// activate a tenant belonging to another product by guessing the slug.
func TestActivate_RejectsProductMismatch(t *testing.T) {
	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusProvisioning})
	svc := newTestService(repo)

	wrongProduct := ProductGMS
	_, err := svc.Activate(context.Background(), "mangala", &wrongProduct, testActor())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Activate with the wrong product = %v, want ErrConflict", err)
	}
	if repo.statusOf("mangala") != StatusProvisioning {
		t.Fatal("a rejected activation must not change the stored status")
	}
	if repo.transitionCalls != 0 {
		t.Fatal("a rejected activation reached the repository")
	}
}

func TestActivate_UnknownTenant(t *testing.T) {
	svc := newTestService(newFakeRepository())

	_, err := svc.Activate(context.Background(), "nosuchtenant", nil, testActor())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Activate(unknown) = %v, want ErrNotFound", err)
	}
}

func TestActivate_InvalidSlugRejectedBeforeLookup(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo)

	if _, err := svc.Activate(context.Background(), "AB", nil, testActor()); !errors.Is(err, ErrInvalidSlug) {
		t.Fatalf("Activate(AB) = %v, want ErrInvalidSlug", err)
	}
}

// --- Disable ---

func TestDisable_FromActive(t *testing.T) {
	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusActive})
	svc := newTestService(repo)

	route, err := svc.Disable(context.Background(), "mangala", testActor())
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if route.Status != StatusDisabled {
		t.Fatalf("status = %s, want DISABLED", route.Status)
	}
	if route.Routable() {
		t.Fatal("a DISABLED route must not be routable")
	}

	events := repo.recordedEvents()
	if len(events) != 1 || events[0].Action != ActionDisable {
		t.Fatalf("expected one DISABLE event, got %+v", events)
	}
	if events[0].From != StatusActive || events[0].To != StatusDisabled {
		t.Fatalf("disable event = %+v, want ACTIVE -> DISABLED", events[0])
	}
}

func TestDisable_AlreadyDisabledIsANoOp(t *testing.T) {
	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusDisabled})
	svc := newTestService(repo)

	if _, err := svc.Disable(context.Background(), "mangala", testActor()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if repo.transitionCalls != 0 {
		t.Fatal("an already-DISABLED route should not have been written")
	}
}

func TestDisable_FromProvisioning(t *testing.T) {
	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "garage1", Product: ProductGMS, Status: StatusProvisioning})
	svc := newTestService(repo)

	route, err := svc.Disable(context.Background(), "garage1", testActor())
	if err != nil {
		t.Fatalf("Disable from PROVISIONING: %v", err)
	}
	if route.Status != StatusDisabled {
		t.Fatalf("status = %s, want DISABLED", route.Status)
	}
}

func TestDisable_UnknownTenant(t *testing.T) {
	svc := newTestService(newFakeRepository())

	if _, err := svc.Disable(context.Background(), "nosuchtenant", testActor()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Disable(unknown) = %v, want ErrNotFound", err)
	}
}

// --- Release ---

func TestRelease_DeletesRouteAndRecordsEvent(t *testing.T) {
	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "garage1", Product: ProductGMS, Status: StatusDisabled})
	svc := newTestService(repo)

	released, err := svc.Release(context.Background(), "garage1", testActor())
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if released.Status != StatusDisabled {
		t.Fatalf("returned status = %s, want the pre-release status", released.Status)
	}
	if repo.exists("garage1") {
		t.Fatal("the route still exists after release")
	}

	events := repo.recordedEvents()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].Action != ActionRelease {
		t.Errorf("event action = %s, want RELEASE", events[0].Action)
	}
	// The row is gone, so the event is the only remaining record of it.
	if events[0].TenantSlug != "garage1" || events[0].Product != ProductGMS {
		t.Errorf("release event lost identifying detail: %+v", events[0])
	}
	if events[0].To != "" {
		t.Errorf("a RELEASE event should have no resulting status, got %q", events[0].To)
	}
}

// TestRelease_ActiveTenantWarns checks that taking a live site offline is
// allowed but never silent.
func TestRelease_ActiveTenantWarns(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusActive})
	svc := NewService(repo, logger)

	if _, err := svc.Release(context.Background(), "mangala", testActor()); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if !strings.Contains(logs.String(), "releasing an ACTIVE tenant") {
		t.Fatalf("releasing an active tenant did not warn:\n%s", logs.String())
	}
}

func TestRelease_UnknownTenant(t *testing.T) {
	svc := newTestService(newFakeRepository())

	if _, err := svc.Release(context.Background(), "nosuchtenant", testActor()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Release(unknown) = %v, want ErrNotFound", err)
	}
}

// --- Get / List ---

// TestGet_ReservedSlugRemainsReachable guards the deliberate asymmetry between
// ValidateSlug and ValidateSlugFormat. If lookups used the stricter validator,
// a reserved slug that somehow reached the registry could never be disabled or
// released.
func TestGet_ReservedSlugRemainsReachable(t *testing.T) {
	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "admin", Product: ProductPOS, Status: StatusDisabled})
	svc := newTestService(repo)

	route, err := svc.Get(context.Background(), "admin")
	if err != nil {
		t.Fatalf("Get(admin) = %v, want the row so it can be cleaned up", err)
	}
	if route.TenantSlug != "admin" {
		t.Fatalf("got %q", route.TenantSlug)
	}
}

func TestGet_TrimsAndRejectsMalformed(t *testing.T) {
	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusActive})
	svc := newTestService(repo)

	route, err := svc.Get(context.Background(), "  mangala  ")
	if err != nil {
		t.Fatalf("Get should trim before lookup: %v", err)
	}
	if route.TenantSlug != "mangala" {
		t.Fatalf("got %q", route.TenantSlug)
	}

	if _, err := svc.Get(context.Background(), "MANGALA"); !errors.Is(err, ErrInvalidSlug) {
		t.Fatalf("Get(MANGALA) = %v, want ErrInvalidSlug", err)
	}
	if _, err := svc.Get(context.Background(), "ab"); !errors.Is(err, ErrInvalidSlug) {
		t.Fatalf("Get(ab) = %v, want ErrInvalidSlug", err)
	}
}

func TestList_Filters(t *testing.T) {
	repo := newFakeRepository()
	repo.seed(Route{TenantSlug: "garage1", Product: ProductGMS, Status: StatusActive})
	repo.seed(Route{TenantSlug: "garage2", Product: ProductGMS, Status: StatusDisabled})
	repo.seed(Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusActive})
	svc := newTestService(repo)

	ctx := context.Background()

	t.Run("all", func(t *testing.T) {
		routes, err := svc.List(ctx, ListFilter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(routes) != 3 {
			t.Fatalf("got %d routes, want 3", len(routes))
		}
		// Ordered by slug, which is the primary key and therefore the keyset
		// pagination order.
		if routes[0].TenantSlug != "garage1" || routes[2].TenantSlug != "mangala" {
			t.Fatalf("routes are not ordered by slug: %+v", routes)
		}
	})

	t.Run("by product", func(t *testing.T) {
		product := ProductGMS
		routes, err := svc.List(ctx, ListFilter{Product: &product})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(routes) != 2 {
			t.Fatalf("got %d GMS routes, want 2", len(routes))
		}
	})

	t.Run("by status", func(t *testing.T) {
		status := StatusActive
		routes, err := svc.List(ctx, ListFilter{Status: &status})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(routes) != 2 {
			t.Fatalf("got %d ACTIVE routes, want 2", len(routes))
		}
	})

	t.Run("keyset pagination", func(t *testing.T) {
		page, err := svc.List(ctx, ListFilter{Limit: 2})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page) != 2 {
			t.Fatalf("got %d routes on page 1, want 2", len(page))
		}

		next, err := svc.List(ctx, ListFilter{Limit: 2, After: page[len(page)-1].TenantSlug})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(next) != 1 {
			t.Fatalf("got %d routes on page 2, want 1", len(next))
		}
		if next[0].TenantSlug != "mangala" {
			t.Fatalf("page 2 started at %q, want mangala", next[0].TenantSlug)
		}
	})
}

// --- error propagation ---

func TestService_PropagatesRepositoryFailures(t *testing.T) {
	sentinel := errors.New("registry unavailable")

	t.Run("Get", func(t *testing.T) {
		repo := newFakeRepository()
		repo.getErr = sentinel

		if _, err := newTestService(repo).Get(context.Background(), "mangala"); !errors.Is(err, sentinel) {
			t.Fatalf("Get error = %v, want the repository error", err)
		}
	})

	t.Run("Reserve", func(t *testing.T) {
		repo := newFakeRepository()
		repo.reserveErr = sentinel

		if _, err := newTestService(repo).Reserve(context.Background(), "mangala", ProductPOS, testActor()); !errors.Is(err, sentinel) {
			t.Fatalf("Reserve error = %v, want the repository error", err)
		}
	})

	t.Run("Activate", func(t *testing.T) {
		repo := newFakeRepository()
		repo.getErr = sentinel

		if _, err := newTestService(repo).Activate(context.Background(), "mangala", nil, testActor()); !errors.Is(err, sentinel) {
			t.Fatalf("Activate error = %v, want the repository error", err)
		}
	})

	t.Run("Release", func(t *testing.T) {
		repo := newFakeRepository()
		repo.seed(Route{TenantSlug: "mangala", Product: ProductPOS, Status: StatusDisabled})
		repo.releaseErr = sentinel

		if _, err := newTestService(repo).Release(context.Background(), "mangala", testActor()); !errors.Is(err, sentinel) {
			t.Fatalf("Release error = %v, want the repository error", err)
		}
	})
}

// TestFullLifecycle walks the documented tenant registration flow end to end
// (§62): reserve, product provisions, activate, suspend, re-enable, release.
func TestFullLifecycle(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo)
	ctx := context.Background()

	route, err := svc.Reserve(ctx, "garage1", ProductGMS, testActor())
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if route.Status != StatusProvisioning {
		t.Fatalf("after reserve: %s", route.Status)
	}

	product := ProductGMS
	route, err = svc.Activate(ctx, "garage1", &product, testActor())
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !route.Routable() {
		t.Fatalf("after activate: %s, want routable", route.Status)
	}

	route, err = svc.Disable(ctx, "garage1", testActor())
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if route.Routable() {
		t.Fatalf("after disable: %s, want not routable", route.Status)
	}

	route, err = svc.Activate(ctx, "garage1", &product, testActor())
	if err != nil {
		t.Fatalf("re-Activate: %v", err)
	}
	if !route.Routable() {
		t.Fatalf("after re-activate: %s, want routable", route.Status)
	}

	if _, err := svc.Release(ctx, "garage1", testActor()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if repo.exists("garage1") {
		t.Fatal("the route survived release")
	}

	// Every mutation left a record, even though the row is gone.
	events := repo.recordedEvents()
	wantActions := []Action{ActionReserve, ActionActivate, ActionDisable, ActionActivate, ActionRelease}
	if len(events) != len(wantActions) {
		t.Fatalf("recorded %d events, want %d: %+v", len(events), len(wantActions), events)
	}
	for i, want := range wantActions {
		if events[i].Action != want {
			t.Errorf("event %d = %s, want %s", i, events[i].Action, want)
		}
	}
}
