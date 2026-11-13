package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/httpapi"
	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/tenant"
)

// --- the upstream stub ----------------------------------------------------

// capturedRequest is one request as the upstream saw it.
//
// It is recorded before the stub's responder runs, so a test can inspect a
// request whose response was an error — which is most of what a product outage
// leaves behind to look at.
type capturedRequest struct {
	method string
	path   string
	query  string
	host   string
	header http.Header
	body   string
}

// stub stands in for one product upstream.
//
// It records what it received rather than only what it answered, because most
// of what M5 has to prove is about the request a product is handed — which
// upstream, under which Host, with which headers — and a status code cannot
// show any of that.
type stub struct {
	name   string
	server *httptest.Server

	mu      sync.Mutex
	respond func(http.ResponseWriter, *http.Request)
	seen    []capturedRequest
}

func newStub(t *testing.T, name string) *stub {
	t.Helper()

	s := &stub{name: name}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stub) url() string { return s.server.URL }

// stop closes the listener, which is how a product outage is simulated. A
// closed httptest server refuses connections, so the Gateway's transport sees
// the same error it would see from a product that is down.
func (s *stub) stop() { s.server.Close() }

// setRespond replaces the default responder. It is mutex-guarded because the
// server handler reads it from its own goroutine.
func (s *stub) setRespond(fn func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = fn
}

func (s *stub) handle(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}

	s.mu.Lock()
	s.seen = append(s.seen, capturedRequest{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.RawQuery,
		host:   r.Host,
		header: r.Header.Clone(),
		body:   string(body),
	})
	respond := s.respond
	s.mu.Unlock()

	if respond != nil {
		respond(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "served by "+s.name)
}

func (s *stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

func (s *stub) requests() []capturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]capturedRequest, len(s.seen))
	copy(out, s.seen)
	return out
}

func (s *stub) last(t *testing.T) capturedRequest {
	t.Helper()

	got := s.requests()
	if len(got) == 0 {
		t.Fatalf("upstream %s received no request", s.name)
	}
	return got[len(got)-1]
}

// --- the fixture ----------------------------------------------------------

// stubRegistry is the registry the pipeline resolves against.
//
// M5 is about what happens *after* a route is resolved, so the registry is a
// map rather than the real repository: a test that had to provision a tenant
// to reach the proxy would be testing M1 again.
type stubRegistry struct {
	routes map[string]tenant.Route
}

func (s *stubRegistry) GetBySlug(_ context.Context, slug string) (tenant.Route, error) {
	rt, ok := s.routes[slug]
	if !ok {
		return tenant.Route{}, fmt.Errorf("%w: %q", tenant.ErrNotFound, slug)
	}
	return rt, nil
}

// fixture is the whole public pipeline — the production middleware chain, the
// production handler, and the real proxy — wired to four recording stubs.
//
// The chain is the production one rather than a bare handler because two of
// M5's properties depend on middleware: the headers the upstream receives are
// written by routing.ApplyForwarded from the Edge that TrustedProxy installed,
// and a streamed response has to survive statusRecorder on its way out (see
// streaming_test.go).
type fixture struct {
	t   *testing.T
	cfg *config.Config
	p   *Proxy

	posFrontend *stub
	posBackend  *stub
	gmsFrontend *stub
	gmsBackend  *stub

	handler http.Handler
}

// stubs returns the four upstreams in a fixed order, so a test can index them.
func (f *fixture) stubs() []*stub {
	return []*stub{f.posFrontend, f.posBackend, f.gmsFrontend, f.gmsBackend}
}

