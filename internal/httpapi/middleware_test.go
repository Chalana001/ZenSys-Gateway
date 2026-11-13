package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testRequestIDHeader = "X-Request-ID"

func TestRequestID_SetsHeaderAndContext(t *testing.T) {
	var seen string

	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = RequestIDFrom(r.Context())
		}),
		RequestID(testRequestIDHeader),
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("the request ID is not present in the request context")
	}
	if got := rec.Header().Get(testRequestIDHeader); got != seen {
		t.Fatalf("response header %q = %q, context value = %q", testRequestIDHeader, got, seen)
	}
	if len(seen) != 32 {
		t.Errorf("request ID %q has length %d, want 32 hex characters", seen, len(seen))
	}
}

// TestRequestID_IgnoresInboundValue documents the V1 behaviour: a client
// cannot choose its own request ID.
//
// Accepting an inbound ID is only safe once the request is known to have come
// from a trusted proxy. Until that trust decision is implemented, honouring a
// client value would let anyone forge entries in a product's logs.
func TestRequestID_IgnoresInboundValue(t *testing.T) {
	handler := Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		RequestID(testRequestIDHeader),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(testRequestIDHeader, "attacker-chosen-id")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(testRequestIDHeader); got == "attacker-chosen-id" {
		t.Fatal("an inbound request ID was accepted from an untrusted client")
	}
}

func TestRequestID_IsUniquePerRequest(t *testing.T) {
	seen := make(map[string]bool)

	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := RequestIDFrom(r.Context())
			if seen[id] {
				t.Errorf("duplicate request ID %q", id)
			}
			seen[id] = true
		}),
		RequestID(testRequestIDHeader),
	)

	for i := 0; i < 100; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
}

func TestRequestIDFrom_MissingReturnsEmpty(t *testing.T) {
	if got := RequestIDFrom(context.Background()); got != "" {
		t.Fatalf("RequestIDFrom on a bare context = %q, want empty", got)
	}
}

func TestRecover_TurnsPanicIntoInternalError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	handler := Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("something went wrong: secret-value-123")
		}),
		RequestID(testRequestIDHeader),
		Recover(logger),
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	// The panic value must not reach the client.
	if body := rec.Body.String(); strings.Contains(body, "secret-value-123") {
		t.Fatalf("the panic value leaked into the response:\n%s", body)
	}
	if strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatal("a stack trace leaked into the response")
	}

	// It must reach the log, otherwise the failure is undiagnosable.
	if !strings.Contains(logs.String(), "secret-value-123") {
		t.Error("the panic value was not logged")
	}
	if !strings.Contains(logs.String(), "panic recovered") {
		t.Error("the recovery was not logged with a recognisable message")
	}
}

// TestRecover_RepanicsAbortHandler checks that http.ErrAbortHandler is passed
// through rather than converted into a 500. net/http uses it as a sentinel for
// a deliberately aborted handler.
func TestRecover_RepanicsAbortHandler(t *testing.T) {
	handler := Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}),
		Recover(discardLogger()),
	)

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected http.ErrAbortHandler to be re-panicked")
		}
		if recovered != http.ErrAbortHandler {
			t.Fatalf("re-panicked with %v, want http.ErrAbortHandler", recovered)
		}
	}()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRecover_DoesNotTouchNormalResponses(t *testing.T) {
	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusTeapot, map[string]string{"ok": "true"})
		}),
		Recover(discardLogger()),
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}
}

func TestAccessLog_RecordsStatusAndRequestID(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("nope"))
		}),
		RequestID(testRequestIDHeader),
		AccessLog(logger),
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/orders", nil))

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("log line is not valid JSON: %v\n%s", err, logs.String())
	}

	if entry["status"] != float64(http.StatusNotFound) {
		t.Errorf("logged status = %v, want 404", entry["status"])
	}
	if entry["path"] != "/api/orders" {
		t.Errorf("logged path = %v, want /api/orders", entry["path"])
	}
	if entry["method"] != http.MethodGet {
		t.Errorf("logged method = %v, want GET", entry["method"])
	}
	if entry["request_id"] == "" || entry["request_id"] == nil {
		t.Error("logged request_id is empty")
	}
	if _, ok := entry["duration_ms"]; !ok {
		t.Error("logged entry has no duration_ms")
	}
}

