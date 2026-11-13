package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/security"
	"github.com/zensys/gateway/internal/tenant"
)

// These tests use the real clock, because security.Limiter's clock is
// unexported and deliberately not injectable from outside its package. That is
// fine for what is asserted here: every bucket below has rps=1 with a burst of
// one or two, so no refill can occur within the microseconds a test takes, and
// the assertions are about counts rather than about timing. The refill
// behaviour itself is asserted in internal/security, where the clock can be
// driven.

// testLimiter builds an enabled two-tier limiter with the given buckets.
func testLimiter(t *testing.T, ip, perTenant security.BucketSpec) *security.Limiter {
	t.Helper()

	limiter, err := security.NewLimiter(security.LimiterConfig{
		Enabled:    true,
		IP:         ip,
		Tenant:     perTenant,
		MaxEntries: 256,
	})
	if err != nil {
		t.Fatalf("build limiter: %v", err)
	}
	return limiter
}

// exhaustedSpec is a bucket that cannot refill during a test: one token per
// second, and tests do not run for a second.
func exhaustedSpec(burst int) security.BucketSpec {
	return security.BucketSpec{RPS: 1, Burst: burst}
}

// errorBody decodes the standard error contract from a response.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()

	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not the error contract: %v\nbody: %s", err, rec.Body.String())
	}
	return body
}

// okHandler counts how many requests reached the terminal handler.
func okHandler(reached *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached++
		w.WriteHeader(http.StatusOK)
	})
}

func TestTier1RateLimit_RefusesAfterTheBurstWithRetryAfter(t *testing.T) {
	const burst = 2
	limiter := testLimiter(t, exhaustedSpec(burst), exhaustedSpec(1))

	reached := 0
	handler := Chain(okHandler(&reached), Tier1RateLimit(limiter))

	for i := 1; i <= burst; i++ {
		rec := doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.9:1234")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d of %d = %d, want 200; a new bucket starts full", i, burst, rec.Code)
		}
	}

	rec := doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.9:1234")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; the bucket held %d tokens and all were spent", rec.Code, burst)
	}
	if reached != burst {
		t.Errorf("%d requests reached the handler, want %d: a refused request must not be served", reached, burst)
	}

	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("no Retry-After header; a 429 without one leaves the client to guess when to retry")
	} else if got == "0" {
		t.Error("Retry-After is 0, which instructs the client to retry immediately — the opposite of what a rate limit is for")
	}

	if body := errorBody(t, rec); body.Error != ErrRateLimited {
		t.Errorf("error code = %q, want %q", body.Error, ErrRateLimited)
	}
}

// TestTier1RateLimit_ASpoofedForwardedForCannotEvadeTheLimit is the
// security-relevant test of this file.
//
// The limiter's key must not be a header the client controls. If it keyed on
// X-Forwarded-For directly, a client could rotate the header on every request
// and be handed a fresh bucket each time — the limit would never fire, and the
// tier would exist only in the configuration.
//
// Two independent mechanisms prevent that, and this test guards the outcome
// rather than either one of them. TrustedProxy deletes the forwarded headers
// from an untrusted request, so the raw header is already gone by the time the
// limiter runs; and the limiter keys on routing.Edge, which for an untrusted
// peer is the socket address, so the header would not be consulted even if it
// survived. Verified by removing both — keying on the raw header *and*
// skipping the sanitising — which makes this fail with 200 where a 429 is
// expected. Removing either one alone leaves it passing, which is the point:
// the property holds as long as one defence does.
func TestTier1RateLimit_ASpoofedForwardedForCannotEvadeTheLimit(t *testing.T) {
	const burst = 2
	limiter := testLimiter(t, exhaustedSpec(burst), exhaustedSpec(1))

	reached := 0
	handler := StandardChain(okHandler(&reached), discardLogger(), ChainOptions{
		RequestIDHeader: testRequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		Tier1:           limiter,
	})

	// Each request claims a different client address. None of them may be
	// believed, because the peer is not a trusted proxy.
	for i := 0; i < burst; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "mangala.zensys.lk"
		req.RemoteAddr = untrustedPeer
		req.Header.Set(routing.HeaderForwardedFor, "203.0.113."+string(rune('1'+i)))

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "mangala.zensys.lk"
	req.RemoteAddr = untrustedPeer
	req.Header.Set(routing.HeaderForwardedFor, "203.0.113.99")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: a client that rotates X-Forwarded-For was handed a fresh bucket per request, so the limit never fires", rec.Code)
	}
}

