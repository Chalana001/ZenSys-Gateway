package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/security"
	"github.com/zensys/gateway/internal/tenant"
)

// --- test tokens ---

// Distinct, long enough to look like real service tokens, and deliberately not
// prefixes of one another so that a length-insensitive comparison cannot pass
// by accident.
const (
	posServiceToken = "pos-0123456789abcdef0123456789abcdef"
	gmsServiceToken = "gms-fedcba9876543210fedcba9876543210"

	// platformServiceToken manages both products. It is what makes the
	// service's own product-assertion check reachable: authorization can only
	// let a mismatched assertion through when the caller may manage both.
	platformServiceToken = "platform-abcdef0123456789abcdef0123456789"
)

// --- an in-memory registry ---

// memRepository is a complete tenant.Repository backed by a map.
//
// The lifecycle tests need the *real* Service and the real HTTP handler, so a
// stub that only records calls would not do: the state machine, the
// compare-and-swap, and the audit events all have to behave the way the
// database makes them behave, or the tests prove nothing about the API. This
// mirrors internal/tenant/mysql_repository.go's contract, including that
// Transition is a compare-and-swap on the stored status and that a no-op is an
// ErrConflict rather than a silent success.
type memRepository struct {
	mu     sync.Mutex
	routes map[string]tenant.Route
	events []tenant.Event
	now    func() time.Time

	// failWith makes every method return this error, for the registry-outage
	// path.
	failWith error
}

func newMemRepository() *memRepository {
	return &memRepository{
		routes: map[string]tenant.Route{},
		now:    time.Now,
	}
}

func (m *memRepository) seed(slug string, product tenant.Product, status tenant.Status) tenant.Route {
	m.mu.Lock()
	defer m.mu.Unlock()

	route := tenant.Route{
		TenantSlug: slug,
		Product:    product,
		Status:     status,
		CreatedAt:  m.now().UTC(),
		UpdatedAt:  m.now().UTC(),
	}
	m.routes[slug] = route
	return route
}

func (m *memRepository) GetBySlug(_ context.Context, slug string) (tenant.Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWith != nil {
		return tenant.Route{}, m.failWith
	}
	route, ok := m.routes[slug]
	if !ok {
		return tenant.Route{}, fmt.Errorf("%w: %q", tenant.ErrNotFound, slug)
	}
	return route, nil
}

func (m *memRepository) List(_ context.Context, filter tenant.ListFilter) ([]tenant.Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWith != nil {
		return nil, m.failWith
	}

	slugs := make([]string, 0, len(m.routes))
	for slug := range m.routes {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	out := make([]tenant.Route, 0, len(slugs))
	for _, slug := range slugs {
		route := m.routes[slug]

		if filter.After != "" && slug <= filter.After {
			continue
		}
		if filter.Product != nil && route.Product != *filter.Product {
			continue
		}
		if filter.Status != nil && route.Status != *filter.Status {
			continue
		}
		out = append(out, route)
		if len(out) == filter.ClampLimit() {
			break
		}
	}
	return out, nil
}

func (m *memRepository) Reserve(_ context.Context, route tenant.Route, event tenant.Event) (tenant.Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWith != nil {
		return tenant.Route{}, m.failWith
	}
	if _, exists := m.routes[route.TenantSlug]; exists {
		return tenant.Route{}, fmt.Errorf("%w: %q", tenant.ErrAlreadyExists, route.TenantSlug)
	}

	route.CreatedAt = m.now().UTC()
	route.UpdatedAt = route.CreatedAt
	m.routes[route.TenantSlug] = route
	m.events = append(m.events, event)

	return route, nil
}

func (m *memRepository) Transition(_ context.Context, slug string, from, to tenant.Status, event tenant.Event) (tenant.Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWith != nil {
		return tenant.Route{}, m.failWith
	}

	current, ok := m.routes[slug]
	if !ok {
		return tenant.Route{}, fmt.Errorf("%w: %q", tenant.ErrNotFound, slug)
	}
	if current.Status != from {
		return tenant.Route{}, fmt.Errorf("%w: %q moved from %s to %s while the transition was in flight",
			tenant.ErrConflict, slug, from, current.Status)
	}

	current.Status = to
	current.UpdatedAt = m.now().UTC()
	m.routes[slug] = current
	m.events = append(m.events, event)

	return current, nil
}

func (m *memRepository) Release(_ context.Context, slug string, event tenant.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWith != nil {
		return m.failWith
	}
	if _, ok := m.routes[slug]; !ok {
		return fmt.Errorf("%w: %q", tenant.ErrNotFound, slug)
	}

	delete(m.routes, slug)
	m.events = append(m.events, event)
	return nil
}

func (m *memRepository) Ping(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failWith
}

func (m *memRepository) recordedEvents() []tenant.Event {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]tenant.Event, len(m.events))
	copy(out, m.events)
	return out
}

func (m *memRepository) statusOf(slug string) (tenant.Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	route, ok := m.routes[slug]
	return route.Status, ok
}

// --- fixtures ---

// internalFixture wires the real lifecycle service, the real cache, and the
// real internal handler together.
//
// Everything is real except the database. The authenticator is the production
// tokenAuth, the service is the production tenant.Service, and the chain is the
// production StandardChain — so a failure here is a failure of the code the
// process runs, not of a stand-in for it.
type internalFixture struct {
	internal http.Handler
	public   http.Handler
	repo     *memRepository
	cache    *tenant.Cache
	upstream *recordingUpstream
	logs     *bytes.Buffer
}

func newInternalFixture(t *testing.T, cfg *config.Config, tune ...func(*memRepository)) *internalFixture {
	t.Helper()

	repo := newMemRepository()
	for _, fn := range tune {
		fn(repo)
	}

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cache := tenant.NewCache(repo, tenant.CacheOptions{
		RouteTTL:    cfg.Cache.RouteTTL.Std(),
		NegativeTTL: cfg.Cache.NegativeTTL.Std(),
		MaxEntries:  cfg.Cache.MaxEntries,
	})

	// The lifecycle service reads through the repository and drops the cached
	// route after every mutation — the wiring the milestone's first exit
	// criterion is about.
	lifecycle := tenant.NewService(repo, logger).WithInvalidator(cache)

	auth, err := security.NewTokenAuthenticator([]security.TokenSpec{
		{Service: "pos", Token: posServiceToken, Products: []tenant.Product{tenant.ProductPOS}},
		{Service: "gms", Token: gmsServiceToken, Products: []tenant.Product{tenant.ProductGMS}},
		{Service: "platform", Token: platformServiceToken, Products: []tenant.Product{tenant.ProductPOS, tenant.ProductGMS}},
	})
	if err != nil {
		t.Fatalf("build the token authenticator: %v", err)
	}

	internalMux := http.NewServeMux()
	NewHealthHandler(logger, nil).Register(internalMux)
	NewInternalHandler(logger, auth, lifecycle).Register(internalMux)
	internalMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusNotFound, ErrNotFound, "No such endpoint.")
	})

	options := ChainOptions{
		RequestIDHeader: cfg.Proxy.RequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
	}

	// The public side of the same process, sharing the same cache. It is what
	// makes "the next request reflects the change immediately" observable
	// rather than asserted.
	apex := NewApexHandler(cfg.Platform.ApexBehavior, cfg.Platform.LandingDir, logger)
	upstream := &recordingUpstream{}
	public := StandardChain(
		NewPublicHandler(cfg, logger, routing.NewResolver(cache), apex, upstream),
		logger, options,
	)

	return &internalFixture{
		internal: StandardChain(internalMux, logger, options),
		public:   public,
		repo:     repo,
		cache:    cache,
		upstream: upstream,
		logs:     logs,
	}
}

