package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/httpapi"
)

// bodyLimit is small enough that a test can exceed it without generating
// megabytes of data, and large enough that the fixtures' own requests pass.
const bodyLimit = 1024

// TestProxy_OversizedBodyIsRejectedBeforeItReachesAProduct is the plan's
// §8.1 step 3 and its error table: a body over server.max_body_bytes is a
// 413 payload_too_large.
//
// It is a M5 concern rather than an M4 one because until M5 nothing on the
// public listener read a request body at all — the terminal step answered 503
// without looking at one. The body path, and therefore the body limit, became
// observable at the moment the proxy started forwarding.
//
// The upstream assertion matters as much as the status: a 413 that had already
// opened a connection and pushed part of the body into a product would be
// answering the right question while doing the wrong thing.
func TestProxy_OversizedBodyIsRejectedBeforeItReachesAProduct(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.Server.MaxBodyBytes = bodyLimit })

	oversized := bytes.Repeat([]byte("a"), bodyLimit*4)

	req := httptest.NewRequest(http.MethodPost, "/submit", bytes.NewReader(oversized))
	req.Host = "mangala.zensys.lk"

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec.Body.Bytes()); body.Error != httpapi.ErrPayloadTooLarge {
		t.Errorf("error code = %q, want %q", body.Error, httpapi.ErrPayloadTooLarge)
	}
	if got := f.posFrontend.count(); got != 0 {
		t.Errorf("the POS frontend received %d requests; an oversized body must be refused before any upstream is touched", got)
	}
}

// TestProxy_BodyWithinTheLimitReachesTheProduct is the converse, and it is
// what keeps the test above from passing for the wrong reason: a Gateway that
// rejected every POST would satisfy it.
func TestProxy_BodyWithinTheLimitReachesTheProduct(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.Server.MaxBodyBytes = bodyLimit })

	const payload = "hello-from-the-test"

	req := httptest.NewRequest(http.MethodPost, "/submit", bytes.NewReader([]byte(payload)))
	req.Host = "mangala.zensys.lk"

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := f.posFrontend.last(t).body; got != payload {
		t.Errorf("the upstream received body %q, want %q", got, payload)
	}
}

// TestProxy_ChunkedBodyOverTheLimitIsA413 covers the case a Content-Length
// check cannot catch.
//
// A chunked body has no declared length, so the limit is only discovered while
// the body is being read — which happens inside the transport, during the
// round trip. The resulting failure is a transport error, and mapping it to
// the same 502 as an unreachable product would tell the client to retry a
// request that can never succeed.
func TestProxy_ChunkedBodyOverTheLimitIsA413(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.Server.MaxBodyBytes = bodyLimit })

	server := httptest.NewServer(f.handler)
	t.Cleanup(server.Close)

	// io.LimitReader returns a type http.NewRequest does not recognise, so
	// ContentLength is left unset and the client sends the body chunked. A
	// *bytes.Reader here would declare a length and take the other path.
	body := io.LimitReader(io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("a"), bodyLimit*4))), int64(bodyLimit*4))

	req, err := http.NewRequest(http.MethodPost, server.URL+"/submit", body)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	req.Host = "mangala.zensys.lk"

	if req.ContentLength != 0 {
		t.Fatalf("ContentLength = %d; the request was not sent chunked, so this test is not testing what it says",
			req.ContentLength)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 413; body = %s", resp.StatusCode, raw)
	}
}
