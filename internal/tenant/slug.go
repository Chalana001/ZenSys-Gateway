package tenant

import (
	"fmt"
	"regexp"
	"strings"
)

// Slug length bounds.
//
// SlugMaxLength is 63, not the 64 implied by the architecture document's §9
// regex (`^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`, which permits 1 + 62 + 1 = 64).
//
// A tenant slug IS a DNS label: it is the leftmost label of
// {tenant}.zensys.lk. DNS labels are capped at 63 octets by RFC 1035. A
// 64-character slug would therefore be reservable through the lifecycle API
// and yet could never resolve — a tenant that exists in the registry and is
// permanently unreachable, which is worse than an outright rejection because
// nothing would report it as broken.
//
// The database column and CHECK constraint use the same bound, so the rule is
// enforced identically in both places.
const (
	SlugMinLength = 3
	SlugMaxLength = 63
)

// SlugPattern is the authoritative platform slug format, as a string.
//
// It is exported so that the internal platform-policy endpoint can hand it to
// POS and GMS. Products validating slugs with their own copy of this rule is
// how the two drift apart (§9, §10).
const SlugPattern = `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`

var slugRe = regexp.MustCompile(SlugPattern)

// ValidateSlugFormat checks the slug's shape: length, character set, and
// hyphen placement. It does not check whether the slug is reserved.
//
// Lookups use this rather than ValidateSlug. A reserved slug that somehow
// reached the registry — inserted by hand during a migration, say — must still
// be readable, disableable, and releasable; rejecting it at lookup time would
// make it impossible to clean up.
func ValidateSlugFormat(slug string) error {
	if slug == "" {
		return fmt.Errorf("%w: a tenant slug is required", ErrInvalidSlug)
	}

	if len(slug) < SlugMinLength {
		return fmt.Errorf("%w: %q is %d characters; the minimum is %d",
			ErrInvalidSlug, slug, len(slug), SlugMinLength)
	}

	if len(slug) > SlugMaxLength {
		return fmt.Errorf("%w: %q is %d characters; the maximum is %d (a DNS label limit)",
			ErrInvalidSlug, slug, len(slug), SlugMaxLength)
	}

	// Checked before the regex so the message can name the actual problem
	// rather than restating the pattern.
	if slug != strings.ToLower(slug) {
		return fmt.Errorf("%w: %q must be lowercase", ErrInvalidSlug, slug)
	}

	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") {
		return fmt.Errorf("%w: %q must not start or end with a hyphen", ErrInvalidSlug, slug)
	}

	if !slugRe.MatchString(slug) {
		return fmt.Errorf("%w: %q may contain only lowercase letters, digits, and hyphens",
			ErrInvalidSlug, slug)
	}

	return nil
}

// ValidateSlug checks both the slug's shape and whether it is a reserved
// platform hostname. Use it when creating a route.
func ValidateSlug(slug string) error {
	if err := ValidateSlugFormat(slug); err != nil {
		return err
	}
	if IsReserved(slug) {
		return fmt.Errorf("%w: %q is reserved for platform use", ErrReservedHost, slug)
	}
	return nil
}

// NormalizeSlug lowercases and trims a candidate slug.
//
// This is safe for host-derived input because DNS hostnames are
// case-insensitive: MANGALA.zensys.lk and mangala.zensys.lk are the same host,
// so lowercasing is the correct interpretation rather than a lenient one.
//
// It is deliberately NOT applied to the lifecycle API's input. A product
// sending "Mangala" to Reserve should be corrected, not silently accommodated,
// because the slug it stores must match what it will later send for
// activation and what it will display to the tenant.
func NormalizeSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}
