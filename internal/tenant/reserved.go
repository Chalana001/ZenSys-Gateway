// Package tenant holds the domain model for platform tenant routing.
//
// Nothing in this package may import internal/config: the domain model is
// deliberately independent of how the deployment is configured, so that the
// registry, cache, and resolver can be tested without a config file.
package tenant

// reservedHosts is the authoritative list of platform subdomains that must
// never become customer tenants (architecture doc §10).
//
// This list lives in Go rather than in configuration on purpose. §10 requires
// "one authoritative definition in the Gateway"; a config-file list could be
// accidentally emptied or truncated by a bad deploy, and the failure mode
// would be silent — a platform hostname becoming a routable tenant.
// Configuration may only *add* to this list, via platform.reserved_hosts_extra.
//
// The list is sorted for stable diffs.
var reservedHosts = []string{
	"account",
	"accounts",
	"admin",
	"api",
	"app",
	"assets",
	"auth",
	"billing",
	"blog",
	"cdn",
	"console",
	"dashboard",
	"demo",
	"dev",
	"docs",
	"download",
	"email",
	"files",
	"ftp",
	"gateway",
	"help",
	"imap",
	"internal",
	"login",
	"mail",
	"media",
	"ns",
	"ns1",
	"ns2",
	"payments",
	"portal",
	"register",
	"smtp",
	"ssh",
	"staging",
	"static",
	"status",
	"support",
	"test",
	"uploads",
	"www",
}

// reservedIndex is a lookup set built once from reservedHosts.
var reservedIndex = func() map[string]struct{} {
	m := make(map[string]struct{}, len(reservedHosts))
	for _, h := range reservedHosts {
		m[h] = struct{}{}
	}
	return m
}()

// IsReserved reports whether label is a reserved platform hostname.
//
// The caller is responsible for normalizing label to lowercase first; this
// function does not normalize, so that an un-normalized input cannot
// accidentally be treated as reserved (or worse, fail to be).
func IsReserved(label string) bool {
	_, ok := reservedIndex[label]
	return ok
}

// ReservedHosts returns a copy of the authoritative reserved hostname list.
//
// A copy is returned so that callers cannot mutate the package-level
// definition. This list is exposed to products through the internal
// platform-policy endpoint so they can validate a slug before requesting a
// reservation, rather than reimplementing the rules and drifting.
func ReservedHosts() []string {
	out := make([]string, len(reservedHosts))
	copy(out, reservedHosts)
	return out
}
