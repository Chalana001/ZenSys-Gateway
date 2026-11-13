package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/tenant"
)

// headerUpstream is the terminal step for the header-contract tests.
//
// It does what M5's proxy Rewrite will do: read the connection facts and the
// routing decision out of the request context, then write the outbound headers
// with routing.ApplyForwarded. Calling the same function the proxy will call
// means these tests prove the pipeline and the header policy compose — what
// they cannot prove is that M5 calls it, which is what M5's own tests are for.
//
// It records the inbound headers separately from the outbound ones so that the
// two halves of §24's delete-then-set rule can be asserted independently: the
// client's value is gone *before* the upstream sees the request, and the
// canonical value is present.
type headerUpstream struct {
	policy routing.OutboundPolicy

	Inbound  http.Header
	Received http.Header

	Edge    routing.Edge
	Info    routing.Info
	HasInfo bool
}

func (u *headerUpstream) Serve(w http.ResponseWriter, r *http.Request, info routing.Info) {
	u.Inbound = r.Header.Clone()
	u.Edge, _ = routing.EdgeFrom(r.Context())
	u.Info, u.HasInfo = routing.InfoFrom(r.Context())

	out := r.Header.Clone()
	routing.ApplyForwarded(out, u.Edge, info.Tenant, u.policy)
	u.Received = out

	w.WriteHeader(http.StatusOK)
}

func newHeaderPipeline(
	t *testing.T,
	cfg *config.Config,
	registry routing.Lookup,
	policy routing.OutboundPolicy,
) (http.Handler, *headerUpstream) {
	t.Helper()

	logger := discardLogger()
	apex := NewApexHandler(cfg.Platform.ApexBehavior, cfg.Platform.LandingDir, logger)
	upstream := &headerUpstream{policy: policy}

	handler := NewPublicHandler(cfg, logger, routing.NewResolver(registry), apex, upstream)

	return StandardChain(handler, logger, ChainOptions{
		RequestIDHeader: cfg.Proxy.RequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
	}), upstream
}

