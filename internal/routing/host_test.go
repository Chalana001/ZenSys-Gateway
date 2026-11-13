package routing

import (
	"errors"
	"strings"
	"testing"
)

const rootDomain = "zensys.lk"

// TestParseHost is the §74 acceptance table plus the §8.3 hardening rules.
//
// Every rejection produces the same public 404, so these cases are about
// proving *which* rule fires — which is what an operator reads in the log.
func TestParseHost(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr error
		// wantKind and wantSlug are checked only when wantErr is nil.
		wantKind Kind
		wantName string
		wantSlug string
	}{
		// ---- the happy path -------------------------------------------------
		{
			name:     "tenant",
			raw:      "mangala.zensys.lk",
			wantKind: KindTenant,
			wantName: "mangala.zensys.lk",
			wantSlug: "mangala",
		},
		{
			name:     "uppercase is normalized, not rejected",
			raw:      "MANGALA.ZENSYS.LK",
			wantKind: KindTenant,
			wantName: "mangala.zensys.lk",
			wantSlug: "mangala",
		},
		{
			name:     "mixed case",
			raw:      "MaNgAlA.ZenSys.Lk",
			wantKind: KindTenant,
			wantName: "mangala.zensys.lk",
			wantSlug: "mangala",
		},
		{
			name:     "explicit port",
			raw:      "mangala.zensys.lk:8080",
			wantKind: KindTenant,
			wantName: "mangala.zensys.lk",
			wantSlug: "mangala",
		},
		{
			name:     "port 443",
			raw:      "mangala.zensys.lk:443",
			wantKind: KindTenant,
			wantName: "mangala.zensys.lk",
			wantSlug: "mangala",
		},
		{
			name:     "trailing dot is legal FQDN notation",
			raw:      "mangala.zensys.lk.",
			wantKind: KindTenant,
			wantName: "mangala.zensys.lk",
			wantSlug: "mangala",
		},
		{
			name:     "trailing dot and uppercase together",
			raw:      "MANGALA.ZENSYS.LK.",
			wantKind: KindTenant,
			wantName: "mangala.zensys.lk",
			wantSlug: "mangala",
		},
		{
			name:     "hyphenated slug",
			raw:      "abc-motors.zensys.lk",
			wantKind: KindTenant,
			wantName: "abc-motors.zensys.lk",
			wantSlug: "abc-motors",
		},
		{
			name:     "slug with digits",
			raw:      "garage-01.zensys.lk",
			wantKind: KindTenant,
			wantName: "garage-01.zensys.lk",
			wantSlug: "garage-01",
		},
		{
			name:     "leading whitespace is trimmed",
			raw:      "  mangala.zensys.lk",
			wantKind: KindTenant,
			wantName: "mangala.zensys.lk",
			wantSlug: "mangala",
		},
		{
			name:     "trailing whitespace is trimmed",
			raw:      "mangala.zensys.lk  ",
			wantKind: KindTenant,
			wantName: "mangala.zensys.lk",
			wantSlug: "mangala",
		},

		// ---- apex -----------------------------------------------------------
		{
			name:     "apex",
			raw:      "zensys.lk",
			wantKind: KindApex,
			wantName: "zensys.lk",
		},
		{
			name:     "apex uppercase",
			raw:      "ZENSYS.LK",
			wantKind: KindApex,
			wantName: "zensys.lk",
		},
		{
			name:     "apex with port",
			raw:      "zensys.lk:443",
			wantKind: KindApex,
			wantName: "zensys.lk",
		},
		{
			name:     "apex with trailing dot",
			raw:      "zensys.lk.",
			wantKind: KindApex,
			wantName: "zensys.lk",
		},

		// ---- reserved hosts are a pipeline concern, not a parser concern ----
		//
		// ParseHost must classify "admin" as an ordinary tenant label. The
		// reserved check happens later, after the multi-label check, because
		// §65 fixes that ordering. If this parser rejected it, the pipeline
		// could not distinguish "reserved" from "malformed" in its logs.
		{
			name:     "reserved host parses as a tenant label",
			raw:      "admin.zensys.lk",
			wantKind: KindTenant,
			wantName: "admin.zensys.lk",
			wantSlug: "admin",
		},
		{
			name:     "www parses as a tenant label",
			raw:      "www.zensys.lk",
			wantKind: KindTenant,
			wantName: "www.zensys.lk",
			wantSlug: "www",
		},

		// ---- slug-policy violations are also a later step -------------------
		//
		// "ab" is a perfectly legal DNS label and "-abc" is a legal character
		// sequence, so the parser accepts both. tenant.ValidateSlugFormat
		// rejects them at §8.1 step 10. Keeping the rule in one place is why
		// the parser does not duplicate it.
		{
			name:     "two-character label is a legal DNS label",
			raw:      "ab.zensys.lk",
			wantKind: KindTenant,
			wantName: "ab.zensys.lk",
			wantSlug: "ab",
		},
		{
			name:     "leading hyphen is left to the slug policy",
			raw:      "-abc.zensys.lk",
			wantKind: KindTenant,
			wantName: "-abc.zensys.lk",
			wantSlug: "-abc",
		},

		// ---- structural rejections ------------------------------------------
		{
			name:    "multi-label is rejected",
			raw:     "a.b.zensys.lk",
			wantErr: ErrMultiLabel,
		},
		{
			name:    "three labels below the root",
			raw:     "a.b.c.zensys.lk",
			wantErr: ErrMultiLabel,
		},
		{
			name:    "wrong domain",
			raw:     "mangala.example.com",
			wantErr: ErrWrongDomain,
		},
		{
			name:    "root domain as a suffix of a longer name",
			raw:     "mangala.notzensys.lk",
			wantErr: ErrWrongDomain,
		},
		{
			name:    "root domain is not matched as a bare substring",
			raw:     "evilzensys.lk",
			wantErr: ErrWrongDomain,
		},
		{
			name:    "root domain appended to another domain",
			raw:     "zensys.lk.evil.com",
			wantErr: ErrWrongDomain,
		},
		{
			name:    "root domain only, as a label",
			raw:     "zensys.lk.zensys.lk",
			wantErr: ErrMultiLabel,
		},
		{
			name:    "unrelated domain",
			raw:     "localhost",
			wantErr: ErrWrongDomain,
		},

		// ---- IP literals -----------------------------------------------------
		{
			name:    "IPv4 literal",
			raw:     "127.0.0.1",
			wantErr: ErrIPLiteral,
		},
		{
			name:    "IPv4 literal with port",
			raw:     "127.0.0.1:8080",
			wantErr: ErrIPLiteral,
		},
		{
			name:    "IPv4 literal that is not loopback",
			raw:     "10.0.0.1",
			wantErr: ErrIPLiteral,
		},
		{
			name:    "IPv6 literal, bracketed",
			raw:     "[::1]",
			wantErr: ErrIPLiteral,
		},
		{
			name:    "IPv6 literal with port",
			raw:     "[::1]:8080",
			wantErr: ErrIPLiteral,
		},
		{
			name:    "IPv6 literal, bare",
			raw:     "::1",
			wantErr: ErrIPLiteral,
		},
		{
			name:    "IPv4-mapped IPv6",
			raw:     "[::ffff:127.0.0.1]",
			wantErr: ErrIPLiteral,
		},

		// ---- character allow-list --------------------------------------------
		{
			name:    "CR in host",
			raw:     "mangala\r.zensys.lk",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "LF in host",
			raw:     "mangala\n.zensys.lk",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "CRLF injection attempt",
			raw:     "mangala.zensys.lk\r\nX-Evil: 1",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "percent-encoding",
			raw:     "mangala%2Ezensys.lk",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "underscore",
			raw:     "mangala_x.zensys.lk",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "space inside the host",
			raw:     "mangala shop.zensys.lk",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "at sign",
			raw:     "mangala@zensys.lk",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "slash",
			raw:     "mangala/zensys.lk",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "non-ASCII",
			raw:     "mangalä.zensys.lk",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "wildcard",
			raw:     "*.zensys.lk",
			wantErr: ErrMalformedHost,
		},

		// ---- IDN --------------------------------------------------------------
		{
			name:    "punycode label",
			raw:     "xn--mangala-xyz.zensys.lk",
			wantErr: ErrIDNHost,
		},
		{
			name:    "punycode label, uppercase",
			raw:     "XN--MANGALA-XYZ.zensys.lk",
			wantErr: ErrIDNHost,
		},

		// ---- ports -------------------------------------------------------------
		{
			name:    "empty port",
			raw:     "mangala.zensys.lk:",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "non-numeric port",
			raw:     "mangala.zensys.lk:http",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "two colons",
			raw:     "mangala.zensys.lk:8080:9090",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "unclosed bracket",
			raw:     "[::1:8080",
			wantErr: ErrMalformedHost,
		},

		// ---- empty labels -------------------------------------------------------
		{
			name:    "empty host",
			raw:     "",
			wantErr: ErrEmptyHost,
		},
		{
			name:    "whitespace-only host",
			raw:     "   ",
			wantErr: ErrEmptyHost,
		},
		{
			name:    "dot only",
			raw:     ".",
			wantErr: ErrEmptyHost,
		},
		{
			name:    "double dot",
			raw:     "mangala..zensys.lk",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "leading dot",
			raw:     ".zensys.lk",
			wantErr: ErrMalformedHost,
		},
		{
			name:    "trailing double dot",
			raw:     "mangala.zensys.lk..",
			wantErr: ErrMalformedHost,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, err := ParseHost(tc.raw, rootDomain)

			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("ParseHost(%q) = %+v, want error %v", tc.raw, host, tc.wantErr)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseHost(%q) error = %v, want %v", tc.raw, err, tc.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseHost(%q) returned unexpected error: %v", tc.raw, err)
			}
			if host.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", host.Kind, tc.wantKind)
			}
			if host.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", host.Name, tc.wantName)
			}
			if host.Slug != tc.wantSlug {
				t.Errorf("Slug = %q, want %q", host.Slug, tc.wantSlug)
			}
			if host.Raw != tc.raw {
				t.Errorf("Raw = %q, want %q (the original must be preserved for logging)", host.Raw, tc.raw)
			}
		})
	}
}