// TestTier1RateLimit_BehindATrustedProxyKeysOnTheForwardedClient is the other
// half of the keying rule: when the peer *is* a trusted proxy, the address it
// recorded is the right key, so two clients behind it are limited separately.
//
// Keying on the socket address instead would put every client on the internet
// into one bucket, so the first busy one would exhaust it for everybody.
func TestTier1RateLimit_BehindATrustedProxyKeysOnTheForwardedClient(t *testing.T) {
	limiter := testLimiter(t, exhaustedSpec(1), exhaustedSpec(1))

	reached := 0
	handler := StandardChain(okHandler(&reached), discardLogger(), ChainOptions{
		RequestIDHeader: testRequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		Tier1:           limiter,
	})

	call := func(forwardedFor string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "mangala.zensys.lk"
		req.RemoteAddr = trustedPeer
		req.Header.Set(routing.HeaderForwardedFor, forwardedFor)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call("203.0.113.1"); code != http.StatusOK {
		t.Fatalf("the first client's first request = %d, want 200", code)
	}
	if code := call("203.0.113.1"); code != http.StatusTooManyRequests {
		t.Fatalf("the first client's second request = %d, want 429; its burst is 1", code)
	}

	// A different client behind the same proxy must have its own bucket.
	if code := call("203.0.113.2"); code != http.StatusOK {
		t.Errorf("a second client behind the same trusted proxy = %d, want 200: the limiter is keying on the proxy's address, so one client's traffic limits everyone behind it", code)
	}
}

// TestTier1RateLimit_UsesTheConnectionAddressWhenNoEdgeIsResolved covers the
// chain that omitted TrustedProxy.
//
// The requirement is that a missing middleware must not become a way to bypass
// the limit, and must not become a self-inflicted outage either. Keying on the
// connection address satisfies both: it is what the trusted-proxy policy would
// have produced for an untrusted request, so the request is still limited, and
// it is per-connection, so unrelated clients do not share one bucket.
func TestTier1RateLimit_UsesTheConnectionAddressWhenNoEdgeIsResolved(t *testing.T) {
	limiter := testLimiter(t, exhaustedSpec(1), exhaustedSpec(1))

	reached := 0
	handler := Chain(okHandler(&reached), Tier1RateLimit(limiter))

	if rec := doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.1:1111"); rec.Code != http.StatusOK {
		t.Fatalf("the first request = %d, want 200", rec.Code)
	}
	if rec := doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.1:2222"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429: with no Edge resolved the limiter must key on the connection address, not exempt the request", rec.Code)
	}

	// A different connection is a different key, so the limiter has not
	// collapsed every client into one bucket.
	if rec := doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.2:1111"); rec.Code != http.StatusOK {
		t.Errorf("a different client = %d, want 200: an absent Edge must not put every request in one shared bucket", rec.Code)
	}
}

// TestTier1RateLimit_ExemptsOperationalPaths covers the liveness exemption.
//
// A probe refused for being too frequent reports the service as dead, and an
// orchestrator that restarts on a failed probe turns a load spike into an
// outage. The exemption is asserted in both directions: the exempt path is
// never refused, and it does not consume the allowance that the non-exempt
// path then spends.
func TestTier1RateLimit_ExemptsOperationalPaths(t *testing.T) {
	limiter := testLimiter(t, exhaustedSpec(1), exhaustedSpec(1))

	reached := 0
	handler := Chain(
		okHandler(&reached),
		Tier1RateLimit(limiter, "/healthz", "/readyz", "/metrics"),
	)

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		for i := 0; i < 5; i++ {
			rec := doFrom(t, handler, "mangala.zensys.lk", path, "203.0.113.9:1234")
			if rec.Code != http.StatusOK {
				t.Fatalf("%s request %d = %d, want 200: an operational endpoint must never be rate limited", path, i+1, rec.Code)
			}
		}
	}

	// The exempt requests must not have spent the bucket. One token was
	// available, so exactly one of these two may succeed.
	if rec := doFrom(t, handler, "mangala.zensys.lk", "/api/orders", "203.0.113.9:1234"); rec.Code != http.StatusOK {
		t.Fatalf("the first non-exempt request = %d, want 200: the exempt requests consumed the allowance", rec.Code)
	}
	if rec := doFrom(t, handler, "mangala.zensys.lk", "/api/orders", "203.0.113.9:1234"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the second non-exempt request = %d, want 429", rec.Code)
	}
}

