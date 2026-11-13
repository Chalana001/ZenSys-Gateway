package tenant

import (
	"regexp"
	"slices"
	"testing"
)

var dnsLabelRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// TestReservedHosts_IsSorted keeps the list reviewable. An unsorted list makes
// it easy to add a duplicate without noticing during review.
func TestReservedHosts_IsSorted(t *testing.T) {
	if !slices.IsSorted(reservedHosts) {
		t.Fatal("the reserved host list must be sorted for stable diffs")
	}
}

func TestReservedHosts_HasNoDuplicates(t *testing.T) {
	seen := make(map[string]bool, len(reservedHosts))
	for _, h := range reservedHosts {
		if seen[h] {
			t.Errorf("%q appears more than once", h)
		}
		seen[h] = true
	}
}

// TestReservedHosts_AreValidLabels checks that every entry could actually
// collide with a tenant label. An entry containing a dot or an uppercase
// letter would never match a normalized tenant label and would be dead weight.
func TestReservedHosts_AreValidLabels(t *testing.T) {
	for _, h := range reservedHosts {
		if !dnsLabelRe.MatchString(h) {
			t.Errorf("%q is not a valid DNS label and can never match a tenant label", h)
		}
	}
}

// TestReservedHosts_CoversPlatformInfrastructure checks the entries that the
// architecture document names explicitly (§10). Losing one of these would let
// a customer register a hostname that shadows platform infrastructure.
func TestReservedHosts_CoversPlatformInfrastructure(t *testing.T) {
	required := []string{
		"www", "admin", "api", "gateway", "status", "app", "console",
		"support", "help", "docs", "mail", "email", "smtp", "imap",
		"ftp", "ssh", "static", "assets", "cdn", "media", "files",
		"download", "uploads", "auth", "login", "register", "account",
		"accounts", "billing", "payments", "dashboard", "portal", "dev",
		"test", "staging", "demo", "internal", "blog", "ns", "ns1", "ns2",
	}

	for _, h := range required {
		if !IsReserved(h) {
			t.Errorf("%q must be reserved but is not", h)
		}
	}
}

func TestIsReserved(t *testing.T) {
	tests := []struct {
		label string
		want  bool
	}{
		{"admin", true},
		{"api", true},
		{"www", true},
		{"mangala", false},
		{"garage1", false},
		{"abc-motors", false},
		{"", false},
		// Normalization is the caller's job. An uppercase label must not
		// match, so that a caller who forgets to lowercase gets a miss rather
		// than silently receiving a reserved-host decision.
		{"Admin", false},
		{"ADMIN", false},
		{"admin.zensys.lk", false},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			if got := IsReserved(tt.label); got != tt.want {
				t.Fatalf("IsReserved(%q) = %v, want %v", tt.label, got, tt.want)
			}
		})
	}
}

// TestReservedHosts_ReturnsACopy checks that a caller cannot mutate the
// package-level definition. The list is a security boundary; a caller with a
// slice reference could otherwise remove an entry.
func TestReservedHosts_ReturnsACopy(t *testing.T) {
	first := ReservedHosts()
	if len(first) == 0 {
		t.Fatal("ReservedHosts returned an empty list")
	}

	original := first[0]
	first[0] = "mutated-by-caller"

	second := ReservedHosts()
	if second[0] != original {
		t.Fatalf("mutating the returned slice changed the package-level list: %q became %q", original, second[0])
	}
	if IsReserved("mutated-by-caller") {
		t.Fatal("a caller was able to add a reserved host by mutating the returned slice")
	}
}
