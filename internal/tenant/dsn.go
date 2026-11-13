package tenant

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// DSNOption customises DSN normalisation.
type DSNOption func(*mysql.Config)

// WithMultiStatements permits several statements in a single Exec.
//
// Only the migration tool uses this. It is deliberately not the default: the
// Gateway's runtime connection never needs to send more than one statement per
// call, and leaving it off removes a class of escalation where a single
// injected statement becomes a whole script.
func WithMultiStatements() DSNOption {
	return func(c *mysql.Config) { c.MultiStatements = true }
}

// NormalizeDSN validates and canonicalises a registry DSN.
//
// Failing here, at startup, is much better than failing later with an opaque
// scan error on the first request that reads a timestamp.
func NormalizeDSN(dsn string, opts ...DSNOption) (string, error) {
	if strings.TrimSpace(dsn) == "" {
		return "", errors.New("registry DSN is empty")
	}

	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("registry DSN is not valid: %w", err)
	}

	if cfg.DBName == "" {
		return "", errors.New("registry DSN must name a database")
	}

	if !cfg.ParseTime {
		return "", errors.New("registry DSN must set parseTime=true, otherwise DATETIME columns cannot scan into time.Time")
	}

	// Pin the session timezone and the driver's parser to UTC together.
	//
	// MySQL DATETIME carries no timezone: CURRENT_TIMESTAMP(3) is evaluated in
	// the server session's timezone, while the driver interprets what it reads
	// according to cfg.Loc. If those two disagree, every timestamp the API
	// returns is silently offset by the difference — a bug that looks like
	// data corruption and is easy to misdiagnose. Setting both to UTC removes
	// the possibility rather than depending on how the server happens to be
	// configured.
	cfg.Loc = time.UTC
	if cfg.Params == nil {
		cfg.Params = make(map[string]string, 1)
	}
	if _, ok := cfg.Params["time_zone"]; !ok {
		cfg.Params["time_zone"] = "'+00:00'"
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return cfg.FormatDSN(), nil
}
