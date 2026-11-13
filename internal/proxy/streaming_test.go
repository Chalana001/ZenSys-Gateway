package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/httpapi"
)

// TestProxy_StandardChainExposesFlush pins the one property of the middleware
// chain that a streaming response depends on.
//
// httputil.ReverseProxy flushes through http.NewResponseController, which
// reaches Flush by unwrapping the ResponseWriter. AccessLog and Recover both
// wrap the writer in statusRecorder, so if that type ever stopped exposing
// Unwrap the controller would find no Flusher, the flush would become a silent
// no-op, and every streamed response would buffer until the handler returned.
// That failure looks like a slow product, not like a bug in a wrapper, which
// is why it is asserted here rather than left to the streaming test to
// diagnose.
func TestProxy_StandardChainExposesFlush(t *testing.T) {
	var (
		attempted bool
		flushErr  error
	)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempted = true
		flushErr = http.NewResponseController(w).Flush()
	})

	cfg := config.Default()
	server := httptest.NewServer(httpapi.StandardChain(inner, discardLogger(), httpapi.ChainOptions{
		RequestIDHeader: cfg.Proxy.RequestIDHeader,
		TrustedProxies:  trustedLoopback(t),
		MaxBodyBytes:    cfg.Server.MaxBodyBytes,
	}))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if !attempted {
		t.Fatal("the probe handler never ran")
	}
	if flushErr != nil {
		t.Errorf("a handler inside the standard chain cannot flush: %v\n"+
			"the streaming exit criterion depends on this; check that statusRecorder still exposes Unwrap", flushErr)
	}
}

// TestProxy_StreamingResponseIsNotBuffered is M5's streaming exit criterion.
//
// The response is deliberately NOT text/event-stream and DOES carry a
// Content-Length. Both choices exist so the test cannot pass for a reason
// other than the one under test: httputil.ReverseProxy already flushes
// immediately for an event-stream content type and for a response of unknown
// length, so either would prove only that Go's heuristic still exists. A plain
// text/plain response of declared length is delivered incrementally only
// because FlushInterval is -1 — which is the setting this test is here to pin.
//
// The proof is timing, and the timing is the assertion: the first half of the
// body must arrive while the upstream is still holding the response open. If
// the Gateway buffered, the client would see nothing until the handler
// returned.
func TestProxy_StreamingResponseIsNotBuffered(t *testing.T) {
	f := newFixture(t)

	const (
		first  = "first half of the body\n"
		second = "second half of the body\n"
	)

	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	f.posFrontend.setRespond(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(first)+len(second)))
		w.WriteHeader(http.StatusOK)

		_, _ = io.WriteString(w, first)
		_ = http.NewResponseController(w).Flush()

		// Hold the response open. Nothing else reaches the client until this
		// returns, so anything the client has received by then was flushed on
		// the way through.
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		_, _ = io.WriteString(w, second)
		_ = http.NewResponseController(w).Flush()
	})

	server := httptest.NewServer(f.handler)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	req.Host = "mangala.zensys.lk"

	// The request runs in a goroutine with its own deadline, because both ways
	// this can fail are silent otherwise. If the response is buffered, nothing
	// is written until the upstream handler returns — so Do blocks until the
	// Gateway's upstream deadline expires and then reports a bare EOF, which
	// names neither the cause nor the fix.
	type doResult struct {
		resp *http.Response
		err  error
	}
	doDone := make(chan doResult, 1)

	go func() {
		resp, err := http.DefaultClient.Do(req)
		doDone <- doResult{resp: resp, err: err}
	}()

	const buffered = "the Gateway buffered the response instead of flushing it " +
		"(FlushInterval must be -1, and the response writer must stay flushable)"

	var resp *http.Response

	select {
	case res := <-doDone:
		if res.err != nil {
			t.Fatalf("request: %v\n%s: the connection closed with no response, because nothing was written until the upstream deadline expired", res.err, buffered)
		}
		resp = res.resp

	case <-time.After(3 * time.Second):
		t.Fatalf("no response headers arrived within 3s\n%s", buffered)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// One reader for the whole body: a second bufio.Reader would start from
	// the socket and silently drop whatever the first one had already buffered.
	br := bufio.NewReader(resp.Body)

	got, err := readExactlyWithin(t, br, len(first), 3*time.Second)
	if err != nil {
		t.Fatalf("%v\n"+
			"the first %d bytes never arrived while the upstream was still holding the response open, "+
			"so the Gateway buffered them (FlushInterval must be -1, and the response writer must stay flushable)",
			err, len(first))
	}
	if got != first {
		t.Fatalf("received %q, want %q", got, first)
	}

	// Only now let the upstream finish.
	unblock()

	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read the remainder of the body: %v", err)
	}
	if string(rest) != second {
		t.Errorf("the remainder of the body is %q, want %q", rest, second)
	}
}

// readExactlyWithin reads exactly n bytes, failing if they do not all arrive
// within d.
//
// The read runs in a goroutine because the failure this guards against is a
// read that never completes. A blocking read would turn that into a test
// timeout with no explanation, which is exactly the diagnostic the test above
// is trying to produce.
func readExactlyWithin(t *testing.T, r io.Reader, n int, d time.Duration) (string, error) {
	t.Helper()

	type result struct {
		buf []byte
		err error
	}

	done := make(chan result, 1)

	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(r, buf)
		done <- result{buf: buf, err: err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			return string(res.buf), fmt.Errorf("read %d of %d bytes: %w", len(res.buf), n, res.err)
		}
		return string(res.buf), nil

	case <-time.After(d):
		return "", fmt.Errorf("timed out after %s waiting for %d bytes", d, n)
	}
}
