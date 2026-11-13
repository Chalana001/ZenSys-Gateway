package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zensys/gateway/internal/security"
	"github.com/zensys/gateway/internal/tenant"
)

// Lifecycle is the tenant lifecycle operations the internal API exposes.
//
// It is satisfied by *tenant.Service. Declaring the interface here rather than
// taking the concrete type keeps the HTTP layer testable without a database,
// and keeps the dependency pointing one way: this package knows about the
// lifecycle rules, and the lifecycle package does not know that HTTP exists.
type Lifecycle interface {
	Get(ctx context.Context, slug string) (tenant.Route, error)
	List(ctx context.Context, filter tenant.ListFilter) ([]tenant.Route, error)
	Reserve(ctx context.Context, slug string, product tenant.Product, actor tenant.Actor) (tenant.Route, error)
	Activate(ctx context.Context, slug string, product *tenant.Product, actor tenant.Actor) (tenant.Route, error)
	Disable(ctx context.Context, slug string, actor tenant.Actor) (tenant.Route, error)
	Release(ctx context.Context, slug string, actor tenant.Actor) (tenant.Route, error)
}

// maxInternalBodyBytes caps a lifecycle request body.
//
// Every request the API accepts is a small JSON object; the largest is a slug
// and a product name. The limit is far below server.max_body_bytes because a
// lifecycle call has no reason to carry a payload, and a bound that is
// obviously too large for any legitimate request is the cheapest protection
// against one that is not.
const maxInternalBodyBytes = 64 << 10

// InternalHandler serves the tenant lifecycle API on the private listener
// (§39, §80).
//
// It is mounted only on the internal listener. That is not a configuration
// choice: the public listener's mux has no `/internal/` pattern at all, so
// there is no address at which this handler can be reached from the public
// side (architecture doc §40).
type InternalHandler struct {
	logger    *slog.Logger
	auth      security.Authenticator
	lifecycle Lifecycle
}

// NewInternalHandler builds the lifecycle API.
//
// auth must not be nil. There is deliberately no "no authentication"
// implementation of Authenticator to fall back to: an unauthenticated
// lifecycle API can delete a tenant's routing with one request, and making
// that reachable by omission is not a risk worth carrying for the sake of a
// shorter constructor.
func NewInternalHandler(logger *slog.Logger, auth security.Authenticator, lifecycle Lifecycle) *InternalHandler {
	return &InternalHandler{logger: logger, auth: auth, lifecycle: lifecycle}
}

// endpoint is one lifecycle operation.
//
// The table returned by endpoints is the single source of truth for the API's
// surface. The router and the wrong-method response are both derived from it,
// so an endpoint cannot be reachable while being absent from the Allow header,
// or vice versa — a drift that is invisible until a client trusts the header.
type endpoint struct {
	method  string
	pattern string
	handler http.HandlerFunc
}

// endpoints lists the lifecycle API's surface (§13.1).
func (h *InternalHandler) endpoints() []endpoint {
	return []endpoint{
		{http.MethodPost, "/internal/v1/tenant-routes/reserve", h.reserve},
		{http.MethodPost, "/internal/v1/tenant-routes/activate", h.activate},
		{http.MethodPost, "/internal/v1/tenant-routes/disable", h.disable},
		{http.MethodPost, "/internal/v1/tenant-routes/release", h.release},
		{http.MethodGet, "/internal/v1/tenant-routes/{tenant}", h.getRoute},
		{http.MethodGet, "/internal/v1/tenant-routes", h.listRoutes},
		{http.MethodGet, "/internal/v1/platform-policy", h.platformPolicy},
	}
}