// newFixture builds the pipeline. The optional mutators run after the defaults
// are set and before anything is constructed, so a test can change
// configuration the way a deployment would rather than by reaching inside.
func newFixture(t *testing.T, tune ...func(*config.Config)) *fixture {
	t.Helper()

	posFrontend := newStub(t, "POS/frontend")
	posBackend := newStub(t, "POS/backend")
	gmsFrontend := newStub(t, "GMS/frontend")
	gmsBackend := newStub(t, "GMS/backend")

	cfg := config.Default()
	cfg.Platform.RootDomain = "zensys.lk"
	cfg.Platform.ApexBehavior = config.ApexBehaviorNotFound
	cfg.Platform.LandingDir = ""
	cfg.Products = map[string]config.ProductConfig{
		"POS": {
			FrontendUpstream:   posFrontend.url(),
			BackendUpstream:    posBackend.url(),
			UpstreamTimeout:    config.Duration(5 * time.Second),
			InjectTenantHeader: true,
			TenantHeaderName:   routing.HeaderTenantID,
		},
		"GMS": {
			FrontendUpstream:   gmsFrontend.url(),
			BackendUpstream:    gmsBackend.url(),
			UpstreamTimeout:    config.Duration(5 * time.Second),
			InjectTenantHeader: true,
			TenantHeaderName:   routing.HeaderTenantID,
		},
	}
	cfg.Proxy.PreserveHost = true

	for _, fn := range tune {
		fn(cfg)
	}

	p, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("build the proxy: %v", err)
	}

	logger := discardLogger()
	apex := httpapi.NewApexHandler(cfg.Platform.ApexBehavior, cfg.Platform.LandingDir, logger)
	handler := httpapi.NewPublicHandler(cfg, logger, routing.NewResolver(&stubRegistry{
		routes: map[string]tenant.Route{
			"mangala": activeRoute("mangala", tenant.ProductPOS),
			"gms1":    activeRoute("gms1", tenant.ProductGMS),
		},
	}), apex, p)

	return &fixture{
		t:           t,
		cfg:         cfg,
		p:           p,
		posFrontend: posFrontend,
		posBackend:  posBackend,
		gmsFrontend: gmsFrontend,
		gmsBackend:  gmsBackend,
		handler: httpapi.StandardChain(handler, logger, httpapi.ChainOptions{
			RequestIDHeader: cfg.Proxy.RequestIDHeader,
			TrustedProxies:  trustedLoopback(t),
			MaxBodyBytes:    cfg.Server.MaxBodyBytes,
		}),
	}
}

// get issues a public request for host and returns the recorder.
func (f *fixture) get(host, target string) *httptest.ResponseRecorder {
	f.t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// trustedLoopback mirrors config.Default()'s proxy.trusted_proxies, so the
// tests exercise the shipped trust set rather than a convenient one invented
// for the test.
func trustedLoopback(t *testing.T) *routing.TrustedProxies {
	t.Helper()

	tp, err := routing.ParseTrustedProxies(config.Default().Proxy.TrustedProxies)
	if err != nil {
		t.Fatalf("parse the default trusted proxies: %v", err)
	}
	return tp
}

func activeRoute(slug string, product tenant.Product) tenant.Route {
	return tenant.Route{TenantSlug: slug, Product: product, Status: tenant.StatusActive}
}

// tuneProduct overrides one product's configuration. Products is a map, so the
// value has to be written back for the change to be seen.
func tuneProduct(name string, fn func(*config.ProductConfig)) func(*config.Config) {
	return func(c *config.Config) {
		pc := c.Products[name]
		fn(&pc)
		c.Products[name] = pc
	}
}

// decodeError reads the Gateway's error response, failing the test if the body
// is not one.
func decodeError(t *testing.T, body []byte) httpapi.ErrorResponse {
	t.Helper()

	var out httpapi.ErrorResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the response body is not the Gateway's JSON error response: %v; body = %s", err, body)
	}
	return out
}

// --- §90 routing matrix ---------------------------------------------------

