package tenant

import (
	"fmt"
	"time"
)

// Product identifies a ZenSys product the Gateway can route to.
//
// The set is closed in V1 and mirrored by a CHECK constraint on
// tenant_routes.product. Adding a product is deliberately a migration plus a
// configuration entry, not a runtime decision: which products exist is a
// platform-level commitment, and a free-text product value would let a typo at
// provisioning time create a route that no upstream configuration can serve.
type Product string

// Known products.
const (
	ProductPOS Product = "POS"
	ProductGMS Product = "GMS"
)

// Products returns every product known to V1.
func Products() []Product {
	return []Product{ProductPOS, ProductGMS}
}

// Valid reports whether p is a product the Gateway can route for.
func (p Product) Valid() bool {
	for _, known := range Products() {
		if p == known {
			return true
		}
	}
	return false
}

// String implements fmt.Stringer.
func (p Product) String() string { return string(p) }

// Status is the lifecycle state of a tenant route.
type Status string

// Lifecycle states.
const (
	// StatusProvisioning means the slug is reserved but the product has not
	// confirmed its own tenant is ready. Not routable.
	StatusProvisioning Status = "PROVISIONING"

	// StatusActive means the route serves public traffic.
	StatusActive Status = "ACTIVE"

	// StatusDisabled means the route exists but must not reach a product.
	StatusDisabled Status = "DISABLED"
)

// Valid reports whether s is a recognised lifecycle state.
func (s Status) Valid() bool {
	switch s {
	case StatusProvisioning, StatusActive, StatusDisabled:
		return true
	}
	return false
}

// Routable reports whether a route in this state may serve public traffic.
//
// Only ACTIVE is routable. PROVISIONING and DISABLED both fail closed, and
// both are indistinguishable from an unknown tenant to the caller
// (ADR 0001 decision 5).
func (s Status) Routable() bool { return s == StatusActive }

// String implements fmt.Stringer.
func (s Status) String() string { return string(s) }

// Route is one platform routing record: who owns a hostname, which product
// should receive it, and whether routing is currently enabled.
//
// It contains no customer data and no credentials, by design (§61).
type Route struct {
	TenantSlug string
	Product    Product
	Status     Status
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Routable reports whether this route may serve public traffic.
func (r Route) Routable() bool { return r.Status.Routable() }

// transitions maps each status to the statuses it may move to.
//
// PROVISIONING has no inbound transition because it is the starting state:
// Reserve creates a route directly in it.
//
// Two of these edges go beyond the linear PROVISIONING -> ACTIVE -> DISABLED
// diagram in the architecture document (§34):
//
//   - DISABLED -> ACTIVE, so a suspended tenant can be re-enabled without
//     being re-provisioned from scratch.
//   - PROVISIONING -> DISABLED, so a provisioning failure can be recorded
//     cleanly instead of leaving an orphaned route stuck mid-flight.
//
// Both are cheap now and awkward to add later, once products depend on the
// lifecycle API's refusal behaviour.
var transitions = map[Status][]Status{
	StatusProvisioning: {StatusActive, StatusDisabled},
	StatusActive:       {StatusDisabled},
	StatusDisabled:     {StatusActive},
}

// CanTransition reports whether a status change from -> to is legal.
//
// A transition from a status to itself is not legal here. Idempotency is
// handled a layer up, in Service.Activate and Service.Disable, which treat
// "already in the target state" as a successful no-op rather than as a
// transition. That keeps this table purely about state *changes*, and means a
// retried activation does not have to be special-cased by the caller.
func CanTransition(from, to Status) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// AllowedTransitions returns the statuses reachable from from.
func AllowedTransitions(from Status) []Status {
	allowed := transitions[from]
	out := make([]Status, len(allowed))
	copy(out, allowed)
	return out
}

// ParseProduct converts a string to a Product, rejecting unknown values.
func ParseProduct(s string) (Product, error) {
	p := Product(s)
	if !p.Valid() {
		return "", fmt.Errorf("%w: %q is not a configured product", ErrInvalidProduct, s)
	}
	return p, nil
}

// ParseStatus converts a string to a Status, rejecting unknown values.
func ParseStatus(s string) (Status, error) {
	st := Status(s)
	if !st.Valid() {
		return "", fmt.Errorf("%w: %q is not a recognised status", ErrInvalidStatus, s)
	}
	return st, nil
}
