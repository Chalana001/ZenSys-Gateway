package routing

import (
	"net"
	"net/http"
	"strings"
)

// Canonical header names from the architecture document's §10.1 header
// contract.
//
// They are constants rather than literals at each use site because the same
// names appear on both sides of the proxy — read from the client, written to
// the product — and a typo in one of them would silently disable a rule rather
// than fail.
const (
	HeaderTenantID       = "X-Tenant-ID"
	HeaderRequestID      = "X-Request-ID"
	HeaderForwardedFor   = "X-Forwarded-For"
	HeaderForwardedHost  = "X-Forwarded-Host"
	HeaderForwardedProto = "X-Forwarded-Proto"
)

// Scheme values for Edge.Proto.
const (
	ProtoHTTP  = "http"
	ProtoHTTPS = "https"
)

// maxForwardedValueLength bounds a forwarded header value the Gateway is
// willing to accept from a trusted proxy.
//
// A legitimate X-Forwarded-Host or X-Forwarded-For entry is far shorter than
// this. The bound exists so that a misbehaving or compromised trusted proxy
// cannot make the Gateway copy an arbitrarily large value into every upstream
// request and every log line.
const maxForwardedValueLength = 255

// ResolveEdge applies the trusted-proxy policy to one request.
//
// The policy is deliberately asymmetric. A forwarded header is believed only
// when the connection demonstrably came from an address named in
// trusted_proxies; otherwise the Gateway asserts what it can observe for
// itself and ignores the headers entirely.
//
// Note what is not read here: the forwarded host is never used to decide which
// tenant a request belongs to. Routing always parses the request's own Host
// header. A trusted proxy that sends an X-Forwarded-Host for a different
// tenant therefore changes only what the Gateway advertises downstream, never
// where the request goes.
func ResolveEdge(r *http.Request, tp *TrustedProxies) Edge {
	edge := Edge{
		Proto:    ProtoHTTP,
		Host:     r.Host,
		ClientIP: remoteIPString(r.RemoteAddr),
		Trusted:  tp.Trusts(r.RemoteAddr),
	}

	// r.TLS is nil whenever TLS was terminated in front of the Gateway, which
	// is the normal production topology — Cloudflare, then Nginx on loopback.
	// It is therefore only a fallback here: for a proxied request the
	// authoritative answer is the trusted proxy's X-Forwarded-Proto, read
	// below.
	if r.TLS != nil {
		edge.Proto = ProtoHTTPS
	}

	if !edge.Trusted {
		return edge
	}

	if proto := strings.ToLower(strings.TrimSpace(r.Header.Get(HeaderForwardedProto))); proto == ProtoHTTP || proto == ProtoHTTPS {
		edge.Proto = proto
	}

	if host, ok := forwardedHost(r.Header.Get(HeaderForwardedHost)); ok {
		edge.Host = host
	}

	if ip, ok := forwardedClient(r.Header.Values(HeaderForwardedFor), tp); ok {
		edge.ClientIP = ip
	}

	return edge
}

// forwardedHost validates an inbound X-Forwarded-Host.
//
// The value is only ever advertised downstream and never used for routing, so
// this check is about shape rather than authority: it must be a host, with an
// optional port, and nothing else. A scheme, a path, userinfo, or a comma is
// refused rather than repaired, because a comma would let the value be read as
// two hosts by a downstream consumer that splits on it.
func forwardedHost(raw string) (string, bool) {
	host := strings.TrimSpace(raw)
	if host == "" || len(host) > maxForwardedValueLength {
		return "", false
	}

	if strings.ContainsAny(host, " \t\r\n,;/\\?#@") {
		return "", false
	}

	// Strip a port, but leave a bracketed IPv6 literal intact: its colons are
	// part of the address, not a port separator.
	if i := strings.IndexByte(host, ':'); i >= 0 && !strings.HasPrefix(host, "[") {
		host = host[:i]
	}

	if host == "" || len(host) > maxHostLength {
		return "", false
	}

	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_', c == ':', c == '[', c == ']':
		default:
			return "", false
		}
	}

	return host, true
}

// forwardedClient resolves the client address from X-Forwarded-For.
//
// It walks the list right to left and returns the first entry that is not
// itself a trusted proxy. The direction is the whole point: each hop appends
// the address it received from, so the entries nearest the right were written
// by the Gateway's own trusted proxies, while the leftmost entry is whatever
// the client chose to send. Reading the leftmost value — the obvious
// implementation — would let any client set its own apparent address simply by
// sending "X-Forwarded-For: 1.2.3.4" and having the first proxy append to it.
//
// If every entry is a trusted address, the chain was built entirely by trusted
// hops and the leftmost entry is the origin the first of them recorded, so
// that is returned. If nothing parses, the caller keeps the connection
// address.
func forwardedClient(values []string, tp *TrustedProxies) (string, bool) {
	entries := make([]string, 0, len(values))
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				entries = append(entries, part)
			}
		}
	}
	if len(entries) == 0 {
		return "", false
	}

	for i := len(entries) - 1; i >= 0; i-- {
		ip := parseForwardedIP(entries[i])
		if ip == nil || tp.Contains(ip) {
			continue
		}
		return ip.String(), true
	}

	if ip := parseForwardedIP(entries[0]); ip != nil {
		return ip.String(), true
	}
	return "", false
}

