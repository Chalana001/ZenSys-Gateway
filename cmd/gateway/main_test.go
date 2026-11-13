package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/httpapi"
	"github.com/zensys/gateway/internal/proxy"
	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/security"
	"github.com/zensys/gateway/internal/tenant"
)

// discardLogger keeps test output readable. The behaviours under test are the
// returned errors and the observable network state, not the log lines.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testConfig returns a configuration that satisfies every readiness check, so
// a failing assertion points at the check rather than at the fixture.
func testConfig() *config.Config {
	cfg := config.Default()
	cfg.Products = map[string]config.ProductConfig{
		"POS": {
			FrontendUpstream: "http://127.0.0.1:3001",
			BackendUpstream:  "http://127.0.0.1:8082",
		},
		"GMS": {
			FrontendUpstream: "http://127.0.0.1:3002",
			BackendUpstream:  "http://127.0.0.1:8083",
		},
	}
	return cfg
}

// deadRegistry returns a repository whose database is unreachable.
//
// sql.Open does not connect, so this yields a perfectly usable handle for an
// address nothing is listening on. Port 3399 is used because it is not a port
// the Gateway or its products bind.
func deadRegistry(t *testing.T) *tenant.MySQLRepository {
	t.Helper()

	db, err := sql.Open("mysql", "root:root@tcp(127.0.0.1:3399)/zensys_gateway?parseTime=true")
	if err != nil {
		t.Fatalf("open registry pool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return tenant.NewMySQLRepository(db, tenant.MySQLOptions{QueryTimeout: time.Second})
}

func checkNames(checks []httpapi.Check) []string {
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Name)
	}
	return names
}

func findCheck(t *testing.T, checks []httpapi.Check, name string) httpapi.Check {
	t.Helper()

	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no readiness check named %q; got %v", name, checkNames(checks))
	return httpapi.Check{}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// freeAddr reserves a port and releases it, so the test can hand a concrete
// address to newServer. There is a small window between the close and the
// bind; that is acceptable for a test and avoids the alternative of reaching
// into the server to discover an ephemeral port after the fact.
func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

func waitUntilServing(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing accepted a connection on %s", addr)
}

// TestReadinessChecks_RegistryIsOnlyCheckedWhenOneExists pins the contract that
// a nil registry produces no registry check. Without this, a test fixture that
// forgets to supply a registry would silently report ready.
func TestReadinessChecks_RegistryIsOnlyCheckedWhenOneExists(t *testing.T) {
	cfg := testConfig()

	withoutRegistry := checkNames(readinessChecks(cfg, nil))
	if contains(withoutRegistry, "registry") {
		t.Errorf("readiness reported a registry check with no registry: %v", withoutRegistry)
	}

	withRegistry := checkNames(readinessChecks(cfg, deadRegistry(t)))
	if !contains(withRegistry, "registry") {
		t.Errorf("readiness did not report a registry check: %v", withRegistry)
	}
}

// TestReadinessChecks_RegistryCheckQueriesLiveState proves the check asks the
// database each time rather than replaying a result captured at startup.
//
// The Gateway is explicitly designed to start with an unreachable registry and
// report itself not-ready until it recovers (§46). A check that recorded its
// answer once would report "ok" forever and make /readyz useless for the one
// condition it exists to report.
func TestReadinessChecks_RegistryCheckQueriesLiveState(t *testing.T) {
	checks := readinessChecks(testConfig(), deadRegistry(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := findCheck(t, checks, "registry").Run(ctx); err == nil {
		t.Fatal("registry check passed against an unreachable database")
	}
}

func TestReadinessChecks_UpstreamsCheckFailsOnMissingUpstream(t *testing.T) {
	cfg := testConfig()
	// A product with a frontend but no backend cannot route /api traffic.
	cfg.Products["POS"] = config.ProductConfig{FrontendUpstream: "http://127.0.0.1:3001"}

	checks := readinessChecks(cfg, nil)
	if err := findCheck(t, checks, "upstreams").Run(context.Background()); err == nil {
		t.Fatal("upstreams check passed with a missing backend upstream")
	}
}

func TestReadinessChecks_UpstreamsCheckFailsOnMissingProduct(t *testing.T) {
	cfg := testConfig()
	delete(cfg.Products, "GMS")

	checks := readinessChecks(cfg, nil)
	if err := findCheck(t, checks, "upstreams").Run(context.Background()); err == nil {
		t.Fatal("upstreams check passed with a required product absent")
	}
}

// TestServe_DrainsOnContextCancel covers the graceful shutdown path end to end:
// the listener must serve, then stop accepting and return nil once the context
// is cancelled.
func TestServe_DrainsOnContextCancel(t *testing.T) {
	addr := freeAddr(t)
	logger := discardLogger()

	srv := newServer(addr, testConfig(), logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- serve(ctx, srv, logger, "test", 5*time.Second) }()

	waitUntilServing(t, addr)

	// Keep-alives are disabled so the client cannot leave a pooled connection
	// open that Shutdown would then wait on.
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}

	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("request before shutdown: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status before shutdown = %d, want 200", resp.StatusCode)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v; a graceful shutdown must return nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after the context was cancelled")
	}

	// The port must actually be released, not merely slow to answer.
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("listener still accepting connections after shutdown")
	}
}