// --- request helpers ---

// call issues a lifecycle request.
//
// body is encoded as JSON when non-empty. A token of "" sends no Authorization
// header at all, which is how the unauthenticated case is expressed.
func (f *internalFixture) call(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = trustedPeer
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	f.internal.ServeHTTP(rec, req)
	return rec
}

// post is the common case: a JSON body with the POS token.
func (f *internalFixture) post(t *testing.T, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, http.MethodPost, path, token, body)
}

// get issues a GET with the given token.
func (f *internalFixture) get(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, http.MethodGet, path, token, "")
}

// decode unmarshals a response body, failing the test when it is not JSON.
func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response body is not valid JSON (%v): %s", err, rec.Body.String())
	}
	return out
}

// errorCode returns the machine-readable error code of a response.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) ErrorCode {
	t.Helper()
	return decode[ErrorResponse](t, rec).Error
}

// wantStatus asserts the status code and, on failure, prints the body — which
// for an error response is the only place the reason appears.
func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()

	if rec.Code != want {
		t.Fatalf("status: got %d, want %d; body: %s", rec.Code, want, strings.TrimSpace(rec.Body.String()))
	}
}

// --- exit criterion: authentication ---

// TestInternal_UnauthenticatedRequestsAreRefused covers the 401 half of the
// authentication criterion.
//
// Every lifecycle endpoint is tried, not just one: authentication is applied
// by a wrapper around the whole subtree, and a test that checked one endpoint
// would still pass if a later endpoint were registered outside that wrapper.
func TestInternal_UnauthenticatedRequestsAreRefused(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	endpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/internal/v1/tenant-routes/reserve", `{"tenant":"garage1","product":"POS"}`},
		{http.MethodPost, "/internal/v1/tenant-routes/activate", `{"tenant":"garage1","product":"POS"}`},
		{http.MethodPost, "/internal/v1/tenant-routes/disable", `{"tenant":"garage1"}`},
		{http.MethodPost, "/internal/v1/tenant-routes/release", `{"tenant":"garage1","confirm":"garage1"}`},
		{http.MethodGet, "/internal/v1/tenant-routes/garage1", ""},
		{http.MethodGet, "/internal/v1/tenant-routes", ""},
		{http.MethodGet, "/internal/v1/platform-policy", ""},
	}

	for _, endpoint := range endpoints {
		t.Run(endpoint.method+" "+endpoint.path, func(t *testing.T) {
			rec := f.call(t, endpoint.method, endpoint.path, "", endpoint.body)
			wantStatus(t, rec, http.StatusUnauthorized)

			if code := errorCode(t, rec); code != ErrUnauthorized {
				t.Errorf("error code: got %q, want %q", code, ErrUnauthorized)
			}
		})
	}
}

// TestInternal_UnknownAndMalformedTokensAreRefused covers the rest of the 401
// surface. The response is identical in every case, which is the property that
// stops an unauthenticated prober from learning anything about the token set.
func TestInternal_UnknownAndMalformedTokensAreRefused(t *testing.T) {
	f := newInternalFixture(t, testConfig())
	path := "/internal/v1/tenant-routes/garage1"

	tokens := []struct {
		name  string
		token string
	}{
		{name: "unknown token", token: "not-a-configured-token-0000000000000000"},
		{name: "prefix of a real token", token: posServiceToken[:len(posServiceToken)-1]},
		{name: "real token with a suffix", token: posServiceToken + "x"},
	}

	// The GMS token is deliberately absent from this list. It authenticates
	// successfully and is then refused by authorization, which is a 403 rather
	// than a 401 — asserted in TestInternal_ReadingAnotherProductsRouteIsForbidden.
	for _, tt := range tokens {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.get(t, path, tt.token)
			wantStatus(t, rec, http.StatusUnauthorized)
		})
	}

	// A malformed header must be refused without reaching the comparison at
	// all. These are issued directly so the raw header can be controlled.
	headers := []struct {
		name   string
		values []string
	}{
		{name: "no scheme", values: []string{posServiceToken}},
		{name: "wrong scheme", values: []string{"Basic " + posServiceToken}},
		{name: "scheme with no token", values: []string{"Bearer "}},
		{name: "two headers", values: []string{"Bearer " + posServiceToken, "Bearer " + gmsServiceToken}},
	}

	for _, tt := range headers {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			for _, v := range tt.values {
				req.Header.Add("Authorization", v)
			}
			rec := httptest.NewRecorder()
			f.internal.ServeHTTP(rec, req)

			wantStatus(t, rec, http.StatusUnauthorized)
		})
	}
}

// --- exit criterion: a POS token cannot touch a GMS tenant ---