// TestProxy_RoutingMatrix is the four §90 routing rows.
//
// Each row asserts not only that the intended upstream was reached but that
// the other three were not. "Routed to the POS frontend" is only meaningful if
// the request did not also reach the POS backend, and a matrix that checked
// only the positive half would pass against an implementation that sent every
// request everywhere.
func TestProxy_RoutingMatrix(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		target    string
		want      int // index into fixture.stubs()
		wantPath  string
		wantQuery string
	}{
		{
			name:     "active POS tenant at /",
			host:     "mangala.zensys.lk",
			target:   "/",
			want:     0,
			wantPath: "/",
		},
		{
			name:     "active POS tenant under /api",
			host:     "mangala.zensys.lk",
			target:   "/api/orders",
			want:     1,
			wantPath: "/api/orders",
		},
		{
			name:     "active GMS tenant at /",
			host:     "gms1.zensys.lk",
			target:   "/",
			want:     2,
			wantPath: "/",
		},
		{
			name:     "active GMS tenant under /api/v1",
			host:     "gms1.zensys.lk",
			target:   "/api/v1/tenants",
			want:     3,
			wantPath: "/api/v1/tenants",
		},
		{
			// §21: the Gateway routes, it does not rewrite. The path and query
			// reach the product exactly as the client sent them, which is why
			// the products' own route tables are the only ones that matter.
			name:      "the path and query reach the product unchanged",
			host:      "mangala.zensys.lk",
			target:    "/api/orders?limit=10&after=42",
			want:      1,
			wantPath:  "/api/orders",
			wantQuery: "limit=10&after=42",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)

			rec := f.get(tc.host, tc.target)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}

			for i, s := range f.stubs() {
				got := s.count()

				if i == tc.want {
					if got != 1 {
						t.Errorf("%s received %d requests, want exactly 1", s.name, got)
					}
					continue
				}
				if got != 0 {
					t.Errorf("%s received %d requests; %s%s must reach exactly one upstream",
						s.name, got, tc.host, tc.target)
				}
			}

			got := f.stubs()[tc.want].last(t)
			if got.path != tc.wantPath {
				t.Errorf("the upstream received path %q, want %q", got.path, tc.wantPath)
			}
			if got.query != tc.wantQuery {
				t.Errorf("the upstream received query %q, want %q", got.query, tc.wantQuery)
			}
		})
	}
}

// TestProxy_UpstreamSeesThePublicHostname is §25 and §44.
//
// The product is handed the hostname the client asked for, not the internal
// address of the upstream. For GMS this is not cosmetic: it resolves its own
// tenant from the Host header, so an internal address here would break tenant
// resolution inside the product rather than merely look wrong in a log.
func TestProxy_UpstreamSeesThePublicHostname(t *testing.T) {
	f := newFixture(t)

	if rec := f.get("mangala.zensys.lk", "/"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if got := f.posFrontend.last(t).host; got != "mangala.zensys.lk" {
		t.Errorf("the upstream saw Host %q, want the public hostname %q", got, "mangala.zensys.lk")
	}
}

// TestProxy_PreserveHostOffIsWired is the converse of the test above, and it
// is what makes that test mean something.
//
// Without it, the assertion could pass because Go happened to put the right
// value there, and preserve_host would be an unwired setting nobody noticed.
func TestProxy_PreserveHostOffIsWired(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.Proxy.PreserveHost = false })

	if rec := f.get("mangala.zensys.lk", "/"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	want := strings.TrimPrefix(f.posFrontend.url(), "http://")
	if got := f.posFrontend.last(t).host; got != want {
		t.Errorf("with preserve_host false the upstream saw Host %q, want its own address %q", got, want)
	}
}

