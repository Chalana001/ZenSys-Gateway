package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// minimalConfig is a valid configuration with no landing page required, so
// that load tests do not need a temporary directory.
const minimalConfig = `
server:
  public_addr: "127.0.0.1:8080"
  internal_addr: "127.0.0.1:8081"
  read_header_timeout: 5s
  idle_timeout: 60s
  shutdown_grace: 15s
  max_header_bytes: 65536
  max_body_bytes: 10485760

platform:
  root_domain: "zensys.lk"
  apex_behavior: "not_found"

registry:
  driver: "mysql"
  dsn_env: "ZENSYS_TEST_DSN"

products:
  POS:
    frontend_upstream: "http://127.0.0.1:3001"
    backend_upstream: "http://127.0.0.1:8082"
    upstream_timeout: 30s
    inject_tenant_header: true
    tenant_header_name: "X-Tenant-ID"
  GMS:
    frontend_upstream: "http://127.0.0.1:3002"
    backend_upstream: "http://127.0.0.1:8083"
    upstream_timeout: 30s

internal_auth:
  mode: "token"
  tokens:
    - service: "pos"
      token_env: "ZENSYS_TEST_TOKEN_POS"
      may_manage_products: ["POS"]
    - service: "gms"
      token_env: "ZENSYS_TEST_TOKEN_GMS"
      may_manage_products: ["GMS"]
`

// writeConfig writes content to a temporary file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// unsetenv removes an environment variable for the duration of the test.
// t.Setenv cannot unset, only assign.
func unsetenv(t *testing.T, key string) {
	t.Helper()

	old, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func setValidSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("ZENSYS_TEST_DSN", "user:pass@tcp(127.0.0.1:3306)/zensys_gateway?parseTime=true")
	t.Setenv("ZENSYS_TEST_TOKEN_POS", strings.Repeat("p", 32))
	t.Setenv("ZENSYS_TEST_TOKEN_GMS", strings.Repeat("g", 32))
}

func TestLoad_ResolvesSecrets(t *testing.T) {
	setValidSecrets(t)

	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Registry.DSN != "user:pass@tcp(127.0.0.1:3306)/zensys_gateway?parseTime=true" {
		t.Fatalf("registry DSN was not resolved from the environment")
	}

	byService := map[string]string{}
	for _, tok := range cfg.InternalAuth.Tokens {
		byService[tok.Service] = tok.Token
	}
	if byService["pos"] != strings.Repeat("p", 32) {
		t.Errorf("POS token was not resolved")
	}
	if byService["gms"] != strings.Repeat("g", 32) {
		t.Errorf("GMS token was not resolved")
	}
}

func TestLoad_AppliesDefaultsForOmittedKeys(t *testing.T) {
	setValidSecrets(t)

	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Values absent from minimalConfig must come from Default, not from the
	// zero value of the field.
	if cfg.Cache.RouteTTL.Std() != 60*time.Second {
		t.Errorf("cache.route_ttl default = %v, want 60s", cfg.Cache.RouteTTL)
	}
	if cfg.Cache.NegativeTTL.Std() != 30*time.Second {
		t.Errorf("cache.negative_ttl default = %v, want 30s", cfg.Cache.NegativeTTL)
	}
	if cfg.Routing.BackendPathPrefix != "/api" {
		t.Errorf("routing.backend_path_prefix default = %q, want /api", cfg.Routing.BackendPathPrefix)
	}
	if !cfg.Proxy.PreserveHost {
		t.Error("proxy.preserve_host default should be true")
	}
	if !cfg.RateLimit.Enabled {
		t.Error("rate_limit.enabled default should be true")
	}
	if cfg.Observability.LogFormat != "json" {
		t.Errorf("observability.log_format default = %q, want json", cfg.Observability.LogFormat)
	}
	if cfg.Observability.HealthOnPublic {
		t.Error("observability.health_on_public default should be false")
	}
	if cfg.Registry.MaxOpenConns != 20 {
		t.Errorf("registry.max_open_conns default = %d, want 20", cfg.Registry.MaxOpenConns)
	}
}

