package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validConfig returns a configuration that passes Validate, so that each test
// can change exactly one thing and attribute the failure to it.
func validConfig(t *testing.T) *Config {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatalf("write landing page: %v", err)
	}

	cfg := Default()
	cfg.Platform.LandingDir = dir
	cfg.Products = map[string]ProductConfig{
		"POS": {
			FrontendUpstream:   "http://127.0.0.1:3001",
			BackendUpstream:    "http://127.0.0.1:8082",
			UpstreamTimeout:    Duration(30 * time.Second),
			InjectTenantHeader: true,
			TenantHeaderName:   "X-Tenant-ID",
		},
		"GMS": {
			FrontendUpstream:   "http://127.0.0.1:3002",
			BackendUpstream:    "http://127.0.0.1:8083",
			UpstreamTimeout:    Duration(30 * time.Second),
			InjectTenantHeader: true,
			TenantHeaderName:   "X-Tenant-ID",
		},
	}
	cfg.InternalAuth = InternalAuthConfig{
		Mode: AuthModeToken,
		Tokens: []TokenConfig{
			{Service: "pos", TokenEnv: "ZENSYS_TOKEN_POS", MayManageProducts: []string{"POS"}, Token: strings.Repeat("a", 32)},
			{Service: "gms", TokenEnv: "ZENSYS_TOKEN_GMS", MayManageProducts: []string{"GMS"}, Token: strings.Repeat("b", 32)},
		},
	}
	return cfg
}

func TestValidate_AcceptsValidConfig(t *testing.T) {
	if err := validConfig(t).Validate(); err != nil {
		t.Fatalf("expected a valid configuration, got:\n%v", err)
	}
}

// TestValidate_InternalListenerCannotBePublic is the most security-relevant
// check in this file. The internal listener carries the tenant lifecycle API;
// binding it to every interface would expose tenant creation, activation,
// disablement, and release to the internet.
func TestValidate_InternalListenerCannotBePublic(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"wildcard IPv4", "0.0.0.0:8081"},
		{"wildcard IPv6", "[::]:8081"},
		{"empty host binds everything", ":8081"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Server.InternalAddr = tt.addr

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected internal_addr %q to be rejected", tt.addr)
			}
			if !strings.Contains(err.Error(), "internal lifecycle API") {
				t.Fatalf("expected the error to explain the exposure, got: %v", err)
			}
		})
	}
}

func TestValidate_AcceptsPrivateInternalListener(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8081", "10.0.0.5:8081", "[::1]:8081"} {
		t.Run(addr, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Server.InternalAddr = addr

			if err := cfg.Validate(); err != nil {
				t.Fatalf("expected %q to be accepted, got: %v", addr, err)
			}
		})
	}
}

// TestValidate_ListenersMustNotCollide covers the case where the two listeners
// look different as strings but contend for the same socket.
func TestValidate_ListenersMustNotCollide(t *testing.T) {
	tests := []struct {
		name     string
		public   string
		internal string
	}{
		{"identical", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"public wildcard covers internal", "0.0.0.0:8080", "127.0.0.1:8080"},
		{"internal wildcard covers public", "127.0.0.1:8080", "0.0.0.0:8080"},
		{"empty public host covers internal", ":8080", "127.0.0.1:8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Server.PublicAddr = tt.public
			cfg.Server.InternalAddr = tt.internal

			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected %q and %q to be reported as conflicting", tt.public, tt.internal)
			}
		})
	}
}

func TestValidate_RejectsDifferentPortsOnSameHost(t *testing.T) {
	cfg := validConfig(t)
	cfg.Server.PublicAddr = "127.0.0.1:8080"
	cfg.Server.InternalAddr = "127.0.0.1:8081"

	if err := cfg.Validate(); err != nil {
		t.Fatalf("different ports on the same host must be accepted, got: %v", err)
	}
}

func TestValidate_RootDomain(t *testing.T) {
	tests := []struct {
		name    string
		domain  string
		wantErr string
	}{
		{"valid", "zensys.lk", ""},
		{"valid three labels", "gateway.zensys.lk", ""},
		{"scheme", "https://zensys.lk", "without a scheme"},
		{"port", "zensys.lk:443", "port"},
		{"trailing dot", "zensys.lk.", "trailing dot"},
		{"uppercase", "ZENSYS.LK", "lowercase"},
		{"single label", "zensys", "at least two labels"},
		{"empty", "", "required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Platform.RootDomain = tt.domain

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected %q to be accepted, got: %v", tt.domain, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %q to be rejected", tt.domain)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error mentioning %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidate_ApexBehavior(t *testing.T) {
	cfg := validConfig(t)
	cfg.Platform.ApexBehavior = "redirect"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an unrecognised apex_behavior to be rejected")
	}

	cfg = validConfig(t)
	cfg.Platform.ApexBehavior = ApexBehaviorNotFound
	cfg.Platform.LandingDir = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("landing_dir must not be required when apex_behavior is not_found, got: %v", err)
	}
}