// TestParseHost_ApexHasNoSlug guards the invariant that makes the pipeline's
// branch on Kind safe: if the apex ever carried a slug, a future change to the
// branch order could route the platform's own domain to a tenant.
func TestParseHost_ApexHasNoSlug(t *testing.T) {
	host, err := ParseHost("zensys.lk", rootDomain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host.Slug != "" {
		t.Errorf("apex Slug = %q, want empty", host.Slug)
	}
}

// TestParseHost_RejectsOverlongHost covers the 253-character DNS limit.
func TestParseHost_RejectsOverlongHost(t *testing.T) {
	// Five 60-character labels (304 characters with separators) plus
	// ".zensys.lk" is well past the 253-character limit, while every
	// individual label stays under 63 so the label check cannot fire first.
	label := strings.Repeat("a", 60)
	raw := strings.Join([]string{label, label, label, label, label}, ".") + "." + rootDomain

	if len(raw) <= maxHostLength {
		t.Fatalf("test fixture is only %d characters; it must exceed %d", len(raw), maxHostLength)
	}

	if _, err := ParseHost(raw, rootDomain); !errors.Is(err, ErrMalformedHost) {
		t.Fatalf("error = %v, want ErrMalformedHost", err)
	}
}

// TestParseHost_LabelLengthBoundary pins the 63-character DNS label limit from
// both sides.
//
// This is the same boundary as tenant.SlugMaxLength, and the two must agree:
// a slug that the registry accepts but the host parser cannot parse would be a
// tenant that exists and is unreachable. The architecture document's §9 regex
// permits 64 characters, which is the bug this test exists to prevent from
// returning.
func TestParseHost_LabelLengthBoundary(t *testing.T) {
	atLimit := strings.Repeat("a", 63)
	overLimit := strings.Repeat("a", 64)

	host, err := ParseHost(atLimit+"."+rootDomain, rootDomain)
	if err != nil {
		t.Fatalf("a 63-character label must be accepted: %v", err)
	}
	if host.Slug != atLimit {
		t.Errorf("Slug length = %d, want 63", len(host.Slug))
	}

	if _, err := ParseHost(overLimit+"."+rootDomain, rootDomain); !errors.Is(err, ErrMalformedHost) {
		t.Errorf("a 64-character label must be rejected, got %v", err)
	}
}

// TestParseHost_DoesNotNormalizeTheRootDomain documents that the root domain is
// a precondition, not an input to be repaired.
//
// config.Load validates it. Silently lowercasing it here would mean a
// misconfigured root domain produced a Gateway that looked healthy while
// matching nothing.
func TestParseHost_DoesNotNormalizeTheRootDomain(t *testing.T) {
	if _, err := ParseHost("mangala.zensys.lk", "ZENSYS.LK"); !errors.Is(err, ErrWrongDomain) {
		t.Fatalf("error = %v, want ErrWrongDomain: the parser must not repair the root domain", err)
	}
}

// TestParseHost_RejectionReasonsAreDistinct guards against two rules collapsing
// into one error, which would erase the log distinction the pipeline relies on.
//
// The errors are compared by unwrapping to a sentinel rather than by value:
// ParseHost wraps each sentinel with context, so two rejections of the same
// kind are different error values and would trivially look distinct.
func TestParseHost_RejectionReasonsAreDistinct(t *testing.T) {
	sentinels := []error{
		ErrEmptyHost,
		ErrMalformedHost,
		ErrIPLiteral,
		ErrIDNHost,
		ErrWrongDomain,
		ErrMultiLabel,
	}

	inputs := map[string]string{
		"multi-label": "a.b.zensys.lk",
		"wrong":       "mangala.example.com",
		"ip":          "127.0.0.1",
		"punycode":    "xn--abc.zensys.lk",
		"malformed":   "mangala_x.zensys.lk",
		"empty":       "",
	}

	seen := make(map[error]string, len(inputs))
	for label, raw := range inputs {
		_, err := ParseHost(raw, rootDomain)
		if err == nil {
			t.Fatalf("%s: expected an error", label)
		}

		var matched error
		for _, s := range sentinels {
			if errors.Is(err, s) {
				matched = s
				break
			}
		}
		if matched == nil {
			t.Fatalf("%s: %v matched no sentinel", label, err)
		}

		if previous, ok := seen[matched]; ok {
			t.Errorf("%s and %s both report %v", label, previous, matched)
		}
		seen[matched] = label
	}

	if len(seen) != len(inputs) {
		t.Errorf("got %d distinct reasons for %d inputs", len(seen), len(inputs))
	}
}