func TestLoad_ValuesInFileOverrideDefaults(t *testing.T) {
	setValidSecrets(t)

	override := strings.Replace(minimalConfig,
		`  read_header_timeout: 5s`,
		"  read_header_timeout: 2s", 1)
	override += "\ncache:\n  route_ttl: 5s\n"

	cfg, err := Load(writeConfig(t, override))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.ReadHeaderTimeout.Std() != 2*time.Second {
		t.Errorf("read_header_timeout = %v, want 2s", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Cache.RouteTTL.Std() != 5*time.Second {
		t.Errorf("route_ttl = %v, want 5s", cfg.Cache.RouteTTL)
	}
}

// TestLoad_MissingSecretIsFatal is the M0 exit criterion that the Gateway
// refuses to start without its secrets, rather than running with an empty one.
func TestLoad_MissingSecretIsFatal(t *testing.T) {
	tests := []struct {
		name     string
		unset    string
		wantHint string
	}{
		{"missing DSN", "ZENSYS_TEST_DSN", "ZENSYS_TEST_DSN"},
		{"missing POS token", "ZENSYS_TEST_TOKEN_POS", "ZENSYS_TEST_TOKEN_POS"},
		{"missing GMS token", "ZENSYS_TEST_TOKEN_GMS", "ZENSYS_TEST_TOKEN_GMS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setValidSecrets(t)
			unsetenv(t, tt.unset)

			_, err := Load(writeConfig(t, minimalConfig))
			if err == nil {
				t.Fatalf("expected Load to fail with %s unset", tt.unset)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Fatalf("expected the error to name %s, got: %v", tt.wantHint, err)
			}
		})
	}
}

func TestLoad_EmptySecretIsFatal(t *testing.T) {
	setValidSecrets(t)
	t.Setenv("ZENSYS_TEST_DSN", "   ") // whitespace only

	_, err := Load(writeConfig(t, minimalConfig))
	if err == nil {
		t.Fatal("expected a whitespace-only DSN to be treated as missing")
	}
}

// TestLoad_ShortServiceTokenIsFatal guards the only credential protecting
// tenant lifecycle mutations.
func TestLoad_ShortServiceTokenIsFatal(t *testing.T) {
	setValidSecrets(t)
	t.Setenv("ZENSYS_TEST_TOKEN_POS", "tooshort")

	_, err := Load(writeConfig(t, minimalConfig))
	if err == nil {
		t.Fatal("expected a short service token to be rejected")
	}
	if !strings.Contains(err.Error(), "minimum is 32") {
		t.Fatalf("expected the error to state the minimum length, got: %v", err)
	}
}

// TestLoad_ErrorNeverContainsTheSecret checks that a startup failure is safe to
// paste into a ticket or a CI log.
func TestLoad_ErrorNeverContainsTheSecret(t *testing.T) {
	const secret = "supersecretpassword"

	t.Run("short token error", func(t *testing.T) {
		setValidSecrets(t)
		t.Setenv("ZENSYS_TEST_TOKEN_POS", secret) // 19 chars, below the minimum

		_, err := Load(writeConfig(t, minimalConfig))
		if err == nil {
			t.Fatal("expected Load to fail")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the error message leaked the secret value:\n%v", err)
		}
	})
}

// TestLoad_RejectsUnknownKeys checks that a typo is reported rather than
// silently leaving a default in place.
func TestLoad_RejectsUnknownKeys(t *testing.T) {
	setValidSecrets(t)

	withTypo := strings.Replace(minimalConfig,
		`  root_domain: "zensys.lk"`,
		"  root_domain: \"zensys.lk\"\n  apex_behaviour: \"landing\"", 1)

	_, err := Load(writeConfig(t, withTypo))
	if err == nil {
		t.Fatal("expected an unknown configuration key to be rejected")
	}
	if !strings.Contains(err.Error(), "apex_behaviour") {
		t.Fatalf("expected the error to name the offending key, got: %v", err)
	}
}

func TestLoad_RejectsInvalidDuration(t *testing.T) {
	setValidSecrets(t)

	bad := strings.Replace(minimalConfig,
		"  read_header_timeout: 5s",
		"  read_header_timeout: 5 seconds", 1)

	_, err := Load(writeConfig(t, bad))
	if err == nil {
		t.Fatal("expected an unparseable duration to be rejected")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Fatalf("expected the error to mention the duration, got: %v", err)
	}
}

func TestLoad_RejectsNegativeDuration(t *testing.T) {
	setValidSecrets(t)

	bad := strings.Replace(minimalConfig,
		"  idle_timeout: 60s",
		"  idle_timeout: -1s", 1)

	_, err := Load(writeConfig(t, bad))
	if err == nil {
		t.Fatal("expected a negative duration to be rejected")
	}
}

// TestLoad_MissingFileIsAClearError checks the message a new contributor sees
// when they have not copied the example configuration yet.
func TestLoad_MissingFileIsAClearError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected a missing configuration file to be an error")
	}
	if !strings.Contains(err.Error(), "open config") {
		t.Fatalf("expected an 'open config' error, got: %v", err)
	}
}

// TestDuration_RoundTrips covers the custom YAML unmarshaller directly.
func TestDuration_RoundTrips(t *testing.T) {
	cfg, err := Load(writeConfigWith(t, minimalConfig, `
cache:
  route_ttl: 90s
  negative_ttl: 15s
  max_entries: 500
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Cache.RouteTTL.Std() != 90*time.Second {
		t.Errorf("route_ttl = %v, want 90s", cfg.Cache.RouteTTL)
	}
	if cfg.Cache.NegativeTTL.Std() != 15*time.Second {
		t.Errorf("negative_ttl = %v, want 15s", cfg.Cache.NegativeTTL)
	}
	if cfg.Cache.MaxEntries != 500 {
		t.Errorf("max_entries = %d, want 500", cfg.Cache.MaxEntries)
	}
}

// writeConfigWith appends extra YAML to the minimal configuration.
func writeConfigWith(t *testing.T, base, extra string) string {
	t.Helper()
	setValidSecrets(t)
	return writeConfig(t, base+"\n"+extra)
}
