package routing

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The two peer addresses the header tests need: 127.0.0.1 is in the default
// trusted set, and 192.0.2.1 is TEST-NET-1, which is what httptest picks and
// which no deployment trusts.
const (
	trustedPeer   = "127.0.0.1:54321"
	untrustedPeer = "192.0.2.1:54321"
)

// newEdgeRequest builds a request the way a listener would hand it over.
func newEdgeRequest(remoteAddr string, header map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "mangala.zensys.lk"
	r.RemoteAddr = remoteAddr
	for k, v := range header {
		r.Header.Set(k, v)
	}
	return r
}

func mustTrusted(t *testing.T, entries ...string) *TrustedProxies {
	t.Helper()

	tp, err := ParseTrustedProxies(entries)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%q): %v", entries, err)
	}
	return tp
}

// TestResolveEdge_UntrustedSourceIsNotBelieved is the core of the trust policy:
// a request that did not come through a named proxy contributes no headers at
// all, no matter what it sent.
func TestResolveEdge_UntrustedSourceIsNotBelieved(t *testing.T) {
	tp := mustTrusted(t, "127.0.0.1/32")

	edge := ResolveEdge(newEdgeRequest(untrustedPeer, map[string]string{
		HeaderForwardedProto: "https",
		HeaderForwardedHost:  "evil.example.com",
		HeaderForwardedFor:   "9.9.9.9",
	}), tp)

	if edge.Trusted {
		t.Fatal("an address outside trusted_proxies was treated as trusted")
	}
	if edge.Proto != ProtoHTTP {
		t.Errorf("Proto = %q, want %q: an untrusted X-Forwarded-Proto was believed", edge.Proto, ProtoHTTP)
	}
	if edge.Host != "mangala.zensys.lk" {
		t.Errorf("Host = %q, want the request's own host: an untrusted X-Forwarded-Host was believed", edge.Host)
	}
	if edge.ClientIP != "192.0.2.1" {
		t.Errorf("ClientIP = %q, want the peer address: an untrusted X-Forwarded-For was believed", edge.ClientIP)
	}
}

// TestResolveEdge_TrustedSourceIsBelieved is the §10.2 case that motivates the
// whole design: the Gateway is behind a TLS-terminating proxy, so r.TLS is nil
// and the only truthful source for the scheme is the trusted proxy's header.
func TestResolveEdge_TrustedSourceIsBelieved(t *testing.T) {
	tp := mustTrusted(t, "127.0.0.1/32")

	r := newEdgeRequest(trustedPeer, map[string]string{
		HeaderForwardedProto: "https",
		HeaderForwardedHost:  "mangala.zensys.lk",
		HeaderForwardedFor:   "203.0.113.9",
	})
	if r.TLS != nil {
		t.Fatal("the fixture is supposed to model a plain-HTTP connection behind a TLS-terminating proxy")
	}

	edge := ResolveEdge(r, tp)

	if !edge.Trusted {
		t.Fatal("a loopback peer was not treated as trusted")
	}
	if edge.Proto != ProtoHTTPS {
		t.Errorf("Proto = %q, want %q", edge.Proto, ProtoHTTPS)
	}
	if edge.Host != "mangala.zensys.lk" {
		t.Errorf("Host = %q, want %q", edge.Host, "mangala.zensys.lk")
	}
	if edge.ClientIP != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want %q", edge.ClientIP, "203.0.113.9")
	}
}

