package routing

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zensys/gateway/internal/tenant"
)

// stubLookup records how it was called so tests can assert not just the result
// but whether the registry was consulted at all.
type stubLookup struct {
	routes  map[string]tenant.Route
	err     error
	lookups []string
}

func (s *stubLookup) GetBySlug(_ context.Context, slug string) (tenant.Route, error) {
	s.lookups = append(s.lookups, slug)

	if s.err != nil {
		return tenant.Route{}, s.err
	}

	route, ok := s.routes[slug]
	if !ok {
		return tenant.Route{}, fmt.Errorf("%w: %q", tenant.ErrNotFound, slug)
	}
	return route, nil
}

func activeRoute(slug string) tenant.Route {
	return tenant.Route{TenantSlug: slug, Product: tenant.ProductPOS, Status: tenant.StatusActive}
}

func TestResolve_ActiveRouteIsReturned(t *testing.T) {
	lookup := &stubLookup{routes: map[string]tenant.Route{"mangala": activeRoute("mangala")}}

	route, err := NewResolver(lookup).Resolve(context.Background(), "mangala")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if route.TenantSlug != "mangala" {
		t.Errorf("TenantSlug = %q, want %q", route.TenantSlug, "mangala")
	}
	if route.Product != tenant.ProductPOS {
		t.Errorf("Product = %q, want %q", route.Product, tenant.ProductPOS)
	}
}

// TestResolve_InactiveRoutesAreRefused covers both non-ACTIVE states.
//
// Refusing here rather than at the call site is what makes it impossible for a
// future code path to proxy to a tenant that is still provisioning or has been
// disabled (architecture doc §8 step 10, §34).
func TestResolve_InactiveRoutesAreRefused(t *testing.T) {
	for _, status := range []tenant.Status{tenant.StatusProvisioning, tenant.StatusDisabled} {
		t.Run(string(status), func(t *testing.T) {
			lookup := &stubLookup{routes: map[string]tenant.Route{
				"mangala": {TenantSlug: "mangala", Product: tenant.ProductPOS, Status: status},
			}}

			route, err := NewResolver(lookup).Resolve(context.Background(), "mangala")
			if !errors.Is(err, ErrTenantDisabled) {
				t.Fatalf("error = %v, want ErrTenantDisabled", err)
			}
			if route != (tenant.Route{}) {
				t.Errorf("route = %+v, want the zero Route so no caller can use it", route)
			}
		})
	}
}

func TestResolve_UnknownTenant(t *testing.T) {
	lookup := &stubLookup{routes: map[string]tenant.Route{}}

	_, err := NewResolver(lookup).Resolve(context.Background(), "nobody")
	if !errors.Is(err, ErrUnknownTenant) {
		t.Fatalf("error = %v, want ErrUnknownTenant", err)
	}
	// The tenant-absence sentinel must not leak through, or a caller
	// branching on it could not tell the two layers apart.
	if errors.Is(err, ErrRegistryDown) {
		t.Error("an absent tenant must not be reported as a registry failure")
	}
}

// TestResolve_RegistryFailureIsNotAnAbsence is the §46 requirement.
//
// A registry that cannot answer says nothing about whether the tenant exists.
// Reporting a 404 here would break a working tenant during a database blip,
// and would make the Gateway's own outage invisible.
func TestResolve_RegistryFailureIsNotAnAbsence(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.5:3306: connection refused")
	lookup := &stubLookup{err: cause}

	_, err := NewResolver(lookup).Resolve(context.Background(), "mangala")

	if !errors.Is(err, ErrRegistryDown) {
		t.Fatalf("error = %v, want ErrRegistryDown", err)
	}
	if errors.Is(err, ErrUnknownTenant) {
		t.Error("a registry failure must not be reported as an unknown tenant")
	}
	// The cause is kept in the chain so the log line names the real problem
	// instead of just "registry unavailable".
	if !errors.Is(err, cause) {
		t.Error("the underlying cause must remain in the error chain for logging")
	}
}

// TestResolve_RegistryFailureWinsOverNotFound documents that the repository's
// own sentinel is the only signal treated as an absence. Anything else, even
// one that happens to wrap ErrNotFound, is a failure to answer.
func TestResolve_RegistryFailureWinsOverNotFound(t *testing.T) {
	lookup := &stubLookup{err: fmt.Errorf("query failed: %w", errors.New("timeout"))}

	_, err := NewResolver(lookup).Resolve(context.Background(), "mangala")
	if !errors.Is(err, ErrRegistryDown) {
		t.Fatalf("error = %v, want ErrRegistryDown", err)
	}
}

func TestResolve_PassesTheSlugThroughUnchanged(t *testing.T) {
	lookup := &stubLookup{routes: map[string]tenant.Route{"abc-motors": activeRoute("abc-motors")}}

	if _, err := NewResolver(lookup).Resolve(context.Background(), "abc-motors"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lookup.lookups) != 1 || lookup.lookups[0] != "abc-motors" {
		t.Errorf("lookups = %v, want exactly [abc-motors]", lookup.lookups)
	}
}