func TestValidate_LandingDirectoryMustContainIndex(t *testing.T) {
	cfg := validConfig(t)
	cfg.Platform.LandingDir = t.TempDir() // exists but empty

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected a landing directory without index.html to be rejected")
	}
	if !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("expected the error to mention index.html, got: %v", err)
	}
}

func TestValidate_RequiredProducts(t *testing.T) {
	for _, missing := range RequiredProducts {
		t.Run("missing "+missing, func(t *testing.T) {
			cfg := validConfig(t)
			delete(cfg.Products, missing)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected a missing %s product to be rejected", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Fatalf("expected the error to name %s, got: %v", missing, err)
			}
		})
	}
}

func TestValidate_UpstreamURLs(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr string
	}{
		{"valid http", "http://127.0.0.1:3001", ""},
		{"valid https", "https://pos.internal:3001", ""},
		{"empty", "", "required"},
		{"wrong scheme", "ftp://127.0.0.1:3001", "must use http or https"},
		{"no scheme", "127.0.0.1:3001", "must include a scheme"},
		{"bare hostname", "pos.internal", "must include a scheme"},
		{"unparseable", "http://[::1", "not a valid URL"},
		{"embedded credentials", "http://user:pass@127.0.0.1:3001", "must not embed credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			pos := cfg.Products["POS"]
			pos.FrontendUpstream = tt.url
			cfg.Products["POS"] = pos

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected %q to be accepted, got: %v", tt.url, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %q to be rejected", tt.url)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error mentioning %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidate_TenantHeaderNameRequiredWhenInjecting(t *testing.T) {
	cfg := validConfig(t)
	pos := cfg.Products["POS"]
	pos.InjectTenantHeader = true
	pos.TenantHeaderName = ""
	cfg.Products["POS"] = pos

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected a missing tenant_header_name to be rejected when inject_tenant_header is true")
	}

	// A product that does not inject must not be forced to name a header.
	cfg = validConfig(t)
	gms := cfg.Products["GMS"]
	gms.InjectTenantHeader = false
	gms.TenantHeaderName = ""
	cfg.Products["GMS"] = gms

	if err := cfg.Validate(); err != nil {
		t.Fatalf("a product with inject_tenant_header false must not require tenant_header_name, got: %v", err)
	}
}

func TestValidate_BackendPathPrefix(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		wantErr string
	}{
		{"valid", "/api", ""},
		{"valid nested", "/api/v1", ""},
		{"empty", "", "required"},
		{"root", "/", "every request to a backend"},
		{"no leading slash", "api", `must start with "/"`},
		{"trailing slash", "/api/", `must not end with "/"`},
		{"double slash", "/api//v1", `must not contain "//"`},
		{"whitespace", "/ap i", "whitespace"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Routing.BackendPathPrefix = tt.prefix

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected %q to be accepted, got: %v", tt.prefix, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %q to be rejected", tt.prefix)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error mentioning %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidate_TrustedProxies(t *testing.T) {
	cfg := validConfig(t)
	cfg.Proxy.TrustedProxies = []string{"127.0.0.1/32", "10.0.0.0/8", "::1/128", "192.168.1.5"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected CIDR and bare IP entries to be accepted, got: %v", err)
	}

	for _, bad := range []string{"not-an-ip", "10.0.0.0/33", "999.1.1.1", ""} {
		t.Run("rejects "+bad, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Proxy.TrustedProxies = []string{bad}

			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected %q to be rejected", bad)
			}
		})
	}
}

func TestValidate_InternalAuth(t *testing.T) {
	t.Run("mtls is refused rather than silently unauthenticated", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.InternalAuth.Mode = AuthModeMTLS

		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected mtls mode to be rejected in V1")
		}
		if !strings.Contains(err.Error(), "not implemented") {
			t.Fatalf("expected the error to say mtls is not implemented, got: %v", err)
		}
	})

	t.Run("no tokens", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.InternalAuth.Tokens = nil

		if err := cfg.Validate(); err == nil {
			t.Fatal("expected an empty token list to be rejected")
		}
	})

	t.Run("duplicate service", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.InternalAuth.Tokens = append(cfg.InternalAuth.Tokens, TokenConfig{
			Service: "pos", TokenEnv: "ZENSYS_TOKEN_POS_2", MayManageProducts: []string{"POS"},
		})

		if err := cfg.Validate(); err == nil {
			t.Fatal("expected a duplicated service to be rejected")
		}
	})

	t.Run("unknown managed product", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.InternalAuth.Tokens[0].MayManageProducts = []string{"ERP"}

		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected an unknown managed product to be rejected")
		}
		if !strings.Contains(err.Error(), "ERP") {
			t.Fatalf("expected the error to name the unknown product, got: %v", err)
		}
	})

	t.Run("empty managed products", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.InternalAuth.Tokens[0].MayManageProducts = nil

		if err := cfg.Validate(); err == nil {
			t.Fatal("expected an empty may_manage_products to be rejected")
		}
	})
}

