package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/tenant"
)

// fakeRegistry is a routing.Lookup that records the slugs it was asked about.
//
// The recording matters more than the return value: several tests assert that
// the registry was *not* consulted, which is how the §65 ordering rule and the
// slug-validation step are actually proven.
//
// It is mutex-guarded because the tenant-isolation test drives it from many
// goroutines at once. Without the lock that test would fail on a race in the
// harness rather than on a real defect, and the race detector would point at
// the test rather than at the code under test.
type fakeRegistry struct {
	mu      sync.Mutex
	routes  map[string]tenant.Route
	err     error
	lookups []string
}

func (f *fakeRegistry) GetBySlug(_ context.Context, slug string) (tenant.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lookups = append(f.lookups, slug)

	if f.err != nil {
		return tenant.Route{}, f.err
	}
	route, ok := f.routes[slug]
	if !ok {
		return tenant.Route{}, fmt.Errorf("%w: %q", tenant.ErrNotFound, slug)
	}
	return route, nil
}

// lookupCount reports how many lookups the registry has served.
func (f *fakeRegistry) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lookups)
}

// recordedLookups returns a copy of the slugs looked up so far.
func (f *fakeRegistry) recordedLookups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.lookups))
	copy(out, f.lookups)
	return out
}

// recordingUpstream stands in for the M5 reverse proxy.
type recordingUpstream struct {
	called bool
	route  tenant.Route
	stage  routing.Stage
}

func (u *recordingUpstream) Serve(w http.ResponseWriter, _ *http.Request, info routing.Info) {
	u.called = true
	u.route = info.Route
	u.stage = info.Stage
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("upstream"))
}

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.Platform.RootDomain = "zensys.lk"
	cfg.Platform.ApexBehavior = ApexNotFound
	cfg.Platform.LandingDir = ""
	return cfg
}

// trustedLoopback mirrors config.Default()'s proxy.trusted_proxies, so the
// tests exercise the same trust set the shipped configuration uses rather than
// a convenient one invented for the test.
func trustedLoopback(t *testing.T) *routing.TrustedProxies {
	t.Helper()

	tp, err := routing.ParseTrustedProxies(config.Default().Proxy.TrustedProxies)
	if err != nil {
		t.Fatalf("parse the default trusted proxies: %v", err)
	}
	return tp
}

func newPipeline(t *testing.T, cfg *config.Config, registry routing.Lookup) (http.Handler, *recordingUpstream) {
	t.Helper()

	logger := discardLogger()
	apex := NewApexHandler(cfg.Platform.ApexBehavior, cfg.Platform.LandingDir, logger)
	upstream := &recordingUpstream{}

	handler := NewPublicHandler(cfg, logger, routing.NewResolver(registry), apex, upstream)

	// The real chain, not a bare handler. The middleware order is part of the
	// security policy — it is what deletes a spoofed X-Tenant-ID and what
	// decides whether an inbound X-Request-ID may be believed — so a test that
	// bypassed it would prove less than it appears to.
	return StandardChain(handler, logger, ChainOptions{
		RequestIDHeader: cfg.Proxy.RequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
	}), upstream
}

// doFrom issues a request with a specific peer address, so a test can choose
// whether the request looks like it came from a trusted proxy.
func doFrom(t *testing.T, h http.Handler, host, path, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// trustedPeer and untrustedPeer are the two peer addresses the header tests
// need: 127.0.0.1 is in the default trusted set, 192.0.2.1 (TEST-NET-1, the
// address httptest picks) is not.
const (
	trustedPeer   = "127.0.0.1:54321"
	untrustedPeer = "192.0.2.1:54321"
)

// do issues a request with the given Host header and returns the recorder.
func do(t *testing.T, h http.Handler, host, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func activeRoute(slug string, product tenant.Product) tenant.Route {
	return tenant.Route{TenantSlug: slug, Product: product, Status: tenant.StatusActive}
}

// TestPublicHandler_ResolvedTenantReachesTheUpstream is the happy path: a
// registered, active tenant is handed to the terminal step with its route.
func TestPublicHandler_ResolvedTenantReachesTheUpstream(t *testing.T) {
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"mangala": activeRoute("mangala", tenant.ProductPOS),
	}}
	handler, upstream := newPipeline(t, testConfig(), registry)

	rec := do(t, handler, "mangala.zensys.lk", "/api/orders")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !upstream.called {
		t.Fatal("the upstream was not reached")
	}
	if upstream.route.TenantSlug != "mangala" {
		t.Errorf("upstream route tenant = %q, want %q", upstream.route.TenantSlug, "mangala")
	}
	if upstream.route.Product != tenant.ProductPOS {
		t.Errorf("upstream route product = %q, want %q", upstream.route.Product, tenant.ProductPOS)
	}
}

