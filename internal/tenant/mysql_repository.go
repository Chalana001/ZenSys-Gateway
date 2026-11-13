package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// MySQLOptions configures the registry connection pool.
type MySQLOptions struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	QueryTimeout    time.Duration
}

// MySQLRepository is the MySQL-backed platform registry.
type MySQLRepository struct {
	db           *sql.DB
	queryTimeout time.Duration
}

const (
	routeColumns        = `tenant_slug, product, status, created_at, updated_at`
	defaultQueryTimeout = 2 * time.Second
	pingTimeout         = 2 * time.Second
)

// MySQL error numbers we react to by name rather than by message text.
const (
	mysqlErrDuplicateEntry = 1062
	mysqlErrCheckViolation = 3819
	mysqlErrDataTooLong    = 1406
)

// OpenMySQL opens and configures a registry connection pool.
//
// sql.Open does not connect, so a nil error here does not mean the registry is
// reachable. That is intentional: the Gateway must start and report itself
// not-ready when the registry is down, rather than crash-looping, so that a
// registry outage does not become a Gateway outage. Unroutable tenants get a
// 503 and cached routes keep serving (§46, §57).
func OpenMySQL(dsn string, opts MySQLOptions) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open registry connection pool: %w", err)
	}

	if opts.MaxOpenConns > 0 {
		db.SetMaxOpenConns(opts.MaxOpenConns)
	}
	if opts.MaxIdleConns > 0 {
		db.SetMaxIdleConns(opts.MaxIdleConns)
	}
	if opts.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}

	return db, nil
}

// NewMySQLRepository wraps an open database handle.
func NewMySQLRepository(db *sql.DB, opts MySQLOptions) *MySQLRepository {
	timeout := opts.QueryTimeout
	if timeout <= 0 {
		timeout = defaultQueryTimeout
	}
	return &MySQLRepository{db: db, queryTimeout: timeout}
}

// GetBySlug implements Repository.
func (r *MySQLRepository) GetBySlug(ctx context.Context, slug string) (Route, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	row := r.db.QueryRowContext(ctx,
		`SELECT `+routeColumns+` FROM tenant_routes WHERE tenant_slug = ?`, slug)

	route, err := scanRoute(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Route{}, fmt.Errorf("%w: %q", ErrNotFound, slug)
	case err != nil:
		return Route{}, fmt.Errorf("read tenant route %q: %w", slug, err)
	}
	return route, nil
}

// List implements Repository.
func (r *MySQLRepository) List(ctx context.Context, filter ListFilter) ([]Route, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	var (
		conditions []string
		args       []any
	)

	if filter.Product != nil {
		conditions = append(conditions, "product = ?")
		args = append(args, string(*filter.Product))
	}
	if filter.Status != nil {
		conditions = append(conditions, "status = ?")
		args = append(args, string(*filter.Status))
	}
	if filter.After != "" {
		conditions = append(conditions, "tenant_slug > ?")
		args = append(args, filter.After)
	}

	// Conditions are built from fixed fragments and values are always bound,
	// so no caller input reaches the SQL text.
	query := `SELECT ` + routeColumns + ` FROM tenant_routes`
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, " AND ")
	}
	query += ` ORDER BY tenant_slug ASC LIMIT ?`
	args = append(args, filter.ClampLimit())

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tenant routes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	routes := make([]Route, 0, 16)
	for rows.Next() {
		route, err := scanRoute(rows)
		if err != nil {
			return nil, fmt.Errorf("scan tenant route: %w", err)
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenant routes: %w", err)
	}

	return routes, nil
}

