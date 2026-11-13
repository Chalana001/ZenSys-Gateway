package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTenantNotFoundAndDisabledAreIndistinguishable is the regression test for
// ADR 0001 decision 5.
//
// Choosing 404 for a disabled tenant is only meaningful if the response is
// genuinely indistinguishable from an unknown tenant. If the bodies differed,
// an anonymous prober could confirm that a hostname belongs to a real business
// simply by comparing the two responses — which defeats the entire reason 404
// was chosen over 403.
func TestTenantNotFoundAndDisabledAreIndistinguishable(t *testing.T) {
	notFound := httptest.NewRecorder()
	WriteTenantNotFound(notFound)

	disabled := httptest.NewRecorder()
	WriteTenantDisabled(disabled)

	if notFound.Code != disabled.Code {
		t.Fatalf("status codes differ: unknown=%d disabled=%d", notFound.Code, disabled.Code)
	}
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", notFound.Code)
	}

	if got, want := disabled.Body.String(), notFound.Body.String(); got != want {
		t.Fatalf("bodies differ:\n unknown tenant: %s\n disabled tenant: %s", want, got)
	}

	if got, want := disabled.Header().Get("Content-Type"), notFound.Header().Get("Content-Type"); got != want {
		t.Fatalf("Content-Type differs: unknown=%q disabled=%q", want, got)
	}
}

func TestWriteError_Shape(t *testing.T) {
	tests := []struct {
		name   string
		write  func(http.ResponseWriter)
		status int
		code   ErrorCode
	}{
		{"unknown tenant", WriteTenantNotFound, http.StatusNotFound, ErrUnknownTenant},
		{"disabled tenant", WriteTenantDisabled, http.StatusNotFound, ErrUnknownTenant},
		{"registry unavailable", WriteRegistryUnavailable, http.StatusServiceUnavailable, ErrRegistryUnavailable},
		{"upstream unavailable", WriteUpstreamUnavailable, http.StatusBadGateway, ErrUpstreamUnavailable},
		{"upstream timeout", WriteUpstreamTimeout, http.StatusServiceUnavailable, ErrUpstreamUnavailable},
		{"internal error", WriteInternalError, http.StatusInternalServerError, ErrInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.write(rec)

			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", ct)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("X-Content-Type-Options: nosniff is missing")
			}

			var body ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != tt.code {
				t.Errorf("error code = %q, want %q", body.Error, tt.code)
			}
			if body.Message == "" {
				t.Error("message is empty")
			}
		})
	}
}

// TestErrorResponsesDoNotLeakInternals checks the §69 contract that no error
// body exposes database errors, connection strings, stack traces, or internal
// addresses.
func TestErrorResponsesDoNotLeakInternals(t *testing.T) {
	forbidden := []string{
		"goroutine",
		"stack",
		"mysql",
		"tcp(",
		"dial",
		"127.0.0.1",
		"10.0.0.",
		"password",
		"dsn",
		".go:",
	}

	writers := map[string]func(http.ResponseWriter){
		"unknown tenant":       WriteTenantNotFound,
		"disabled tenant":      WriteTenantDisabled,
		"registry unavailable": WriteRegistryUnavailable,
		"upstream unavailable": WriteUpstreamUnavailable,
		"internal error":       WriteInternalError,
	}

	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			write(rec)

			body := strings.ToLower(rec.Body.String())
			for _, needle := range forbidden {
				if strings.Contains(body, strings.ToLower(needle)) {
					t.Errorf("error body contains %q:\n%s", needle, rec.Body.String())
				}
			}
		})
	}
}

func TestWriteJSON_EncodesValue(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]string{"tenant": "mangala"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}

	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["tenant"] != "mangala" {
		t.Errorf("tenant = %q, want mangala", got["tenant"])
	}
}

// TestWriteJSON_UnencodableValueFailsClosed checks that a marshalling failure
// produces a valid error response rather than a partial or empty body.
func TestWriteJSON_UnencodableValueFailsClosed(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, make(chan int))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("fallback body is not valid JSON: %v", err)
	}
	if body.Error != ErrInternal {
		t.Errorf("error code = %q, want %q", body.Error, ErrInternal)
	}
}