// doWithHeaders issues a request with explicit headers and peer address.
func doWithHeaders(
	t *testing.T,
	h http.Handler,
	host, path, remoteAddr string,
	header http.Header,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	req.RemoteAddr = remoteAddr
	for k, vv := range header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// injectTenant is the outbound policy the M4 exit criteria are stated against:
// the canonical tenant is written into X-Tenant-ID for products that read it.
var injectTenant = routing.OutboundPolicy{InjectTenantHeader: true}

// mangalaRegistry is the fixture every header test routes against.
func mangalaRegistry() *fakeRegistry {
	return &fakeRegistry{routes: map[string]tenant.Route{
		"mangala": activeRoute("mangala", tenant.ProductPOS),
	}}
}

// TestHeaderContract_SpoofedTenantHeaderNeverReachesTheUpstream is plan §91.1
// and architecture doc §24: the host-derived tenant is canonical, and a
// client-supplied X-Tenant-ID can never influence routing.
//
// Both halves are asserted, because passing only the second would be possible
// by overwriting the client's value — which would still leave it readable by
// anything that inspected the request before the overwrite.
func TestHeaderContract_SpoofedTenantHeaderNeverReachesTheUpstream(t *testing.T) {
	handler, upstream := newHeaderPipeline(t, testConfig(), mangalaRegistry(), injectTenant)

	rec := doWithHeaders(t, handler, "mangala.zensys.lk", "/api/orders", untrustedPeer, http.Header{
		"X-Tenant-ID": {"spoofed"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if got := upstream.Inbound.Get("X-Tenant-ID"); got != "" {
		t.Errorf("the upstream received the client's X-Tenant-ID %q; it must be deleted before anything reads the request", got)
	}
	if got := upstream.Received.Get("X-Tenant-ID"); got != "mangala" {
		t.Errorf("X-Tenant-ID = %q, want %q", got, "mangala")
	}
	if upstream.Info.Tenant != "mangala" {
		t.Errorf("the routing decision was made for tenant %q, want %q", upstream.Info.Tenant, "mangala")
	}
}

// TestHeaderContract_SpoofedTenantHeaderIsIgnoredEvenFromATrustedProxy pins the
// absolute reading of §24. A trusted proxy is trusted to describe the
// connection, not to choose a tenant.
func TestHeaderContract_SpoofedTenantHeaderIsIgnoredEvenFromATrustedProxy(t *testing.T) {
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"mangala": activeRoute("mangala", tenant.ProductPOS),
		"other":   activeRoute("other", tenant.ProductGMS),
	}}
	handler, upstream := newHeaderPipeline(t, testConfig(), registry, injectTenant)

	doWithHeaders(t, handler, "mangala.zensys.lk", "/", trustedPeer, http.Header{
		"X-Tenant-ID": {"other"},
	})

	if upstream.Info.Tenant != "mangala" {
		t.Errorf("tenant = %q, want %q: a trusted proxy must not be able to redirect a request to another tenant",
			upstream.Info.Tenant, "mangala")
	}
	if got := upstream.Received.Get("X-Tenant-ID"); got != "mangala" {
		t.Errorf("X-Tenant-ID = %q, want %q", got, "mangala")
	}
}

// TestHeaderContract_ForwardedHostDoesNotChangeRouting is the other half of the
// same rule, for the header that *is* believed from a trusted proxy.
//
// X-Forwarded-Host is used only to describe the request downstream. If it
// influenced routing, a trusted proxy — or anyone who could forge a request
// from a trusted address — could serve one tenant's traffic under another's
// hostname.
func TestHeaderContract_ForwardedHostDoesNotChangeRouting(t *testing.T) {
	registry := &fakeRegistry{routes: map[string]tenant.Route{
		"mangala": activeRoute("mangala", tenant.ProductPOS),
		"other":   activeRoute("other", tenant.ProductGMS),
	}}
	handler, upstream := newHeaderPipeline(t, testConfig(), registry, injectTenant)

	doWithHeaders(t, handler, "mangala.zensys.lk", "/", trustedPeer, http.Header{
		"X-Forwarded-Host": {"other.zensys.lk"},
	})

	if upstream.Info.Tenant != "mangala" {
		t.Errorf("tenant = %q, want %q: X-Forwarded-Host influenced routing", upstream.Info.Tenant, "mangala")
	}
	if upstream.Info.Host != "mangala.zensys.lk" {
		t.Errorf("the recorded host is %q, want the host the request was routed on", upstream.Info.Host)
	}
	if got := upstream.Received.Get("X-Forwarded-Host"); got != "other.zensys.lk" {
		t.Errorf("X-Forwarded-Host = %q, want the trusted proxy's value: it describes the request, so it is advertised",
			got)
	}
}

// TestHeaderContract_UntrustedForwardedHeadersAreDiscarded is the M4 exit
// criterion for a client that is not behind a named proxy.
func TestHeaderContract_UntrustedForwardedHeadersAreDiscarded(t *testing.T) {
	handler, upstream := newHeaderPipeline(t, testConfig(), mangalaRegistry(), injectTenant)

	doWithHeaders(t, handler, "mangala.zensys.lk", "/", untrustedPeer, http.Header{
		"X-Forwarded-Proto": {"https"},
		"X-Forwarded-Host":  {"evil.example.com"},
		"X-Forwarded-For":   {"9.9.9.9"},
	})

	if got := upstream.Inbound.Get("X-Forwarded-Proto"); got != "" {
		t.Errorf("an untrusted X-Forwarded-Proto survived as %q", got)
	}
	if got := upstream.Received.Get("X-Forwarded-Proto"); got != routing.ProtoHTTP {
		t.Errorf("X-Forwarded-Proto = %q, want %q", got, routing.ProtoHTTP)
	}
	if got := upstream.Received.Get("X-Forwarded-Host"); got != "mangala.zensys.lk" {
		t.Errorf("X-Forwarded-Host = %q, want the request's own host", got)
	}
	if got := upstream.Received.Get("X-Forwarded-For"); got != "192.0.2.1" {
		t.Errorf("X-Forwarded-For = %q, want the peer address", got)
	}
}

// TestHeaderContract_TrustedProxySchemeSurvivesToTheUpstream is the M4 exit
// criterion from plan §10.2, asserted through the whole pipeline: the Gateway
// sees plain HTTP from Nginx, the browser used HTTPS, and the product must be
// told HTTPS.
func TestHeaderContract_TrustedProxySchemeSurvivesToTheUpstream(t *testing.T) {
	handler, upstream := newHeaderPipeline(t, testConfig(), mangalaRegistry(), injectTenant)

	// The trailing entry is the address of the trusted hop itself, which is
	// loopback in the default trust set. It is skipped, leaving the client.
	doWithHeaders(t, handler, "mangala.zensys.lk", "/", trustedPeer, http.Header{
		"X-Forwarded-Proto": {"https"},
		"X-Forwarded-Host":  {"mangala.zensys.lk"},
		"X-Forwarded-For":   {"203.0.113.9, 127.0.0.1"},
	})

	if got := upstream.Received.Get("X-Forwarded-Proto"); got != routing.ProtoHTTPS {
		t.Fatalf("X-Forwarded-Proto = %q, want %q: a product building absolute URLs would emit http:// behind an https:// edge",
			got, routing.ProtoHTTPS)
	}
	if got := upstream.Received.Get("X-Forwarded-For"); got != "203.0.113.9" {
		t.Errorf("X-Forwarded-For = %q, want the resolved client rather than the whole chain", got)
	}
}

// TestHeaderContract_RoutingDecisionIsRecorded covers the context the milestone
// is named for: host, tenant, product, and the full route, reachable without
// re-deriving any of them.
func TestHeaderContract_RoutingDecisionIsRecorded(t *testing.T) {
	handler, upstream := newHeaderPipeline(t, testConfig(), mangalaRegistry(), injectTenant)

	do(t, handler, "mangala.zensys.lk", "/api/orders")

	if !upstream.HasInfo {
		t.Fatal("no routing decision was recorded for a request that resolved")
	}
	if upstream.Info.Host != "mangala.zensys.lk" {
		t.Errorf("Host = %q, want %q", upstream.Info.Host, "mangala.zensys.lk")
	}
	if upstream.Info.Tenant != "mangala" {
		t.Errorf("Tenant = %q, want %q", upstream.Info.Tenant, "mangala")
	}
	if upstream.Info.Product() != tenant.ProductPOS {
		t.Errorf("Product() = %q, want %q", upstream.Info.Product(), tenant.ProductPOS)
	}
	if upstream.Info.Route.TenantSlug != "mangala" {
		t.Errorf("Route.TenantSlug = %q, want %q", upstream.Info.Route.TenantSlug, "mangala")
	}
}

// TestHeaderContract_RejectedRequestsRecordNoDecision pins the distinction the
// access log depends on: a request that never reached a decision must not
// appear to have been routed to an empty tenant.
func TestHeaderContract_RejectedRequestsRecordNoDecision(t *testing.T) {
	handler, upstream := newHeaderPipeline(t, testConfig(), mangalaRegistry(), injectTenant)

	do(t, handler, "unknown.zensys.lk", "/")

	if upstream.HasInfo {
		t.Errorf("a routing decision was recorded for an unresolved host: %+v", upstream.Info)
	}
}

// TestHeaderContract_AccessLogCarriesTheRoutingDecision is the guard for the
// RequestState slot.
//
// The slot exists so that a middleware *outside* the routing handler can see
// what the handler decided. If RequestState were dropped from the chain the
// slot would be absent, InfoFrom would report false, and the log would silently
// lose its tenant and product fields — which is exactly the failure this test
// is here to catch.
func TestHeaderContract_AccessLogCarriesTheRoutingDecision(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := testConfig()
	apex := NewApexHandler(cfg.Platform.ApexBehavior, cfg.Platform.LandingDir, logger)
	upstream := &headerUpstream{policy: injectTenant}
	handler := NewPublicHandler(cfg, logger, routing.NewResolver(mangalaRegistry()), apex, upstream)

	chained := StandardChain(handler, logger, ChainOptions{
		RequestIDHeader: cfg.Proxy.RequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
	})

	do(t, chained, "mangala.zensys.lk", "/api/orders")

	line := buf.String()
	for _, want := range []string{`"tenant":"mangala"`, `"product":"POS"`, `"host":"mangala.zensys.lk"`} {
		if !strings.Contains(line, want) {
			t.Errorf("the access log line does not contain %s:\n%s", want, line)
		}
	}
}

// TestHeaderContract_AccessLogOmitsRoutingFieldsWhenUnrouted is the other side
// of the same guard: an empty tenant in the log would be indistinguishable from
// a tenant whose slug is empty.
func TestHeaderContract_AccessLogOmitsRoutingFieldsWhenUnrouted(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := testConfig()
	apex := NewApexHandler(cfg.Platform.ApexBehavior, cfg.Platform.LandingDir, logger)
	upstream := &headerUpstream{policy: injectTenant}
	handler := NewPublicHandler(cfg, logger, routing.NewResolver(mangalaRegistry()), apex, upstream)

	chained := StandardChain(handler, logger, ChainOptions{
		RequestIDHeader: cfg.Proxy.RequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
	})

	do(t, chained, "unknown.zensys.lk", "/")

	if line := buf.String(); strings.Contains(line, `"tenant"`) {
		t.Errorf("the access log reported a tenant for an unresolved host:\n%s", line)
	}
}

// --- middleware-level -----------------------------------------------------

func TestRequestID_AdoptsInboundIDOnlyFromATrustedProxy(t *testing.T) {
	tests := []struct {
		name        string
		remoteAddr  string
		inbound     string
		wantAdopted bool
	}{
		{"trusted proxy", trustedPeer, "trace-abc-123", true},
		{"untrusted client", untrustedPeer, "trace-abc-123", false},
		{"trusted proxy with no inbound ID", trustedPeer, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen string

			handler := StandardChain(
				http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					seen = RequestIDFrom(r.Context())
				}),
				discardLogger(),
				ChainOptions{
					RequestIDHeader: testRequestIDHeader,
					TrustedProxies:  trustedLoopback(t),
				},
			)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.inbound != "" {
				req.Header.Set(testRequestIDHeader, tc.inbound)
			}
			handler.ServeHTTP(rec, req)

			if tc.wantAdopted {
				if seen != tc.inbound {
					t.Errorf("request ID = %q, want the inbound %q", seen, tc.inbound)
				}
			} else {
				if seen == tc.inbound {
					t.Errorf("request ID = %q; an inbound ID from this source must not be adopted", seen)
				}
				if seen == "" {
					t.Error("no request ID was generated")
				}
			}

			if got := rec.Header().Get(testRequestIDHeader); got != seen {
				t.Errorf("echoed %q, context has %q", got, seen)
			}
		})
	}
}

