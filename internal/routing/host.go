// Package routing turns a request hostname into a routing decision.
//
// It is deliberately free of HTTP concerns. ParseHost takes a hostname string
// and returns a classification, so every rule in architecture doc §8 — and the
// hardening rules in the implementation plan §8.3 — can be table-tested
// without constructing a request.
package routing

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// DNS limits (RFC 1035 §2.3.4 and §3.1).
//
// maxHostLength is the limit for a name in textual form without a trailing
// dot. The 255-octet figure in the RFC counts the length bytes of the wire
// format, which do not appear in the presentation form.
const (
	maxHostLength  = 253
	maxLabelLength = 63
)

// Kind classifies a request hostname.
type Kind int

const (
	// KindApex is the root domain itself, such as "zensys.lk". It is a
	// platform hostname and never a tenant.
	KindApex Kind = iota

	// KindTenant is exactly one label below the root domain, such as
	// "mangala.zensys.lk".
	KindTenant
)

// String implements fmt.Stringer, so the value can go straight into a log
// field without a lookup table at the call site.
func (k Kind) String() string {
	switch k {
	case KindApex:
		return "apex"
	case KindTenant:
		return "tenant"
	default:
		return "unknown"
	}
}

// Host errors.
//
// Every one of these produces the same public response: a 404 in the standard
// error contract. They are distinct values so that logs and metrics can tell
// an operator *which* rule rejected a request, without that distinction being
// observable to the caller (architecture doc §45).
var (
	// ErrEmptyHost means the request carried no Host header at all.
	ErrEmptyHost = errors.New("routing: empty host")

	// ErrMalformedHost means the host could not be parsed as a DNS name: it
	// contained a disallowed character, an empty label, an overlong name or
	// label, or a malformed port.
	ErrMalformedHost = errors.New("routing: malformed host")

	// ErrIPLiteral means the host is an IP address rather than a name.
	ErrIPLiteral = errors.New("routing: host is an IP literal")

	// ErrIDNHost means the tenant label is punycode, such as
	// "xn--mangala-xyz". The platform slug policy is ASCII, so an IDN host
	// can never name a real tenant.
	ErrIDNHost = errors.New("routing: host is an internationalised domain name")

	// ErrWrongDomain means the host is not under the configured root domain.
	ErrWrongDomain = errors.New("routing: host is not under the root domain")

	// ErrMultiLabel means more than one label sits below the root domain,
	// such as "a.b.zensys.lk".
	ErrMultiLabel = errors.New("routing: multiple labels below the root domain")
)

// Host is the result of parsing and classifying a request hostname.
type Host struct {
	// Raw is the hostname exactly as received, before normalization.
	Raw string

	// Name is the normalized hostname: port stripped, lowercased, and with a
	// single trailing dot removed.
	Name string

	// Kind is the classification.
	Kind Kind

	// Slug is the tenant label, set only when Kind is KindTenant.
	//
	// It is the *hostname* label, not yet validated against the slug policy.
	// That is a separate step in the pipeline (plan §8.1 step 10) so that the
	// hostname rules and the slug policy each have exactly one home.
	Slug string
}

