// Package config defines the Gateway configuration model together with its
// loading, defaulting, secret-resolution, and validation rules.
//
// Configuration is split into two layers:
//
//   - a YAML file describing the shape of the deployment, and
//   - environment variables carrying the three secrets (registry DSN, POS
//     service token, GMS service token).
//
// Secrets never appear in the YAML. The file references environment variable
// *names*; Load resolves them at startup and fails loudly when one is missing.
// This keeps credentials out of version control and out of the config file
// entirely (architecture doc §51, §61).
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// RequiredProducts are the products the Gateway must have upstream
// configuration for.
//
// This is the closed set defined by the architecture (POS and GMS). Additional
// products may be configured, but these two must always be present; a missing
// or misspelled key (for example a lowercase "pos") is a startup failure
// rather than a request-time surprise.
var RequiredProducts = []string{"POS", "GMS"}

// minServiceTokenLength is the shortest acceptable internal API service token.
// A short token is a weak token, and this is the only credential protecting
// tenant lifecycle mutations.
const minServiceTokenLength = 32

// Duration is a time.Duration that unmarshals from a Go duration string such
// as "30s" or "5m".
//
// yaml.v3 cannot decode into time.Duration directly: it would interpret the
// scalar as an integer nanosecond count, so "30s" would fail and "30" would
// silently mean 30 nanoseconds. A dedicated type makes the intended format
// explicit and produces a readable error when it is wrong.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("expected a duration string such as \"30s\"")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: expected a Go duration such as \"30s\" or \"5m\"", s)
	}
	if parsed < 0 {
		return fmt.Errorf("duration %q must not be negative", s)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String returns the Go duration string form.
func (d Duration) String() string { return time.Duration(d).String() }

// Config is the complete Gateway configuration.
type Config struct {
	Server        ServerConfig             `yaml:"server"`
	Platform      PlatformConfig           `yaml:"platform"`
	Registry      RegistryConfig           `yaml:"registry"`
	Cache         CacheConfig              `yaml:"cache"`
	Products      map[string]ProductConfig `yaml:"products"`
	Routing       RoutingConfig            `yaml:"routing"`
	Proxy         ProxyConfig              `yaml:"proxy"`
	InternalAuth  InternalAuthConfig       `yaml:"internal_auth"`
	RateLimit     RateLimitConfig          `yaml:"rate_limit"`
	Observability ObservabilityConfig      `yaml:"observability"`
}

// ServerConfig describes the two HTTP listeners and their limits.
//
// The two listeners are a security boundary, not a convenience: the internal
// listener carries the tenant lifecycle API and must never be reachable from
// the public one (architecture doc §40).
type ServerConfig struct {
	PublicAddr        string   `yaml:"public_addr"`
	InternalAddr      string   `yaml:"internal_addr"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
	ShutdownGrace     Duration `yaml:"shutdown_grace"`
	MaxHeaderBytes    int      `yaml:"max_header_bytes"`
	MaxBodyBytes      int64    `yaml:"max_body_bytes"`
}

// PlatformConfig describes the public hostname model.
type PlatformConfig struct {
	RootDomain   string `yaml:"root_domain"`
	ApexBehavior string `yaml:"apex_behavior"` // "landing" | "not_found"
	LandingDir   string `yaml:"landing_dir"`

	// ReservedHostsExtra augments the authoritative list compiled into
	// internal/tenant. It can only add, never remove (see reserved.go).
	ReservedHostsExtra []string `yaml:"reserved_hosts_extra"`
}

// RegistryConfig describes the platform tenant registry database.
type RegistryConfig struct {
	Driver          string   `yaml:"driver"`
	DSNEnv          string   `yaml:"dsn_env"`
	MaxOpenConns    int      `yaml:"max_open_conns"`
	MaxIdleConns    int      `yaml:"max_idle_conns"`
	ConnMaxLifetime Duration `yaml:"conn_max_lifetime"`
	QueryTimeout    Duration `yaml:"query_timeout"`

	// DSN is resolved from DSNEnv at load time and is never written to a
	// config file, logged, or included in an error message.
	DSN string `yaml:"-"`
}

// CacheConfig describes the route cache.
type CacheConfig struct {
	RouteTTL    Duration `yaml:"route_ttl"`
	NegativeTTL Duration `yaml:"negative_ttl"`
	MaxEntries  int      `yaml:"max_entries"`
}

// ProductConfig describes one product's upstreams and header expectations.
//
// Upstreams are product-level and never tenant-level (architecture doc §15).
type ProductConfig struct {
	FrontendUpstream string   `yaml:"frontend_upstream"`
	BackendUpstream  string   `yaml:"backend_upstream"`
	UpstreamTimeout  Duration `yaml:"upstream_timeout"`

	// InjectTenantHeader controls whether the canonical tenant is written
	// into a header for this product. POS uses X-Tenant-ID plus host
	// fallback; GMS resolves from the host. Products that do not read the
	// header should set this to false rather than receive a header they
	// ignore (architecture doc §24, §44).
	InjectTenantHeader bool   `yaml:"inject_tenant_header"`
	TenantHeaderName   string `yaml:"tenant_header_name"`
}

// RoutingConfig describes the frontend/backend split.
type RoutingConfig struct {
	BackendPathPrefix       string `yaml:"backend_path_prefix"`
	PathPrefixCaseSensitive bool   `yaml:"path_prefix_case_sensitive"`
}

// ProxyConfig describes reverse-proxy behaviour.
type ProxyConfig struct {
	PreserveHost                  bool     `yaml:"preserve_host"`
	RequestIDHeader               string   `yaml:"request_id_header"`
	TrustedProxies                []string `yaml:"trusted_proxies"`
	UpstreamResponseHeaderTimeout Duration `yaml:"upstream_response_header_timeout"`
}

// InternalAuthConfig describes how lifecycle API callers are authenticated.
type InternalAuthConfig struct {
	Mode   string        `yaml:"mode"` // "token" | "mtls"
	Tokens []TokenConfig `yaml:"tokens"`
}

// TokenConfig maps one service token to the products that service may manage.
type TokenConfig struct {
	Service           string   `yaml:"service"`
	TokenEnv          string   `yaml:"token_env"`
	MayManageProducts []string `yaml:"may_manage_products"`

	// Token is resolved from TokenEnv at load time.
	Token string `yaml:"-"`
}

// RateLimitConfig describes the two-tier limiter.
type RateLimitConfig struct {
	Enabled bool        `yaml:"enabled"`
	IP      LimitConfig `yaml:"ip"`
	Tenant  LimitConfig `yaml:"tenant"`

	// MaxEntries bounds the number of distinct keys each tier remembers.
	//
	// It exists because the limiter's keys are chosen by whoever is sending
	// requests, so an unbounded table is a memory exhaustion vector. It is
	// configurable rather than fixed because the right bound depends on how
	// many distinct clients the deployment expects within one refill period.
	MaxEntries int `yaml:"max_entries"`
}

// LimitConfig is a token-bucket rate limit.
type LimitConfig struct {
	RPS   float64 `yaml:"rps"`
	Burst int     `yaml:"burst"`
}

// ObservabilityConfig describes logging and metrics.
type ObservabilityConfig struct {
	LogLevel       string `yaml:"log_level"`
	LogFormat      string `yaml:"log_format"`
	MetricsEnabled bool   `yaml:"metrics_enabled"`

	// HealthOnPublic exposes /healthz and /readyz on the public listener as
	// well as the internal one. Defaults to false: the public listener is a
	// host-based catch-all, so serving /healthz there would shadow a path a
	// product may legitimately own on {tenant}.zensys.lk.
	HealthOnPublic bool `yaml:"health_on_public"`
}

// Default returns a Config populated with the values the Gateway falls back to
// when the configuration file omits a key.
//
// Load decodes the YAML *into* this struct, so an omitted key keeps its
// default rather than becoming a zero value. That is why Default exists
// instead of relying on struct literals at the call site.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			PublicAddr:        "0.0.0.0:8080",
			InternalAddr:      "127.0.0.1:8081",
			ReadHeaderTimeout: Duration(5 * time.Second),
			IdleTimeout:       Duration(60 * time.Second),
			ShutdownGrace:     Duration(15 * time.Second),
			MaxHeaderBytes:    64 << 10,
			MaxBodyBytes:      10 << 20,
		},
		Platform: PlatformConfig{
			RootDomain:   "zensys.lk",
			ApexBehavior: ApexBehaviorLanding,
			LandingDir:   "./configs/landing",
		},
		Registry: RegistryConfig{
			Driver:          "mysql",
			DSNEnv:          "ZENSYS_REGISTRY_DSN",
			MaxOpenConns:    20,
			MaxIdleConns:    10,
			ConnMaxLifetime: Duration(5 * time.Minute),
			QueryTimeout:    Duration(2 * time.Second),
		},
		Cache: CacheConfig{
			RouteTTL:    Duration(60 * time.Second),
			NegativeTTL: Duration(30 * time.Second),
			MaxEntries:  10000,
		},
		Products: map[string]ProductConfig{},
		Routing: RoutingConfig{
			BackendPathPrefix:       "/api",
			PathPrefixCaseSensitive: true,
		},
		Proxy: ProxyConfig{
			PreserveHost:                  true,
			RequestIDHeader:               "X-Request-ID",
			TrustedProxies:                []string{"127.0.0.1/32", "::1/128"},
			UpstreamResponseHeaderTimeout: Duration(30 * time.Second),
		},
		InternalAuth: InternalAuthConfig{
			Mode: "token",
		},
		RateLimit: RateLimitConfig{
			Enabled:    true,
			IP:         LimitConfig{RPS: 200, Burst: 400},
			Tenant:     LimitConfig{RPS: 50, Burst: 100},
			MaxEntries: 4096,
		},
		Observability: ObservabilityConfig{
			LogLevel:       "info",
			LogFormat:      "json",
			MetricsEnabled: true,
			HealthOnPublic: false,
		},
	}
}

// Apex behavior values.
const (
	ApexBehaviorLanding  = "landing"
	ApexBehaviorNotFound = "not_found"
)

// Internal auth modes.
const (
	AuthModeToken = "token"
	AuthModeMTLS  = "mtls"
)

// Product returns the configuration for the named product.
func (c *Config) Product(name string) (ProductConfig, bool) {
	p, ok := c.Products[name]
	return p, ok
}

// ApexLabel returns the first label of the root domain, which is the label
// that must never be treated as a tenant.
//
// For "zensys.lk" this is "zensys". The apex is matched by exact equality
// against RootDomain, never by label extraction, so this value is used only
// for validation and diagnostics.
func (c *Config) ApexLabel() string {
	for i := 0; i < len(c.Platform.RootDomain); i++ {
		if c.Platform.RootDomain[i] == '.' {
			return c.Platform.RootDomain[:i]
		}
	}
	return c.Platform.RootDomain
}