func TestRequestID_RejectsMalformedInboundIDs(t *testing.T) {
	tests := []struct {
		name    string
		inbound string
		reason  string
	}{
		{"too long", strings.Repeat("a", maxRequestIDLength+1), "an unbounded value would be copied into every log line"},
		{"a space", "abc 123", "whitespace would split the value"},
		{"a comma", "abc,123", "a downstream consumer splitting on commas would read two values"},
		{"a newline", "abc\n123", "a newline is an attempt at log injection"},
		{"a carriage return", "abc\r123", "a carriage return is an attempt at log injection"},
		{"a slash", "abc/123", "outside the accepted identifier alphabet"},
		{"a quote", `abc"123`, "outside the accepted identifier alphabet"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen string

			handler := StandardChain(
				http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					seen = RequestIDFrom(r.Context())
				}),
				discardLogger(),
				ChainOptions{
					RequestIDHeader: testRequestIDHeader,
					TrustedProxies:  trustedLoopback(t),
				},
			)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = trustedPeer
			req.Header.Set(testRequestIDHeader, tc.inbound)
			handler.ServeHTTP(rec, req)

			if seen == tc.inbound {
				t.Fatalf("the malformed ID %q was adopted: %s", tc.inbound, tc.reason)
			}
			if seen == "" || len(seen) != 32 {
				t.Errorf("request ID = %q, want a generated 32-character ID", seen)
			}
		})
	}
}