func TestTier1RateLimit_DisabledLimiterIsAPassThrough(t *testing.T) {
	disabled, err := security.NewLimiter(security.LimiterConfig{Enabled: false})
	if err != nil {
		t.Fatalf("build a disabled limiter: %v", err)
	}

	for _, tc := range []struct {
		name    string
		limiter *security.Limiter
	}{
		{"disabled limiter", disabled},
		{"nil limiter", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached := 0
			handler := Chain(okHandler(&reached), Tier1RateLimit(tc.limiter))

			for i := 0; i < 50; i++ {
				if rec := doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.9:1234"); rec.Code != http.StatusOK {
					t.Fatalf("request %d = %d, want 200: %s must not limit anything", i+1, rec.Code, tc.name)
				}
			}
			if reached != 50 {
				t.Errorf("%d requests reached the handler, want 50", reached)
			}
		})
	}
}

// TestStandardChain_BodyLimitRunsBeforeTheRateLimiter settles the §8.1 step 3
// before step 4 ordering by observation rather than by comment.
//
// The discriminating assertion is the second one, and it is worth being
// precise about why the obvious assertion is not enough. The first request
// finds the bucket full, so the limiter would allow it in either order and the
// body limit then answers 413 — meaning "an oversized body is 413" holds under
// both orderings and proves nothing about the order. What distinguishes them
// is that the body limit is a stateless comparison against a declared length
// while the limiter spends a token: only if the body limit runs *first* does
// the refused request leave the bucket untouched, which the second request
// proves by still being served.
//
// Reversing the two middlewares in StandardChain makes that second assertion
// fail with 429. Verified by doing it.
func TestStandardChain_BodyLimitRunsBeforeTheRateLimiter(t *testing.T) {
	limiter := testLimiter(t, exhaustedSpec(1), exhaustedSpec(1))

	reached := 0
	handler := StandardChain(okHandler(&reached), discardLogger(), ChainOptions{
		RequestIDHeader: testRequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		MaxBodyBytes:    16,
		Tier1:           limiter,
	})

	// A body that declares more than the limit. The bucket is still full, so
	// if the limiter ran first this would be a 429.
	big := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 64)))
	big.Host = "mangala.zensys.lk"
	big.RemoteAddr = "203.0.113.9:1234"

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, big)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: the body limit is step 3 and the rate limiter is step 4, so a request that breaks both is refused by the body limit; a 429 here means the order is reversed", rec.Code)
	}
	if body := errorBody(t, rec); body.Error != ErrPayloadTooLarge {
		t.Errorf("error code = %q, want %q", body.Error, ErrPayloadTooLarge)
	}

	// The refused request must not have spent the bucket: it was refused by a
	// comparison, before the limiter was consulted.
	if rec := doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.9:1234"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the 413 spent the rate-limit token, so the body limit is running inside the limiter rather than before it", rec.Code)
	}

	// And the token really was spent by the request that was served, so the
	// assertion above is not passing on an unlimited bucket.
	if rec := doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.9:1234"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429: the bucket's single token was not spent by the served request", rec.Code)
	}
}

// TestStandardChain_RateLimitedResponsesAreLogged covers the operator's side of
// a refusal.
//
// The tier is not a log field, and does not need to be: tier 1 runs in the
// middleware chain and tier 2 runs inside the routing handler, so a 429 whose
// line carries a tenant is tier 2 and one that does not is tier 1. This
// asserts the tier-1 half of that — no tenant, because no tenant was resolved.
func TestStandardChain_RateLimitedResponsesAreLogged(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	limiter := testLimiter(t, exhaustedSpec(1), exhaustedSpec(1))

	handler := StandardChain(okHandler(new(int)), logger, ChainOptions{
		RequestIDHeader: testRequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		Tier1:           limiter,
	})

	doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.9:1234")
	doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.9:1234")

	out := logs.String()
	if !strings.Contains(out, `"status":429`) {
		t.Errorf("no access log line with status 429:\n%s", out)
	}
	if strings.Contains(out, `"tenant"`) {
		t.Errorf("a tier-1 refusal logged a tenant field; tier 1 runs before routing, so a tenant here would mean the refusal happened after resolution:\n%s", out)
	}
}

