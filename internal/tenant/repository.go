package tenant

import "context"

// List pagination bounds.
const (
	DefaultListLimit = 100
	MaxListLimit     = 1000
)

// ListFilter narrows a List call.
//
// Pagination is keyset-based on tenant_slug rather than offset-based: the slug
// is the primary key, so "WHERE tenant_slug > ?" is an index range scan with
// stable results. Offset pagination would re-scan everything before the offset
// and could skip or repeat rows if a tenant were inserted mid-pagination.
type ListFilter struct {
	// Product restricts to one product. Nil means any.
	Product *Product

	// Status restricts to one lifecycle state. Nil means any.
	Status *Status

	// After is the last slug seen on the previous page. Empty starts from the
	// beginning.
	After string

	// Limit is the maximum number of routes to return. Zero applies
	// DefaultListLimit; values above MaxListLimit are clamped.
	Limit int
}

// ClampLimit returns the effective page size for this filter.
func (f ListFilter) ClampLimit() int {
	switch {
	case f.Limit <= 0:
		return DefaultListLimit
	case f.Limit > MaxListLimit:
		return MaxListLimit
	default:
		return f.Limit
	}
}

// Repository is the persistence boundary for the platform tenant registry.
//
// Every mutating method takes the Event to record alongside the change, so the
// audit trail is written in the same transaction as the mutation rather than
// best-effort afterwards.
//
// Implementations must be safe for concurrent use.
type Repository interface {
	// GetBySlug returns the route for slug, or an error wrapping ErrNotFound.
	GetBySlug(ctx context.Context, slug string) (Route, error)

	// List returns routes matching the filter, ordered by slug.
	List(ctx context.Context, filter ListFilter) ([]Route, error)

	// Reserve creates route and returns it as stored, with database-assigned
	// timestamps. Returns an error wrapping ErrAlreadyExists if the slug is
	// taken.
	Reserve(ctx context.Context, route Route, event Event) (Route, error)

	// Transition moves a route from one status to another atomically.
	//
	// The update is a compare-and-swap: it only applies if the stored status
	// still equals from. If the row changed underneath the caller, this
	// returns an error wrapping ErrConflict rather than silently applying a
	// transition that was decided against stale state.
	Transition(ctx context.Context, slug string, from, to Status, event Event) (Route, error)

	// Release deletes a route. Returns an error wrapping ErrNotFound if it
	// does not exist.
	Release(ctx context.Context, slug string, event Event) error

	// Ping reports whether the registry is reachable.
	Ping(ctx context.Context) error
}