// TestProxy_ProductResponsesArePassedThroughUnchanged is §47: the Gateway
// forwards, it does not reinterpret.
//
// The product's own 404 is the interesting row. The Gateway has a 404 of its
// own — the fail-closed response for an unresolvable host — and the two must
// stay distinguishable, because one means "this hostname is not a tenant" and
// the other means "this tenant has no such page".
func TestProxy_ProductResponsesArePassedThroughUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		ctype   string
		headers map[string]string
	}{
		{
			name:    "500 from the product",
			status:  http.StatusInternalServerError,
			body:    `{"error":"the product blew up"}`,
			ctype:   "application/json",
			headers: map[string]string{"X-Product": "pos"},
		},
		{
			name:   "the product's own 404",
			status: http.StatusNotFound,
			body:   "<!doctype html><h1>No such order</h1>",
			ctype:  "text/html; charset=utf-8",
		},
		{
			name:   "201 created",
			status: http.StatusCreated,
			body:   `{"id":7}`,
			ctype:  "application/json",
		},
		{
			name:    "302 redirect",
			status:  http.StatusFound,
			headers: map[string]string{"Location": "/login"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.posFrontend.setRespond(func(w http.ResponseWriter, _ *http.Request) {
				if tc.ctype != "" {
					w.Header().Set("Content-Type", tc.ctype)
				}
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})

			rec := f.get("mangala.zensys.lk", "/")

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d passed through unchanged; body = %s",
					rec.Code, tc.status, rec.Body.String())
			}
			if got := rec.Body.String(); got != tc.body {
				t.Errorf("body = %q, want %q", got, tc.body)
			}
			if tc.ctype != "" {
				if got := rec.Header().Get("Content-Type"); got != tc.ctype {
					t.Errorf("Content-Type = %q, want %q", got, tc.ctype)
				}
			}
			for k, v := range tc.headers {
				if got := rec.Header().Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}

			if tc.status == http.StatusNotFound && strings.Contains(rec.Body.String(), "unknown_tenant") {
				t.Error("the product's 404 was replaced by the Gateway's unknown-tenant response")
			}
		})
	}
}

// TestProxy_ProductOutageIsIsolated is the §90 row for an unavailable product,
// and the behavioural half of §48.
//
// Both products are configured in one Proxy in one process. Taking POS down
// must produce an error for a POS tenant and leave GMS tenants completely
// untouched — including not sending GMS a single request on POS's behalf.
func TestProxy_ProductOutageIsIsolated(t *testing.T) {
	f := newFixture(t)

	f.posFrontend.stop()
	f.posBackend.stop()

	rec := f.get("mangala.zensys.lk", "/")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("a POS tenant got %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec.Body.Bytes()); body.Error != httpapi.ErrUpstreamUnavailable {
		t.Errorf("error code = %q, want %q", body.Error, httpapi.ErrUpstreamUnavailable)
	}

	// The same Gateway process, one request later.
	rec = f.get("gms1.zensys.lk", "/api/v1/tenants")
	if rec.Code != http.StatusOK {
		t.Fatalf("a GMS tenant got %d, want 200: one product's outage affected another; body = %s",
			rec.Code, rec.Body.String())
	}
	if got := f.gmsBackend.count(); got != 1 {
		t.Errorf("the GMS backend received %d requests, want 1", got)
	}
	if got := f.gmsFrontend.count(); got != 0 {
		t.Errorf("the GMS frontend received %d requests, want 0: /api/v1/... is backend traffic", got)
	}
}

