package routing

import (
	"context"

	"github.com/zensys/gateway/internal/tenant"
)

// Info is the routing decision for one request: which public hostname was
// asked for, which tenant that names, and the registry record it resolved to.
//
// It describes a *decision*, not a lookup in progress. A request that never
// reached a decision — an unknown host, a reserved host, a registry outage —
// has no Info, and consumers must treat its absence as "not routed" rather
// than as an empty route.
type Info struct {
	// Host is the normalized public hostname, such as "mangala.zensys.lk".
	Host string

	// Tenant is the canonical slug, always equal to Route.TenantSlug.
	Tenant string

	// Stage is which of the product's two upstreams the request is for.
	Stage Stage

	// Route is the registry record the decision was made from.
	Route tenant.Route
}

// Product returns the product the request is being routed to.
//
// It is a method rather than a field so that the product cannot disagree with
// the route it was derived from.
func (i Info) Product() tenant.Product { return i.Route.Product }

// Edge records what the Gateway could establish about the connection a request
// arrived on, after the trusted-proxy policy has been applied.
//
// When Trusted is false these values describe the TCP connection and nothing
// else. When it is true they may additionally reflect headers a configured
// trusted proxy sent, because that proxy is the only party that saw the
// client's own connection.
type Edge struct {
	// Proto is the scheme the client used, as far as the Gateway can tell.
	Proto string

	// Host is the hostname to advertise downstream.
	//
	// It is NOT the hostname the request was routed on, and the two are
	// allowed to differ. Routing always parses the request's own Host header
	// (see ResolveEdge), so a forwarded value can change what a product is
	// told but never which tenant a request reaches.
	Host string

	// ClientIP is the resolved client address, without a port.
	ClientIP string

	// Trusted reports whether the request arrived from a configured trusted
	// proxy.
	Trusted bool
}

// State is the per-request slot the routing decision is recorded in.
//
// It exists because context.Context only propagates inward. A middleware that
// wraps the routing handler — the access log, which reports the tenant and
// product on every line (plan §15.1) — cannot observe a value the handler
// wrote into a derived context, because the handler's derived request never
// travels back out to it. A small mutable slot installed by an outer
// middleware and filled in by the handler is the standard remedy.
//
// The slot is created per request, reachable only through the request context,
// and never shared between requests, so it is not the package-level mutable
// state architecture doc §31 forbids. internal/archtest enforces that
// separately: it fails the build if any package-level variable could hold
// routing state.
type State struct {
	info Info
	set  bool
}

// Set records the routing decision.
func (s *State) Set(info Info) {
	if s == nil {
		return
	}
	s.info = info
	s.set = true
}

// Get returns the recorded decision and whether one was recorded.
func (s *State) Get() (Info, bool) {
	if s == nil {
		return Info{}, false
	}
	return s.info, s.set
}

type stateKey struct{}

// WithState returns a context carrying slot.
func WithState(ctx context.Context, slot *State) context.Context {
	return context.WithValue(ctx, stateKey{}, slot)
}

// StateFrom returns the slot carried by ctx, if any.
func StateFrom(ctx context.Context) (*State, bool) {
	slot, ok := ctx.Value(stateKey{}).(*State)
	return slot, ok
}

// SetInfo records info in the context's slot.
//
// It is a no-op when no slot is present, which happens only in a chain that
// omitted the middleware installing one. Nothing is lost in that case: with no
// slot there is no consumer on the outward side either, and the inward
// consumers read the route from the handler's own arguments.
func SetInfo(ctx context.Context, info Info) {
	if slot, ok := StateFrom(ctx); ok {
		slot.Set(info)
	}
}

// InfoFrom returns the routing decision recorded for this request.
func InfoFrom(ctx context.Context) (Info, bool) {
	slot, ok := StateFrom(ctx)
	if !ok {
		return Info{}, false
	}
	return slot.Get()
}

type edgeKey struct{}

// WithEdge returns a context carrying the connection facts for a request.
//
// Edge is carried by value, unlike Info, because it is decided once — before
// routing runs — and never revised. Only Info needs a mutable slot, because
// only Info is produced by a handler that outer middleware then reads.
func WithEdge(ctx context.Context, edge Edge) context.Context {
	return context.WithValue(ctx, edgeKey{}, edge)
}

// EdgeFrom returns the connection facts carried by ctx.
//
// A context with no Edge reports false, and callers must treat that as
// untrusted. Failing closed matters here: if the trusted-proxy middleware is
// absent from a chain, the safe reading is that nothing has been verified, not
// that everything is fine.
func EdgeFrom(ctx context.Context) (Edge, bool) {
	edge, ok := ctx.Value(edgeKey{}).(Edge)
	return edge, ok
}