// Reserve implements Repository.
func (r *MySQLRepository) Reserve(ctx context.Context, route Route, event Event) (Route, error) {
	var stored Route

	err := r.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO tenant_routes (tenant_slug, product, status) VALUES (?, ?, ?)`,
			route.TenantSlug, string(route.Product), string(route.Status))

		switch {
		case isDuplicateKey(err):
			return fmt.Errorf("%w: %q", ErrAlreadyExists, route.TenantSlug)
		case isCheckViolation(err), isDataTooLong(err):
			// The service validates before calling, so a CHECK firing means a
			// validation path was bypassed. That is a bug in the Gateway, not
			// a bad request, so it is surfaced as an internal error.
			return fmt.Errorf("registry rejected tenant route %q: %w", route.TenantSlug, err)
		case err != nil:
			return fmt.Errorf("insert tenant route: %w", err)
		}

		if err := insertEvent(ctx, tx, event); err != nil {
			return err
		}

		row := tx.QueryRowContext(ctx,
			`SELECT `+routeColumns+` FROM tenant_routes WHERE tenant_slug = ?`, route.TenantSlug)

		read, err := scanRoute(row)
		if err != nil {
			return fmt.Errorf("read back tenant route: %w", err)
		}
		stored = read
		return nil
	})

	if err != nil {
		return Route{}, err
	}
	return stored, nil
}

// Transition implements Repository.
func (r *MySQLRepository) Transition(ctx context.Context, slug string, from, to Status, event Event) (Route, error) {
	var stored Route

	err := r.inTx(ctx, func(tx *sql.Tx) error {
		// Compare-and-swap. The "AND status = ?" clause is what makes this
		// safe under concurrency: the transition only applies if the stored
		// status still matches what the caller read. Without it, two callers
		// could each read PROVISIONING and each write a different target
		// status, and the last writer would silently win.
		res, err := tx.ExecContext(ctx,
			`UPDATE tenant_routes SET status = ? WHERE tenant_slug = ? AND status = ?`,
			string(to), slug, string(from))
		if err != nil {
			return fmt.Errorf("update tenant route %q: %w", slug, err)
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("update tenant route %q: %w", slug, err)
		}

		if affected == 0 {
			// Either the row is gone, or its status moved between the caller's
			// read and this write. Distinguishing the two lets the caller tell
			// "no such tenant" from "retry with fresh state".
			var current string
			scanErr := tx.QueryRowContext(ctx,
				`SELECT status FROM tenant_routes WHERE tenant_slug = ?`, slug).Scan(&current)

			switch {
			case errors.Is(scanErr, sql.ErrNoRows):
				return fmt.Errorf("%w: %q", ErrNotFound, slug)
			case scanErr != nil:
				return fmt.Errorf("read tenant route %q: %w", slug, scanErr)
			}

			return fmt.Errorf("%w: %q moved from %s to %s while the transition was in flight",
				ErrConflict, slug, from, current)
		}

		if err := insertEvent(ctx, tx, event); err != nil {
			return err
		}

		row := tx.QueryRowContext(ctx,
			`SELECT `+routeColumns+` FROM tenant_routes WHERE tenant_slug = ?`, slug)

		read, err := scanRoute(row)
		if err != nil {
			return fmt.Errorf("read back tenant route: %w", err)
		}
		stored = read
		return nil
	})

	if err != nil {
		return Route{}, err
	}
	return stored, nil
}

// Release implements Repository.
func (r *MySQLRepository) Release(ctx context.Context, slug string, event Event) error {
	return r.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM tenant_routes WHERE tenant_slug = ?`, slug)
		if err != nil {
			return fmt.Errorf("delete tenant route %q: %w", slug, err)
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete tenant route %q: %w", slug, err)
		}
		if affected == 0 {
			return fmt.Errorf("%w: %q", ErrNotFound, slug)
		}

		return insertEvent(ctx, tx, event)
	})
}

// Ping implements Repository.
func (r *MySQLRepository) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	return r.db.PingContext(ctx)
}

// inTx runs fn inside a transaction, rolling back if it returns an error.
//
// The event write shares this transaction with the mutation it describes, so
// the audit trail cannot record a change that was rolled back, nor miss one
// that committed.
func (r *MySQLRepository) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin registry transaction: %w", err)
	}

	// Rolling back after a successful Commit returns ErrTxDone, which is the
	// expected outcome and is ignored.
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit registry transaction: %w", err)
	}

	return nil
}

// insertEvent appends one audit record within the caller's transaction.
func insertEvent(ctx context.Context, tx *sql.Tx, event Event) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO tenant_route_events
		   (tenant_slug, action, from_status, to_status, product, actor_service, actor_request_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.TenantSlug,
		string(event.Action),
		nullableStatus(event.From),
		nullableStatus(event.To),
		string(event.Product),
		event.Actor.Service,
		event.Actor.RequestID,
	)
	if err != nil {
		return fmt.Errorf("record %s event for %q: %w", event.Action, event.TenantSlug, err)
	}
	return nil
}

// nullableStatus maps an empty status to SQL NULL.
//
// RESERVE has no previous status and RELEASE has no resulting status. Storing
// an empty string instead would make "there was no previous state"
// indistinguishable from "the previous state was the empty string".
func nullableStatus(s Status) any {
	if s == "" {
		return nil
	}
	return string(s)
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRoute reads one route row and validates the stored values.
func scanRoute(s rowScanner) (Route, error) {
	var (
		slug    string
		product string
		status  string
		created time.Time
		updated time.Time
	)

	if err := s.Scan(&slug, &product, &status, &created, &updated); err != nil {
		return Route{}, err
	}

	route := Route{
		TenantSlug: slug,
		Product:    Product(product),
		Status:     Status(status),
		CreatedAt:  created.UTC(),
		UpdatedAt:  updated.UTC(),
	}

	// The CHECK constraints make these unreachable in normal operation, but a
	// hand-edited row or a future schema change could produce one. Failing
	// loudly beats returning a Route whose Product silently matches no
	// configured upstream.
	if !route.Product.Valid() {
		return Route{}, fmt.Errorf("stored route %q has unrecognised product %q", slug, product)
	}
	if !route.Status.Valid() {
		return Route{}, fmt.Errorf("stored route %q has unrecognised status %q", slug, status)
	}

	return route, nil
}

func isDuplicateKey(err error) bool   { return mysqlErrorNumber(err) == mysqlErrDuplicateEntry }
func isCheckViolation(err error) bool { return mysqlErrorNumber(err) == mysqlErrCheckViolation }
func isDataTooLong(err error) bool    { return mysqlErrorNumber(err) == mysqlErrDataTooLong }

func mysqlErrorNumber(err error) uint16 {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number
	}
	return 0
}