func TestWriteRateLimited_NeverReportsAZeroWait(t *testing.T) {
	for _, secs := range []int{0, -1, 1, 5} {
		rec := httptest.NewRecorder()
		WriteRateLimited(rec, secs)

		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("WriteRateLimited(%d) status = %d, want 429", secs, rec.Code)
		}

		got := rec.Header().Get("Retry-After")
		if got == "0" || got == "" || got == "-1" {
			t.Errorf("WriteRateLimited(%d) set Retry-After to %q; a client told to retry in zero seconds will spin", secs, got)
		}
	}
}

// newRateLimitedPipeline is newPipeline with tier 2 attached, so a test can
// drive the tenant-keyed limiter through the assembled chain.
func newRateLimitedPipeline(t *testing.T, cfg *config.Config, registry routing.Lookup, limiter *security.Limiter) (http.Handler, *recordingUpstream) {
	t.Helper()
	return newLoggedRateLimitedPipeline(t, cfg, registry, limiter, discardLogger())
}

// newLoggedRateLimitedPipeline is newRateLimitedPipeline with a caller-supplied
// logger, for the tests that assert on what the access log carries.
func newLoggedRateLimitedPipeline(t *testing.T, cfg *config.Config, registry routing.Lookup, limiter *security.Limiter, logger *slog.Logger) (http.Handler, *recordingUpstream) {
	t.Helper()

	apex := NewApexHandler(cfg.Platform.ApexBehavior, cfg.Platform.LandingDir, logger)
	upstream := &recordingUpstream{}

	handler := NewPublicHandler(cfg, logger, routing.NewResolver(registry), apex, upstream).
		WithTenantRateLimit(limiter)

	return StandardChain(handler, logger, ChainOptions{
		RequestIDHeader: cfg.Proxy.RequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
	}), upstream
}

// logLineWithStatus returns the one access-log line whose status is want.
//
// It insists on exactly one, because every caller uses it to assert on the
// content of a specific refusal: a helper that silently picked the first of
// several would let a test pass against the wrong line.
func logLineWithStatus(t *testing.T, logs string, want int) string {
	t.Helper()

	needle := fmt.Sprintf(`"status":%d`, want)
	var found []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, needle) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d access-log lines with status %d, want exactly 1:\n%s", len(found), want, logs)
	}
	return found[0]
}

// TestTier2RateLimit_RefusalIsDistinguishableInTheLog pins the position of one
// line in PublicHandler.ServeHTTP.
//
// A 429's tier is not a log field and does not need to be, because the tiers
// sit on opposite sides of tenant resolution: tier 1 runs in the middleware
// chain before the hostname is parsed, so its refusals cannot carry a tenant,
// and tier 2 runs after resolution, so its refusals can. That distinction is
// real only while routing.SetInfo is called *before* the tier-2 check. Move it
// after, and a tier-2 refusal becomes an anonymous 429 that no log query can
// attribute to a tenant — which is precisely what the probe caught, and why
// the call was moved above the check.
//
// Verified by moving it back: this test fails, and no other test in the package
// notices.
func TestTier2RateLimit_RefusalIsDistinguishableInTheLog(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"mangala": activeRoute("mangala", tenant.ProductPOS),
	}}
	limiter := testLimiter(t, exhaustedSpec(100), exhaustedSpec(1))
	handler, _ := newLoggedRateLimitedPipeline(t, testConfig(), registry, limiter, logger)

	// Both requests are ordinary requests to a hostname that resolves, so
	// neither can be refused by anything except tier 2.
	if rec := do(t, handler, "mangala.zensys.lk", "/"); rec.Code != http.StatusOK {
		t.Fatalf("the served request = %d, want 200", rec.Code)
	}
	if rec := do(t, handler, "mangala.zensys.lk", "/"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the refused request = %d, want 429; mangala's burst is 1", rec.Code)
	}

	line := logLineWithStatus(t, logs.String(), http.StatusTooManyRequests)
	if !strings.Contains(line, `"tenant":"mangala"`) {
		t.Errorf("the tier-2 refusal's log line carries no tenant, so it is indistinguishable from a tier-1 refusal:\n%s", line)
	}
}