// Register mounts the lifecycle endpoints on mux.
//
// Authentication is applied once, around the whole subtree, rather than
// repeated in each handler. A per-handler check is one someone can forget to
// add to the seventh endpoint; a subtree wrapper cannot be bypassed by adding
// a handler, because a handler that is not under it is not routable.
func (h *InternalHandler) Register(mux *http.ServeMux) {
	api := http.NewServeMux()

	// pathOnly registers the same patterns without a method, so it can answer
	// "is there an endpoint at this path at all?".
	//
	// It exists because the catch-all below — which is what gives an unknown
	// path the JSON error contract instead of net/http's plain-text 404 —
	// suppresses ServeMux's own 405 response. Without it, a wrong method would
	// be reported as an unknown endpoint. Reusing a ServeMux rather than
	// hand-matching keeps the wildcard and precedence rules identical to the
	// real router's, so the two cannot disagree.
	pathOnly := http.NewServeMux()
	allowed := make(map[string][]string, len(h.endpoints()))

	for _, e := range h.endpoints() {
		api.HandleFunc(e.method+" "+e.pattern, e.handler)

		// The handler is never called: pathOnly is only ever asked which
		// pattern a path matches.
		pathOnly.HandleFunc(e.pattern, func(http.ResponseWriter, *http.Request) {})

		methods := append(allowed[e.pattern], e.method)
		// net/http answers HEAD wherever GET is allowed, and a client that
		// reads Allow to decide what it may send should see that.
		if e.method == http.MethodGet {
			methods = append(methods, http.MethodHead)
		}
		allowed[e.pattern] = methods
	}

	api.HandleFunc("/internal/v1/", func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := pathOnly.Handler(r); pattern != "" {
			w.Header().Set("Allow", strings.Join(allowed[pattern], ", "))
			WriteMethodNotAllowed(w)
			return
		}
		WriteError(w, http.StatusNotFound, ErrNotFound, "No such endpoint.")
	})

	mux.Handle("/internal/v1/", h.authenticate(api))
}

// authenticate establishes the calling service and records it for the handlers
// and the access log.
//
// The identity is written into the request's slot rather than into a derived
// context. A derived context only travels inward, and the access log sits
// *outside* this handler — so a value installed here by deriving would be
// invisible to it, which is exactly the problem RequestState exists to solve.
//
// This depends on RequestState being in the chain. If it is not, every request
// is refused with a 401 rather than being allowed through unidentified, which
// is the right direction for the failure to point.
func (h *InternalHandler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := h.auth.Identify(r)
		if err != nil {
			// Logged at warn, not debug: an unauthenticated lifecycle call is
			// either a misconfigured provisioning job or someone probing a
			// private listener, and both are worth seeing. The token itself is
			// never logged, and neither is the header — only the fact that it
			// did not resolve.
			h.logger.Warn("internal API call is not authenticated",
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
			)
			WriteUnauthorized(w)
			return
		}

		SetIdentity(r.Context(), identity)
		next.ServeHTTP(w, r)
	})
}

// --- request and response shapes (§13.2) ---

// reserveRequest is the body of POST /tenant-routes/reserve.
type reserveRequest struct {
	Tenant  string `json:"tenant"`
	Product string `json:"product"`
}

// tenantRequest is the body of the operations that name only a tenant.
//
// disable and release deliberately do not accept a product. The product is
// read from the registry and the caller is authorized against the *stored*
// value, which removes the whole class of bug where a caller asserts a product
// that does not match the route it is acting on.
type tenantRequest struct {
	Tenant string `json:"tenant"`

	// Confirm must equal Tenant for a release. It is ignored by disable,
	// because only release is destructive.
	Confirm string `json:"confirm"`
}