func TestResolveEdge_ProtoFallbacks(t *testing.T) {
	tp := mustTrusted(t, "127.0.0.1/32")

	tests := []struct {
		name        string
		remoteAddr  string
		forwarded   string
		tls         bool
		wantProto   string
		wantTrusted bool
	}{
		{"plain http, nothing forwarded", untrustedPeer, "", false, ProtoHTTP, false},
		{"direct tls", untrustedPeer, "", true, ProtoHTTPS, false},
		{"trusted proxy says https", trustedPeer, "https", false, ProtoHTTPS, true},
		{"trusted proxy says http", trustedPeer, "http", false, ProtoHTTP, true},
		{"trusted proxy says something else", trustedPeer, "gopher", false, ProtoHTTP, true},
		{"trusted proxy sends an empty value", trustedPeer, "", false, ProtoHTTP, true},
		{"trusted proxy sends mixed case", trustedPeer, "HTTPS", false, ProtoHTTPS, true},
		{"untrusted client cannot claim https", untrustedPeer, "https", false, ProtoHTTP, false},
		{"tls wins over nothing", trustedPeer, "", true, ProtoHTTPS, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newEdgeRequest(tc.remoteAddr, map[string]string{HeaderForwardedProto: tc.forwarded})
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}

			edge := ResolveEdge(r, tp)

			if edge.Proto != tc.wantProto {
				t.Errorf("Proto = %q, want %q", edge.Proto, tc.wantProto)
			}
			if edge.Trusted != tc.wantTrusted {
				t.Errorf("Trusted = %v, want %v", edge.Trusted, tc.wantTrusted)
			}
		})
	}
}

