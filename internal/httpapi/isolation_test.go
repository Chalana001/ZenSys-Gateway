package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/tenant"
)

// isolationTenants is the number of distinct tenants driven concurrently.
//
// The plan sets the floor at 50. Enough tenants must be in flight at once that
// a shared slot anywhere in the pipeline — a reused buffer, a package-level
// variable, a mis-keyed cache — would be hit by two different tenants within
// the same test run rather than by luck.
const isolationTenants = 60

// echoUpstream writes the resolved route into the response body.
//
// It stands in for the M5 reverse proxy, which will forward to a product with
// the tenant already decided. Echoing the route is what makes a cross-tenant
// mix-up *observable*: the response body is the routing decision, so a wrong
// tenant cannot hide behind a plausible-looking 200.
type echoUpstream struct{}

func (echoUpstream) Serve(w http.ResponseWriter, _ *http.Request, info routing.Info) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "%s|%s|%s", info.Tenant, info.Route.Product, info.Route.Status)
}

// isolationFixture builds a pipeline serving isolationTenants tenants, half POS
// and half GMS.
type isolationFixture struct {
	handler  http.Handler
	cache    *tenant.Cache
	registry *fakeRegistry
	hosts    []string
}

func newIsolationFixture(t *testing.T) *isolationFixture {
	t.Helper()

	routes := make(map[string]tenant.Route, isolationTenants)
	hosts := make([]string, 0, isolationTenants)

	for i := 0; i < isolationTenants; i++ {
		slug := fmt.Sprintf("tenant-%02d", i)

		// Alternating products is deliberate: if product selection leaked
		// between tenants, half the responses would carry the wrong one and
		// the failure would be obvious rather than a one-in-a-hundred fluke.
		product := tenant.ProductPOS
		if i%2 == 1 {
			product = tenant.ProductGMS
		}

		routes[slug] = tenant.Route{TenantSlug: slug, Product: product, Status: tenant.StatusActive}
		hosts = append(hosts, slug+".zensys.lk")
	}

	registry := &fakeRegistry{routes: routes}
	cache := tenant.NewCache(registry, tenant.CacheOptions{
		RouteTTL:    time.Minute,
		NegativeTTL: time.Minute,
		MaxEntries:  10 * isolationTenants,
	})

	cfg := testConfig()
	logger := discardLogger()

	handler := NewPublicHandler(
		cfg,
		logger,
		routing.NewResolver(cache),
		NewApexHandler(ApexNotFound, "", logger),
		echoUpstream{},
	)

	return &isolationFixture{handler: handler, cache: cache, registry: registry, hosts: hosts}
}

// expectedBody is the response the tenant owning host must receive.
func expectedBody(host string) string {
	slug := strings.TrimSuffix(host, ".zensys.lk")

	product := tenant.ProductPOS
	if n := slug[len(slug)-1] - '0'; n%2 == 1 {
		product = tenant.ProductGMS
	}

	return fmt.Sprintf("%s|%s|%s", slug, product, tenant.StatusActive)
}

// TestTenantsRemainIsolatedUnderConcurrentLoad is the §31 requirement, and the
// single most important test in the project.
//
// One Gateway process serves every tenant. Tenant identity therefore has to be
// request-scoped: it must travel with the request and never sit in a
// package-level variable that a concurrent request could overwrite. The failure
// mode this guards against is not a crash — it is tenant A's request being
// served with tenant B's route, which is a cross-tenant data leak.
//
// The race detector is enabled, so a shared mutable slot would be reported
// directly rather than only showing up as a wrong answer.
func TestTenantsRemainIsolatedUnderConcurrentLoad(t *testing.T) {
	const requestsPerTenant = 8

	f := newIsolationFixture(t)

	var wg sync.WaitGroup
	mismatches := make(chan string, isolationTenants*requestsPerTenant)

	for _, host := range f.hosts {
		want := expectedBody(host)

		for i := 0; i < requestsPerTenant; i++ {
			wg.Add(1)
			go func(host, want string) {
				defer wg.Done()

				req := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
				req.Host = host
				rec := httptest.NewRecorder()

				f.handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					mismatches <- fmt.Sprintf("host %s: status %d, body %q", host, rec.Code, rec.Body.String())
					return
				}
				if got := rec.Body.String(); got != want {
					mismatches <- fmt.Sprintf("host %s received %q, want %q", host, got, want)
				}
			}(host, want)
		}
	}

	wg.Wait()
	close(mismatches)

	var failures int
	for msg := range mismatches {
		failures++
		if failures <= 10 {
			t.Error(msg)
		}
	}

	total := isolationTenants * requestsPerTenant
	if failures > 0 {
		t.Fatalf("%d of %d responses carried the wrong tenant", failures, total)
	}

	// Every tenant was read from the registry exactly once: the first request
	// for each slug missed and the rest were collapsed onto it or served from
	// the cache. More than one read per tenant would mean the stampede
	// protection is not working; fewer is impossible, since every slug has to
	// be read at least once.
	if got := f.registry.lookupCount(); got != isolationTenants {
		t.Errorf("registry reads = %d, want exactly %d (one per tenant)", got, isolationTenants)
	}
}