// TestInternal_POSServiceCannotTouchGMSTenants is the §42 criterion, exercised
// on every mutating endpoint.
func TestInternal_POSServiceCannotTouchGMSTenants(t *testing.T) {
	f := newInternalFixture(t, testConfig(), func(repo *memRepository) {
		repo.seed("garage1", tenant.ProductGMS, tenant.StatusActive)
	})

	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "reserve a GMS tenant",
			path: "/internal/v1/tenant-routes/reserve",
			body: `{"tenant":"newgms","product":"GMS"}`,
		},
		{
			name: "activate a GMS tenant",
			path: "/internal/v1/tenant-routes/activate",
			body: `{"tenant":"garage1","product":"GMS"}`,
		},
		{
			name: "disable a GMS tenant",
			path: "/internal/v1/tenant-routes/disable",
			body: `{"tenant":"garage1"}`,
		},
		{
			name: "release a GMS tenant",
			path: "/internal/v1/tenant-routes/release",
			body: `{"tenant":"garage1","confirm":"garage1"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.post(t, tt.path, posServiceToken, tt.body)
			wantStatus(t, rec, http.StatusForbidden)

			if code := errorCode(t, rec); code != ErrForbidden {
				t.Errorf("error code: got %q, want %q", code, ErrForbidden)
			}
		})
	}

	// The GMS tenant must be untouched. A 403 that still performed the
	// mutation would satisfy a test that only looked at the status code.
	t.Run("the GMS route is unchanged", func(t *testing.T) {
		status, exists := f.repo.statusOf("garage1")
		if !exists {
			t.Fatal("the GMS route was released by a POS token")
		}
		if status != tenant.StatusActive {
			t.Errorf("the GMS route status changed to %s", status)
		}
		if _, exists := f.repo.statusOf("newgms"); exists {
			t.Error("a POS token reserved a GMS tenant")
		}
	})

	// And no audit event was written, because no mutation happened.
	if events := f.repo.recordedEvents(); len(events) != 0 {
		t.Errorf("a refused request recorded %d audit events: %+v", len(events), events)
	}
}

// TestInternal_GMSServiceCannotTouchPOSTenants is the mirror image, so the
// check cannot be a rule that only happens to work in one direction.
func TestInternal_GMSServiceCannotTouchPOSTenants(t *testing.T) {
	f := newInternalFixture(t, testConfig(), func(repo *memRepository) {
		repo.seed("mangala", tenant.ProductPOS, tenant.StatusActive)
	})

	rec := f.post(t, "/internal/v1/tenant-routes/disable", gmsServiceToken, `{"tenant":"mangala"}`)
	wantStatus(t, rec, http.StatusForbidden)

	if _, exists := f.repo.statusOf("mangala"); !exists {
		t.Fatal("a GMS token released a POS route")
	}
}

// TestInternal_ReadingAnotherProductsRouteIsForbidden covers the read side.
func TestInternal_ReadingAnotherProductsRouteIsForbidden(t *testing.T) {
	f := newInternalFixture(t, testConfig(), func(repo *memRepository) {
		repo.seed("garage1", tenant.ProductGMS, tenant.StatusActive)
	})

	rec := f.get(t, "/internal/v1/tenant-routes/garage1", posServiceToken)
	wantStatus(t, rec, http.StatusForbidden)
}

// --- exit criterion: duplicate reserve ---

func TestInternal_DuplicateReserveIsAConflict(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	first := f.post(t, "/internal/v1/tenant-routes/reserve", posServiceToken, `{"tenant":"garage1","product":"POS"}`)
	wantStatus(t, first, http.StatusCreated)

	second := f.post(t, "/internal/v1/tenant-routes/reserve", posServiceToken, `{"tenant":"garage1","product":"POS"}`)
	wantStatus(t, second, http.StatusConflict)

	if code := errorCode(t, second); code != ErrConflict {
		t.Errorf("error code: got %q, want %q", code, ErrConflict)
	}
}

// --- exit criterion: release requires a matching confirmation ---

func TestInternal_ReleaseRequiresMatchingConfirmation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    int
		confirm bool
	}{
		{name: "no confirm field", body: `{"tenant":"garage1"}`, want: http.StatusBadRequest},
		{name: "empty confirm", body: `{"tenant":"garage1","confirm":""}`, want: http.StatusBadRequest},
		{name: "mismatched confirm", body: `{"tenant":"garage1","confirm":"garage2"}`, want: http.StatusBadRequest},
		{name: "confirm is the product", body: `{"tenant":"garage1","confirm":"POS"}`, want: http.StatusBadRequest},
		{name: "matching confirm", body: `{"tenant":"garage1","confirm":"garage1"}`, want: http.StatusOK, confirm: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInternalFixture(t, testConfig(), func(repo *memRepository) {
				repo.seed("garage1", tenant.ProductPOS, tenant.StatusActive)
			})

			rec := f.post(t, "/internal/v1/tenant-routes/release", posServiceToken, tt.body)
			wantStatus(t, rec, tt.want)

			_, exists := f.repo.statusOf("garage1")
			if tt.confirm && exists {
				t.Error("a confirmed release did not delete the route")
			}
			if !tt.confirm && !exists {
				t.Error("an unconfirmed release deleted the route")
			}
		})
	}
}

// TestInternal_ReleaseChecksConfirmationBeforeReadingTheRegistry pins the
// ordering, which is a disclosure property and not an optimisation.
//
// If the registry were read first, an unauthenticated-for-this-product caller
// could tell "no such slug" (404) from "exists but not yours" (403) — and the
// second answer confirms the slug exists. Checking the confirmation first means
// a request that cannot possibly succeed never reaches the lookup.
func TestInternal_ReleaseChecksConfirmationBeforeReadingTheRegistry(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	rec := f.post(t, "/internal/v1/tenant-routes/release", posServiceToken, `{"tenant":"does-not-exist"}`)
	wantStatus(t, rec, http.StatusBadRequest)
}

// --- exit criterion: every mutation invalidates the cache ---

// TestInternal_MutationsTakeEffectOnTheNextPublicRequest is the criterion that
// matters most, and the only one that spans both listeners.
//
// It drives the public pipeline and the lifecycle API against one cache, so
// "the next request reflects it immediately" is measured rather than asserted.
// The TTLs are set to an hour: if invalidation were broken, the assertions
// would fail rather than pass slowly.
func TestInternal_MutationsTakeEffectOnTheNextPublicRequest(t *testing.T) {
	cfg := testConfig()
	cfg.Cache.RouteTTL = config.Duration(time.Hour)
	cfg.Cache.NegativeTTL = config.Duration(time.Hour)

	f := newInternalFixture(t, cfg)

	const host = "garage1.zensys.lk"
	const slug = "garage1"

	// 1. A public request for a slug that does not exist yet. This caches the
	//    *absence*, which is the harder case: a plain route cache would be
	//    cleared by the reservation, but a negative entry is keyed on a slug
	//    that had no row when it was written.
	before := do(t, f.public, host, "/")
	if before.Code != http.StatusNotFound {
		t.Fatalf("an unregistered tenant should be 404, got %d", before.Code)
	}

	// 2. Reserve and activate through the lifecycle API.
	rec := f.post(t, "/internal/v1/tenant-routes/reserve", posServiceToken, `{"tenant":"garage1","product":"POS"}`)
	wantStatus(t, rec, http.StatusCreated)

	reserved := decode[routeResponse](t, rec)
	if reserved.Status != string(tenant.StatusProvisioning) {
		t.Errorf("a new route should be PROVISIONING, got %s", reserved.Status)
	}

	// A reserved-but-not-active tenant is still not routable. It must be the
	// same 404 as before, not a leak that something changed.
	midway := do(t, f.public, host, "/")
	if midway.Code != http.StatusNotFound {
		t.Fatalf("a PROVISIONING tenant must not be routable, got %d", midway.Code)
	}

	rec = f.post(t, "/internal/v1/tenant-routes/activate", posServiceToken, `{"tenant":"garage1","product":"POS"}`)
	wantStatus(t, rec, http.StatusOK)

	// 3. The very next public request must route. No sleeping, no TTL.
	after := do(t, f.public, host, "/")
	if after.Code != http.StatusOK {
		t.Fatalf("an activated tenant must be routable on the next request, got %d", after.Code)
	}
	if !f.upstream.called {
		t.Error("the upstream was never reached for an activated tenant")
	}
	if f.upstream.route.TenantSlug != slug {
		t.Errorf("routed to %q, want %q", f.upstream.route.TenantSlug, slug)
	}

	// 4. Disabling must also take effect immediately — this is the direction
	//    where a stale cache keeps serving a tenant that was just suspended.
	rec = f.post(t, "/internal/v1/tenant-routes/disable", posServiceToken, `{"tenant":"garage1"}`)
	wantStatus(t, rec, http.StatusOK)

	disabled := do(t, f.public, host, "/")
	if disabled.Code != http.StatusNotFound {
		t.Fatalf("a disabled tenant must stop routing on the next request, got %d", disabled.Code)
	}

	// 5. And the 404 must be byte-identical to the unknown-tenant 404, which
	//    is the property that keeps tenant existence from leaking.
	unknown := do(t, f.public, "nobody.zensys.lk", "/")
	if disabled.Body.String() != unknown.Body.String() {
		t.Errorf("disabled and unknown responses differ:\n disabled: %s\n  unknown: %s",
			disabled.Body.String(), unknown.Body.String())
	}

	// 6. Releasing removes the row, and the next request must miss again.
	rec = f.post(t, "/internal/v1/tenant-routes/release", posServiceToken, `{"tenant":"garage1","confirm":"garage1"}`)
	wantStatus(t, rec, http.StatusOK)

	if _, exists := f.repo.statusOf(slug); exists {
		t.Fatal("the route was not released")
	}
	released := do(t, f.public, host, "/")
	if released.Code != http.StatusNotFound {
		t.Fatalf("a released tenant must not route, got %d", released.Code)
	}
}

// TestInternal_InvalidationIsNotCarriedByTheRouteCacheItself proves the
// invalidation comes from the lifecycle service and not from the cache
// happening to expire.
//
// Without it, the test above would pass on a cache with a zero TTL for the
// wrong reason.
func TestInternal_InvalidationIsNotCarriedByTheRouteCacheItself(t *testing.T) {
	cfg := testConfig()
	cfg.Cache.RouteTTL = config.Duration(time.Hour)
	cfg.Cache.NegativeTTL = config.Duration(time.Hour)

	f := newInternalFixture(t, cfg)

	// Seed an ACTIVE route and read it through the public path so it is cached.
	f.repo.seed("mangala", tenant.ProductPOS, tenant.StatusActive)
	if rec := do(t, f.public, "mangala.zensys.lk", "/"); rec.Code != http.StatusOK {
		t.Fatalf("seeded tenant should route, got %d", rec.Code)
	}

	// Change the registry behind the cache's back. With an hour-long TTL the
	// cache must keep serving the old answer — proving the TTL is real and
	// that the assertions above were not passing on an always-miss cache.
	f.repo.mu.Lock()
	route := f.repo.routes["mangala"]
	route.Status = tenant.StatusDisabled
	f.repo.routes["mangala"] = route
	f.repo.mu.Unlock()

	if rec := do(t, f.public, "mangala.zensys.lk", "/"); rec.Code != http.StatusOK {
		t.Fatalf("the cache served the new value without an invalidation; got %d, which means the TTL is not being honoured", rec.Code)
	}

	// Now invalidate the way a mutation does.
	f.cache.Invalidate("mangala")

	if rec := do(t, f.public, "mangala.zensys.lk", "/"); rec.Code != http.StatusNotFound {
		t.Fatalf("after an invalidation the new value must be visible, got %d", rec.Code)
	}
}

// --- exit criterion: the internal API is not reachable from the public one ---

// TestInternal_LifecyclePathsAreNotServedOnThePublicListener covers the
// isolation criterion.
//
// The discriminating observable is the *upstream*: if the lifecycle API were
// mounted on the public listener, the request would be answered by the
// Gateway and the product would never see it. Asserting on the upstream rather
// than on the status code is what makes this a real check — a 200 could
// otherwise be either.
func TestInternal_LifecyclePathsAreNotServedOnThePublicListener(t *testing.T) {
	f := newInternalFixture(t, testConfig(), func(repo *memRepository) {
		repo.seed("mangala", tenant.ProductPOS, tenant.StatusActive)
	})

	paths := []string{
		"/internal/v1/tenant-routes",
		"/internal/v1/tenant-routes/mangala",
		"/internal/v1/platform-policy",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			f.upstream.called = false

			// With a registered tenant host the path belongs to the product:
			// the Gateway has no business reserving it.
			rec := do(t, f.public, "mangala.zensys.lk", path)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected the request to be proxied to the product, got %d", rec.Code)
			}
			if !f.upstream.called {
				t.Fatal("the lifecycle path was answered by the Gateway instead of being proxied to the product")
			}

			// The response must not be a lifecycle response. A route listing
			// or a policy document here would mean the API is publicly
			// reachable.
			body := rec.Body.String()
			if strings.Contains(body, "reserved_hosts") || strings.Contains(body, "next_after") {
				t.Errorf("the public listener returned a lifecycle response: %s", body)
			}
		})
	}

	// With a host that is not a tenant, the same path is an ordinary 404 from
	// the public contract.
	rec := do(t, f.public, "nobody.zensys.lk", "/internal/v1/tenant-routes")
	wantStatus(t, rec, http.StatusNotFound)
	if code := errorCode(t, rec); code != ErrUnknownTenant {
		t.Errorf("error code: got %q, want %q", code, ErrUnknownTenant)
	}
}

// --- request validation ---

func TestInternal_RequestValidation(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		want int
	}{
		{name: "empty body", path: "/internal/v1/tenant-routes/reserve", body: "x", want: http.StatusBadRequest},
		{name: "malformed JSON", path: "/internal/v1/tenant-routes/reserve", body: "{", want: http.StatusBadRequest},
		{name: "missing tenant", path: "/internal/v1/tenant-routes/reserve", body: `{"product":"POS"}`, want: http.StatusBadRequest},
		{name: "missing product", path: "/internal/v1/tenant-routes/reserve", body: `{"tenant":"garage1"}`, want: http.StatusBadRequest},
		{name: "unknown product", path: "/internal/v1/tenant-routes/reserve", body: `{"tenant":"garage1","product":"ERP"}`, want: http.StatusBadRequest},
		{name: "lowercase product", path: "/internal/v1/tenant-routes/reserve", body: `{"tenant":"garage1","product":"pos"}`, want: http.StatusBadRequest},
		{name: "misspelled field", path: "/internal/v1/tenant-routes/reserve", body: `{"tenant":"garage1","prodcut":"POS"}`, want: http.StatusBadRequest},
		{name: "invalid slug", path: "/internal/v1/tenant-routes/reserve", body: `{"tenant":"Garage 1","product":"POS"}`, want: http.StatusBadRequest},
		{name: "slug too short", path: "/internal/v1/tenant-routes/reserve", body: `{"tenant":"ab","product":"POS"}`, want: http.StatusBadRequest},
		{name: "reserved host as a slug", path: "/internal/v1/tenant-routes/reserve", body: `{"tenant":"www","product":"POS"}`, want: http.StatusBadRequest},
		{name: "two JSON documents", path: "/internal/v1/tenant-routes/reserve", body: `{"tenant":"garage1","product":"POS"}{"tenant":"x"}`, want: http.StatusBadRequest},
		{name: "JSON array instead of an object", path: "/internal/v1/tenant-routes/reserve", body: `["garage1"]`, want: http.StatusBadRequest},

		// activate requires the asserted product, because that assertion is
		// what stops a product activating a tenant it does not own.
		{name: "activate without a product", path: "/internal/v1/tenant-routes/activate", body: `{"tenant":"garage1"}`, want: http.StatusBadRequest},

		{name: "disable without a tenant", path: "/internal/v1/tenant-routes/disable", body: `{}`, want: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInternalFixture(t, testConfig())
			rec := f.post(t, tt.path, posServiceToken, tt.body)
			wantStatus(t, rec, tt.want)

			if code := errorCode(t, rec); code != ErrBadRequest {
				t.Errorf("error code: got %q, want %q", code, ErrBadRequest)
			}
		})
	}
}

// TestInternal_UnknownPathsAndMethodsAreHandledInTheErrorContract keeps a
// machine client from having to parse two content types for one class of
// failure.
func TestInternal_UnknownPathsAndMethodsAreHandledInTheErrorContract(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	t.Run("unknown path under the API prefix", func(t *testing.T) {
		rec := f.get(t, "/internal/v1/tenant-routes/nope/extra", posServiceToken)
		wantStatus(t, rec, http.StatusNotFound)
		if code := errorCode(t, rec); code != ErrNotFound {
			t.Errorf("error code: got %q, want %q", code, ErrNotFound)
		}
	})

	t.Run("wrong method on a known path", func(t *testing.T) {
		// A wildcard path with the wrong method. `GET /tenant-routes/reserve`
		// is *not* this case: it matches the GET-by-tenant endpoint, which is
		// correct — "reserve" is a legal slug — and returns a tenant lookup.
		rec := f.post(t, "/internal/v1/tenant-routes/mangala", posServiceToken, `{}`)
		wantStatus(t, rec, http.StatusMethodNotAllowed)

		if code := errorCode(t, rec); code != ErrMethodNotAllowed {
			t.Errorf("error code: got %q, want %q", code, ErrMethodNotAllowed)
		}

		// The Allow header must name what the endpoint does accept. Without
		// it a client is told the verb is wrong but not which one is right.
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
			t.Errorf("Allow header: got %q, want it to name GET", allow)
		}
	})

	t.Run("Allow is derived from the endpoint table", func(t *testing.T) {
		// A GET-only endpoint and a POST-only endpoint. Asserting on both is
		// what keeps the header from being a constant that happens to be right
		// for one of them.
		tests := []struct {
			path      string
			method    string
			wantAllow string
			notAllow  string
		}{
			{path: "/internal/v1/platform-policy", method: http.MethodPut, wantAllow: "GET", notAllow: "POST"},
			{path: "/internal/v1/tenant-routes/reserve", method: http.MethodPut, wantAllow: "POST", notAllow: "GET"},
		}

		for _, tt := range tests {
			t.Run(tt.path, func(t *testing.T) {
				req := httptest.NewRequest(tt.method, tt.path, strings.NewReader("{}"))
				req.Header.Set("Authorization", "Bearer "+posServiceToken)
				rec := httptest.NewRecorder()
				f.internal.ServeHTTP(rec, req)

				wantStatus(t, rec, http.StatusMethodNotAllowed)

				allow := rec.Header().Get("Allow")
				if !strings.Contains(allow, tt.wantAllow) {
					t.Errorf("Allow: got %q, want it to name %s", allow, tt.wantAllow)
				}
				if strings.Contains(allow, tt.notAllow) {
					t.Errorf("Allow: got %q, which must not name %s", allow, tt.notAllow)
				}
			})
		}
	})

	t.Run("unknown path outside the API prefix", func(t *testing.T) {
		rec := f.get(t, "/nope", posServiceToken)
		wantStatus(t, rec, http.StatusNotFound)
		if code := errorCode(t, rec); code != ErrNotFound {
			t.Errorf("error code: got %q, want %q", code, ErrNotFound)
		}
	})
}

// --- reads ---

func TestInternal_GetRoute(t *testing.T) {
	f := newInternalFixture(t, testConfig(), func(repo *memRepository) {
		repo.seed("mangala", tenant.ProductPOS, tenant.StatusActive)
	})

	t.Run("an existing route", func(t *testing.T) {
		rec := f.get(t, "/internal/v1/tenant-routes/mangala", posServiceToken)
		wantStatus(t, rec, http.StatusOK)

		body := decode[routeResponse](t, rec)
		if body.Tenant != "mangala" || body.Product != "POS" || body.Status != "ACTIVE" {
			t.Errorf("unexpected body: %+v", body)
		}
		if body.CreatedAt == "" || body.UpdatedAt == "" {
			t.Errorf("timestamps must be present: %+v", body)
		}
		// The documented wire format, pinned. A client parsing this has no
		// other way to know the shape.
		if !strings.HasSuffix(body.CreatedAt, "Z") || !strings.Contains(body.CreatedAt, ".") {
			t.Errorf("created_at %q is not the documented UTC millisecond form", body.CreatedAt)
		}
	})

	t.Run("an unknown route", func(t *testing.T) {
		rec := f.get(t, "/internal/v1/tenant-routes/nobody", posServiceToken)
		wantStatus(t, rec, http.StatusNotFound)
		if code := errorCode(t, rec); code != ErrNotFound {
			t.Errorf("error code: got %q, want %q", code, ErrNotFound)
		}
	})

	t.Run("an invalid slug is a bad request, not a missing route", func(t *testing.T) {
		rec := f.get(t, "/internal/v1/tenant-routes/NOT%20VALID", posServiceToken)
		wantStatus(t, rec, http.StatusBadRequest)
	})
}

func TestInternal_ListRoutes(t *testing.T) {
	seed := func(repo *memRepository) {
		repo.seed("mangala", tenant.ProductPOS, tenant.StatusActive)
		repo.seed("kandy", tenant.ProductPOS, tenant.StatusDisabled)
		repo.seed("garage1", tenant.ProductGMS, tenant.StatusActive)
		repo.seed("garage2", tenant.ProductGMS, tenant.StatusProvisioning)
	}

	t.Run("a POS token sees only POS routes", func(t *testing.T) {
		f := newInternalFixture(t, testConfig(), seed)

		rec := f.get(t, "/internal/v1/tenant-routes", posServiceToken)
		wantStatus(t, rec, http.StatusOK)

		body := decode[listResponse](t, rec)
		if len(body.Routes) != 2 {
			t.Fatalf("expected 2 POS routes, got %d: %+v", len(body.Routes), body.Routes)
		}
		for _, route := range body.Routes {
			if route.Product != "POS" {
				t.Errorf("a POS token was shown a %s route: %+v", route.Product, route)
			}
		}
	})

	t.Run("a GMS token sees only GMS routes", func(t *testing.T) {
		f := newInternalFixture(t, testConfig(), seed)

		rec := f.get(t, "/internal/v1/tenant-routes", gmsServiceToken)
		wantStatus(t, rec, http.StatusOK)

		body := decode[listResponse](t, rec)
		if len(body.Routes) != 2 {
			t.Fatalf("expected 2 GMS routes, got %d: %+v", len(body.Routes), body.Routes)
		}
		for _, route := range body.Routes {
			if route.Product != "GMS" {
				t.Errorf("a GMS token was shown a %s route: %+v", route.Product, route)
			}
		}
	})

	t.Run("asking for another product's routes is forbidden", func(t *testing.T) {
		f := newInternalFixture(t, testConfig(), seed)

		rec := f.get(t, "/internal/v1/tenant-routes?product=GMS", posServiceToken)
		wantStatus(t, rec, http.StatusForbidden)
	})

	t.Run("status filter", func(t *testing.T) {
		f := newInternalFixture(t, testConfig(), seed)

		rec := f.get(t, "/internal/v1/tenant-routes?status=DISABLED", posServiceToken)
		wantStatus(t, rec, http.StatusOK)

		body := decode[listResponse](t, rec)
		if len(body.Routes) != 1 || body.Routes[0].Tenant != "kandy" {
			t.Fatalf("unexpected result: %+v", body.Routes)
		}
	})

	t.Run("pagination", func(t *testing.T) {
		f := newInternalFixture(t, testConfig(), seed)

		rec := f.get(t, "/internal/v1/tenant-routes?limit=1", posServiceToken)
		wantStatus(t, rec, http.StatusOK)

		first := decode[listResponse](t, rec)
		if len(first.Routes) != 1 {
			t.Fatalf("expected 1 route, got %d", len(first.Routes))
		}
		if first.NextAfter != first.Routes[0].Tenant {
			t.Errorf("next_after: got %q, want %q", first.NextAfter, first.Routes[0].Tenant)
		}

		rec = f.get(t, "/internal/v1/tenant-routes?limit=1&after="+first.NextAfter, posServiceToken)
		wantStatus(t, rec, http.StatusOK)

		second := decode[listResponse](t, rec)
		if len(second.Routes) != 1 {
			t.Fatalf("expected 1 route on the second page, got %d", len(second.Routes))
		}
		if second.Routes[0].Tenant == first.Routes[0].Tenant {
			t.Error("the second page repeated the first page's route")
		}

		// This page is full, so it advertises another — even though it is the
		// last one. That is the documented contract: next_after is set when
		// the page is full, and a full page may be the last. Proving otherwise
		// would need one extra row fetched per request, and one extra row is
		// enough to push a request past the repository's page-size ceiling.
		if second.NextAfter == "" {
			t.Error("a full page must advertise the next one")
		}

		// Which is why the terminating condition is an empty page, not an
		// empty next_after. A client that loops on next_after makes one extra
		// request; a client that loops on the result being empty is exact.
		rec = f.get(t, "/internal/v1/tenant-routes?limit=1&after="+second.NextAfter, posServiceToken)
		wantStatus(t, rec, http.StatusOK)

		third := decode[listResponse](t, rec)
		if len(third.Routes) != 0 {
			t.Fatalf("expected an empty page past the end, got %+v", third.Routes)
		}
		if third.NextAfter != "" {
			t.Errorf("an empty page must not advertise another, got %q", third.NextAfter)
		}
	})

	t.Run("invalid query parameters", func(t *testing.T) {
		f := newInternalFixture(t, testConfig(), seed)

		for _, query := range []string{"?status=BOGUS", "?limit=0", "?limit=-1", "?limit=abc"} {
			rec := f.get(t, "/internal/v1/tenant-routes"+query, posServiceToken)
			wantStatus(t, rec, http.StatusBadRequest)
		}
	})

	t.Run("an empty result is an empty array, not null", func(t *testing.T) {
		f := newInternalFixture(t, testConfig())

		rec := f.get(t, "/internal/v1/tenant-routes", posServiceToken)
		wantStatus(t, rec, http.StatusOK)

		if !strings.Contains(rec.Body.String(), `"routes":[]`) {
			t.Errorf("expected an empty JSON array, got %s", rec.Body.String())
		}
	})
}

func TestInternal_PlatformPolicy(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	rec := f.get(t, "/internal/v1/platform-policy", posServiceToken)
	wantStatus(t, rec, http.StatusOK)

	body := decode[tenant.SlugPolicy](t, rec)
	if body.Pattern != tenant.SlugPattern {
		t.Errorf("pattern: got %q, want %q", body.Pattern, tenant.SlugPattern)
	}
	if body.MinLength != tenant.SlugMinLength || body.MaxLength != tenant.SlugMaxLength {
		t.Errorf("lengths: got %d..%d, want %d..%d",
			body.MinLength, body.MaxLength, tenant.SlugMinLength, tenant.SlugMaxLength)
	}
	if len(body.ReservedHosts) != len(tenant.ReservedHosts()) {
		t.Errorf("reserved hosts: got %d, want %d", len(body.ReservedHosts), len(tenant.ReservedHosts()))
	}

	// It requires authentication but no product authorization: it is the same
	// static document for every caller.
	if rec := f.get(t, "/internal/v1/platform-policy", gmsServiceToken); rec.Code != http.StatusOK {
		t.Errorf("a GMS token should be able to read the platform policy, got %d", rec.Code)
	}
}

// policyAccepts implements the published policy the way a product would: from
// the pattern, the lengths, and the reserved-host list in the response, and
// from nothing else.
//
// It deliberately does not call tenant.ValidateSlug. Reusing the Gateway's own
// validator would make the test below compare a function with itself; the
// property being checked is that the *document* is sufficient to predict the
// API's answer, which only an independent implementation can establish.
func policyAccepts(policy tenant.SlugPolicy, slug string) bool {
	if len(slug) < policy.MinLength || len(slug) > policy.MaxLength {
		return false
	}

	re, err := regexp.Compile(policy.Pattern)
	if err != nil {
		return false
	}
	if !re.MatchString(slug) {
		return false
	}

	for _, reserved := range policy.ReservedHosts {
		if slug == reserved {
			return false
		}
	}
	return true
}

// testAuthenticator builds the production token authenticator for tests that
// need one but do not need the whole fixture.
func testAuthenticator(t *testing.T) security.Authenticator {
	t.Helper()

	auth, err := security.NewTokenAuthenticator([]security.TokenSpec{
		{Service: "pos", Token: posServiceToken, Products: []tenant.Product{tenant.ProductPOS}},
		{Service: "gms", Token: gmsServiceToken, Products: []tenant.Product{tenant.ProductGMS}},
		{Service: "platform", Token: platformServiceToken, Products: []tenant.Product{tenant.ProductPOS, tenant.ProductGMS}},
	})
	if err != nil {
		t.Fatalf("build the token authenticator: %v", err)
	}
	return auth
}

// TestInternal_PlatformPolicyAgreesWithReserve is the point of the endpoint.
//
// A product validates a slug against this document before calling reserve. If
// the two disagree, the product's validation is worse than none: it would let
// through a slug that reserve then rejects, which is the 409-at-provisioning
// problem the endpoint exists to remove.
func TestInternal_PlatformPolicyAgreesWithReserve(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	rec := f.get(t, "/internal/v1/platform-policy", posServiceToken)
	wantStatus(t, rec, http.StatusOK)
	policy := decode[tenant.SlugPolicy](t, rec)

	candidates := []string{
		"ab",           // too short
		"a",            // too short
		"garage1",      // valid
		"Garage1",      // uppercase
		"-garage",      // leading hyphen
		"garage-",      // trailing hyphen
		"www",          // reserved
		"api",          // reserved
		"gar age",      // space
		"gar_age",      // underscore
		"garage.other", // dot
	}

	for _, candidate := range candidates {
		t.Run(candidate, func(t *testing.T) {
			// What a product would conclude from the policy document.
			documentAccepts := policyAccepts(policy, candidate)

			// What the API actually does. A slug already reserved by an
			// earlier subtest is a different answer, so each candidate gets a
			// fresh registry.
			fresh := newInternalFixture(t, testConfig())
			reserve := fresh.post(t, "/internal/v1/tenant-routes/reserve", posServiceToken,
				fmt.Sprintf(`{"tenant":%q,"product":"POS"}`, candidate))
			apiAccepts := reserve.Code == http.StatusCreated

			if documentAccepts != apiAccepts {
				t.Errorf("the policy document says %v but reserve answered %d; a product validating against it would be misled",
					documentAccepts, reserve.Code)
			}
		})
	}
}

// --- error mapping ---

// stubLifecycle returns a fixed error from every operation, so the error
// mapping can be exercised for conditions a working registry cannot produce.
type stubLifecycle struct {
	err error
}

func (s stubLifecycle) Get(context.Context, string) (tenant.Route, error) {
	return tenant.Route{}, s.err
}
func (s stubLifecycle) List(context.Context, tenant.ListFilter) ([]tenant.Route, error) {
	return nil, s.err
}
func (s stubLifecycle) Reserve(context.Context, string, tenant.Product, tenant.Actor) (tenant.Route, error) {
	return tenant.Route{}, s.err
}
func (s stubLifecycle) Activate(context.Context, string, *tenant.Product, tenant.Actor) (tenant.Route, error) {
	return tenant.Route{}, s.err
}
func (s stubLifecycle) Disable(context.Context, string, tenant.Actor) (tenant.Route, error) {
	return tenant.Route{}, s.err
}
func (s stubLifecycle) Release(context.Context, string, tenant.Actor) (tenant.Route, error) {
	return tenant.Route{}, s.err
}

// TestInternal_ErrorMapping pins the §13.3 status table.
//
// The illegal-transition case is here rather than in an end-to-end test
// because no request can reach it: every lifecycle state can move to both
// ACTIVE and DISABLED, and the endpoints only ever ask for one of those two.
// The mapping is still load-bearing — it is what a future state machine or a
// new endpoint will rely on — so it is tested directly rather than left
// unverified because it is currently unreachable.
func TestInternal_ErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		code ErrorCode
	}{
		{name: "not found", err: fmt.Errorf("%w: x", tenant.ErrNotFound), want: http.StatusNotFound, code: ErrNotFound},
		{name: "already exists", err: fmt.Errorf("%w: x", tenant.ErrAlreadyExists), want: http.StatusConflict, code: ErrConflict},
		{name: "invalid transition", err: fmt.Errorf("%w: x", tenant.ErrInvalidTransition), want: http.StatusConflict, code: ErrConflict},
		{name: "conflict", err: fmt.Errorf("%w: x", tenant.ErrConflict), want: http.StatusConflict, code: ErrConflict},
		{name: "invalid slug", err: fmt.Errorf("%w: x", tenant.ErrInvalidSlug), want: http.StatusBadRequest, code: ErrBadRequest},
		{name: "reserved host", err: fmt.Errorf("%w: x", tenant.ErrReservedHost), want: http.StatusBadRequest, code: ErrBadRequest},
		{name: "invalid product", err: fmt.Errorf("%w: x", tenant.ErrInvalidProduct), want: http.StatusBadRequest, code: ErrBadRequest},
		{name: "invalid status", err: fmt.Errorf("%w: x", tenant.ErrInvalidStatus), want: http.StatusBadRequest, code: ErrBadRequest},
		{name: "forbidden", err: fmt.Errorf("%w: x", security.ErrForbidden), want: http.StatusForbidden, code: ErrForbidden},
		{name: "registry down", err: fmt.Errorf("dial tcp: connection refused"), want: http.StatusServiceUnavailable, code: ErrRegistryUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			logger := discardLogger()
			handler := NewInternalHandler(logger, testAuthenticator(t), stubLifecycle{err: tt.err})

			mux := http.NewServeMux()
			handler.Register(mux)

			// The real chain, because the handler records the authenticated
			// caller in the slot RequestState installs. A bare mux would make
			// every request look unauthenticated and the test would be
			// measuring the wrong thing.
			chained := StandardChain(mux, logger, ChainOptions{
				RequestIDHeader: cfg.Proxy.RequestIDHeader,
				TrustedProxies:  trustedLoopback(t),
				MaxBodyBytes:    cfg.Server.MaxBodyBytes,
			})

			req := httptest.NewRequest(http.MethodGet, "/internal/v1/tenant-routes/garage1", nil)
			req.Header.Set("Authorization", "Bearer "+posServiceToken)
			rec := httptest.NewRecorder()
			chained.ServeHTTP(rec, req)

			wantStatus(t, rec, tt.want)
			if code := errorCode(t, rec); code != tt.code {
				t.Errorf("error code: got %q, want %q", code, tt.code)
			}
		})
	}
}

// TestInternal_UnrecognisedErrorsDoNotLeakTheirCause proves the 503 path does
// not echo a driver error, which could carry a DSN or an address (§69).
func TestInternal_UnrecognisedErrorsDoNotLeakTheirCause(t *testing.T) {
	const secret = "root:hunter2@tcp(127.0.0.1:3306)/zensys_gateway"

	cfg := testConfig()
	logger := discardLogger()
	handler := NewInternalHandler(logger, testAuthenticator(t), stubLifecycle{err: fmt.Errorf("registry query failed: %s", secret)})

	mux := http.NewServeMux()
	handler.Register(mux)
	chained := StandardChain(mux, logger, ChainOptions{
		RequestIDHeader: cfg.Proxy.RequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
	})

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/tenant-routes/garage1", nil)
	req.Header.Set("Authorization", "Bearer "+posServiceToken)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	wantStatus(t, rec, http.StatusServiceUnavailable)

	if strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("the response leaked the registry cause: %s", rec.Body.String())
	}
}

// TestInternal_RegistryOutageIsA503 covers the same path with the real
// service, so the mapping is not only proven against a stub.
func TestInternal_RegistryOutageIsA503(t *testing.T) {
	f := newInternalFixture(t, testConfig())
	f.repo.failWith = fmt.Errorf("dial tcp 127.0.0.1:3306: connect: connection refused")

	rec := f.post(t, "/internal/v1/tenant-routes/reserve", posServiceToken, `{"tenant":"garage1","product":"POS"}`)
	wantStatus(t, rec, http.StatusServiceUnavailable)

	if code := errorCode(t, rec); code != ErrRegistryUnavailable {
		t.Errorf("error code: got %q, want %q", code, ErrRegistryUnavailable)
	}
}

// --- audit ---

// TestInternal_MutationsRecordAnActor proves the audit trail names the calling
// service and ties the change to a request (§50).
//
// The actor is what makes a release attributable. Without it the registry
// would say a tenant was released and nothing would say by whom.
func TestInternal_MutationsRecordAnActor(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	rec := f.post(t, "/internal/v1/tenant-routes/reserve", posServiceToken, `{"tenant":"garage1","product":"POS"}`)
	wantStatus(t, rec, http.StatusCreated)

	requestID := rec.Header().Get("X-Request-ID")
	if requestID == "" {
		t.Fatal("the response carries no request ID")
	}

	events := f.repo.recordedEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(events))
	}

	event := events[0]
	if event.Action != tenant.ActionReserve {
		t.Errorf("action: got %q, want %q", event.Action, tenant.ActionReserve)
	}
	if event.Actor.Service != "pos" {
		t.Errorf("actor service: got %q, want %q", event.Actor.Service, "pos")
	}
	if event.Actor.RequestID != requestID {
		t.Errorf("actor request ID: got %q, want %q", event.Actor.RequestID, requestID)
	}
	if event.TenantSlug != "garage1" || event.Product != tenant.ProductPOS {
		t.Errorf("event does not describe the mutation: %+v", event)
	}
}

// TestInternal_AccessLogNamesTheCallingService proves an internal mutation is
// attributable from the log alone.
func TestInternal_AccessLogNamesTheCallingService(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	f.post(t, "/internal/v1/tenant-routes/reserve", posServiceToken, `{"tenant":"garage1","product":"POS"}`)

	logs := f.logs.String()
	if !strings.Contains(logs, "service=pos") {
		t.Errorf("the access log does not name the calling service:\n%s", logs)
	}

	// The token must never appear, at any level, in any field.
	if strings.Contains(logs, posServiceToken) || strings.Contains(logs, gmsServiceToken) {
		t.Error("a service token was written to the log")
	}
}

// TestInternal_UnauthenticatedCallsAreLoggedAtWarn proves a refused lifecycle
// call is visible without debug logging enabled.
func TestInternal_UnauthenticatedCallsAreLoggedAtWarn(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	f.get(t, "/internal/v1/tenant-routes", "")

	logs := f.logs.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "not authenticated") {
		t.Errorf("an unauthenticated lifecycle call was not logged at warn:\n%s", logs)
	}
}

// --- the full lifecycle, end to end through the API ---

// TestInternal_FullLifecycle walks the state machine the way a provisioning
// job does, asserting the status at each step.
func TestInternal_FullLifecycle(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	steps := []struct {
		name   string
		path   string
		body   string
		want   int
		status tenant.Status
	}{
		{
			name: "reserve",
			path: "/internal/v1/tenant-routes/reserve",
			body: `{"tenant":"garage1","product":"GMS"}`,
			want: http.StatusCreated, status: tenant.StatusProvisioning,
		},
		{
			name: "activate",
			path: "/internal/v1/tenant-routes/activate",
			body: `{"tenant":"garage1","product":"GMS"}`,
			want: http.StatusOK, status: tenant.StatusActive,
		},
		{
			name: "activate again is idempotent",
			path: "/internal/v1/tenant-routes/activate",
			body: `{"tenant":"garage1","product":"GMS"}`,
			want: http.StatusOK, status: tenant.StatusActive,
		},
		{
			name: "disable",
			path: "/internal/v1/tenant-routes/disable",
			body: `{"tenant":"garage1"}`,
			want: http.StatusOK, status: tenant.StatusDisabled,
		},
		{
			name: "disable again is idempotent",
			path: "/internal/v1/tenant-routes/disable",
			body: `{"tenant":"garage1"}`,
			want: http.StatusOK, status: tenant.StatusDisabled,
		},
		{
			name: "re-enable",
			path: "/internal/v1/tenant-routes/activate",
			body: `{"tenant":"garage1","product":"GMS"}`,
			want: http.StatusOK, status: tenant.StatusActive,
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			rec := f.post(t, step.path, gmsServiceToken, step.body)
			wantStatus(t, rec, step.want)

			body := decode[routeResponse](t, rec)
			if body.Status != string(step.status) {
				t.Errorf("status: got %s, want %s", body.Status, step.status)
			}

			stored, exists := f.repo.statusOf("garage1")
			if !exists || stored != step.status {
				t.Errorf("stored status: got %s (exists=%v), want %s", stored, exists, step.status)
			}
		})
	}

	// One event per mutation, including the idempotent no-ops? No — a no-op
	// writes nothing, because nothing changed. Asserting the exact count is
	// what keeps the audit trail meaningful: an event log full of "activated"
	// entries for activations that did nothing would be worse than no log.
	events := f.repo.recordedEvents()
	if len(events) != 4 {
		t.Errorf("expected 4 audit events (reserve, activate, disable, activate), got %d: %+v", len(events), events)
	}
}

// TestInternal_ActivateWithTheWrongProductIsAConflict covers the §42
// protection that survives authorization.
//
// Authorization refuses a caller that may not manage the asserted product. The
// case that gets past it is a caller that manages *both* products asserting one
// that does not match the stored route — and the service must catch that,
// because otherwise a caller could activate a tenant it does not own simply by
// naming the right product. Without a two-product token this path is
// unreachable and the check would be untested.
func TestInternal_ActivateWithTheWrongProductIsAConflict(t *testing.T) {
	f := newInternalFixture(t, testConfig(), func(repo *memRepository) {
		repo.seed("mangala", tenant.ProductPOS, tenant.StatusProvisioning)
		repo.seed("garage1", tenant.ProductGMS, tenant.StatusProvisioning)
	})

	t.Run("asserting the wrong product is refused", func(t *testing.T) {
		rec := f.post(t, "/internal/v1/tenant-routes/activate", platformServiceToken,
			`{"tenant":"mangala","product":"GMS"}`)
		wantStatus(t, rec, http.StatusConflict)

		if code := errorCode(t, rec); code != ErrConflict {
			t.Errorf("error code: got %q, want %q", code, ErrConflict)
		}

		// And the route must be untouched. A 409 that still activated would
		// satisfy a test that only read the status code.
		if status, _ := f.repo.statusOf("mangala"); status != tenant.StatusProvisioning {
			t.Errorf("the route was activated despite the conflict: status is %s", status)
		}
	})

	t.Run("asserting the right product succeeds", func(t *testing.T) {
		rec := f.post(t, "/internal/v1/tenant-routes/activate", platformServiceToken,
			`{"tenant":"mangala","product":"POS"}`)
		wantStatus(t, rec, http.StatusOK)

		if status, _ := f.repo.statusOf("mangala"); status != tenant.StatusActive {
			t.Errorf("status: got %s, want ACTIVE", status)
		}
	})
}

// TestInternal_MultiProductServiceSeesEverything proves the list narrowing is
// a rule about the caller's permissions and not a fixed product filter.
func TestInternal_MultiProductServiceSeesEverything(t *testing.T) {
	f := newInternalFixture(t, testConfig(), func(repo *memRepository) {
		repo.seed("mangala", tenant.ProductPOS, tenant.StatusActive)
		repo.seed("kandy", tenant.ProductPOS, tenant.StatusDisabled)
		repo.seed("garage1", tenant.ProductGMS, tenant.StatusActive)
		repo.seed("garage2", tenant.ProductGMS, tenant.StatusProvisioning)
	})

	rec := f.get(t, "/internal/v1/tenant-routes", platformServiceToken)
	wantStatus(t, rec, http.StatusOK)

	body := decode[listResponse](t, rec)
	if len(body.Routes) != 4 {
		t.Fatalf("a two-product service should see all 4 routes, got %d: %+v", len(body.Routes), body.Routes)
	}
}

// TestInternal_ConcurrentReservesProduceOneWinner proves the API inherits the
// repository's compare-and-swap rather than resolving a race by itself.
func TestInternal_ConcurrentReservesProduceOneWinner(t *testing.T) {
	f := newInternalFixture(t, testConfig())

	const goroutines = 8
	codes := make([]int, goroutines)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := f.post(t, "/internal/v1/tenant-routes/reserve", posServiceToken, `{"tenant":"garage1","product":"POS"}`)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	var created, conflicts int
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}

	if created != 1 {
		t.Errorf("expected exactly one successful reserve, got %d", created)
	}
	if conflicts != goroutines-1 {
		t.Errorf("expected %d conflicts, got %d", goroutines-1, conflicts)
	}
}
