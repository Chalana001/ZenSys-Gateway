package tenant

import (
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestNormalizeDSN(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		wantErr string
	}{
		{"valid", "user:pass@tcp(127.0.0.1:3306)/zensys_gateway?parseTime=true", ""},
		{"valid with params", "user:pass@tcp(host:3306)/zensys_gateway?parseTime=true&charset=utf8mb4", ""},
		{"empty", "", "empty"},
		{"whitespace only", "   ", "empty"},
		{"missing parseTime", "user:pass@tcp(127.0.0.1:3306)/zensys_gateway", "parseTime=true"},
		{"parseTime false", "user:pass@tcp(127.0.0.1:3306)/zensys_gateway?parseTime=false", "parseTime=true"},
		{"no database", "user:pass@tcp(127.0.0.1:3306)/", "must name a database"},
		{"malformed", "this is not a dsn", "not valid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeDSN(tt.dsn)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("NormalizeDSN(%q) = %v, want nil", tt.dsn, err)
				}
				return
			}

			if err == nil {
				t.Fatalf("NormalizeDSN(%q) = nil, want an error mentioning %q", tt.dsn, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NormalizeDSN(%q) error = %q, want it to mention %q", tt.dsn, err, tt.wantErr)
			}
		})
	}
}

// TestNormalizeDSN_PinsTimezoneToUTC is the regression test for a silent
// timestamp bug.
//
// MySQL DATETIME carries no timezone, so the server evaluates CURRENT_TIMESTAMP
// in its session timezone while the driver parses what it reads according to
// cfg.Loc. If those disagree, every timestamp the API returns is offset by the
// difference — which looks like data corruption rather than a config problem.
func TestNormalizeDSN_PinsTimezoneToUTC(t *testing.T) {
	normalized, err := NormalizeDSN("user:pass@tcp(127.0.0.1:3306)/zensys_gateway?parseTime=true")
	if err != nil {
		t.Fatalf("NormalizeDSN: %v", err)
	}

	cfg, err := mysql.ParseDSN(normalized)
	if err != nil {
		t.Fatalf("re-parse normalized DSN: %v", err)
	}

	if cfg.Loc.String() != "UTC" {
		t.Errorf("loc = %s, want UTC", cfg.Loc)
	}
	if got := cfg.Params["time_zone"]; got != "'+00:00'" {
		t.Errorf("time_zone = %q, want '+00:00'", got)
	}
	if !cfg.ParseTime {
		t.Error("parseTime was dropped during normalisation")
	}
	if cfg.DBName != "zensys_gateway" {
		t.Errorf("dbname = %q, want zensys_gateway", cfg.DBName)
	}
}

// TestNormalizeDSN_RespectsExplicitTimezone checks that an operator who has
// deliberately set a timezone is not overridden.
func TestNormalizeDSN_RespectsExplicitTimezone(t *testing.T) {
	normalized, err := NormalizeDSN("user:pass@tcp(127.0.0.1:3306)/zensys_gateway?parseTime=true&time_zone=%27%2B05%3A30%27")
	if err != nil {
		t.Fatalf("NormalizeDSN: %v", err)
	}

	cfg, err := mysql.ParseDSN(normalized)
	if err != nil {
		t.Fatalf("re-parse normalized DSN: %v", err)
	}

	if got := cfg.Params["time_zone"]; got == "'+00:00'" {
		t.Fatal("an explicitly configured time_zone was overwritten with UTC")
	}
}

// TestNormalizeDSN_MultiStatementsIsOptIn checks that the runtime DSN does not
// enable multi-statement execution. The Gateway never needs more than one
// statement per call, and leaving it off removes a class of escalation where
// one injected statement becomes a whole script.
func TestNormalizeDSN_MultiStatementsIsOptIn(t *testing.T) {
	const raw = "user:pass@tcp(127.0.0.1:3306)/zensys_gateway?parseTime=true"

	runtimeDSN, err := NormalizeDSN(raw)
	if err != nil {
		t.Fatalf("NormalizeDSN: %v", err)
	}
	runtimeCfg, err := mysql.ParseDSN(runtimeDSN)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if runtimeCfg.MultiStatements {
		t.Error("multi-statement execution must be off for the runtime connection")
	}

	migrationDSN, err := NormalizeDSN(raw, WithMultiStatements())
	if err != nil {
		t.Fatalf("NormalizeDSN with multi-statements: %v", err)
	}
	migrationCfg, err := mysql.ParseDSN(migrationDSN)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !migrationCfg.MultiStatements {
		t.Error("the migration DSN must permit multi-statement execution")
	}
}

// TestNormalizeDSN_ErrorNeverContainsThePassword checks that a startup failure
// is safe to paste into a ticket or a CI log.
func TestNormalizeDSN_ErrorNeverContainsThePassword(t *testing.T) {
	const password = "hunter2-super-secret"

	// Missing parseTime is the error path most likely to be hit by an operator.
	_, err := NormalizeDSN("user:" + password + "@tcp(127.0.0.1:3306)/zensys_gateway")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("the DSN error leaked the password:\n%v", err)
	}
}