// TestRequestID_FailsClosedWithoutTheTrustMiddleware documents the property
// that makes the two middlewares independently safe: if TrustedProxy is missing
// from a chain, every inbound ID is refused rather than every inbound ID being
// believed.
func TestRequestID_FailsClosedWithoutTheTrustMiddleware(t *testing.T) {
	var seen string

	handler := Chain(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = RequestIDFrom(r.Context())
		}),
		RequestID(testRequestIDHeader),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = trustedPeer
	req.Header.Set(testRequestIDHeader, "trace-abc-123")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if seen == "trace-abc-123" {
		t.Error("an inbound ID was adopted with no trusted-proxy middleware in the chain")
	}
}

// TestRequestState_IsReachableFromOutsideTheHandler is the property the slot
// exists for: a middleware wrapping the handler reads what the handler decided.
//
// The reader's position is the point of the test. It stands where AccessLog
// stands — inside RequestState, outside the routing handler — and not one layer
// further out. A middleware placed *outside* RequestState cannot see the
// decision, because the slot is installed by deriving the context and that
// derivation only travels inward. StandardChain keeps RequestState outermost
// for exactly this reason, and its documentation says so.
func TestRequestState_IsReachableFromOutsideTheHandler(t *testing.T) {
	var fromOutside routing.Info
	var foundOutside bool

	// Stands in for AccessLog: after the handler returns, it reads the
	// decision off the request it was given.
	reader := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			fromOutside, foundOutside = routing.InfoFrom(r.Context())
		})
	}

	// Chain lists the outermost middleware first, so RequestState comes first
	// and the reader — like AccessLog — sits inside it.
	handler := Chain(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			routing.SetInfo(r.Context(), routing.Info{Host: "mangala.zensys.lk", Tenant: "mangala"})
		}),
		RequestState(),
		Middleware(reader),
	)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !foundOutside {
		t.Fatal("the routing decision written by the handler was not visible to the middleware outside it")
	}
	if fromOutside.Tenant != "mangala" {
		t.Errorf("Tenant = %q, want %q", fromOutside.Tenant, "mangala")
	}
}

