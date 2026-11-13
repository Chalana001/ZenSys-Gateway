package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/tenant"
)

var (
	// dnsLabelRe matches a single DNS label: 1-63 characters, lowercase
	// alphanumeric with interior hyphens only.
	dnsLabelRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

	// domainRe matches a fully qualified domain of at least two labels.
	domainRe = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

	// httpTokenRe matches a valid HTTP header field name (RFC 9110 token).
	httpTokenRe = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

	// productNameRe matches a product identifier key.
	productNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,15}$`)
)

// Validate checks the configuration for every problem it can detect, and
// returns all of them at once.
//
// Returning the complete list matters: fixing configuration one error per
// restart is a miserable loop, and a partially-correct config is the most
// likely state during initial deployment.
//
// Validate assumes resolveSecrets has already run. It checks the *structure*
// of the configuration, not whether secrets were successfully resolved.
func (c *Config) Validate() error {
	var errs []error

	errs = append(errs, c.validateServer()...)
	errs = append(errs, c.validatePlatform()...)
	errs = append(errs, c.validateRegistry()...)
	errs = append(errs, c.validateCache()...)
	errs = append(errs, c.validateProducts()...)
	errs = append(errs, c.validateRouting()...)
	errs = append(errs, c.validateProxy()...)
	errs = append(errs, c.validateInternalAuth()...)
	errs = append(errs, c.validateRateLimit()...)
	errs = append(errs, c.validateObservability()...)

	return errors.Join(errs...)
}

func (c *Config) validateServer() []error {
	var errs []error

	if c.Server.PublicAddr == "" {
		errs = append(errs, errors.New("server.public_addr is required"))
	}
	if c.Server.InternalAddr == "" {
		errs = append(errs, errors.New("server.internal_addr is required"))
	}

	_, _, pubErr := splitAddr(c.Server.PublicAddr)
	if c.Server.PublicAddr != "" && pubErr != nil {
		errs = append(errs, fmt.Errorf("server.public_addr %q: %w", c.Server.PublicAddr, pubErr))
	}

	internalHost, _, intErr := splitAddr(c.Server.InternalAddr)
	if c.Server.InternalAddr != "" && intErr != nil {
		errs = append(errs, fmt.Errorf("server.internal_addr %q: %w", c.Server.InternalAddr, intErr))
	}

	// The internal listener carries the tenant lifecycle API and is a security
	// boundary (architecture doc §40, §91). Binding it to every interface is
	// the exact misconfiguration that boundary exists to prevent, so it is
	// rejected rather than warned about.
	if intErr == nil && c.Server.InternalAddr != "" {
		if internalHost == "" {
			errs = append(errs, fmt.Errorf(
				"server.internal_addr %q has no host: an empty host binds every interface, which would expose the internal lifecycle API publicly; use a loopback or private address",
				c.Server.InternalAddr))
		} else if isWildcardHost(internalHost) {
			errs = append(errs, fmt.Errorf(
				"server.internal_addr %q binds every interface, which would expose the internal lifecycle API publicly; use a loopback or private address",
				c.Server.InternalAddr))
		}
	}

	if c.Server.PublicAddr != "" && c.Server.InternalAddr != "" && listenersConflict(c.Server.PublicAddr, c.Server.InternalAddr) {
		errs = append(errs, fmt.Errorf(
			"server.public_addr %q and server.internal_addr %q would occupy the same socket; the internal listener must be separate from the public one",
			c.Server.PublicAddr, c.Server.InternalAddr))
	}

	if c.Server.ReadHeaderTimeout <= 0 {
		errs = append(errs, errors.New("server.read_header_timeout must be greater than zero"))
	}
	if c.Server.IdleTimeout <= 0 {
		errs = append(errs, errors.New("server.idle_timeout must be greater than zero"))
	}
	if c.Server.ShutdownGrace <= 0 {
		errs = append(errs, errors.New("server.shutdown_grace must be greater than zero"))
	}
	if c.Server.MaxHeaderBytes <= 0 {
		errs = append(errs, errors.New("server.max_header_bytes must be greater than zero"))
	}
	if c.Server.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("server.max_body_bytes must be greater than zero"))
	}

	return errs
}

func (c *Config) validatePlatform() []error {
	var errs []error

	rd := c.Platform.RootDomain
	switch {
	case rd == "":
		errs = append(errs, errors.New("platform.root_domain is required"))
	default:
		if strings.Contains(rd, "://") {
			errs = append(errs, fmt.Errorf("platform.root_domain %q must be a bare domain, without a scheme", rd))
		}
		if strings.Contains(rd, ":") {
			errs = append(errs, fmt.Errorf("platform.root_domain %q must not include a port", rd))
		}
		if strings.HasSuffix(rd, ".") {
			errs = append(errs, fmt.Errorf("platform.root_domain %q must not have a trailing dot", rd))
		}
		if rd != strings.ToLower(rd) {
			errs = append(errs, fmt.Errorf("platform.root_domain %q must be lowercase", rd))
		}
		if !domainRe.MatchString(rd) {
			errs = append(errs, fmt.Errorf("platform.root_domain %q is not a valid domain of at least two labels", rd))
		}
	}

	switch c.Platform.ApexBehavior {
	case ApexBehaviorLanding, ApexBehaviorNotFound:
	case "":
		errs = append(errs, errors.New("platform.apex_behavior is required"))
	default:
		errs = append(errs, fmt.Errorf("platform.apex_behavior %q is not recognised; valid values are %q and %q",
			c.Platform.ApexBehavior, ApexBehaviorLanding, ApexBehaviorNotFound))
	}

	if c.Platform.ApexBehavior == ApexBehaviorLanding {
		if c.Platform.LandingDir == "" {
			errs = append(errs, errors.New("platform.landing_dir is required when apex_behavior is \"landing\""))
		} else {
			index := filepath.Join(c.Platform.LandingDir, "index.html")
			if _, err := os.Stat(index); err != nil {
				errs = append(errs, fmt.Errorf("platform.landing_dir %q does not contain a readable index.html", c.Platform.LandingDir))
			}
		}
	}

	apex := c.ApexLabel()
	seen := make(map[string]bool, len(c.Platform.ReservedHostsExtra))
	for i, h := range c.Platform.ReservedHostsExtra {
		switch {
		case h == "":
			errs = append(errs, fmt.Errorf("platform.reserved_hosts_extra[%d] is empty", i))
			continue
		case h != strings.ToLower(h):
			errs = append(errs, fmt.Errorf("platform.reserved_hosts_extra[%d] %q must be lowercase", i, h))
			continue
		case !dnsLabelRe.MatchString(h):
			errs = append(errs, fmt.Errorf("platform.reserved_hosts_extra[%d] %q is not a valid DNS label", i, h))
			continue
		}
		if seen[h] {
			errs = append(errs, fmt.Errorf("platform.reserved_hosts_extra[%d] %q is listed more than once", i, h))
			continue
		}
		seen[h] = true

		if h == apex {
			errs = append(errs, fmt.Errorf("platform.reserved_hosts_extra[%d] %q is the apex label and is already reserved", i, h))
		}
		if tenant.IsReserved(h) {
			errs = append(errs, fmt.Errorf("platform.reserved_hosts_extra[%d] %q is already in the authoritative reserved list", i, h))
		}
	}

	return errs
}

func (c *Config) validateRegistry() []error {
	var errs []error

	if c.Registry.Driver != "mysql" {
		errs = append(errs, fmt.Errorf("registry.driver %q is not supported; V1 supports \"mysql\"", c.Registry.Driver))
	}
	if c.Registry.DSNEnv == "" {
		errs = append(errs, errors.New("registry.dsn_env must name the environment variable holding the registry DSN"))
	}
	if c.Registry.MaxOpenConns <= 0 {
		errs = append(errs, errors.New("registry.max_open_conns must be greater than zero"))
	}
	if c.Registry.MaxIdleConns < 0 {
		errs = append(errs, errors.New("registry.max_idle_conns must not be negative"))
	}
	if c.Registry.MaxIdleConns > c.Registry.MaxOpenConns {
		errs = append(errs, fmt.Errorf("registry.max_idle_conns (%d) must not exceed registry.max_open_conns (%d)",
			c.Registry.MaxIdleConns, c.Registry.MaxOpenConns))
	}
	if c.Registry.ConnMaxLifetime < 0 {
		errs = append(errs, errors.New("registry.conn_max_lifetime must not be negative"))
	}
	if c.Registry.QueryTimeout <= 0 {
		errs = append(errs, errors.New("registry.query_timeout must be greater than zero"))
	}

	return errs
}

func (c *Config) validateCache() []error {
	var errs []error

	if c.Cache.RouteTTL <= 0 {
		errs = append(errs, errors.New("cache.route_ttl must be greater than zero"))
	}
	if c.Cache.NegativeTTL <= 0 {
		errs = append(errs, errors.New("cache.negative_ttl must be greater than zero"))
	}
	if c.Cache.MaxEntries <= 0 {
		errs = append(errs, errors.New("cache.max_entries must be greater than zero"))
	}

	return errs
}

func (c *Config) validateProducts() []error {
	var errs []error

	for name, p := range c.Products {
		if !productNameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("products key %q is not a valid product name (expected uppercase letters, digits, or underscores)", name))
		}
		errs = append(errs, validateUpstream(name, "frontend_upstream", p.FrontendUpstream)...)
		errs = append(errs, validateUpstream(name, "backend_upstream", p.BackendUpstream)...)

		if p.UpstreamTimeout <= 0 {
			errs = append(errs, fmt.Errorf("products.%s.upstream_timeout must be greater than zero", name))
		}
		if p.InjectTenantHeader {
			if p.TenantHeaderName == "" {
				errs = append(errs, fmt.Errorf("products.%s.tenant_header_name is required when inject_tenant_header is true", name))
			} else if !httpTokenRe.MatchString(p.TenantHeaderName) {
				errs = append(errs, fmt.Errorf("products.%s.tenant_header_name %q is not a valid HTTP header name", name, p.TenantHeaderName))
			}
		}
	}

	for _, required := range RequiredProducts {
		if _, ok := c.Products[required]; !ok {
			errs = append(errs, fmt.Errorf("products.%s is required but missing; the Gateway cannot route for a product it has no upstreams for", required))
		}
	}

	return errs
}

// validateUpstream checks one product upstream URL.
func validateUpstream(product, field, raw string) []error {
	if raw == "" {
		return []error{fmt.Errorf("products.%s.%s is required", product, field)}
	}

	// Check for a missing scheme before parsing. url.Parse rejects a bare
	// "host:port" with "first path segment in URL cannot contain colon", which
	// does not tell an operator what to fix. Omitting the scheme is the single
	// most likely way to get this wrong.
	if !strings.Contains(raw, "://") {
		return []error{fmt.Errorf("products.%s.%s %q must include a scheme, for example \"http://%s\"", product, field, raw, raw)}
	}

	u, err := url.Parse(raw)
	if err != nil {
		return []error{fmt.Errorf("products.%s.%s %q is not a valid URL: %w", product, field, raw, err)}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return []error{fmt.Errorf("products.%s.%s %q must use http or https", product, field, raw)}
	}
	if u.Host == "" {
		return []error{fmt.Errorf("products.%s.%s %q has no host", product, field, raw)}
	}

	// Credentials embedded in an upstream URL would be copied into proxy
	// errors and access logs. The Gateway has no product credentials by
	// design (architecture doc §61), so this is rejected outright.
	if u.User != nil {
		return []error{fmt.Errorf("products.%s.%s %q must not embed credentials", product, field, raw)}
	}

	return nil
}

func (c *Config) validateRouting() []error {
	var errs []error

	p := c.Routing.BackendPathPrefix
	switch {
	case p == "":
		errs = append(errs, errors.New("routing.backend_path_prefix is required"))
	case !strings.HasPrefix(p, "/"):
		errs = append(errs, fmt.Errorf("routing.backend_path_prefix %q must start with \"/\"", p))
	case p == "/":
		errs = append(errs, errors.New("routing.backend_path_prefix must not be \"/\": that would route every request to a backend and leave no frontend traffic"))
	case strings.HasSuffix(p, "/"):
		errs = append(errs, fmt.Errorf("routing.backend_path_prefix %q must not end with \"/\"", p))
	case strings.Contains(p, "//"):
		errs = append(errs, fmt.Errorf("routing.backend_path_prefix %q must not contain \"//\"", p))
	case strings.ContainsAny(p, " \t"):
		errs = append(errs, fmt.Errorf("routing.backend_path_prefix %q must not contain whitespace", p))
	}

	return errs
}

func (c *Config) validateProxy() []error {
	var errs []error

	if c.Proxy.RequestIDHeader == "" {
		errs = append(errs, errors.New("proxy.request_id_header is required"))
	} else if !httpTokenRe.MatchString(c.Proxy.RequestIDHeader) {
		errs = append(errs, fmt.Errorf("proxy.request_id_header %q is not a valid HTTP header name", c.Proxy.RequestIDHeader))
	}

	if c.Proxy.UpstreamResponseHeaderTimeout <= 0 {
		errs = append(errs, errors.New("proxy.upstream_response_header_timeout must be greater than zero"))
	}

	// The parsing rule lives in internal/routing, which is also what reads the
	// list at request time. Validating with a second implementation here would
	// let a value pass startup and then be rejected — or worse, trusted — by
	// the code that actually applies the policy.
	if _, err := routing.ParseTrustedProxies(c.Proxy.TrustedProxies); err != nil {
		errs = append(errs, fmt.Errorf("proxy.trusted_proxies: %w", err))
	}

	return errs
}

func (c *Config) validateInternalAuth() []error {
	var errs []error

	switch c.InternalAuth.Mode {
	case AuthModeToken:
	case AuthModeMTLS:
		// Refusing to start is deliberate. Silently falling back to
		// unauthenticated operation would leave the lifecycle API open while
		// the operator believes it is protected by certificates.
		errs = append(errs, errors.New("internal_auth.mode \"mtls\" is not implemented in V1; use \"token\""))
		return errs
	case "":
		errs = append(errs, errors.New("internal_auth.mode is required"))
		return errs
	default:
		errs = append(errs, fmt.Errorf("internal_auth.mode %q is not recognised; valid values are %q and %q",
			c.InternalAuth.Mode, AuthModeToken, AuthModeMTLS))
		return errs
	}

	if len(c.InternalAuth.Tokens) == 0 {
		errs = append(errs, errors.New("internal_auth.tokens must contain at least one service token; otherwise the lifecycle API is unauthenticated"))
	}

	seen := make(map[string]bool, len(c.InternalAuth.Tokens))
	for i, t := range c.InternalAuth.Tokens {
		label := t.Service
		if label == "" {
			label = fmt.Sprintf("#%d", i)
			errs = append(errs, fmt.Errorf("internal_auth.tokens[%d]: service is required", i))
		} else if seen[t.Service] {
			errs = append(errs, fmt.Errorf("internal_auth.tokens[%d]: service %q is configured more than once", i, t.Service))
		}
		seen[t.Service] = true

		if t.TokenEnv == "" {
			errs = append(errs, fmt.Errorf("internal_auth.tokens[%s]: token_env is required", label))
		}

		if len(t.MayManageProducts) == 0 {
			errs = append(errs, fmt.Errorf("internal_auth.tokens[%s]: may_manage_products must not be empty; a token that may manage nothing should not be configured", label))
		}
		for _, p := range t.MayManageProducts {
			if _, ok := c.Products[p]; !ok {
				errs = append(errs, fmt.Errorf("internal_auth.tokens[%s]: may_manage_products lists %q, which is not a configured product", label, p))
			}
		}
	}

	return errs
}

func (c *Config) validateRateLimit() []error {
	var errs []error

	if !c.RateLimit.Enabled {
		// A disabled limiter builds nothing from these values, so they are not
		// checked. Rejecting them would force a deployment that does not want
		// rate limiting to write plausible-looking numbers it does not use.
		return nil
	}

	// A non-finite rate is refused rather than passed through. x/time/rate
	// treats an infinite limit as "no limit", so a YAML `.inf` — which is
	// valid YAML and a natural way to write "unlimited" — would silently
	// disable the tier while `enabled: true` said otherwise. Disabling a tier
	// is what `enabled: false` is for, and it should be spelled that way.
	for _, tier := range []struct {
		name string
		lim  LimitConfig
	}{
		{"ip", c.RateLimit.IP},
		{"tenant", c.RateLimit.Tenant},
	} {
		switch {
		case math.IsInf(tier.lim.RPS, 0) || math.IsNaN(tier.lim.RPS):
			errs = append(errs, fmt.Errorf("rate_limit.%s.rps must be a finite number; an infinite rate disables the tier, which is what rate_limit.enabled: false is for", tier.name))
		case tier.lim.RPS <= 0:
			errs = append(errs, fmt.Errorf("rate_limit.%s.rps must be greater than zero when rate limiting is enabled", tier.name))
		}
		if tier.lim.Burst <= 0 {
			errs = append(errs, fmt.Errorf("rate_limit.%s.burst must be greater than zero when rate limiting is enabled", tier.name))
		}
	}

	// The bound is not optional. Zero is a value a caller reaches by leaving
	// the key out, and an unbounded key table is the defect this field exists
	// to prevent, so it is rejected rather than defaulted here — the default
	// lives in config.Default, where an omitted key is a deliberate choice
	// rather than a zero value that reached validation.
	if c.RateLimit.MaxEntries <= 0 {
		errs = append(errs, errors.New("rate_limit.max_entries must be greater than zero when rate limiting is enabled; it bounds the limiter's key table, whose keys are chosen by the caller"))
	}

	return errs
}

func (c *Config) validateObservability() []error {
	var errs []error

	switch c.Observability.LogLevel {
	case "debug", "info", "warn", "error":
	case "":
		errs = append(errs, errors.New("observability.log_level is required"))
	default:
		errs = append(errs, fmt.Errorf("observability.log_level %q is not recognised; valid values are \"debug\", \"info\", \"warn\", \"error\"",
			c.Observability.LogLevel))
	}

	switch c.Observability.LogFormat {
	case "json", "text":
	case "":
		errs = append(errs, errors.New("observability.log_format is required"))
	default:
		errs = append(errs, fmt.Errorf("observability.log_format %q is not recognised; valid values are \"json\" and \"text\"",
			c.Observability.LogFormat))
	}

	return errs
}

// splitAddr splits a host:port listener address, returning a clear error when
// the port is missing.
func splitAddr(addr string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		return "", "", fmt.Errorf("expected a host:port address: %w", err)
	}
	return host, port, nil
}

// isWildcardHost reports whether a listener host binds every interface.
func isWildcardHost(host string) bool {
	switch host {
	case "0.0.0.0", "::", "[::]":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsUnspecified()
	}
	return false
}

// listenersConflict reports whether two listener addresses would contend for
// the same socket.
//
// String comparison is not enough: ":8080", "0.0.0.0:8080", and
// "127.0.0.1:8080" are different strings, and the first two collide with
// everything on port 8080.
func listenersConflict(a, b string) bool {
	ah, ap, aerr := splitAddr(a)
	bh, bp, berr := splitAddr(b)
	if aerr != nil || berr != nil {
		return false // already reported by the caller
	}
	if ap != bp {
		return false
	}
	if ah == bh {
		return true
	}
	return isWildcardHost(ah) || isWildcardHost(bh) || ah == "" || bh == ""
}
