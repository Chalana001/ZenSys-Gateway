package tenant

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateSlug_AcceptsValidSlugs(t *testing.T) {
	valid := []string{
		"abc",
		"mangala",
		"garage1",
		"abc-motors",
		"garage-01",
		"a1b",
		"123",
		"a-b-c",
		// Exactly the maximum: 63 characters, the DNS label limit.
		strings.Repeat("a", 63),
	}

	for _, slug := range valid {
		t.Run(slug, func(t *testing.T) {
			if err := ValidateSlug(slug); err != nil {
				t.Fatalf("ValidateSlug(%q) = %v, want nil", slug, err)
			}
		})
	}
}

func TestValidateSlug_RejectsInvalidSlugs(t *testing.T) {
	tests := []struct {
		name    string
		slug    string
		wantErr error
		wantMsg string
	}{
		{"empty", "", ErrInvalidSlug, "required"},
		{"two characters", "ab", ErrInvalidSlug, "minimum is 3"},
		{"one character", "a", ErrInvalidSlug, "minimum is 3"},
		{"uppercase", "ABC", ErrInvalidSlug, "lowercase"},
		{"mixed case", "Mangala", ErrInvalidSlug, "lowercase"},
		{"leading hyphen", "-abc", ErrInvalidSlug, "hyphen"},
		{"trailing hyphen", "abc-", ErrInvalidSlug, "hyphen"},
		{"underscore", "abc_def", ErrInvalidSlug, "only lowercase letters"},
		{"space", "my shop", ErrInvalidSlug, "only lowercase letters"},
		{"dot", "abc.def", ErrInvalidSlug, "only lowercase letters"},
		{"leading digit is fine but a symbol is not", "abc!", ErrInvalidSlug, "only lowercase letters"},
		{"slash", "abc/def", ErrInvalidSlug, "only lowercase letters"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSlug(tt.slug)
			if err == nil {
				t.Fatalf("ValidateSlug(%q) = nil, want an error", tt.slug)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateSlug(%q) error = %v, want it to wrap %v", tt.slug, err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tt.wantMsg)
			}
		})
	}
}

// TestValidateSlug_LengthBoundary is the regression test for the off-by-one
// between the architecture document's §9 regex (which permits 64 characters)
// and the DNS label limit (63).
//
// A 64-character slug would be reservable but could never resolve, because it
// is the leftmost label of {tenant}.zensys.lk. Rejecting it at the boundary is
// the whole point.
func TestValidateSlug_LengthBoundary(t *testing.T) {
	atLimit := strings.Repeat("a", SlugMaxLength)
	if err := ValidateSlug(atLimit); err != nil {
		t.Fatalf("a %d-character slug must be accepted, got: %v", SlugMaxLength, err)
	}

	overLimit := strings.Repeat("a", SlugMaxLength+1)
	err := ValidateSlug(overLimit)
	if err == nil {
		t.Fatalf("a %d-character slug must be rejected", SlugMaxLength+1)
	}
	if !errors.Is(err, ErrInvalidSlug) {
		t.Fatalf("expected ErrInvalidSlug, got: %v", err)
	}
	if !strings.Contains(err.Error(), "maximum is 63") {
		t.Errorf("the error should state the DNS-derived maximum, got: %q", err)
	}
}

// TestValidateSlug_RejectsReservedHosts covers §10: a reserved platform
// hostname can never become a tenant.
func TestValidateSlug_RejectsReservedHosts(t *testing.T) {
	for _, reserved := range ReservedHosts() {
		t.Run(reserved, func(t *testing.T) {
			err := ValidateSlug(reserved)
			if err == nil {
				t.Fatalf("ValidateSlug(%q) = nil; a reserved hostname must not be reservable", reserved)
			}

			// A few entries are shorter than the slug minimum — "ns" is two
			// characters — so they are rejected for length before the reserved
			// check is ever reached. That is fine: the property that matters
			// is that they cannot become tenants.
			//
			// But a reserved host that *is* a valid slug shape must be
			// rejected because it is reserved, not incidentally. Otherwise the
			// reserved list could be silently ineffective for that entry.
			if ValidateSlugFormat(reserved) == nil && !errors.Is(err, ErrReservedHost) {
				t.Fatalf("ValidateSlug(%q) = %v; a shape-valid reserved host must report ErrReservedHost", reserved, err)
			}
		})
	}
}

