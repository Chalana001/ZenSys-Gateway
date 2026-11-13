package routing

import (
	"context"
	"errors"
	"fmt"

	"github.com/zensys/gateway/internal/tenant"
)

// Resolver errors.
//
// These three are the outcomes the HTTP layer distinguishes. To an anonymous
// caller they are indistinguishable: an unknown tenant and an inactive one
// produce a byte-identical 404 (ADR 0001 decision 5). They are separate values
// so that logs and metrics can tell them apart, where the distinction is
// useful and not attacker-visible.
var (
	// ErrUnknownTenant means the slug is not in the registry.
	ErrUnknownTenant = errors.New("routing: unknown tenant")

	// ErrTenantDisabled means the slug exists but is not ACTIVE, so it is
	// either still PROVISIONING or has been DISABLED.
	ErrTenantDisabled = errors.New("routing: tenant is not active")

	// ErrRegistryDown means the registry could not answer. This is *not* the
	// same as the tenant being absent: the Gateway must not guess, so it
	// reports itself unable to decide rather than returning a 404
	// (architecture doc §46).
	ErrRegistryDown = errors.New("routing: registry unavailable")
)

// Lookup reads a route by slug.
//
// It is satisfied by tenant.Repository. Declaring the narrow interface here
// rather than depending on the repository keeps the resolver testable without
// a database, and keeps the dependency pointing in one direction.
type Lookup interface {
	GetBySlug(ctx context.Context, slug string) (tenant.Route, error)
}

// Resolver turns a tenant slug into an active route.
type Resolver struct {
	registry Lookup
}

// NewResolver builds a resolver over the given lookup.
func NewResolver(registry Lookup) *Resolver {
	return &Resolver{registry: registry}
}

// Resolve returns the ACTIVE route for slug.
//
// A route that exists but is not ACTIVE is refused here rather than by the
// caller, so that no code path can accidentally proxy to a tenant that has
// been disabled or has not finished provisioning (architecture doc §8 step 10,
// §34).
func (r *Resolver) Resolve(ctx context.Context, slug string) (tenant.Route, error) {
	route, err := r.registry.GetBySlug(ctx, slug)
	switch {
	case errors.Is(err, tenant.ErrNotFound):
		return tenant.Route{}, fmt.Errorf("%w: %q", ErrUnknownTenant, slug)

	case err != nil:
		// Both errors are wrapped: the sentinel so callers can branch on it,
		// and the cause so the log line names the actual failure instead of
		// just "registry unavailable".
		return tenant.Route{}, fmt.Errorf("%w: %w", ErrRegistryDown, err)
	}

	if route.Status != tenant.StatusActive {
		return tenant.Route{}, fmt.Errorf("%w: %q is %s", ErrTenantDisabled, slug, route.Status)
	}

	return route, nil
}