// TestRequestState_OutsideTheSlotSeesNothing documents the cost of the design
// so that it is a known property rather than a surprise.
//
// It is the reason StandardChain puts RequestState first and why the ordering
// comment there is load-bearing rather than decorative.
func TestRequestState_OutsideTheSlotSeesNothing(t *testing.T) {
	var found bool

	inner := Chain(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			routing.SetInfo(r.Context(), routing.Info{Tenant: "mangala"})
		}),
		RequestState(),
	)

	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r)
		_, found = routing.InfoFrom(r.Context())
	})

	outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if found {
		t.Error("a middleware outside RequestState saw the routing decision; the slot is no longer scoped as documented")
	}
}

// TestRequestState_AbsentSlotIsNotFatal pins the degraded behaviour: a chain
// without the slot must still serve requests rather than panic.
func TestRequestState_AbsentSlotIsNotFatal(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routing.SetInfo(r.Context(), routing.Info{Tenant: "mangala"})

		if _, ok := routing.InfoFrom(r.Context()); ok {
			t.Error("a decision was reported with no slot installed")
		}
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestStandardChain_TrustedProxyRunsBeforeRequestID pins the one ordering
// constraint inside the chain that the request-ID policy depends on.
//
// If RequestID ran first it would read the header before TrustedProxy had
// decided whether the request came from a proxy, and would have to make the
// trust decision itself — or, worse, guess.
func TestStandardChain_TrustedProxyRunsBeforeRequestID(t *testing.T) {
	var edgeVisibleToRequestID bool

	// The probe stands in for RequestID: it sits inside TrustedProxy and must
	// already be able to see the edge facts TrustedProxy installed.
	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, edgeVisibleToRequestID = routing.EdgeFrom(r.Context())
			w.WriteHeader(http.StatusOK)
		}),
		RequestState(),
		TrustedProxy(trustedLoopback(t), testRequestIDHeader),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = trustedPeer
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !edgeVisibleToRequestID {
		t.Error("the middleware inside TrustedProxy could not see the edge facts")
	}
}