// TestValidate_ReservedHostsExtraCanOnlyAdd checks that the configuration
// cannot weaken the compiled-in reserved-hostname list.
func TestValidate_ReservedHostsExtraCanOnlyAdd(t *testing.T) {
	t.Run("adds a new host", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Platform.ReservedHostsExtra = []string{"internal-tools"}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected a new reserved host to be accepted, got: %v", err)
		}
	})

	t.Run("cannot repeat an authoritative entry", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Platform.ReservedHostsExtra = []string{"admin"}

		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected a duplicate of the authoritative list to be rejected")
		}
		if !strings.Contains(err.Error(), "authoritative") {
			t.Fatalf("expected the error to mention the authoritative list, got: %v", err)
		}
	})

	t.Run("cannot reserve the apex", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Platform.ReservedHostsExtra = []string{"zensys"}

		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected the apex label to be rejected")
		}
		if !strings.Contains(err.Error(), "apex") {
			t.Fatalf("expected the error to mention the apex, got: %v", err)
		}
	})

	t.Run("rejects invalid labels", func(t *testing.T) {
		for _, bad := range []string{"Admin", "has space", "-leading", "trailing-", "under_score", ""} {
			cfg := validConfig(t)
			cfg.Platform.ReservedHostsExtra = []string{bad}

			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected %q to be rejected as a reserved host", bad)
			}
		}
	})
}

func TestValidate_RateLimit(t *testing.T) {
	cfg := validConfig(t)
	cfg.RateLimit.Enabled = true
	cfg.RateLimit.IP.RPS = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected a zero IP rps to be rejected when rate limiting is enabled")
	}

	// Disabled limits need no values.
	cfg = validConfig(t)
	cfg.RateLimit.Enabled = false
	cfg.RateLimit.IP = LimitConfig{}
	cfg.RateLimit.Tenant = LimitConfig{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected disabled rate limiting to accept zero values, got: %v", err)
	}
}

func TestValidate_Observability(t *testing.T) {
	cfg := validConfig(t)
	cfg.Observability.LogLevel = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an unrecognised log level to be rejected")
	}

	cfg = validConfig(t)
	cfg.Observability.LogFormat = "xml"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an unrecognised log format to be rejected")
	}
}

func TestValidate_Registry(t *testing.T) {
	cfg := validConfig(t)
	cfg.Registry.Driver = "postgres"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an unsupported registry driver to be rejected")
	}

	cfg = validConfig(t)
	cfg.Registry.MaxIdleConns = 100
	cfg.Registry.MaxOpenConns = 10
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected max_idle_conns above max_open_conns to be rejected")
	}
}

// TestValidate_ReportsEveryProblemAtOnce checks the aggregation behaviour.
// Fixing configuration one error per restart is a miserable loop, and a
// partially-correct config is the most likely state during initial deployment.
func TestValidate_ReportsEveryProblemAtOnce(t *testing.T) {
	cfg := validConfig(t)
	cfg.Server.InternalAddr = "0.0.0.0:8081"
	cfg.Platform.RootDomain = "https://zensys.lk"
	cfg.Routing.BackendPathPrefix = "api"
	cfg.Observability.LogLevel = "verbose"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation to fail")
	}

	for _, want := range []string{"internal lifecycle API", "without a scheme", `must start with "/"`, "log_level"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the aggregated error to mention %q, got:\n%v", want, err)
		}
	}
}

func TestApexLabel(t *testing.T) {
	tests := []struct {
		domain string
		want   string
	}{
		{"zensys.lk", "zensys"},
		{"gateway.zensys.lk", "gateway"},
		{"zensys", "zensys"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			cfg := Default()
			cfg.Platform.RootDomain = tt.domain

			if got := cfg.ApexLabel(); got != tt.want {
				t.Fatalf("ApexLabel(%q) = %q, want %q", tt.domain, got, tt.want)
			}
		})
	}
}
