package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHeaderSizeLimit_RefusesAnOversizedBlockInTheErrorContract is the reason
// HeaderSizeLimit exists.
//
// net/http refuses an oversized header block itself, but it does so by writing
// a bare text 431 straight to the connection before any middleware runs, so
// the response is outside the error contract and nothing records it. This
// middleware catches the band between the configured limit and net/http's hard
// cap and answers in the contract instead.
func TestHeaderSizeLimit_RefusesAnOversizedBlockInTheErrorContract(t *testing.T) {
	const limit = 1024

	reached := 0
	handler := Chain(okHandler(&reached), HeaderSizeLimit(limit))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Big", strings.Repeat("A", limit*2))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d, want 431", rec.Code)
	}
	if reached != 0 {
		t.Error("the request reached the handler")
	}
	if body := errorBody(t, rec); body.Error != ErrHeadersTooLarge {
		t.Errorf("error code = %q, want %q", body.Error, ErrHeadersTooLarge)
	}

	// The message must not name the limit: a caller who can read it can send
	// requests just under it.
	if body := rec.Body.String(); strings.Contains(body, "1024") {
		t.Errorf("the response names the limit:\n%s", body)
	}
}

func TestHeaderSizeLimit_AllowsABlockUnderTheLimit(t *testing.T) {
	const limit = 1024

	reached := 0
	handler := Chain(okHandler(&reached), HeaderSizeLimit(limit))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Small", strings.Repeat("A", 100))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if reached != 1 {
		t.Error("the request did not reach the handler")
	}
}

// TestHeaderSizeLimit_CountsTheRequestLine pins that the URI shares the
// header block's budget.
//
// This mirrors what net/http does — it reads the request line as part of the
// header block and answers an over-long URI with 431 rather than 414 — so a
// URI is bounded even though there is no separate URI limit. The test asserts
// the behaviour rather than the mechanism, so a future change to net/http's
// accounting would show up here.
func TestHeaderSizeLimit_CountsTheRequestLine(t *testing.T) {
	const limit = 1024

	reached := 0
	handler := Chain(okHandler(&reached), HeaderSizeLimit(limit))

	req := httptest.NewRequest(http.MethodGet, "/"+strings.Repeat("p", limit*2), nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d, want 431: an over-long request line must be refused, since it is part of the same header block budget", rec.Code)
	}
	if reached != 0 {
		t.Error("the request reached the handler")
	}
}

func TestHeaderSizeLimit_ZeroDisablesTheCheck(t *testing.T) {
	// Consistent with MaxBodyBytes: a non-positive limit means "no policy
	// limit", and net/http's hard cap is still in force underneath. It does
	// not mean "refuse everything".
	reached := 0
	handler := Chain(okHandler(&reached), HeaderSizeLimit(0))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Big", strings.Repeat("A", 8192))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; a zero limit must disable the check, not fail it closed", rec.Code)
	}
}

// TestHeaderSizeLimit_TheRefusalReachesTheAccessLog is the observable half of
// the reason this middleware exists.
//
// A refusal net/http produces never reaches the access log, so an operator
// sees a client's oversized headers only as a connection that produced no
// line. Running the check inside the chain means the 431 is recorded like any
// other response.
func TestHeaderSizeLimit_TheRefusalReachesTheAccessLog(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	handler := StandardChain(okHandler(new(int)), logger, ChainOptions{
		RequestIDHeader: testRequestIDHeader,
		MaxHeaderBytes:  1024,
	})

	req := httptest.NewRequest(http.MethodGet, "/oversized", nil)
	req.Header.Set("X-Big", strings.Repeat("A", 4096))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d, want 431", rec.Code)
	}

	out := logs.String()
	if !strings.Contains(out, `"status":431`) {
		t.Errorf("the 431 was not logged; this is exactly the gap the middleware closes:\n%s", out)
	}
	if !strings.Contains(out, "/oversized") {
		t.Errorf("the access log line does not name the path:\n%s", out)
	}
}