// TestAccessLog_NeverLogsQueryString guards against credentials leaking into
// logs: reset tokens and similar routinely travel in query strings.
func TestAccessLog_NeverLogsQueryString(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	handler := Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		AccessLog(logger),
	)

	req := httptest.NewRequest(http.MethodGet, "/reset?token=super-secret-token", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(logs.String(), "super-secret-token") {
		t.Fatalf("the query string leaked into the access log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "/reset") {
		t.Errorf("the path was not logged:\n%s", logs.String())
	}
}

func TestAccessLog_QuietPathsLogAtDebug(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	handler := Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		AccessLog(logger, "/healthz", "/readyz"),
	)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if logs.Len() != 0 {
		t.Fatalf("a quiet path was logged at info level:\n%s", logs.String())
	}
}

func TestAccessLog_ServerErrorsLogAtWarn(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}),
		AccessLog(logger),
	)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("a 502 was not logged at warn level:\n%s", logs.String())
	}
}

// TestAccessLog_QuietPathStillEscalatesOnServerError checks that demoting
// health and metrics to debug does not hide a failing health endpoint. A 500
// on /readyz is precisely the event an operator needs to see.
func TestAccessLog_QuietPathStillEscalatesOnServerError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}),
		AccessLog(logger, "/healthz", "/readyz"),
	)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("a 500 on a quiet path was suppressed; expected a warn-level entry:\n%s", logs.String())
	}
}

// TestChain_FirstMiddlewareIsOutermost pins the ordering contract, which is
// load-bearing: AccessLog must sit outside Recover so that a recovered panic
// is still recorded as a 500.
func TestChain_FirstMiddlewareIsOutermost(t *testing.T) {
	var order []string

	tag := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+":before")
				next.ServeHTTP(w, r)
				order = append(order, name+":after")
			})
		}
	}

	handler := Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			order = append(order, "handler")
		}),
		tag("first"),
		tag("second"),
		tag("third"),
	)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{
		"first:before", "second:before", "third:before",
		"handler",
		"third:after", "second:after", "first:after",
	}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestStatusRecorder_RecordsFirstStatusOnly(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}

	rec.WriteHeader(http.StatusNotFound)
	rec.WriteHeader(http.StatusInternalServerError) // must be ignored

	if rec.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (the first status written wins)", rec.status)
	}
}

func TestStatusRecorder_WriteImpliesOK(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}

	if _, err := rec.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if rec.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.status)
	}
	if rec.bytes != 5 {
		t.Fatalf("bytes = %d, want 5", rec.bytes)
	}
}

// TestStatusRecorder_UnwrapExposesFlusher checks that wrapping the
// ResponseWriter does not break streaming responses. Without Unwrap,
// http.ResponseController cannot reach the underlying Flusher and server-sent
// events would buffer until the handler returned.
func TestStatusRecorder_UnwrapExposesFlusher(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}

	if _, ok := rec.Unwrap().(http.Flusher); !ok {
		t.Fatal("Unwrap did not expose the underlying Flusher")
	}
	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("ResponseController could not flush through the wrapper: %v", err)
	}
}

// TestMaxBodyBytes_RefusesADeclaredOversizedBodyBeforeTheHandler is the plan's
// §8.1 step 3: a request whose Content-Length is already known to be over the
// limit is answered 413 without the handler — and therefore without routing,
// and without an upstream connection — being reached.
func TestMaxBodyBytes_RefusesADeclaredOversizedBodyBeforeTheHandler(t *testing.T) {
	called := false

	handler := Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }),
		MaxBodyBytes(8),
	)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("this body is longer than eight bytes"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("the handler ran for a body that was already known to be too large")
	}
}

// TestMaxBodyBytes_LimitsABodyWithNoDeclaredLength is the other half, and the
// reason the limit is enforced on the reader as well.
//
// A chunked body declares no length, so there is nothing to check up front.
// The error surfaces while the body is being read — during the round trip, in
// the proxy's case — which is why internal/proxy maps *http.MaxBytesError to
// the same 413 rather than reporting a transport failure.
func TestMaxBodyBytes_LimitsABodyWithNoDeclaredLength(t *testing.T) {
	var readErr error

	handler := Chain(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 1024)
			_, readErr = r.Body.Read(buf)
		}),
		MaxBodyBytes(8),
	)

	// io.MultiReader returns a type httptest.NewRequest does not recognise, so
	// no length is inferred; -1 is what a chunked request looks like once it
	// reaches a handler.
	body := io.MultiReader(strings.NewReader("this body is longer than eight bytes"))
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.ContentLength = -1

	handler.ServeHTTP(httptest.NewRecorder(), req)

	var maxErr *http.MaxBytesError
	if !errors.As(readErr, &maxErr) {
		t.Fatalf("expected a *http.MaxBytesError from reading an oversized body, got: %v", readErr)
	}
}
