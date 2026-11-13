package tenant

// Action is a recorded lifecycle mutation.
type Action string

// Recorded actions.
const (
	ActionReserve  Action = "RESERVE"
	ActionActivate Action = "ACTIVATE"
	ActionDisable  Action = "DISABLE"
	ActionRelease  Action = "RELEASE"
)

// Valid reports whether a is a recognised action.
func (a Action) Valid() bool {
	switch a {
	case ActionReserve, ActionActivate, ActionDisable, ActionRelease:
		return true
	}
	return false
}

// Actor identifies who requested a lifecycle change.
//
// This is the authenticated calling service, not a product user. The Gateway
// has no concept of product users (§26).
type Actor struct {
	// Service is the authenticated caller's identity, for example "pos".
	Service string

	// RequestID ties the change back to a single request across the Gateway
	// and the calling product's logs (§50).
	RequestID string
}

// Event is one audit record for a lifecycle mutation.
//
// Events are written in the same transaction as the mutation they describe, so
// the trail cannot drift from the stored state. This matters most for Release,
// which deletes the routing record outright: without an event log there is no
// way to answer who released a tenant, when, or in response to which request.
type Event struct {
	TenantSlug string
	Action     Action

	// From is the status before the change. Empty for RESERVE.
	From Status

	// To is the status after the change. Empty for RELEASE.
	To Status

	Product Product
	Actor   Actor
}