// TestStandardChain_HeaderRefusalDoesNotSpendARateLimitToken extends the
// stateless-before-stateful rule to the third check.
//
// HeaderSizeLimit, MaxBodyBytes, and Tier1RateLimit run in that order, so the
// two checks that hold no state refuse before the one that does. A request
// refused for its headers therefore leaves the bucket untouched.
func TestStandardChain_HeaderRefusalDoesNotSpendARateLimitToken(t *testing.T) {
	limiter := testLimiter(t, exhaustedSpec(1), exhaustedSpec(1))

	reached := 0
	handler := StandardChain(okHandler(&reached), discardLogger(), ChainOptions{
		RequestIDHeader: testRequestIDHeader,
		MaxHeaderBytes:  1024,
		Tier1:           limiter,
	})

	big := httptest.NewRequest(http.MethodGet, "/", nil)
	big.RemoteAddr = "203.0.113.9:1234"
	big.Header.Set("X-Big", strings.Repeat("A", 4096))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, big)

	if rec.Code != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d, want 431", rec.Code)
	}

	// The bucket is untouched, so this must be served.
	if rec := doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.9:1234"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the 431 spent the rate-limit token, so the header check is running inside the limiter", rec.Code)
	}
	// And the token was really there to spend.
	if rec := doFrom(t, handler, "mangala.zensys.lk", "/", "203.0.113.9:1234"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429: the bucket's single token was not spent by the served request", rec.Code)
	}
}

// TestAccessLog_TruncatesAClientChosenPath closes the log-amplification hole.
//
// Every other field in a log line has a length the Gateway chooses. The path
// is the one the client chooses, and it is bounded only by the header-block
// limit, so a client sending 64 KiB paths could otherwise write megabytes per
// second to the log — and those requests are refused by the size checks, which
// run before the rate limiter, so nothing would throttle them.
func TestAccessLog_TruncatesAClientChosenPath(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	handler := Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		AccessLog(logger),
	)

	long := "/" + strings.Repeat("p", 8192)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, long, nil))

	out := logs.String()
	if strings.Contains(out, strings.Repeat("p", 8192)) {
		t.Fatalf("the full client-chosen path reached the log; a client can size its own log line:\n%.200s", out)
	}
	// The limit counts the leading slash, so the retained prefix is the first
	// maxLoggedPathLength bytes of the path and the marker follows it.
	if !strings.Contains(out, "/"+strings.Repeat("p", maxLoggedPathLength-1)+"...") {
		t.Errorf("the truncated path is not marked, so a reader cannot tell the request was longer than the line shows:\n%.600s", out)
	}
}

func TestTruncatePath(t *testing.T) {
	short := "/api/orders"
	if got := truncatePath(short); got != short {
		t.Errorf("truncatePath(%q) = %q, want it unchanged", short, got)
	}

	exact := "/" + strings.Repeat("x", maxLoggedPathLength-1)
	if got := truncatePath(exact); got != exact {
		t.Errorf("truncatePath of a path exactly at the limit changed it; the boundary must be inclusive")
	}

	over := "/" + strings.Repeat("x", maxLoggedPathLength)
	got := truncatePath(over)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncatePath of an over-long path = %q, want a truncation marker", got)
	}
	if len(got) != maxLoggedPathLength+3 {
		t.Errorf("truncatePath produced %d bytes, want %d", len(got), maxLoggedPathLength+3)
	}
}

func TestHeaderBlockSize_CountsTheRequestLineAndEveryField(t *testing.T) {
	// Two fields, one of them repeated, plus a known request line.
	req := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
	req.Header.Set("X-One", "value")
	req.Header.Add("X-Multi", "a")
	req.Header.Add("X-Multi", "bb")

	// "GET" + 1 + "/api/orders" + 1 + "HTTP/1.1" + 2
	want := 3 + 1 + len("/api/orders") + 1 + 8 + 2
	// "X-One" + ": " + "value" + CRLF
	want += len("X-One") + 2 + len("value") + 2
	// "X-Multi" twice, once per value.
	want += (len("X-Multi") + 2 + 1 + 2) + (len("X-Multi") + 2 + 2 + 2)

	if got := headerBlockSize(req); got != want {
		t.Errorf("headerBlockSize = %d, want %d", got, want)
	}
}

func TestHeaderBlockSize_FallsBackToTheURLWhenRequestURIIsAbsent(t *testing.T) {
	// A client-side request has no RequestURI. The fallback keeps the
	// measurement total rather than wrong by the whole request line.
	req := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
	req.RequestURI = ""

	withURI := httptest.NewRequest(http.MethodGet, "/api/orders", nil)

	if got, want := headerBlockSize(req), headerBlockSize(withURI); got != want {
		t.Errorf("headerBlockSize without a RequestURI = %d, with one = %d; the fallback must produce the same measurement", got, want)
	}
}