// TestPublicHandler_NormalizedHostResolves proves the pipeline uses the
// normalized name rather than the raw header, so a client sending uppercase or
// a port still resolves.
func TestPublicHandler_NormalizedHostResolves(t *testing.T) {
	for _, raw := range []string{
		"MANGALA.ZENSYS.LK",
		"mangala.zensys.lk:8080",
		"mangala.zensys.lk.",
		"mangala.ZENSYS.lk:443",
	} {
		t.Run(raw, func(t *testing.T) {
			registry := &fakeRegistry{routes: map[string]tenant.Route{
				"mangala": activeRoute("mangala", tenant.ProductPOS),
			}}
			handler, upstream := newPipeline(t, testConfig(), registry)

			if rec := do(t, handler, raw, "/"); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if !upstream.called {
				t.Error("the upstream was not reached")
			}
		})
	}
}

// TestPublicHandler_RejectedHostsNeverReachTheRegistry is the §65 ordering rule.
//
// A reserved host, a malformed host, and a multi-label host must all be refused
// before any lookup. Otherwise anyone could generate database load with
// hostnames that can never be tenants, and the reserved-host guarantee would
// depend on the registry happening to be empty.
func TestPublicHandler_RejectedHostsNeverReachTheRegistry(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{"reserved: admin", "admin.zensys.lk"},
		{"reserved: www", "www.zensys.lk"},
		{"reserved: api", "api.zensys.lk"},
		{"reserved: gateway", "gateway.zensys.lk"},
		{"multi-label", "a.b.zensys.lk"},
		{"wrong domain", "mangala.example.com"},
		{"ip literal", "127.0.0.1"},
		{"malformed: underscore", "mangala_x.zensys.lk"},
		{"malformed: CRLF", "mangala.zensys.lk\r\nX-Evil: 1"},
		{"punycode", "xn--mangala-xyz.zensys.lk"},
		{"empty host", ""},
		{"invalid slug: too short", "ab.zensys.lk"},
		{"invalid slug: two characters after lowercasing", "AB.zensys.lk"},
		{"invalid slug: leading hyphen", "-abc.zensys.lk"},
		{"invalid slug: trailing hyphen", "abc-.zensys.lk"},
		{"invalid slug: too long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.zensys.lk"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry := &fakeRegistry{routes: map[string]tenant.Route{}}
			handler, upstream := newPipeline(t, testConfig(), registry)

			rec := do(t, handler, tc.host, "/")

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			if upstream.called {
				t.Error("the upstream was reached for a rejected host")
			}
			if registry.lookupCount() != 0 {
				t.Errorf("the registry was consulted for %v, which can never be a tenant", registry.recordedLookups())
			}
		})
	}
}