// routeResponse is the JSON form of a route.
//
// It is a separate type from tenant.Route so that the wire format is a
// deliberate choice rather than whatever the internal struct happens to look
// like. The field names are the API's; renaming a Go field must not silently
// rename a JSON key.
type routeResponse struct {
	Tenant    string `json:"tenant"`
	Product   string `json:"product"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// timestampLayout renders timestamps as UTC with millisecond precision.
//
// Always UTC and always the same width, so a client can compare two values as
// strings and does not have to guess which offset it is looking at.
const timestampLayout = "2006-01-02T15:04:05.000Z"

func newRouteResponse(route tenant.Route) routeResponse {
	return routeResponse{
		Tenant:    route.TenantSlug,
		Product:   route.Product.String(),
		Status:    route.Status.String(),
		CreatedAt: formatTimestamp(route.CreatedAt),
		UpdatedAt: formatTimestamp(route.UpdatedAt),
	}
}

func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timestampLayout)
}

// listResponse is the body of GET /tenant-routes.
type listResponse struct {
	Routes []routeResponse `json:"routes"`

	// NextAfter is the slug to pass as ?after= for the next page, and is empty
	// when this page was not full.
	//
	// A full page may still be the last one, in which case the next request
	// returns an empty list. That is the honest trade: proving there is
	// another page needs one extra row fetched, and one extra row is enough to
	// push a request past the repository's page-size ceiling.
	NextAfter string `json:"next_after,omitempty"`
}

// --- handlers ---

// reserve claims a slug for a product, creating the route in PROVISIONING.
func (h *InternalHandler) reserve(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		WriteUnauthorized(w)
		return
	}

	var req reserveRequest
	if !h.decode(w, r, &req) {
		return
	}
	if req.Tenant == "" {
		WriteBadRequest(w, "The \"tenant\" field is required.")
		return
	}
	if req.Product == "" {
		WriteBadRequest(w, "The \"product\" field is required.")
		return
	}

	product, err := tenant.ParseProduct(req.Product)
	if err != nil {
		WriteBadRequest(w, "The \"product\" field must name a product the Gateway routes for.")
		return
	}

	// Authorize before validating the slug. The check that stops a POS token
	// from acting on GMS tenants should not depend on the rest of the request
	// being well-formed.
	if err := identity.AuthorizeProduct(product); err != nil {
		h.writeForbidden(w, r, identity, product, err)
		return
	}

	route, err := h.lifecycle.Reserve(r.Context(), req.Tenant, product, actorOf(r, identity))
	if err != nil {
		h.writeLifecycleError(w, r, err)
		return
	}

	WriteJSON(w, http.StatusCreated, newRouteResponse(route))
}

// activate moves a route to ACTIVE.
func (h *InternalHandler) activate(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		WriteUnauthorized(w)
		return
	}

	var req reserveRequest
	if !h.decode(w, r, &req) {
		return
	}
	if req.Tenant == "" {
		WriteBadRequest(w, "The \"tenant\" field is required.")
		return
	}
	if req.Product == "" {
		WriteBadRequest(w, "The \"product\" field is required.")
		return
	}

	product, err := tenant.ParseProduct(req.Product)
	if err != nil {
		WriteBadRequest(w, "The \"product\" field must name a product the Gateway routes for.")
		return
	}
	if err := identity.AuthorizeProduct(product); err != nil {
		h.writeForbidden(w, r, identity, product, err)
		return
	}

	// The asserted product is passed through so the service can check it
	// against the stored one. A caller that names the wrong product gets a 409
	// rather than activating a tenant it does not own (§42).
	route, err := h.lifecycle.Activate(r.Context(), req.Tenant, &product, actorOf(r, identity))
	if err != nil {
		h.writeLifecycleError(w, r, err)
		return
	}

	WriteJSON(w, http.StatusOK, newRouteResponse(route))
}

// disable stops a route from reaching its product.
func (h *InternalHandler) disable(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		WriteUnauthorized(w)
		return
	}

	var req tenantRequest
	if !h.decode(w, r, &req) {
		return
	}
	if req.Tenant == "" {
		WriteBadRequest(w, "The \"tenant\" field is required.")
		return
	}

	if _, err := h.authorizeStoredProduct(r.Context(), identity, req.Tenant); err != nil {
		h.writeLifecycleError(w, r, err)
		return
	}

	updated, err := h.lifecycle.Disable(r.Context(), req.Tenant, actorOf(r, identity))
	if err != nil {
		h.writeLifecycleError(w, r, err)
		return
	}

	WriteJSON(w, http.StatusOK, newRouteResponse(updated))
}

// release deletes a route outright.
//
// This is the only destructive operation in the API, and the confirmation
// field is what makes it impossible to perform by accident (§38).
func (h *InternalHandler) release(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		WriteUnauthorized(w)
		return
	}

	var req tenantRequest
	if !h.decode(w, r, &req) {
		return
	}
	if req.Tenant == "" {
		WriteBadRequest(w, "The \"tenant\" field is required.")
		return
	}

	// Checked before the registry is read, deliberately. A missing or
	// mismatched confirmation is a client bug, and answering it without a
	// database round trip means a caller that forgot the field cannot use the
	// 400-versus-404 distinction to probe which slugs exist.
	if req.Confirm != req.Tenant {
		WriteBadRequest(w, "The \"confirm\" field must repeat the tenant slug exactly.")
		return
	}

	if _, err := h.authorizeStoredProduct(r.Context(), identity, req.Tenant); err != nil {
		h.writeLifecycleError(w, r, err)
		return
	}

	released, err := h.lifecycle.Release(r.Context(), req.Tenant, actorOf(r, identity))
	if err != nil {
		h.writeLifecycleError(w, r, err)
		return
	}

	WriteJSON(w, http.StatusOK, newRouteResponse(released))
}

// getRoute returns one route.
func (h *InternalHandler) getRoute(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		WriteUnauthorized(w)
		return
	}

	route, err := h.lifecycle.Get(r.Context(), r.PathValue("tenant"))
	if err != nil {
		h.writeLifecycleError(w, r, err)
		return
	}

	if err := identity.AuthorizeProduct(route.Product); err != nil {
		h.writeForbidden(w, r, identity, route.Product, err)
		return
	}

	WriteJSON(w, http.StatusOK, newRouteResponse(route))
}

// listRoutes returns routes matching the query parameters.
func (h *InternalHandler) listRoutes(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFrom(r.Context())
	if !ok {
		WriteUnauthorized(w)
		return
	}

	query := r.URL.Query()
	filter := tenant.ListFilter{After: query.Get("after")}

	var requested *tenant.Product
	if value := query.Get("product"); value != "" {
		product, err := tenant.ParseProduct(value)
		if err != nil {
			WriteBadRequest(w, "The \"product\" parameter must name a product the Gateway routes for.")
			return
		}
		requested = &product
	}

	if value := query.Get("status"); value != "" {
		status, err := tenant.ParseStatus(value)
		if err != nil {
			WriteBadRequest(w, "The \"status\" parameter must be PROVISIONING, ACTIVE, or DISABLED.")
			return
		}
		filter.Status = &status
	}

	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 {
			WriteBadRequest(w, "The \"limit\" parameter must be a positive integer.")
			return
		}
		filter.Limit = limit
	}

	filter, err := identity.AuthorizeList(requested, filter)
	if err != nil {
		h.writeLifecycleError(w, r, err)
		return
	}

	routes, err := h.lifecycle.List(r.Context(), filter)
	if err != nil {
		h.writeLifecycleError(w, r, err)
		return
	}

	response := listResponse{Routes: make([]routeResponse, 0, len(routes))}
	for _, route := range routes {
		response.Routes = append(response.Routes, newRouteResponse(route))
	}
	if len(routes) > 0 && len(routes) == filter.ClampLimit() {
		response.NextAfter = routes[len(routes)-1].TenantSlug
	}

	WriteJSON(w, http.StatusOK, response)
}

// platformPolicy returns the authoritative slug and reserved-host rules
// (§10, [PROPOSED] in the plan).
//
// It is exposed so that a product validates a slug before calling reserve,
// which turns a 409 at provisioning time into a validation error at
// form-submission time. It requires authentication but no product
// authorization: the response is static platform configuration, identical for
// every caller, and it discloses nothing about any tenant.
func (h *InternalHandler) platformPolicy(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, tenant.Policy())
}

// --- helpers ---

// authorizeStoredProduct reads the route and authorizes the caller against the
// product the registry says it belongs to.
//
// This is the plan's rule for disable and release: the product is not taken
// from the caller, so a caller cannot name a product that does not match the
// route it is acting on.
//
// The read-then-mutate pair is not a race that matters. A slug's product is
// written once, by Reserve, and tenant_slug is unique, so the product cannot
// change between this read and the mutation — and the mutation itself is a
// compare-and-swap on status, so a concurrent status change is caught by the
// repository rather than applied against stale state.
func (h *InternalHandler) authorizeStoredProduct(ctx context.Context, identity security.ServiceIdentity, slug string) (tenant.Route, error) {
	route, err := h.lifecycle.Get(ctx, slug)
	if err != nil {
		return tenant.Route{}, err
	}

	if err := identity.AuthorizeProduct(route.Product); err != nil {
		return tenant.Route{}, err
	}

	return route, nil
}

// actorOf builds the audit identity for a mutation.
func actorOf(r *http.Request, identity security.ServiceIdentity) tenant.Actor {
	return tenant.Actor{
		Service:   identity.Service,
		RequestID: RequestIDFrom(r.Context()),
	}
}

// decode reads a JSON request body.
//
// Unknown fields are rejected. This is an API between services of the same
// platform, and a misspelled field is far more likely to be a bug in a caller
// than a forward-compatibility requirement; silently ignoring `"prodcut"`
// would turn a typo into a 400 about a missing product, which sends the reader
// to the wrong place.
func (h *InternalHandler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	// The outer middleware already bounded the body at server.max_body_bytes;
	// this is the tighter bound that actually applies to a lifecycle call.
	body := http.MaxBytesReader(w, r.Body, maxInternalBodyBytes)

	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			WritePayloadTooLarge(w)
			return false
		}
		if errors.Is(err, io.EOF) {
			WriteBadRequest(w, "A JSON request body is required.")
			return false
		}
		WriteBadRequest(w, "The request body is not valid JSON.")
		return false
	}

	// Two JSON documents in one body is a client bug. Using the first and
	// ignoring the rest would hide it, and which of the two was acted on would
	// depend on the parser.
	if decoder.More() {
		WriteBadRequest(w, "The request body must contain exactly one JSON object.")
		return false
	}

	return true
}

// writeForbidden records and reports a product authorization failure.
func (h *InternalHandler) writeForbidden(w http.ResponseWriter, r *http.Request, identity security.ServiceIdentity, product tenant.Product, err error) {
	h.logger.Warn("internal API call is not authorized for this product",
		"request_id", RequestIDFrom(r.Context()),
		"service", identity.Service,
		"product", product.String(),
		"method", r.Method,
		"path", r.URL.Path,
		"error", err,
	)
	WriteForbidden(w)
}

// writeLifecycleError maps a lifecycle error to the §13.3 status table.
//
// The mapping is exhaustive over the sentinels the tenant package defines, and
// everything else is a 503. That default is deliberate: the lifecycle API's
// only dependency is the registry, so an error it does not recognise came from
// the registry, and 503 tells a provisioning job to retry where 500 tells it
// not to. The cost is that a genuine bug also reports 503; the cause is in the
// log, and a metric for it lands in M8.
func (h *InternalHandler) writeLifecycleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, security.ErrForbidden):
		WriteForbidden(w)

	case errors.Is(err, tenant.ErrNotFound):
		WriteRouteNotFound(w)

	case errors.Is(err, tenant.ErrAlreadyExists):
		WriteConflict(w, "The tenant slug is already reserved.")

	case errors.Is(err, tenant.ErrInvalidTransition):
		WriteConflict(w, "The route cannot move to that status from its current one.")

	case errors.Is(err, tenant.ErrConflict):
		WriteConflict(w, "The request conflicts with the stored route.")

	case errors.Is(err, tenant.ErrInvalidSlug):
		WriteBadRequest(w, "The tenant slug does not satisfy the platform slug rules. See GET /internal/v1/platform-policy.")

	case errors.Is(err, tenant.ErrReservedHost):
		WriteBadRequest(w, "The tenant slug is a reserved platform hostname and can never become a tenant.")

	case errors.Is(err, tenant.ErrInvalidProduct):
		WriteBadRequest(w, "The \"product\" field must name a product the Gateway routes for.")

	case errors.Is(err, tenant.ErrInvalidStatus):
		WriteBadRequest(w, "The \"status\" parameter must be PROVISIONING, ACTIVE, or DISABLED.")

	default:
		// Never echo the cause. It may carry a DSN, a driver error, or an
		// internal address (§69).
		h.logger.Error("lifecycle operation failed",
			"request_id", RequestIDFrom(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"error", err,
		)
		WriteRegistryUnavailable(w)
	}
}

// identitySlot is the per-request slot the authenticated caller is recorded in.
//
// It mirrors routing.State, and for the same reason: the identity is produced
// by the authentication wrapper but read by a middleware that sits outside it,
// and a context value cannot travel outward.
type identitySlot struct {
	identity security.ServiceIdentity
	set      bool
}

// Set records the authenticated caller.
func (s *identitySlot) Set(identity security.ServiceIdentity) {
	if s == nil {
		return
	}
	s.identity = identity
	s.set = true
}

// Get returns the recorded caller and whether one was recorded.
func (s *identitySlot) Get() (security.ServiceIdentity, bool) {
	if s == nil {
		return security.ServiceIdentity{}, false
	}
	return s.identity, s.set
}

type identitySlotKey struct{}

// withIdentitySlot returns a context carrying slot.
func withIdentitySlot(ctx context.Context, slot *identitySlot) context.Context {
	return context.WithValue(ctx, identitySlotKey{}, slot)
}

// SetIdentity records the authenticated caller in the request's slot.
//
// It is a no-op when no slot is present, which happens only in a chain that
// omitted the middleware installing one. Nothing is lost in that case: with no
// slot the handlers see no identity either, and they refuse the request.
func SetIdentity(ctx context.Context, identity security.ServiceIdentity) {
	if slot, ok := ctx.Value(identitySlotKey{}).(*identitySlot); ok {
		slot.Set(identity)
	}
}

// IdentityFrom returns the authenticated caller recorded for this request.
//
// The bool matters: a request with no identity is one that never passed
// authentication, and treating its absence as an empty identity would make
// "unauthenticated" and "authenticated as the empty service" the same thing.
func IdentityFrom(ctx context.Context) (security.ServiceIdentity, bool) {
	slot, ok := ctx.Value(identitySlotKey{}).(*identitySlot)
	if !ok {
		return security.ServiceIdentity{}, false
	}
	return slot.Get()
}
