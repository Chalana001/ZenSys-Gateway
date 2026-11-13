// Command migrate applies the Gateway's registry schema migrations.
//
// It tracks applied versions in a schema_migrations table and skips what has
// already run, so it is safe to invoke on every deploy.
//
// Usage:
//
//	export ZENSYS_REGISTRY_DSN='user:pass@tcp(host:3306)/zensys_gateway?parseTime=true'
//	go run ./cmd/migrate -dry-run
//	go run ./cmd/migrate
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/zensys/gateway/internal/tenant"
)

const migrationsTableDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    VARCHAR(255) NOT NULL,
  applied_at DATETIME(3)  NOT NULL,
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dsnEnv := flag.String("dsn-env", "ZENSYS_REGISTRY_DSN", "environment variable holding the registry DSN")
	dir := flag.String("dir", "migrations", "directory containing numbered .sql migration files")
	dryRun := flag.Bool("dry-run", false, "report what would be applied without changing the database")
	flag.Parse()

	raw, ok := os.LookupEnv(*dsnEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		return fmt.Errorf("environment variable %s is unset or empty", *dsnEnv)
	}

	// A migration file holds several statements, so this tool enables
	// multi-statement execution. The Gateway's runtime connection deliberately
	// does not — it never needs more than one statement per call, and leaving
	// it off removes a class of escalation where one injected statement
	// becomes a whole script.
	dsn, err := tenant.NormalizeDSN(raw, tenant.WithMultiStatements())
	if err != nil {
		return err
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to registry: %w", err)
	}

	return applyAll(ctx, db, *dir, *dryRun)
}

func applyAll(ctx context.Context, db *sql.DB, dir string, dryRun bool) error {
	if err := ensureMigrationsTable(ctx, db); err != nil {
		return err
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return fmt.Errorf("scan %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return fmt.Errorf("no .sql files found in %s", dir)
	}

	// Lexical order is the execution order, which is why files are named with
	// a zero-padded numeric prefix.
	sort.Strings(paths)

	pending := 0
	for _, path := range paths {
		version := strings.TrimSuffix(filepath.Base(path), ".sql")

		if applied[version] {
			fmt.Printf("  skip     %s (already applied)\n", version)
			continue
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		if dryRun {
			fmt.Printf("  pending  %s\n", version)
			pending++
			continue
		}

		if err := applyOne(ctx, db, version, string(body)); err != nil {
			return err
		}
		fmt.Printf("  applied  %s\n", version)
		pending++
	}

	if pending == 0 {
		fmt.Println("registry schema is up to date")
	} else if dryRun {
		fmt.Printf("%d migration(s) pending\n", pending)
	}

	return nil
}

// applyOne executes a migration and records its version.
//
// MySQL commits implicitly around DDL, so wrapping a migration in a
// transaction would not make it atomic. Migrations must therefore be written
// to be re-runnable, which is why every statement in them is
// CREATE TABLE IF NOT EXISTS.
func applyOne(ctx context.Context, db *sql.DB, version, body string) error {
	if _, err := db.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("apply migration %s: %w", version, err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		version, time.Now().UTC()); err != nil {
		return fmt.Errorf("record migration %s: %w", version, err)
	}

	return nil
}

func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, migrationsTableDDL); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[string]bool)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}

	return applied, nil
}