// ParseHost normalizes raw and classifies it against rootDomain.
//
// rootDomain must be a bare lowercase DNS name with no scheme, port, or
// trailing dot. config.Load guarantees this, and ParseHost deliberately does
// not re-normalize it: there should be exactly one place where the root-domain
// rule lives, and silently repairing a bad value here would hide a
// configuration bug rather than surface it.
//
// The sequence follows architecture doc §8 with the hardening rules from plan
// §8.3. Order matters. Normalization happens *before* the character
// allow-list, so that an uppercase host is corrected rather than rejected —
// DNS is case-insensitive and a client may legitimately send any case — while
// a host containing anything outside the allow-list is rejected outright.
func ParseHost(raw, rootDomain string) (Host, error) {
	host := Host{Raw: raw}

	// Go's header parser already strips surrounding whitespace, so this only
	// matters for direct callers. Trimming first keeps the empty check
	// meaningful rather than letting " " look like a plausible hostname.
	name := strings.TrimSpace(raw)
	if name == "" {
		return host, ErrEmptyHost
	}

	// IP literals are rejected before anything else, and checked twice: once
	// here and once after the port is stripped.
	//
	// Before, because a bracketed literal with no port ("[::1]") makes
	// net.SplitHostPort fail, so the port-stripping step would report it as a
	// malformed authority rather than as the address it plainly is. After,
	// because "[::1]:8080" only becomes recognisable once the brackets and
	// port are gone.
	//
	// Either way the reason matters: "127.0.0.1" is a syntactically plausible
	// hostname and would otherwise flow all the way to slug validation, where
	// it would be rejected for the wrong reason — and where a future
	// relaxation of the slug rules could let it through.
	if isIPLiteral(name) {
		return host, fmt.Errorf("%w: %q", ErrIPLiteral, name)
	}

	name, err := stripPort(name)
	if err != nil {
		return host, err
	}

	if isIPLiteral(name) {
		return host, fmt.Errorf("%w: %q", ErrIPLiteral, name)
	}

	// A single trailing dot is legal FQDN notation ("mangala.zensys.lk.").
	name = strings.TrimSuffix(name, ".")
	name = strings.ToLower(name)

	if name == "" {
		return host, ErrEmptyHost
	}

	if len(name) > maxHostLength {
		return host, fmt.Errorf("%w: %d characters exceeds the %d-character DNS limit",
			ErrMalformedHost, len(name), maxHostLength)
	}

	// One rule rejects several attacks at once: percent-encoding ("%2e" for
	// "."), underscores, any CR or LF that reached this far, spaces, and
	// every non-ASCII byte. Punycode is *not* covered by this rule — "xn--"
	// is entirely within the allow-list — so it is rejected separately below.
	if !isAllowedHostname(name) {
		return host, fmt.Errorf("%w: %q contains characters outside [a-z0-9.-]",
			ErrMalformedHost, name)
	}

	if err := checkLabels(name); err != nil {
		return host, err
	}

	host.Name = name

	switch {
	case name == rootDomain:
		host.Kind = KindApex
		return host, nil

	case strings.HasSuffix(name, "."+rootDomain):
		slug := strings.TrimSuffix(name, "."+rootDomain)

		// The empty-label check in checkLabels already rules out a leading
		// dot, so an empty slug here would mean the root domain was matched
		// against an empty prefix. Belt and braces.
		if slug == "" {
			return host, fmt.Errorf("%w: %q has no label below %q", ErrMultiLabel, name, rootDomain)
		}

		if strings.Contains(slug, ".") {
			return host, fmt.Errorf("%w: %q has more than one label below %q",
				ErrMultiLabel, name, rootDomain)
		}

		// The platform slug policy is ASCII-only (§9), so an IDN host can
		// never name a real tenant. Rejecting it explicitly is clearer than
		// letting it fall through to a registry miss, and it means a future
		// decision to support Unicode has to be made deliberately rather than
		// arriving by accident.
		if strings.HasPrefix(slug, "xn--") {
			return host, fmt.Errorf("%w: %q is punycode", ErrIDNHost, slug)
		}

		host.Kind = KindTenant
		host.Slug = slug
		return host, nil
	}

	return host, fmt.Errorf("%w: %q is not under %q", ErrWrongDomain, name, rootDomain)
}

// stripPort removes a ":port" suffix, leaving a bare hostname.
func stripPort(host string) (string, error) {
	// Only split when a colon is actually present. net.SplitHostPort rejects a
	// hostname with no port, so calling it unconditionally would mean treating
	// that error as the normal case and blurring it together with genuine
	// malformed-host errors.
	if !strings.Contains(host, ":") {
		return host, nil
	}

	// A bracketed IPv6 literal always carries brackets, so "[::1]:8080" and
	// "host:8080" both split correctly here.
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return "", fmt.Errorf("%w: %q could not be split into host and port",
			ErrMalformedHost, host)
	}

	// "mangala.zensys.lk:" — an empty port is not a valid authority.
	if port == "" {
		return "", fmt.Errorf("%w: %q has an empty port", ErrMalformedHost, host)
	}

	// net.SplitHostPort does not check that the port is numeric, so
	// "mangala.zensys.lk:abc" would otherwise be accepted. The port is
	// discarded either way, but accepting a syntactically invalid authority
	// would make this parser's strictness inconsistent: everything else here
	// is validated character by character.
	if !isAllDigits(port) {
		return "", fmt.Errorf("%w: %q has a non-numeric port", ErrMalformedHost, host)
	}

	return h, nil
}

// isAllDigits reports whether s is a non-empty run of ASCII digits.
func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// isIPLiteral reports whether host is an IP address rather than a DNS name.
func isIPLiteral(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}

	// A bracketed literal with no port, such as "[::1]". The bracketed form
	// with a port has already had its brackets removed by stripPort.
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return net.ParseIP(strings.Trim(host, "[]")) != nil
	}

	return false
}

// isAllowedHostname reports whether every byte of host is in [a-z0-9.-].
//
// The input is already lowercased, so an uppercase byte here is itself a
// violation rather than something to fold.
func isAllowedHostname(host string) bool {
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '-':
		default:
			return false
		}
	}
	return true
}

// checkLabels enforces the per-label DNS limits.
//
// Empty labels are rejected because they make a hostname ambiguous: with
// ".zensys.lk" the suffix match would succeed and yield an empty tenant slug,
// and with "mangala..zensys.lk" the prefix would be "mangala." — which looks
// like a tenant whose slug has a trailing dot. Both must fail as malformed
// hostnames rather than as registry misses.
//
// Leading and trailing hyphens are deliberately *not* checked here. They are
// slug-policy rules (§9), enforced by tenant.ValidateSlugFormat, and this
// parser's job is limited to characters and structure against the root domain.
// Duplicating the slug policy here would create a second place to change it.
func checkLabels(name string) error {
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return fmt.Errorf("%w: %q contains an empty label", ErrMalformedHost, name)
		}
		if len(label) > maxLabelLength {
			return fmt.Errorf("%w: label %q is %d characters, exceeding the %d-character DNS limit",
				ErrMalformedHost, label, len(label), maxLabelLength)
		}
	}
	return nil
}
