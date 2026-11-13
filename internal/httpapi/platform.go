package httpapi

import (
	"log/slog"
	"net/http"
	"os"
	"path"
)

// ApexHandler serves the root domain, e.g. https://zensys.lk/.
//
// The apex is never a tenant (architecture doc §8 step 5). Its behaviour is a
// configuration decision: serve the platform landing page, or behave like any
// other unregistered host and return 404 (ADR 0001 decision 4).
type ApexHandler struct {
	behavior string
	logger   *slog.Logger
	landing  http.Handler
}

// Apex behavior values. These mirror config.ApexBehavior* without importing
// the config package, so that this package keeps no dependency on how the
// deployment is configured.
const (
	ApexLanding  = "landing"
	ApexNotFound = "not_found"
)

// NewApexHandler builds the apex handler.
//
// A landing directory that does not exist is not a startup failure: the apex
// is reachable only for one hostname, and refusing to start the whole Gateway
// because a static directory is missing would trade a cosmetic problem for a
// total outage. The misconfiguration is logged once here instead.
func NewApexHandler(behavior, landingDir string, logger *slog.Logger) *ApexHandler {
	h := &ApexHandler{behavior: behavior, logger: logger}

	if behavior != ApexLanding {
		return h
	}

	info, err := os.Stat(landingDir)
	switch {
	case err != nil:
		logger.Error("apex is configured to serve a landing page but the directory is unreadable; the apex will return 404",
			"landing_dir", landingDir, "error", err)
		return h
	case !info.IsDir():
		logger.Error("apex landing_dir is not a directory; the apex will return 404",
			"landing_dir", landingDir)
		return h
	}

	h.landing = http.FileServer(noDirListing{http.Dir(landingDir)})
	return h
}

// ServeHTTP implements http.Handler.
func (h *ApexHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.landing == nil {
		// Either the apex is configured not to serve a page, or the landing
		// directory was unusable at startup. Both are "no page here", which
		// is a 404 in the standard error contract.
		WriteTenantNotFound(w)
		return
	}

	// nosniff on the static assets as well as on JSON responses. The file
	// server sets Content-Type from the file extension, and a browser that
	// sniffs a .txt as HTML would execute it.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.landing.ServeHTTP(w, r)
}

// noDirListing wraps an http.FileSystem so a directory without an index.html
// returns 404 instead of a generated directory listing.
//
// http.FileServer renders a listing for any directory it can open, which would
// expose the landing directory's contents — including any file an operator
// happened to leave there — to anyone who guessed the path.
type noDirListing struct {
	fs http.FileSystem
}

// Open implements http.FileSystem.
func (n noDirListing) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	if !info.IsDir() {
		return f, nil
	}

	// A directory is only servable if it has an index.html for FileServer to
	// render. If it does, returning the directory is correct — FileServer
	// opens the index itself. If it does not, ErrNotExist becomes a 404.
	index, err := n.fs.Open(path.Join(name, "index.html"))
	if err != nil {
		_ = f.Close()
		return nil, os.ErrNotExist
	}
	_ = index.Close()

	return f, nil
}