// parseForwardedIP parses one X-Forwarded-For entry.
//
// The grammar is an IP address, optionally with a port, optionally quoted, and
// with IPv6 optionally bracketed. Hostnames are deliberately not resolved: a
// name in a forwarded list is a claim the Gateway has no way to verify, and
// resolving it would turn every request into a DNS lookup that an attacker
// controls the cost of.
func parseForwardedIP(entry string) net.IP {
	s := strings.Trim(strings.TrimSpace(entry), `"`)
	if s == "" {
		return nil
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	return net.ParseIP(s)
}

// SanitizeInbound removes every inbound header the Gateway must not carry
// forward unchanged. It is the delete half of the delete-then-set policy in
// §10.1.
//
// X-Tenant-ID is deleted unconditionally, including when the request came from
// a trusted proxy. §24's rule is absolute: the tenant is derived from the
// hostname and no other input may influence it. Deleting it here — before
// anything reads the request — means there is no code path, trusted or not,
// configured header or not, on which a client-supplied value can survive to be
// read.
//
// The forwarded headers and the request ID header are deleted only when the
// request did not come from a trusted proxy. ResolveEdge already ignored those
// values, so this is the second half of the same decision rather than a
// separate rule.
func SanitizeInbound(h http.Header, edge Edge, requestIDHeader string) {
	deleteHeader(h, HeaderTenantID)

	if edge.Trusted {
		return
	}

	deleteHeader(h, HeaderForwardedFor)
	deleteHeader(h, HeaderForwardedHost)
	deleteHeader(h, HeaderForwardedProto)

	if requestIDHeader != "" {
		deleteHeader(h, requestIDHeader)
	}
}

// OutboundPolicy is what the resolved product wants written into the upstream
// request.
type OutboundPolicy struct {
	// InjectTenantHeader mirrors products.<name>.inject_tenant_header. When
	// it is false the tenant header is absent rather than empty, so a product
	// that does not read it cannot be confused by seeing it.
	InjectTenantHeader bool

	// TenantHeaderName is the header to write. It defaults to X-Tenant-ID
	// when the name is empty.
	TenantHeaderName string
}

// ApplyForwarded writes the outbound half of the header contract.
//
// It must be called from the proxy's Rewrite, after
// ProxyRequest.SetXForwarded. That ordering is the §10.2 fix, and it is not
// cosmetic: httputil.ProxyRequest strips the inbound X-Forwarded-* headers
// before Rewrite runs, so SetXForwarded alone can only ever describe the
// connection the Gateway itself sees. Behind the real edge chain that
// connection is plain HTTP, so SetXForwarded would advertise
// "X-Forwarded-Proto: http" to every product while the browser was using
// HTTPS — working correctly in development and wrongly in production, which is
// the worst way for a bug to behave.
//
// Everything written here comes from edge, which ResolveEdge already reduced
// to values the Gateway can vouch for, or from the routing decision. Nothing
// is copied from the inbound request.
func ApplyForwarded(h http.Header, edge Edge, tenantSlug string, policy OutboundPolicy) {
	// Delete before setting, so a value the Gateway does not vouch for cannot
	// survive alongside the one it does — including any case variant left
	// behind by SetXForwarded or an earlier hop.
	deleteHeader(h, HeaderForwardedFor)
	deleteHeader(h, HeaderForwardedHost)
	deleteHeader(h, HeaderForwardedProto)
	deleteHeader(h, HeaderTenantID)

	if edge.ClientIP != "" {
		h.Set(HeaderForwardedFor, edge.ClientIP)
	}
	if edge.Host != "" {
		h.Set(HeaderForwardedHost, edge.Host)
	}
	h.Set(HeaderForwardedProto, edge.Proto)

	if !policy.InjectTenantHeader || tenantSlug == "" {
		return
	}

	name := policy.TenantHeaderName
	if name == "" {
		name = HeaderTenantID
	}
	h.Set(name, tenantSlug)
}

// deleteHeader removes name from h, matching case-insensitively.
//
// http.Header.Del canonicalises the key before looking it up, which is correct
// for anything the Gateway set itself but relies on the header having been
// canonicalised on the way in. §24's rule is that a client-supplied
// X-Tenant-ID can never influence routing, and that rule should not depend on
// a net/http implementation detail, so every case variant is removed
// explicitly. Header maps hold a handful of entries, so the scan costs
// nothing.
func deleteHeader(h http.Header, name string) {
	for k := range h {
		if strings.EqualFold(k, name) {
			delete(h, k)
		}
	}
}
