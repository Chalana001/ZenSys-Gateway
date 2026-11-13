package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLive_AlwaysReportsOK(t *testing.T) {
	// A liveness probe must not depend on downstreams. If it did, a
	// downstream outage would cause an orchestrator to restart a healthy
	// process and turn an outage into a restart loop.
	h := NewHealthHandler(discardLogger(), []Check{
		{Name: "registry", Run: func(context.Context) error { return errors.New("down") }},
	})

	rec := httptest.NewRecorder()
	h.Live(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("Live status = %d, want 200", rec.Code)
	}
}

func TestReady_AllChecksPass(t *testing.T) {
	h := NewHealthHandler(discardLogger(), []Check{
		{Name: "config", Run: func(context.Context) error { return nil }},
		{Name: "upstreams", Run: func(context.Context) error { return nil }},
	})

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("Ready status = %d, want 200", rec.Code)
	}

	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Checks["config"] != "ok" || body.Checks["upstreams"] != "ok" {
		t.Errorf("checks = %v, want both ok", body.Checks)
	}
}

func TestReady_FailingCheckReportsUnavailable(t *testing.T) {
	h := NewHealthHandler(discardLogger(), []Check{
		{Name: "config", Run: func(context.Context) error { return nil }},
		{Name: "registry", Run: func(context.Context) error { return errors.New("dial tcp 10.0.0.9:3306: connection refused") }},
	})

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Ready status = %d, want 503", rec.Code)
	}

	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "unavailable" {
		t.Errorf("status = %q, want unavailable", body.Status)
	}
	if body.Checks["registry"] != "failed" {
		t.Errorf("registry check = %q, want failed", body.Checks["registry"])
	}
	if body.Checks["config"] != "ok" {
		t.Errorf("a passing check should still report ok, got %q", body.Checks["config"])
	}
}

// TestReady_DoesNotLeakInternalDetail checks that a readiness failure does not
// disclose internal addresses or driver errors in the response body. Health can
// be exposed publicly via health_on_public, and a raw dial error would name
// internal hosts and ports.
func TestReady_DoesNotLeakInternalDetail(t *testing.T) {
	const internalDetail = "dial tcp 10.0.0.9:3306: connection refused"

	h := NewHealthHandler(discardLogger(), []Check{
		{Name: "registry", Run: func(context.Context) error { return errors.New(internalDetail) }},
	})

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if body := rec.Body.String(); strings.Contains(body, internalDetail) || strings.Contains(body, "10.0.0.9") {
		t.Fatalf("readiness response leaked internal detail:\n%s", body)
	}
}

// TestReady_RunsChecksConcurrently checks that one slow dependency does not
// make readiness take the sum of every dependency's timeout.
func TestReady_RunsChecksConcurrently(t *testing.T) {
	const (
		checkCount = 4
		delay      = 100 * time.Millisecond
	)

	checks := make([]Check, 0, checkCount)
	for i := 0; i < checkCount; i++ {
		checks = append(checks, Check{
			Name: string(rune('a' + i)),
			Run: func(ctx context.Context) error {
				select {
				case <-time.After(delay):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		})
	}

	h := NewHealthHandler(discardLogger(), checks)

	start := time.Now()
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("Ready status = %d, want 200", rec.Code)
	}

	// Serial execution would take 400ms. Allow generous headroom for a
	// loaded CI machine while still failing if the checks are sequential.
	if elapsed > 3*delay {
		t.Fatalf("readiness took %v for %d checks of %v each; checks do not appear to run concurrently",
			elapsed, checkCount, delay)
	}
}

func TestReady_NoChecksIsReady(t *testing.T) {
	h := NewHealthHandler(discardLogger(), nil)

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("Ready status = %d, want 200 when there are no checks", rec.Code)
	}
}

func TestRegister_MountsHealthEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	NewHealthHandler(discardLogger(), nil).Register(mux)

	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("GET %s Content-Type = %q", path, ct)
			}
		})
	}
}

// TestRegister_HealthEndpointsAreGETOnly documents what actually happens to a
// non-GET request in production.
//
// A GET-only pattern means the request does not match, and it then falls
// through to whatever catch-all the listener registered. Both listeners
// register one, so the observable result is a 404 in the standard error
// contract — not the 405 that Go's ServeMux would produce if the health
// endpoints were the only patterns mounted.
func TestRegister_HealthEndpointsAreGETOnly(t *testing.T) {
	mux := http.NewServeMux()
	NewHealthHandler(discardLogger(), nil).Register(mux)

	reachedCatchAll := false
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedCatchAll = true
		WriteError(w, http.StatusNotFound, ErrNotFound, "No such endpoint.")
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if !reachedCatchAll {
		t.Fatal("POST /healthz did not fall through to the catch-all handler")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /healthz status = %d, want 404 from the catch-all", rec.Code)
	}
}

// TestRegister_GETStillMatchesWithCatchAllPresent guards the interaction above
// from the other direction: adding a catch-all must not shadow the health
// endpoints. ServeMux resolves by specificity, not registration order.
func TestRegister_GETStillMatchesWithCatchAllPresent(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusNotFound, ErrNotFound, "No such endpoint.")
	}))
	NewHealthHandler(discardLogger(), nil).Register(mux)

	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200 (the catch-all shadowed it)", path, rec.Code)
			}
		})
	}
}