// TestServe_ReportsBindFailure proves a bind error is returned rather than
// swallowed. If it were swallowed, the process would appear healthy while
// serving nothing, and an orchestrator would never learn about it.
func TestServe_ReportsBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	logger := discardLogger()
	srv := newServer(occupied.Addr().String(), testConfig(), logger, http.NotFoundHandler())

	// The context is never cancelled, so the only way serve can return is via
	// the listener's own error.
	if err := serve(context.Background(), srv, logger, "test", time.Second); err == nil {
		t.Fatal("serve returned nil for an address that is already bound")
	}
}

// assembledPublicHandler builds the public listener's chain exactly as run()
// does, against a registry nothing is listening on.
//
// The dead registry is enough for these tests: the trust and request-ID
// decisions happen before any lookup, so the 503 that follows is a convenient
// terminal state rather than something the assertions depend on.
func assembledPublicHandler(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()

	registry := deadRegistry(t)
	routeCache := tenant.NewCache(registry, tenant.CacheOptions{
		RouteTTL:    cfg.Cache.RouteTTL.Std(),
		NegativeTTL: cfg.Cache.NegativeTTL.Std(),
		MaxEntries:  cfg.Cache.MaxEntries,
	})

	tp, err := routing.ParseTrustedProxies(cfg.Proxy.TrustedProxies)
	if err != nil {
		t.Fatalf("parse trusted proxies: %v", err)
	}

	// The real proxy, not a stub. These tests never reach it — the registry is
	// dead, so every request stops at 503 before a route exists — but wiring
	// the real one means proxy.New is exercised by the process's own assembly
	// path rather than only by its own tests.
	upstream, err := proxy.New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("build proxy: %v", err)
	}

	// The real limiter, built the same way run() builds it, rather than nil.
	// The chain's rate-limit behaviour is a property of the assembled chain —
	// where the middleware sits relative to the body limit and to
	// TrustedProxy — and passing nil here would leave that assembly untested.
	limiter, err := security.NewLimiter(security.LimiterConfig{
		Enabled:    cfg.RateLimit.Enabled,
		IP:         security.BucketSpec{RPS: cfg.RateLimit.IP.RPS, Burst: cfg.RateLimit.IP.Burst},
		Tenant:     security.BucketSpec{RPS: cfg.RateLimit.Tenant.RPS, Burst: cfg.RateLimit.Tenant.Burst},
		MaxEntries: cfg.RateLimit.MaxEntries,
	})
	if err != nil {
		t.Fatalf("build rate limiter: %v", err)
	}

	return publicHandler(cfg, discardLogger(), registry, routeCache, tp, upstream, limiter)
}

// TestPublicChain_AdoptsAnInboundRequestIDFromATrustedProxy drives the
// assembled chain rather than a test double of it.
//
// The unit tests prove RequestID's policy; this proves the policy is actually
// in the chain the process builds, in an order where TrustedProxy has already
// made the trust decision. A missing or reordered middleware would show up
// here and nowhere else.
func TestPublicChain_AdoptsAnInboundRequestIDFromATrustedProxy(t *testing.T) {
	cfg := testConfig()
	handler := assembledPublicHandler(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "mangala.zensys.lk"
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set(cfg.Proxy.RequestIDHeader, "trace-from-nginx")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(cfg.Proxy.RequestIDHeader); got != "trace-from-nginx" {
		t.Errorf("%s = %q, want the inbound value: an inbound ID from a trusted proxy must be adopted",
			cfg.Proxy.RequestIDHeader, got)
	}
}

// TestPublicChain_RefusesAnInboundRequestIDFromAnUntrustedSource is the other
// half, and the security-relevant one: a client must not be able to choose the
// identifier that appears in the Gateway's logs and the products'.
func TestPublicChain_RefusesAnInboundRequestIDFromAnUntrustedSource(t *testing.T) {
	cfg := testConfig()
	handler := assembledPublicHandler(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "mangala.zensys.lk"
	req.RemoteAddr = "192.0.2.1:54321"
	req.Header.Set(cfg.Proxy.RequestIDHeader, "forged-by-client")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get(cfg.Proxy.RequestIDHeader)
	if got == "forged-by-client" {
		t.Fatal("the client's request ID was echoed back; a client must not choose its own identifier")
	}
	if len(got) != 32 {
		t.Errorf("%s = %q, want a generated 32-character ID", cfg.Proxy.RequestIDHeader, got)
	}
}