// TestPublicHandler_All404sAreByteIdentical is the §45 requirement.
//
// Unknown tenant, disabled tenant, reserved host, malformed host, and
// multi-label host must be indistinguishable to a caller. If they differed, an
// anonymous prober could enumerate which hostnames belong to real businesses.
func TestPublicHandler_All404sAreByteIdentical(t *testing.T) {
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		// Exists but is not ACTIVE.
		"garage1": {TenantSlug: "garage1", Product: tenant.ProductGMS, Status: tenant.StatusDisabled},
		// Exists but has not finished provisioning.
		"newco": {TenantSlug: "newco", Product: tenant.ProductPOS, Status: tenant.StatusProvisioning},
	}}
	handler, _ := newPipeline(t, testConfig(), registry)

	rejections := map[string]string{
		"unknown tenant":      "random.zensys.lk",
		"disabled tenant":     "garage1.zensys.lk",
		"provisioning tenant": "newco.zensys.lk",
		"reserved host":       "admin.zensys.lk",
		"multi-label":         "a.b.zensys.lk",
		"wrong domain":        "mangala.example.com",
		"ip literal":          "127.0.0.1",
		"malformed host":      "mangala_x.zensys.lk",
		"punycode":            "xn--mangala-xyz.zensys.lk",
		"invalid slug":        "ab.zensys.lk",
		"apex not_found":      "zensys.lk",
	}

	var (
		referenceHost string
		referenceBody string
		referenceCT   string
	)

	for label, host := range rejections {
		rec := do(t, handler, host, "/")
		body := rec.Body.String()

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", label, rec.Code)
			continue
		}

		if referenceBody == "" {
			referenceHost, referenceBody, referenceCT = label, body, rec.Header().Get("Content-Type")
			continue
		}

		if body != referenceBody {
			t.Errorf("%s body differs from %s:\n  %s got %q\n  %s got %q",
				label, referenceHost, label, body, referenceHost, referenceBody)
		}
		if ct := rec.Header().Get("Content-Type"); ct != referenceCT {
			t.Errorf("%s Content-Type = %q, want %q", label, ct, referenceCT)
		}
	}
}

// TestPublicHandler_DisabledTenantIsNotDistinguishableFromUnknown is the
// specific ADR 0001 decision 5 requirement, asserted separately from the
// all-404s sweep so a failure names the decision rather than a generic body
// mismatch.
func TestPublicHandler_DisabledTenantIsNotDistinguishableFromUnknown(t *testing.T) {
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"garage1": {TenantSlug: "garage1", Product: tenant.ProductGMS, Status: tenant.StatusDisabled},
	}}
	handler, _ := newPipeline(t, testConfig(), registry)

	disabled := do(t, handler, "garage1.zensys.lk", "/")
	unknown := do(t, handler, "random.zensys.lk", "/")

	if disabled.Code != unknown.Code {
		t.Errorf("status differs: disabled = %d, unknown = %d", disabled.Code, unknown.Code)
	}
	if disabled.Body.String() != unknown.Body.String() {
		t.Errorf("body differs:\n  disabled = %q\n  unknown  = %q", disabled.Body.String(), unknown.Body.String())
	}
}

// TestPublicHandler_RegistryFailureIs503Not404 is the §46 requirement.
//
// A registry that cannot answer says nothing about whether the tenant exists,
// so the Gateway must report its own inability rather than claim the hostname
// is unregistered.
func TestPublicHandler_RegistryFailureIs503Not404(t *testing.T) {
	registry := &fakeRegistry{err: errors.New("dial tcp 10.0.0.5:3306: connection refused")}
	handler, upstream := newPipeline(t, testConfig(), registry)

	rec := do(t, handler, "mangala.zensys.lk", "/")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if upstream.called {
		t.Error("the upstream must not be reached when the route is unknown")
	}

	// The response must not name the database, its address, or the driver.
	for _, leak := range []string{"3306", "10.0.0.5", "dial tcp", "connection refused"} {
		if contains(rec.Body.String(), leak) {
			t.Errorf("response body leaks internal detail %q: %s", leak, rec.Body.String())
		}
	}
}

// TestPublicHandler_ConfiguredReservedHostsAreHonoured covers
// platform.reserved_hosts_extra, which may only add to the compiled-in list.
func TestPublicHandler_ConfiguredReservedHostsAreHonoured(t *testing.T) {
	cfg := testConfig()
	cfg.Platform.ReservedHostsExtra = []string{"acme-internal", "staging2"}

	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"acme-internal": activeRoute("acme-internal", tenant.ProductPOS),
	}}
	handler, upstream := newPipeline(t, cfg, registry)

	if rec := do(t, handler, "acme-internal.zensys.lk", "/"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a configured reserved host", rec.Code)
	}
	if upstream.called {
		t.Error("the upstream was reached for a configured reserved host")
	}
	if registry.lookupCount() != 0 {
		t.Errorf("the registry was consulted for a configured reserved host: %v", registry.recordedLookups())
	}
}