// TestTenantIsolationSurvivesCacheFlushUnderLoad covers the other half of §31:
// the cache is never authoritative, so discarding it at any moment — including
// in the middle of concurrent traffic — must change nothing except the hit
// rate.
func TestTenantIsolationSurvivesCacheFlushUnderLoad(t *testing.T) {
	// Enough rounds that the traffic outlives several flushes; a run that
	// finished before the flusher started would prove nothing.
	const rounds = 20

	f := newIsolationFixture(t)

	var stop atomic.Bool
	var flushes atomic.Int64

	// Flush continuously while the traffic runs.
	flusherDone := make(chan struct{})
	go func() {
		defer close(flusherDone)
		for !stop.Load() {
			f.cache.Flush()
			flushes.Add(1)
			time.Sleep(500 * time.Microsecond)
		}
	}()

	var wg sync.WaitGroup
	mismatches := make(chan string, isolationTenants*rounds)

	for _, host := range f.hosts {
		want := expectedBody(host)

		for i := 0; i < rounds; i++ {
			wg.Add(1)
			go func(host, want string) {
				defer wg.Done()

				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Host = host
				rec := httptest.NewRecorder()

				f.handler.ServeHTTP(rec, req)

				if got := rec.Body.String(); got != want {
					mismatches <- fmt.Sprintf("host %s received %q, want %q", host, got, want)
				}
			}(host, want)
		}
	}

	wg.Wait()
	stop.Store(true)
	<-flusherDone
	close(mismatches)

	var failures int
	for msg := range mismatches {
		failures++
		if failures <= 10 {
			t.Error(msg)
		}
	}
	if failures > 0 {
		t.Fatalf("%d responses were wrong while the cache was being flushed", failures)
	}

	if flushes.Load() == 0 {
		t.Fatal("the flusher never ran; the test proved nothing")
	}
	t.Logf("survived %d cache flushes during concurrent traffic", flushes.Load())
}

// TestTenantIsolationSurvivesColdCache starts every tenant cold and fires all
// requests at once, so the singleflight path is exercised for all 60 tenants
// simultaneously rather than one at a time.
func TestTenantIsolationSurvivesColdCache(t *testing.T) {
	const perTenant = 6

	f := newIsolationFixture(t)

	var wg sync.WaitGroup
	mismatches := make(chan string, isolationTenants*perTenant)

	start := make(chan struct{})

	for _, host := range f.hosts {
		want := expectedBody(host)

		for i := 0; i < perTenant; i++ {
			wg.Add(1)
			go func(host, want string) {
				defer wg.Done()

				// Line every goroutine up before any of them runs, so all 360
				// requests hit a completely cold cache at once.
				<-start

				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Host = host
				rec := httptest.NewRecorder()

				f.handler.ServeHTTP(rec, req)

				if got := rec.Body.String(); got != want {
					mismatches <- fmt.Sprintf("host %s received %q, want %q", host, got, want)
				}
			}(host, want)
		}
	}

	close(start)
	wg.Wait()
	close(mismatches)

	var failures int
	for msg := range mismatches {
		failures++
		if failures <= 10 {
			t.Error(msg)
		}
	}
	if failures > 0 {
		t.Fatalf("%d responses were wrong on a cold cache", failures)
	}

	// One read per tenant, even though every request started cold.
	if got := f.registry.lookupCount(); got != isolationTenants {
		t.Errorf("registry reads on a cold cache = %d, want %d", got, isolationTenants)
	}
}

// TestTenantIsolationAcrossDisabledAndActive mixes routable and non-routable
// tenants in the same burst.
//
// A cache that keyed only on "is there an entry" rather than on the slug could
// serve a disabled tenant's 404 to an active one, or worse, route a disabled
// tenant. Interleaving them makes that failure visible.
func TestTenantIsolationAcrossDisabledAndActive(t *testing.T) {
	const perTenant = 6

	routes := make(map[string]tenant.Route)
	hosts := make([]string, 0, isolationTenants)

	for i := 0; i < isolationTenants; i++ {
		slug := fmt.Sprintf("tenant-%02d", i)

		status := tenant.StatusActive
		if i%3 == 0 {
			status = tenant.StatusDisabled
		}

		routes[slug] = tenant.Route{TenantSlug: slug, Product: tenant.ProductPOS, Status: status}
		hosts = append(hosts, slug+".zensys.lk")
	}

	registry := &fakeRegistry{routes: routes}
	cache := tenant.NewCache(registry, tenant.CacheOptions{
		RouteTTL:    time.Minute,
		NegativeTTL: time.Minute,
		MaxEntries:  10 * isolationTenants,
	})

	cfg := testConfig()
	logger := discardLogger()
	handler := NewPublicHandler(
		cfg, logger,
		routing.NewResolver(cache),
		NewApexHandler(ApexNotFound, "", logger),
		echoUpstream{},
	)

	var wg sync.WaitGroup
	mismatches := make(chan string, isolationTenants*perTenant)

	for i, host := range hosts {
		slug := strings.TrimSuffix(host, ".zensys.lk")

		active := i%3 != 0
		wantStatus := http.StatusNotFound
		wantBody := `{"error":"unknown_tenant","message":"The requested host is not registered."}`
		if active {
			wantStatus = http.StatusOK
			wantBody = fmt.Sprintf("%s|%s|%s", slug, tenant.ProductPOS, tenant.StatusActive)
		}

		for r := 0; r < perTenant; r++ {
			wg.Add(1)
			go func(host string, wantStatus int, wantBody string) {
				defer wg.Done()

				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Host = host
				rec := httptest.NewRecorder()

				handler.ServeHTTP(rec, req)

				if rec.Code != wantStatus {
					mismatches <- fmt.Sprintf("host %s: status %d, want %d (body %q)", host, rec.Code, wantStatus, rec.Body.String())
					return
				}
				if got := rec.Body.String(); got != wantBody {
					mismatches <- fmt.Sprintf("host %s received %q, want %q", host, got, wantBody)
				}
			}(host, wantStatus, wantBody)
		}
	}

	wg.Wait()
	close(mismatches)

	var failures int
	for msg := range mismatches {
		failures++
		if failures <= 10 {
			t.Error(msg)
		}
	}
	if failures > 0 {
		t.Fatalf("%d responses mixed up tenants or their status", failures)
	}
}
