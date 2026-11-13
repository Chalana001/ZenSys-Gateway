// Package observability provides the Gateway's structured logging and metrics
// primitives.
package observability

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Log formats.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// NewLogger builds the process logger.
//
// JSON is the default because the Gateway's logs are meant to be shipped and
// queried, not read in a terminal: one line per request with a stable field
// set (architecture doc §51).
func NewLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unrecognised log level %q", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	switch strings.ToLower(format) {
	case FormatJSON, "":
		handler = slog.NewJSONHandler(os.Stdout, opts)
	case FormatText:
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		return nil, fmt.Errorf("unrecognised log format %q", format)
	}

	return slog.New(handler), nil
}

// sensitiveHeaders is an allow-list, not a deny-list.
//
// Only headers named here are ever eligible to be logged. A deny-list would
// have to anticipate every credential-bearing header a product might
// introduce, and would fail silently the first time it missed one. An
// allow-list fails closed: a new header is simply not logged until someone
// deliberately adds it (architecture doc §51).
var sensitiveHeaders = map[string]bool{
	"host":              true,
	"x-request-id":      true,
	"x-tenant-id":       true,
	"x-forwarded-for":   true,
	"x-forwarded-host":  true,
	"x-forwarded-proto": true,
	"user-agent":        true,
	"content-type":      true,
	"accept":            true,
}

// SafeHeaderValue returns the value of a header if it is on the logging
// allow-list, and a redaction marker otherwise.
//
// Callers must use this rather than reading headers directly when logging.
// Returning a marker rather than an empty string keeps the log honest about
// the header having been present.
func SafeHeaderValue(headers map[string][]string, name string) string {
	if !sensitiveHeaders[strings.ToLower(name)] {
		return "[redacted]"
	}
	values := headers[name]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// ErrMissingDSN is returned when the registry DSN is absent at the point a
// connection is attempted. It exists so that the error is a fixed, greppable
// string rather than one that might accidentally embed the DSN itself.
var ErrMissingDSN = errors.New("registry DSN is not configured")