// TestPublicHandler_ReservedHostInTheRegistryIsStillRefused covers the case
// where a reserved name reached the registry by hand — a manual insert during
// a migration, say.
//
// It must still be refused at request time: the compiled-in list is the
// authority, not whatever happens to be stored.
func TestPublicHandler_ReservedHostInTheRegistryIsStillRefused(t *testing.T) {
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"admin": activeRoute("admin", tenant.ProductPOS),
	}}
	handler, upstream := newPipeline(t, testConfig(), registry)

	if rec := do(t, handler, "admin.zensys.lk", "/"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if upstream.called {
		t.Error("the upstream was reached for a reserved host that exists in the registry")
	}
}

// TestPublicHandler_ApexServesTheLandingPage covers the apex happy path.
func TestPublicHandler_ApexServesTheLandingPage(t *testing.T) {
	dir := t.TempDir()
	index := filepath.Join(dir, "index.html")
	if err := os.WriteFile(index, []byte("<h1>ZenSys</h1>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	cfg := testConfig()
	cfg.Platform.ApexBehavior = ApexLanding
	cfg.Platform.LandingDir = dir

	registry := &fakeRegistry{}
	handler, upstream := newPipeline(t, cfg, registry)

	rec := do(t, handler, "zensys.lk", "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), "ZenSys") {
		t.Errorf("body = %q, want the landing page", rec.Body.String())
	}
	if upstream.called {
		t.Error("the apex must not reach a product upstream")
	}
	if registry.lookupCount() != 0 {
		t.Errorf("the apex must not be looked up as a tenant: %v", registry.recordedLookups())
	}
}

// TestPublicHandler_ApexDoesNotListDirectories proves the landing file server
// will not render a listing for a directory without an index.html.
func TestPublicHandler_ApexDoesNotListDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "secret"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret", "notes.txt"), []byte("internal"), 0o600); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}

	cfg := testConfig()
	cfg.Platform.ApexBehavior = ApexLanding
	cfg.Platform.LandingDir = dir

	handler, _ := newPipeline(t, cfg, &fakeRegistry{})

	rec := do(t, handler, "zensys.lk", "/secret/")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a directory without an index", rec.Code)
	}
	if contains(rec.Body.String(), "notes.txt") {
		t.Errorf("the response lists directory contents: %s", rec.Body.String())
	}
}

// TestPublicHandler_ApexLandingWithMissingDirectoryDegrades covers the case
// where the landing directory is not there.
//
// Refusing to start would trade a cosmetic problem for a total outage, so the
// apex degrades to 404 and the failure is visible in the startup log instead.
func TestPublicHandler_ApexLandingWithMissingDirectoryDegrades(t *testing.T) {
	cfg := testConfig()
	cfg.Platform.ApexBehavior = ApexLanding
	cfg.Platform.LandingDir = filepath.Join(t.TempDir(), "does-not-exist")

	handler, upstream := newPipeline(t, cfg, &fakeRegistry{})

	if rec := do(t, handler, "zensys.lk", "/"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if upstream.called {
		t.Error("the upstream must not be reached")
	}
}

// TestPublicHandler_ProductIsTakenFromTheRoute proves the product is decided by
// the registry entry, not by anything the client sent.
func TestPublicHandler_ProductIsTakenFromTheRoute(t *testing.T) {
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"garage1": activeRoute("garage1", tenant.ProductGMS),
	}}
	handler, upstream := newPipeline(t, testConfig(), registry)

	if rec := do(t, handler, "garage1.zensys.lk", "/"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if upstream.route.Product != tenant.ProductGMS {
		t.Errorf("product = %q, want GMS", upstream.route.Product)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
