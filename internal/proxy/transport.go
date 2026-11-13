package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

// deadlineTransport applies a per-request deadline to a round trip and releases
// it when the response body is closed.
//
// The obvious implementation — `context.WithTimeout` plus `defer cancel()` —
// is wrong here, and wrong in a way that only shows up under load. RoundTrip
// returns as soon as the response *headers* arrive; the body is still being
// read afterwards. Cancelling on return would abort every response whose body
// had not already been buffered, which for a streaming endpoint is every one
// of them.
//
// So the cancel function is handed to the body instead. http.Transport always
// returns a non-nil body for a successful round trip, and httputil.ReverseProxy
// always closes it, so the deadline is released on every path that matters.
type deadlineTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

// newDeadlineTransport wraps base, or returns base unchanged when the timeout
// is not positive. Returning base unchanged matters: a zero timeout must mean
// "no deadline", not "a deadline of zero", which would fail every request.
func newDeadlineTransport(base http.RoundTripper, timeout time.Duration) http.RoundTripper {
	if timeout <= 0 {
		return base
	}
	return &deadlineTransport{base: base, timeout: timeout}
}

// RoundTrip implements http.RoundTripper.
func (t *deadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), t.timeout)

	res, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}

	if res.Body == nil {
		// A RoundTripper that returns a nil body is violating its contract,
		// but cancelling here is the only safe thing to do with the deadline.
		cancel()
		return res, nil
	}

	res.Body = &cancelOnClose{ReadCloser: res.Body, cancel: cancel}
	return res, nil
}

// cancelOnClose releases a deadline when the body it belongs to is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// newUpstreamTransport builds the transport for one product upstream.
//
// Each upstream gets its own transport, and therefore its own connection pool.
// That is not an optimisation: sharing one would make the frontend and backend
// of a product contend for connections, and would give the error path for one
// product a live connection pool belonging to another — which is exactly the
// kind of shared state §48's "no cross-product fallback" is about.
func newUpstreamTransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		// No forward proxy, deliberately. The Gateway talks to upstreams on
		// the internal network; honouring HTTP_PROXY here would route internal
		// traffic through whatever the environment happens to name, which is
		// both surprising and a way to leak it off-host.
		Proxy: nil,

		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,

		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,

		// Bounds the wait for response headers specifically, which is the part
		// of a round trip an unresponsive upstream can stall indefinitely.
		ResponseHeaderTimeout: responseHeaderTimeout,

		// The Gateway must not negotiate content encoding on the client's
		// behalf. Left enabled, the Transport would add its own
		// "Accept-Encoding: gzip" and transparently decompress, so the product
		// would send a compressed body that the Gateway then had to re-compress
		// — or, worse, would forward decompressed bytes under the original
		// Content-Encoding. Disabled, a client that asked for gzip gets gzip
		// passed through untouched, and a client that did not gets whatever the
		// product sends.
		DisableCompression: true,
	}
}
