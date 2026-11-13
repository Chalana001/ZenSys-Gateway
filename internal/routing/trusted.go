package routing

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// TrustedProxies is the set of addresses whose forwarded headers the Gateway
// believes.
//
// It is the entire basis of the trust decision in architecture doc §10.2: a
// forwarded header is either believed because the request demonstrably arrived
// through a proxy the operator named, or it is discarded. There is no middle
// ground, no heuristic, and no attempt to "clean up" an untrusted value into
// something usable — a client that can set X-Forwarded-Proto can set it to
// anything, so the only safe response is to ignore it.
//
// The zero value trusts nothing, which is the correct default: a deployment
// that puts the Gateway directly on the internet should trust no forwarded
// header at all.
type TrustedProxies struct {
	nets []*net.IPNet
}

// ParseTrustedProxies builds the set from configuration.
//
// Each entry is CIDR notation ("10.0.0.0/8", "127.0.0.1/32") or a bare IP
// address, which is treated as a single-host network. The bare form is
// accepted because writing "203.0.113.7/32" for one host is an easy thing to
// mistype and there is no ambiguity in omitting the mask.
//
// Every entry is validated and all failures are reported together, for the
// same reason config.Validate aggregates: fixing a list one error per restart
// is a miserable loop. An unparseable entry is an error rather than a warning
// because a typo here silently changes a security decision — "10.0.0.0/8"
// mistyped as "10.0.0.0/18" trusts a different set of addresses and nothing
// downstream would notice.
//
// An empty list is valid and yields a set that trusts nothing.
func ParseTrustedProxies(entries []string) (*TrustedProxies, error) {
	tp := &TrustedProxies{nets: make([]*net.IPNet, 0, len(entries))}

	var errs []error
	for i, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			errs = append(errs, fmt.Errorf("trusted proxy #%d is empty", i))
			continue
		}

		ipnet, err := parseProxyEntry(trimmed)
		if err != nil {
			errs = append(errs, fmt.Errorf("trusted proxy %q: %w", trimmed, err))
			continue
		}
		tp.nets = append(tp.nets, ipnet)
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return tp, nil
}

// parseProxyEntry parses one entry as a network.
func parseProxyEntry(entry string) (*net.IPNet, error) {
	if _, ipnet, err := net.ParseCIDR(entry); err == nil {
		return ipnet, nil
	}

	ip := net.ParseIP(entry)
	if ip == nil {
		return nil, errors.New(`expected CIDR notation such as "10.0.0.0/8" or a bare IP address`)
	}

	bits := 32
	if ip.To4() == nil {
		bits = 128
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

// Len returns the number of configured networks.
func (t *TrustedProxies) Len() int {
	if t == nil {
		return 0
	}
	return len(t.nets)
}

// Contains reports whether ip is inside the trusted set.
func (t *TrustedProxies) Contains(ip net.IP) bool {
	if t == nil || ip == nil {
		return false
	}
	for _, n := range t.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Trusts reports whether a RemoteAddr value belongs to a trusted proxy.
//
// An address that cannot be parsed is not trusted. That direction is the only
// safe one: an unparseable address is not evidence of a trusted peer.
func (t *TrustedProxies) Trusts(remoteAddr string) bool {
	return t.Contains(remoteIP(remoteAddr))
}

// remoteIPString is remoteIP in its canonical textual form, or "" when the
// address cannot be parsed.
//
// The canonical form matters: an IPv4 address that arrived as an IPv4-in-IPv6
// address ("::ffff:127.0.0.1") is reported as "127.0.0.1", so the same client
// does not appear as two different clients depending on how the listener was
// bound.
func remoteIPString(remoteAddr string) string {
	ip := remoteIP(remoteAddr)
	if ip == nil {
		return ""
	}
	return ip.String()
}

// ClientAddr returns the canonical textual form of the address a request
// arrived on, or "" when it cannot be parsed.
//
// It is the address the Gateway can always vouch for, because it comes from
// the connection rather than from a header. ResolveEdge uses it as the client
// address whenever no trusted proxy asserted one, and the rate limiter uses it
// as a fallback when no Edge was resolved at all — which happens only in a
// chain that omitted the trusted-proxy middleware.
//
// The canonical form is what makes it usable as a key: an IPv4 address that
// arrived as IPv4-in-IPv6 ("::ffff:203.0.113.7") is reported as
// "203.0.113.7", so one client cannot present as two keys depending on how the
// listener was bound, and so it cannot be handed two rate-limit allowances.
func ClientAddr(remoteAddr string) string {
	return remoteIPString(remoteAddr)
}

// remoteIP extracts the address from a net/http RemoteAddr value.
//
// RemoteAddr is always host:port for a TCP connection, but it is a plain
// string that a test — or a future listener on a unix socket — can set to a
// bare address, so both forms are accepted rather than the port being assumed.
func remoteIP(remoteAddr string) net.IP {
	if remoteAddr == "" {
		return nil
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		remoteAddr = host
	}
	// A bracketed literal with no port, such as "[::1]".
	remoteAddr = strings.TrimSuffix(strings.TrimPrefix(remoteAddr, "["), "]")
	return net.ParseIP(remoteAddr)
}
