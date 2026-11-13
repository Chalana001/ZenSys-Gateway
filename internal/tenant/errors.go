package tenant

import "errors"

// Sentinel errors for the tenant registry.
//
// Handlers map these to HTTP status codes; nothing downstream should ever need
// to match on an error string.
var (
	// ErrNotFound means no route exists for the slug.
	ErrNotFound = errors.New("tenant route not found")

	// ErrAlreadyExists means the slug is already reserved. Because
	// tenant_slug is globally unique, this applies across products: a slug
	// claimed by POS cannot also be claimed by GMS (§12).
	ErrAlreadyExists = errors.New("tenant slug is already reserved")

	// ErrInvalidSlug means the slug violates the platform format rules.
	ErrInvalidSlug = errors.New("invalid tenant slug")

	// ErrReservedHost means the slug is a reserved platform hostname and can
	// never become a tenant (§10).
	ErrReservedHost = errors.New("tenant slug is a reserved platform hostname")

	// ErrInvalidProduct means the product is not one the Gateway routes for.
	ErrInvalidProduct = errors.New("invalid product")

	// ErrInvalidStatus means the status value is not recognised.
	ErrInvalidStatus = errors.New("invalid status")

	// ErrInvalidTransition means the requested state change is not legal from
	// the route's current status.
	ErrInvalidTransition = errors.New("invalid status transition")

	// ErrConflict means the request was well-formed but conflicts with the
	// stored state — a product mismatch, or a concurrent modification that
	// changed the row between reading and writing it.
	ErrConflict = errors.New("conflicting tenant route state")
)