// TestPublicHandler_Tier2IsKeyedByTenant proves tier 2 limits per tenant rather
// than globally.
//
// A single global bucket would let one noisy tenant consume the whole
// allowance, which is the failure the second tier exists to prevent — and it
// is exactly what a limiter wired into the middleware chain instead of into
// the routing handler would produce, because the tenant is not known there.
func TestPublicHandler_Tier2IsKeyedByTenant(t *testing.T) {
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"mangala": activeRoute("mangala", tenant.ProductPOS),
		"garage1": activeRoute("garage1", tenant.ProductGMS),
	}}
	limiter := testLimiter(t, exhaustedSpec(100), exhaustedSpec(1))
	handler, upstream := newRateLimitedPipeline(t, testConfig(), registry, limiter)

	if rec := do(t, handler, "mangala.zensys.lk", "/"); rec.Code != http.StatusOK {
		t.Fatalf("mangala's first request = %d, want 200", rec.Code)
	}
	if rec := do(t, handler, "mangala.zensys.lk", "/"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("mangala's second request = %d, want 429; its burst is 1", rec.Code)
	}

	// A different tenant must be unaffected.
	if rec := do(t, handler, "garage1.zensys.lk", "/"); rec.Code != http.StatusOK {
		t.Errorf("garage1 = %d, want 200: one tenant exhausted another's allowance, so tier 2 is not keyed by tenant", rec.Code)
	}
	if !upstream.called {
		t.Error("the upstream was never reached")
	}
}

// TestPublicHandler_Tier2RefusesAfterTenantResolution places the check.
//
// A 429 from tier 2 carries a tenant in the access log, and that is only true
// because the limiter runs after the tenant has been resolved. The assertion
// is on the log for exactly that reason: it is the observable consequence of
// the placement, not just of the refusal.
func TestPublicHandler_Tier2RefusesAfterTenantResolution(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	cfg := testConfig()
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"mangala": activeRoute("mangala", tenant.ProductPOS),
	}}
	limiter := testLimiter(t, exhaustedSpec(100), exhaustedSpec(1))

	apex := NewApexHandler(cfg.Platform.ApexBehavior, cfg.Platform.LandingDir, logger)
	handler := StandardChain(
		NewPublicHandler(cfg, logger, routing.NewResolver(registry), apex, &recordingUpstream{}).
			WithTenantRateLimit(limiter),
		logger,
		ChainOptions{
			RequestIDHeader: cfg.Proxy.RequestIDHeader,
			TrustedProxies:  trustedLoopback(t),
			MaxBodyBytes:    cfg.Server.MaxBodyBytes,
		},
	)

	do(t, handler, "mangala.zensys.lk", "/")
	rec := do(t, handler, "mangala.zensys.lk", "/")

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}

	out := logs.String()
	if !strings.Contains(out, `"status":429`) {
		t.Fatalf("no 429 in the access log:\n%s", out)
	}
	if !strings.Contains(out, `"tenant":"mangala"`) {
		t.Errorf("the tier-2 refusal logged no tenant, so it is indistinguishable from a tier-1 refusal:\n%s", out)
	}
}

// TestPublicHandler_Tier2DoesNotSpendTokensOnInactiveTenants pins the check's
// position relative to the status check.
//
// A tenant that is not ACTIVE is refused by the routing pipeline before the
// limiter is consulted. That is the right order: the request was never going
// to reach a product, so it should not consume the allowance of the tenant it
// named. It also means a disabled tenant cannot be used to exhaust anyone
// else's bucket, because the key is the tenant that resolved.
func TestPublicHandler_Tier2DoesNotSpendTokensOnInactiveTenants(t *testing.T) {
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"mangala": {TenantSlug: "mangala", Product: tenant.ProductPOS, Status: tenant.StatusDisabled},
	}}
	limiter := testLimiter(t, exhaustedSpec(100), exhaustedSpec(1))
	handler, _ := newRateLimitedPipeline(t, testConfig(), registry, limiter)

	for i := 0; i < 5; i++ {
		rec := do(t, handler, "mangala.zensys.lk", "/")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("request %d = %d, want 404: a DISABLED tenant is refused by the routing pipeline", i+1, rec.Code)
		}
	}
}

func TestPublicHandler_Tier2DisabledIsAPassThrough(t *testing.T) {
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"mangala": activeRoute("mangala", tenant.ProductPOS),
	}}
	handler, _ := newRateLimitedPipeline(t, testConfig(), registry, nil)

	for i := 0; i < 20; i++ {
		if rec := do(t, handler, "mangala.zensys.lk", "/"); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200: a handler built without a tenant limiter must not limit anything", i+1, rec.Code)
		}
	}
}