// TestProxy_UpstreamTimeoutIsA503WithRetryAfter covers both shapes a deadline
// can take.
//
// isTimeout has to catch two different errors — the deadline the transport
// wrapper installs, and the transport's own response-header timeout — and a
// test that exercised only one of them would leave the other free to regress
// into a 502, which tells a client the wrong thing about whether to retry.
func TestProxy_UpstreamTimeoutIsA503WithRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		tune func(*config.Config)
		why  string
	}{
		{
			name: "per-request deadline",
			tune: tuneProduct("POS", func(pc *config.ProductConfig) {
				pc.UpstreamTimeout = config.Duration(150 * time.Millisecond)
			}),
			why: "upstream_timeout bounds the whole exchange, response body included",
		},
		{
			name: "response header timeout",
			tune: func(c *config.Config) {
				c.Proxy.UpstreamResponseHeaderTimeout = config.Duration(150 * time.Millisecond)

				pc := c.Products["POS"]
				pc.UpstreamTimeout = config.Duration(10 * time.Second)
				c.Products["POS"] = pc
			},
			why: "an upstream that accepts the connection and then says nothing is caught by the transport",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.tune)

			// An upstream that accepts the connection and then holds it open
			// without answering. It watches the request context so that it
			// returns as soon as the Gateway gives up, rather than holding the
			// test's cleanup open for the full duration.
			f.posFrontend.setRespond(func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-time.After(5 * time.Second):
				case <-r.Context().Done():
				}
			})

			start := time.Now()
			rec := f.get("mangala.zensys.lk", "/")
			elapsed := time.Since(start)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (%s); body = %s", rec.Code, tc.why, rec.Body.String())
			}
			if got := rec.Header().Get("Retry-After"); got != "5" {
				t.Errorf("Retry-After = %q, want %q: a bare 503 does not tell a client the condition is temporary",
					got, "5")
			}
			if body := decodeError(t, rec.Body.Bytes()); body.Error != httpapi.ErrUpstreamUnavailable {
				t.Errorf("error code = %q, want %q", body.Error, httpapi.ErrUpstreamUnavailable)
			}

			// Guards against the test passing because the stub failed fast for
			// some unrelated reason.
			if elapsed > 3*time.Second {
				t.Errorf("the request took %s; no deadline fired", elapsed)
			}
		})
	}
}

// TestProxy_EachRouteOwnsItsTransport is the runtime half of §48's structural
// claim, which the doc comment on the route type states in prose.
//
// Sharing a transport between routes would give one product's error path a
// live connection pool belonging to another product, and would make a
// per-product deadline impossible to express. A route owning its own transport
// is what makes "no cross-product fallback" a property of the object graph
// rather than of a code path someone has to remember not to add.
//
// The source-level half of the same claim is in structure_test.go.
func TestProxy_EachRouteOwnsItsTransport(t *testing.T) {
	f := newFixture(t)

	if len(f.p.routes) != 4 {
		t.Fatalf("the fixture configured %d routes, want 4", len(f.p.routes))
	}

	transports := map[http.RoundTripper]string{}
	reverses := map[*httputil.ReverseProxy]string{}

	for k, rt := range f.p.routes {
		label := fmt.Sprintf("%s/%s", k.product, k.stage)

		if prev, ok := transports[rt.reverse.Transport]; ok {
			t.Errorf("%s and %s share a transport", label, prev)
		}
		transports[rt.reverse.Transport] = label

		if prev, ok := reverses[rt.reverse]; ok {
			t.Errorf("%s and %s share a reverse proxy", label, prev)
		}
		reverses[rt.reverse] = label
	}
}

// TestProxy_UnconfiguredRouteIsABadGateway covers the defensive branch in
// Serve.
//
// config.Validate requires both upstreams for every configured product, so
// this is unreachable in a validated deployment. It is tested anyway because
// the alternative to the branch is a nil dereference, and a nil dereference in
// a request path is a much worse outcome than a 502.
func TestProxy_UnconfiguredRouteIsABadGateway(t *testing.T) {
	f := newFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "mangala.zensys.lk"
	rec := httptest.NewRecorder()

	// A product the configuration does not have an upstream for.
	f.p.Serve(rec, req, routing.Info{
		Host:   "mangala.zensys.lk",
		Tenant: "mangala",
		Stage:  routing.StageFrontend,
		Route:  activeRoute("mangala", tenant.Product("ERP")),
	})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if body := decodeError(t, rec.Body.Bytes()); body.Error != httpapi.ErrUpstreamUnavailable {
		t.Errorf("error code = %q, want %q", body.Error, httpapi.ErrUpstreamUnavailable)
	}
}
