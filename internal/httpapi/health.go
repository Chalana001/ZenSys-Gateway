package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// CheckFunc reports whether a dependency is usable.
//
// It returns nil when healthy. The returned error is for logs only: it is
// never written to the response body, because it may contain internal
// addresses or driver errors (architecture doc §69).
type CheckFunc func(ctx context.Context) error

// Check is a named readiness dependency.
type Check struct {
	Name string
	Run  CheckFunc
}

// HealthHandler serves liveness and readiness.
//
// These live on the internal listener by default. The public listener is a
// host-based catch-all, so serving /healthz there would shadow a path a
// product may legitimately own on {tenant}.zensys.lk.
type HealthHandler struct {
	logger  *slog.Logger
	checks  []Check
	timeout time.Duration
}

// NewHealthHandler builds a readiness handler.
//
// checks may be empty, in which case the Gateway is ready as soon as it is
// listening.
func NewHealthHandler(logger *slog.Logger, checks []Check) *HealthHandler {
	return &HealthHandler{
		logger:  logger,
		checks:  checks,
		timeout: 2 * time.Second,
	}
}

// Register mounts the health endpoints on mux.
func (h *HealthHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.Live)
	mux.HandleFunc("GET /readyz", h.Ready)
}

// healthResponse is the body returned by both endpoints.
type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Live reports process liveness.
//
// It deliberately performs no dependency checks. A liveness probe that fails
// because a downstream is unreachable would cause an orchestrator to restart a
// perfectly healthy process, turning a downstream outage into a restart loop
// (architecture doc §49).
func (h *HealthHandler) Live(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// Ready reports whether the Gateway can serve traffic.
//
// Checks run concurrently and share one deadline, so a single slow dependency
// cannot make readiness take the sum of all their timeouts.
//
// The response reports only "ok" or "failed" per check. The underlying error is
// logged instead, because health_on_public can expose this endpoint publicly
// and a raw driver error would leak internal addresses.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	results := make(map[string]string, len(h.checks))
	failures := make(map[string]error, len(h.checks))

	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, check := range h.checks {
		wg.Add(1)
		go func(c Check) {
			defer wg.Done()

			err := c.Run(ctx)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results[c.Name] = "failed"
				failures[c.Name] = err
				return
			}
			results[c.Name] = "ok"
		}(check)
	}

	wg.Wait()

	if len(failures) == 0 {
		WriteJSON(w, http.StatusOK, healthResponse{Status: "ok", Checks: results})
		return
	}

	// Log the detail. A readiness failure with no logged cause is an
	// operational dead end.
	for name, err := range failures {
		h.logger.Warn("readiness check failed",
			"check", name,
			"error", err,
		)
	}

	WriteJSON(w, http.StatusServiceUnavailable, healthResponse{
		Status: "unavailable",
		Checks: results,
	})
}
