package routing

import "strings"

// Stage is which of a product's two upstreams a request is destined for.
//
// The set is closed and deliberately tiny. A product has a frontend that
// serves its application shell and a backend that serves its API
// (architecture doc §17), and the Gateway knows nothing beyond that: it has no
// per-product route table and no idea what lives under /api (§21).
type Stage int

const (
	// StageFrontend is the product's application server.
	StageFrontend Stage = iota

	// StageBackend is the product's API server.
	StageBackend
)

// String implements fmt.Stringer.
//
// The values are the ones the architecture document uses for the `route` label
// (§52), so they can go straight into a log field or a metric label without a
// translation table at the call site.
func (s Stage) String() string {
	switch s {
	case StageFrontend:
		return "frontend"
	case StageBackend:
		return "backend"
	default:
		return "unknown"
	}
}

// Selector implements the frontend/backend split from architecture doc §9.1.
//
// It is the whole of Stage B, and it is one rule: a request whose path is the
// backend prefix, or is under it, goes to the backend; everything else goes to
// the frontend.
type Selector struct {
	backendPrefix string
	caseSensitive bool
}

// NewSelector builds the selector from configuration.
//
// backendPrefix must be a path such as "/api" with a leading slash and no
// trailing one; config.Load guarantees this.
func NewSelector(backendPrefix string, caseSensitive bool) *Selector {
	return &Selector{backendPrefix: backendPrefix, caseSensitive: caseSensitive}
}

// Stage returns the stage a request path belongs to.
//
// The prefix must match a whole path segment. "/api" is the backend, and so is
// "/api/orders"; "/apix" is not, and neither is "/api-v2". A plain
// strings.HasPrefix would send both to the backend, where they would 404 for a
// reason that has nothing to do with the product.
//
// Matching is case-sensitive by default (§9.2): "/API/orders" is frontend
// traffic. HTTP paths are case-sensitive per RFC 9110 and both products
// declare lowercase routes, so folding case would route a request to a backend
// that has no such route — trading a clear frontend 404 for a confusing
// product one.
func (s *Selector) Stage(path string) Stage {
	if s == nil {
		return StageFrontend
	}

	prefix := s.backendPrefix
	if !s.caseSensitive {
		path = strings.ToLower(path)
		prefix = strings.ToLower(prefix)
	}

	// The query string is not part of the path — net/url keeps it in RawQuery
	// — so "/api?x=1" arrives here as "/api" and matches, which is correct.
	if path == prefix || strings.HasPrefix(path, prefix+"/") {
		return StageBackend
	}
	return StageFrontend
}

// BackendPathPrefix returns the configured prefix, for diagnostics.
func (s *Selector) BackendPathPrefix() string {
	if s == nil {
		return ""
	}
	return s.backendPrefix
}