// TestResolveEdge_ForwardedHostIsValidated covers the shape rules on the one
// value the Gateway copies verbatim into an upstream header.
func TestResolveEdge_ForwardedHostIsValidated(t *testing.T) {
	tp := mustTrusted(t, "127.0.0.1/32")

	tests := []struct {
		name string
		sent string
		want string
	}{
		{"a plain host is accepted", "mangala.zensys.lk", "mangala.zensys.lk"},
		{"a host with a port is accepted, minus the port", "mangala.zensys.lk:8443", "mangala.zensys.lk"},
		{"surrounding whitespace is trimmed", "  mangala.zensys.lk  ", "mangala.zensys.lk"},
		{"an empty value falls back to the request host", "", "mangala.zensys.lk"},
		{"a comma would read as two hosts", "a.example,b.example", "mangala.zensys.lk"},
		{"a space would read as two hosts", "a.example b.example", "mangala.zensys.lk"},
		{"a scheme is not a host", "https://mangala.zensys.lk", "mangala.zensys.lk"},
		{"a path is not a host", "mangala.zensys.lk/admin", "mangala.zensys.lk"},
		{"userinfo is not a host", "user@mangala.zensys.lk", "mangala.zensys.lk"},
		{"a query is not a host", "mangala.zensys.lk?x=1", "mangala.zensys.lk"},
		{"a fragment is not a host", "mangala.zensys.lk#x", "mangala.zensys.lk"},
		{"an overlong value is refused", strings.Repeat("a", maxForwardedValueLength+1), "mangala.zensys.lk"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newEdgeRequest(trustedPeer, map[string]string{HeaderForwardedHost: tc.sent})

			if got := ResolveEdge(r, tp).Host; got != tc.want {
				t.Errorf("Host = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveEdge_ForwardedForWalksRightToLeft is the security-relevant half of
// client-IP resolution.
//
// The leftmost entry is whatever the client chose to send, because every hop
// appends rather than replaces. Reading it would let any client pick its own
// apparent address — and, once rate limiting by IP lands in M7, its own rate
// limit.
func TestResolveEdge_ForwardedForWalksRightToLeft(t *testing.T) {
	tp := mustTrusted(t, "127.0.0.1/32", "10.0.0.0/8")

	tests := []struct {
		name   string
		sent   []string
		want   string
		reason string
	}{
		{
			name:   "the rightmost untrusted entry is the client",
			sent:   []string{"203.0.113.9, 10.1.1.1"},
			want:   "203.0.113.9",
			reason: "the trusted hop's own address is skipped",
		},
		{
			name:   "a client-supplied leftmost entry is ignored",
			sent:   []string{"9.9.9.9, 203.0.113.9, 10.1.1.1"},
			want:   "203.0.113.9",
			reason: "the spoofed entry sits to the left of a value a trusted hop wrote",
		},
		{
			name:   "several trusted hops are all skipped",
			sent:   []string{"203.0.113.9, 10.1.1.1, 10.2.2.2, 127.0.0.1"},
			want:   "203.0.113.9",
			reason: "the chain is entirely trusted above the client",
		},
		{
			name:   "an entirely trusted chain falls back to the leftmost entry",
			sent:   []string{"10.1.1.1, 10.2.2.2"},
			want:   "10.1.1.1",
			reason: "the chain was built by trusted hops, so its origin is the leftmost value",
		},
		{
			name:   "hostnames are skipped rather than resolved",
			sent:   []string{"proxy.internal, 203.0.113.9"},
			want:   "203.0.113.9",
			reason: "a name is an unverifiable claim and resolving it would be a DNS lookup per request",
		},
		{
			name:   "a port on an entry is stripped",
			sent:   []string{"203.0.113.9:5678"},
			want:   "203.0.113.9",
			reason: "X-Forwarded-For entries may carry a port",
		},
		{
			name:   "a quoted entry is accepted",
			sent:   []string{`"203.0.113.9"`},
			want:   "203.0.113.9",
			reason: "RFC 7239 allows quoting",
		},
		{
			name:   "an ipv4-mapped address is canonicalised",
			sent:   []string{"::ffff:203.0.113.9"},
			want:   "203.0.113.9",
			reason: "the same client must not appear as two different clients",
		},
		{
			name:   "entries split across several header lines are joined",
			sent:   []string{"203.0.113.9", "10.1.1.1"},
			want:   "203.0.113.9",
			reason: "a proxy may emit X-Forwarded-For more than once",
		},
		{
			name:   "nothing parseable falls back to the peer",
			sent:   []string{"unknown, , ,"},
			want:   "127.0.0.1",
			reason: "an unparseable list is no evidence at all",
		},
		{
			name:   "an empty list falls back to the peer",
			sent:   nil,
			want:   "127.0.0.1",
			reason: "no header means no claim",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newEdgeRequest(trustedPeer, nil)
			for _, v := range tc.sent {
				r.Header.Add(HeaderForwardedFor, v)
			}

			got := ResolveEdge(r, tp).ClientIP
			if got != tc.want {
				t.Errorf("ClientIP = %q, want %q (%s)", got, tc.want, tc.reason)
			}
		})
	}
}

func TestSanitizeInbound(t *testing.T) {
	tests := []struct {
		name    string
		trusted bool
		want    map[string]string
	}{
		{
			name:    "an untrusted request loses every header the Gateway asserts",
			trusted: false,
			want: map[string]string{
				HeaderTenantID:       "",
				HeaderRequestID:      "",
				HeaderForwardedFor:   "",
				HeaderForwardedHost:  "",
				HeaderForwardedProto: "",
				"Authorization":      "Bearer token",
				"Cookie":             "session=abc",
			},
		},
		{
			name:    "a trusted request keeps the forwarded headers",
			trusted: true,
			want: map[string]string{
				HeaderTenantID:       "",
				HeaderRequestID:      "abc-123",
				HeaderForwardedFor:   "203.0.113.9",
				HeaderForwardedHost:  "mangala.zensys.lk",
				HeaderForwardedProto: "https",
				"Authorization":      "Bearer token",
				"Cookie":             "session=abc",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Built with Set rather than as a literal map, so the keys are the
			// canonical ones net/http would have produced. "X-Tenant-ID" is
			// not canonical — net/http stores it as "X-Tenant-Id" — and a
			// literal map would make the assertions below unable to fail.
			h := http.Header{}
			h.Set(HeaderTenantID, "spoofed")
			h.Set(HeaderRequestID, "abc-123")
			h.Set(HeaderForwardedFor, "203.0.113.9")
			h.Set(HeaderForwardedHost, "mangala.zensys.lk")
			h.Set(HeaderForwardedProto, "https")
			h.Set("Authorization", "Bearer token")
			h.Set("Cookie", "session=abc")

			SanitizeInbound(h, Edge{Trusted: tc.trusted}, HeaderRequestID)

			for name, want := range tc.want {
				if got := h.Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// TestSanitizeInbound_RemovesNonCanonicalKeyVariants guards the invariant
// against net/http's canonicalisation rather than relying on it.
//
// A hand-built header map — from a test, a custom transport, or any code that
// bypasses the standard parser — can hold "x-tenant-id" and "X-TENANT-ID" as
// separate keys. http.Header.Del would remove neither, and the spoofed value
// would survive to be read.
func TestSanitizeInbound_RemovesNonCanonicalKeyVariants(t *testing.T) {
	h := http.Header{
		"x-tenant-id":      {"spoofed"},
		"X-TENANT-ID":      {"spoofed"},
		"X-Tenant-Id":      {"spoofed"},
		"x-forwarded-for":  {"9.9.9.9"},
		"X-Forwarded-Host": {"evil.example.com"},
	}

	SanitizeInbound(h, Edge{Trusted: false}, HeaderRequestID)

	for key := range h {
		t.Errorf("header %q survived sanitising", key)
	}
}

// TestApplyForwarded_TrustedProxyPreservesTheEdgeScheme is the M4 exit
// criterion stated in the plan: X-Forwarded-Proto: https arriving from a
// trusted proxy is preserved downstream even though r.TLS is nil.
func TestApplyForwarded_TrustedProxyPreservesTheEdgeScheme(t *testing.T) {
	tp := mustTrusted(t, "127.0.0.1/32")

	r := newEdgeRequest(trustedPeer, map[string]string{
		HeaderForwardedProto: "https",
		HeaderForwardedHost:  "mangala.zensys.lk",
		HeaderForwardedFor:   "203.0.113.9",
	})
	edge := ResolveEdge(r, tp)

	// What the proxy's Rewrite will have in hand after SetXForwarded: the
	// inbound forwarded headers are already gone, and what is left describes
	// only the plain-HTTP hop from Nginx.
	out := http.Header{HeaderForwardedProto: {"http"}}
	ApplyForwarded(out, edge, "mangala", OutboundPolicy{InjectTenantHeader: true})

	if got := out.Get(HeaderForwardedProto); got != ProtoHTTPS {
		t.Fatalf("%s = %q, want %q: the product would build http:// URLs behind an https:// edge",
			HeaderForwardedProto, got, ProtoHTTPS)
	}
	if got := out.Get(HeaderForwardedHost); got != "mangala.zensys.lk" {
		t.Errorf("%s = %q, want %q", HeaderForwardedHost, got, "mangala.zensys.lk")
	}
	if got := out.Get(HeaderForwardedFor); got != "203.0.113.9" {
		t.Errorf("%s = %q, want %q", HeaderForwardedFor, got, "203.0.113.9")
	}
	if got := out.Get(HeaderTenantID); got != "mangala" {
		t.Errorf("%s = %q, want %q", HeaderTenantID, got, "mangala")
	}
}

// TestApplyForwarded_UntrustedSourceGetsTheGatewaysOwnAssertion is the other
// half: with no trusted proxy, the Gateway advertises only what it observed.
func TestApplyForwarded_UntrustedSourceGetsTheGatewaysOwnAssertion(t *testing.T) {
	tp := mustTrusted(t, "127.0.0.1/32")

	edge := ResolveEdge(newEdgeRequest(untrustedPeer, map[string]string{
		HeaderForwardedProto: "https",
		HeaderForwardedHost:  "evil.example.com",
		HeaderForwardedFor:   "9.9.9.9",
	}), tp)

	out := http.Header{}
	ApplyForwarded(out, edge, "mangala", OutboundPolicy{InjectTenantHeader: true})

	if got := out.Get(HeaderForwardedProto); got != ProtoHTTP {
		t.Errorf("%s = %q, want %q", HeaderForwardedProto, got, ProtoHTTP)
	}
	if got := out.Get(HeaderForwardedHost); got != "mangala.zensys.lk" {
		t.Errorf("%s = %q, want the request's own host", HeaderForwardedHost, got)
	}
	if got := out.Get(HeaderForwardedFor); got != "192.0.2.1" {
		t.Errorf("%s = %q, want the peer address", HeaderForwardedFor, got)
	}
}

// TestApplyForwarded_RemovesWhatItDoesNotVouchFor covers the delete half. The
// proxy builds the outbound header map from the inbound request, so anything
// left in it would be copied to the product.
func TestApplyForwarded_RemovesWhatItDoesNotVouchFor(t *testing.T) {
	edge := Edge{Proto: ProtoHTTP, Host: "mangala.zensys.lk", ClientIP: "203.0.113.9"}

	out := http.Header{
		HeaderTenantID:       {"spoofed"},
		"x-tenant-id":        {"spoofed"},
		HeaderForwardedFor:   {"9.9.9.9"},
		HeaderForwardedHost:  {"evil.example.com"},
		HeaderForwardedProto: {"https"},
	}

	ApplyForwarded(out, edge, "mangala", OutboundPolicy{InjectTenantHeader: true})

	// Exactly one key may match the tenant header, whatever its case: a second
	// one would be a spoofed value surviving alongside the real one, and a
	// case-insensitive consumer would pick whichever it happened to read.
	var tenantKeys []string
	for key := range out {
		if strings.EqualFold(key, HeaderTenantID) {
			tenantKeys = append(tenantKeys, key)
		}
	}
	if len(tenantKeys) != 1 {
		t.Errorf("found %d keys matching %s (%v), want exactly one", len(tenantKeys), HeaderTenantID, tenantKeys)
	}
	if got := out.Values(HeaderTenantID); len(got) != 1 || got[0] != "mangala" {
		t.Errorf("%s values = %q, want exactly [mangala]", HeaderTenantID, got)
	}
	if got := out.Get(HeaderForwardedFor); got != "203.0.113.9" {
		t.Errorf("%s = %q, want the resolved client", HeaderForwardedFor, got)
	}
	if got := out.Get(HeaderForwardedHost); got != "mangala.zensys.lk" {
		t.Errorf("%s = %q, want the resolved host", HeaderForwardedHost, got)
	}
}

func TestApplyForwarded_TenantHeaderPolicy(t *testing.T) {
	edge := Edge{Proto: ProtoHTTPS, Host: "mangala.zensys.lk", ClientIP: "203.0.113.9"}

	tests := []struct {
		name     string
		policy   OutboundPolicy
		wantKey  string
		wantVals []string
	}{
		{
			name:     "injected under the default name",
			policy:   OutboundPolicy{InjectTenantHeader: true},
			wantKey:  HeaderTenantID,
			wantVals: []string{"mangala"},
		},
		{
			name:     "injected under a configured name",
			policy:   OutboundPolicy{InjectTenantHeader: true, TenantHeaderName: "X-Zensys-Tenant"},
			wantKey:  "X-Zensys-Tenant",
			wantVals: []string{"mangala"},
		},
		{
			// Absent, not present-and-empty: a product that reads the header
			// would otherwise see a tenant of "" rather than no tenant.
			name:     "not injected when the product does not read it",
			policy:   OutboundPolicy{InjectTenantHeader: false},
			wantKey:  HeaderTenantID,
			wantVals: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := http.Header{}
			ApplyForwarded(out, edge, "mangala", tc.policy)

			got := out.Values(tc.wantKey)
			if len(got) != len(tc.wantVals) {
				t.Fatalf("%s values = %q, want %q", tc.wantKey, got, tc.wantVals)
			}
			for i := range got {
				if got[i] != tc.wantVals[i] {
					t.Errorf("%s values = %q, want %q", tc.wantKey, got, tc.wantVals)
				}
			}
		})
	}
}

// TestApplyForwarded_NoClientIPOmitsTheHeader pins the distinction between an
// absent header and one with an empty value: a product reading an empty
// X-Forwarded-For would log an empty client rather than fall back.
func TestApplyForwarded_NoClientIPOmitsTheHeader(t *testing.T) {
	out := http.Header{}
	ApplyForwarded(out, Edge{Proto: ProtoHTTP, Host: "mangala.zensys.lk"}, "mangala", OutboundPolicy{})

	if _, ok := out[HeaderForwardedFor]; ok {
		t.Errorf("%s is present with value %q; it should be omitted", HeaderForwardedFor, out.Get(HeaderForwardedFor))
	}
}