// TestValidateSlugFormat_AllowsReservedHosts documents the deliberate
// asymmetry between the two validators.
//
// Lookups use ValidateSlugFormat, so a reserved slug that somehow reached the
// registry can still be read, disabled, and released. Using the stricter
// validator there would make such a row impossible to clean up.
func TestValidateSlugFormat_AllowsReservedHosts(t *testing.T) {
	if err := ValidateSlug("admin"); !errors.Is(err, ErrReservedHost) {
		t.Fatalf("ValidateSlug(admin) should reject it as reserved, got: %v", err)
	}

	if err := ValidateSlugFormat("admin"); err != nil {
		t.Fatalf("ValidateSlugFormat(admin) = %v, want nil so the row remains reachable for cleanup", err)
	}
}

func TestNormalizeSlug(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"mangala", "mangala"},
		{"MANGALA", "mangala"},
		{"Mangala", "mangala"},
		{"  mangala  ", "mangala"},
		{"  MANGALA  ", "mangala"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := NormalizeSlug(tt.in); got != tt.want {
				t.Fatalf("NormalizeSlug(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSlugPatternMatchesValidator guards the exported pattern against drifting
// from the compiled validator. The pattern is handed to POS and GMS so they
// validate identically; if the two disagreed, a product would accept a slug
// the Gateway then rejects.
func TestSlugPatternMatchesValidator(t *testing.T) {
	cases := []string{
		"abc", "mangala", "abc-motors", "garage1",
		"ab", "-abc", "abc-", "ABC", "abc_def", "my shop",
		strings.Repeat("a", 63), strings.Repeat("a", 64),
	}

	for _, slug := range cases {
		t.Run(slug, func(t *testing.T) {
			byRegex := slugRe.MatchString(slug)
			byValidator := ValidateSlugFormat(slug) == nil

			// The regex checks shape only, so the two agree whenever the slug
			// is not reserved. No case in this list is reserved.
			if byRegex != byValidator {
				t.Fatalf("SlugPattern and ValidateSlugFormat disagree on %q: regex=%v validator=%v",
					slug, byRegex, byValidator)
			}
		})
	}
}

func TestCheckSlug(t *testing.T) {
	if err := CheckSlug("  mangala  "); err != nil {
		t.Fatalf("CheckSlug should trim before validating, got: %v", err)
	}
	if err := CheckSlug("admin"); !errors.Is(err, ErrReservedHost) {
		t.Fatalf("CheckSlug(admin) = %v, want ErrReservedHost", err)
	}
}

func TestPolicy(t *testing.T) {
	p := Policy()

	if p.Pattern != SlugPattern {
		t.Errorf("Policy().Pattern = %q, want %q", p.Pattern, SlugPattern)
	}
	if p.MinLength != SlugMinLength || p.MaxLength != SlugMaxLength {
		t.Errorf("Policy() lengths = %d..%d, want %d..%d", p.MinLength, p.MaxLength, SlugMinLength, SlugMaxLength)
	}
	if len(p.ReservedHosts) != len(ReservedHosts()) {
		t.Errorf("Policy() exposes %d reserved hosts, want %d", len(p.ReservedHosts), len(ReservedHosts()))
	}

	// The policy is handed to products; mutating it must not affect the
	// Gateway's own list.
	if len(p.ReservedHosts) > 0 {
		p.ReservedHosts[0] = "tampered"
		if IsReserved("tampered") {
			t.Fatal("Policy() exposed a mutable reference to the reserved host list")
		}
	}
}
