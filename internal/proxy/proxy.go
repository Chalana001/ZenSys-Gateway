// Package proxy forwards a resolved request to a product upstream.
//
// It is the terminal step of the public pipeline: by the time a request
// reaches here its tenant, product, and stage have been decided, and the only
// remaining question is which socket to write to. It makes no routing
// decisions of its own — the frontend/backend split is a routing rule and lives
// in internal/routing — and it holds no per-request state of any kind.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/httpapi"
	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/tenant"
)

// key identifies one upstream: a product's frontend or backend.
type key struct {
	product tenant.Product
	stage   routing.Stage
}

// route is everything needed to serve a request from one upstream.
//
// Each route owns its transport and its ReverseProxy. Nothing is shared between
// routes, which is what makes §48 structural rather than aspirational: there is
// no object in this package that holds two products' upstreams, so there is no
// place a fallback between them could be written. The error handler for a POS
// route has no reference to GMS — not a disabled code path, an absent one.
type route struct {
	product string
	stage   routing.Stage
	target  *url.URL
	policy  routing.OutboundPolicy
	reverse *httputil.ReverseProxy
}

// Proxy implements httpapi.Upstream.
type Proxy struct {
	logger       *slog.Logger
	routes       map[key]*route
	preserveHost bool
}

// New builds the proxy for every configured product and stage.
//
// It returns an error rather than panicking when an upstream URL cannot be
// parsed. config.Load already rejects those, so this is unreachable in a
// validated deployment — but "unreachable" is a claim, and the alternative to
// returning an error is a nil dereference on the first request that uses the
// route.
func New(cfg *config.Config, logger *slog.Logger) (*Proxy, error) {
	p := &Proxy{
		logger:       logger,
		routes:       make(map[key]*route, len(cfg.Products)*2),
		preserveHost: cfg.Proxy.PreserveHost,
	}

	responseHeaderTimeout := cfg.Proxy.UpstreamResponseHeaderTimeout.Std()

	for name, pc := range cfg.Products {
		product := tenant.Product(name)

		policy := routing.OutboundPolicy{
			InjectTenantHeader: pc.InjectTenantHeader,
			TenantHeaderName:   pc.TenantHeaderName,
		}

		stages := []struct {
			stage routing.Stage
			field string
			raw   string
		}{
			{routing.StageFrontend, "frontend_upstream", pc.FrontendUpstream},
			{routing.StageBackend, "backend_upstream", pc.BackendUpstream},
		}

		for _, s := range stages {
			target, err := url.Parse(s.raw)
			if err != nil {
				return nil, fmt.Errorf("products.%s.%s: %w", name, s.field, err)
			}

			rt := &route{
				product: name,
				stage:   s.stage,
				target:  target,
				policy:  policy,
			}

			// A deadline per product bounds the whole exchange, including the
			// response body. ResponseHeaderTimeout on the transport bounds only
			// the wait for headers, which an upstream can satisfy and then
			// stall forever on the body.
			transport := newDeadlineTransport(
				newUpstreamTransport(responseHeaderTimeout),
				pc.UpstreamTimeout.Std(),
			)

			rt.reverse = &httputil.ReverseProxy{
				Rewrite: func(pr *httputil.ProxyRequest) {
					p.rewrite(rt, pr)
				},
				ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
					p.handleError(rt, w, r, err)
				},
				Transport: transport,

				// Flush immediately after every write rather than waiting for
				// a timer. Go would flush automatically for a response it
				// recognises as streaming (text/event-stream, or an unknown
				// length), but that recognition is a heuristic over headers a
				// product controls. -1 makes it unconditional, which is what
				// the exit criterion "streaming responses are not buffered"
				// actually requires. The cost is extra flushes on large
				// non-streaming responses, which is bounded and irrelevant at
				// this traffic level.
				FlushInterval: -1,
			}

			p.routes[key{product: product, stage: s.stage}] = rt
		}
	}

	return p, nil
}

// Serve implements httpapi.Upstream.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, info routing.Info) {
	rt, ok := p.routes[key{product: info.Route.Product, stage: info.Stage}]
	if !ok {
		// Unreachable in a validated deployment: config.Validate requires both
		// upstreams for every configured product, and the registry's product
		// CHECK constraint restricts the set to those. It is handled anyway,
		// because the alternative is a nil dereference, and because a future
		// product added to the registry before it is added to the configuration
		// would land here.
		//
		// The response is 502 and not 404: the tenant exists and is active, so
		// the Gateway has a configuration problem, not an unknown host.
		p.logger.Error("no upstream is configured for this route",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"tenant", info.Tenant,
			"product", string(info.Route.Product),
			"stage", info.Stage.String(),
		)
		httpapi.WriteUpstreamUnavailable(w)
		return
	}

	// Rewrite reads the routing decision back out of the request context,
	// because httputil gives it no other channel and a per-route closure must
	// not hold per-request state (§31). PublicHandler is the single writer of
	// that slot; if it is missing, the upstream request would silently go out
	// without the tenant header, so say so rather than letting it happen
	// quietly.
	if _, ok := routing.InfoFrom(r.Context()); !ok {
		p.logger.Error("the routing decision is absent from the request context; the upstream request will not carry the tenant header",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"tenant", info.Tenant,
		)
	}

	rt.reverse.ServeHTTP(w, r)
}

// rewrite builds the outbound request.
//
// The order of the calls is load-bearing, and two of them are not obvious.
func (p *Proxy) rewrite(rt *route, pr *httputil.ProxyRequest) {
	// SetURL rewrites the outbound URL and clears Out.Host. Anything that sets
	// the Host header must therefore come after it, or it will be discarded.
	pr.SetURL(rt.target)

	// SetXForwarded first, then override. httputil strips the inbound
	// X-Forwarded-* headers before Rewrite runs — by design, so a handler
	// cannot accidentally trust them — which leaves SetXForwarded able to
	// describe only the connection the Gateway itself sees. Behind the real
	// edge chain that connection is plain HTTP, so on its own it would tell
	// every product the request arrived over http while the browser used https
	// (architecture doc §10.2).
	pr.SetXForwarded()

	edge, _ := routing.EdgeFrom(pr.In.Context())
	info, _ := routing.InfoFrom(pr.In.Context())
	routing.ApplyForwarded(pr.Out.Header, edge, info.Tenant, rt.policy)

	// §10.3. The product must see the hostname the client asked for, because
	// GMS resolves its tenant from the Host header (§44, §67) and because a
	// product that builds absolute URLs would otherwise emit links to an
	// internal address.
	if p.preserveHost {
		pr.Out.Host = pr.In.Host
	}
}

// handleError maps a failed round trip to a response.
//
// Only transport failures reach here. An upstream that answers with a 500, a
// 404, or anything else is passed through untouched: the Gateway does not
// reinterpret product responses (architecture doc §47).
func (p *Proxy) handleError(rt *route, w http.ResponseWriter, r *http.Request, err error) {
	info, _ := routing.InfoFrom(r.Context())

	attrs := []any{
		"request_id", httpapi.RequestIDFrom(r.Context()),
		"tenant", info.Tenant,
		"product", rt.product,
		"stage", rt.stage.String(),
		"upstream", rt.target.String(),
	}

	// The client hung up, or the server is shutting down. There is nobody left
	// to answer, and recording it as an upstream failure would attribute a
	// condition the upstream had no part in to the upstream's error rate.
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		p.logger.Info("client cancelled the request before the upstream answered", attrs...)
		return
	}

	// A body that exceeded server.max_body_bytes fails here rather than at the
	// point it was wrapped, because a chunked body declares no length and the
	// limit is only reached while the transport is reading it. It is the
	// client's request that was too large, not the product's fault, and
	// answering 502 would blame the product and invite a retry that cannot
	// succeed (plan §8.1 step 3).
	//
	// http.MaxBytesReader is the only source of this error on this path, so
	// there is nothing else a *http.MaxBytesError could mean here.
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		p.logger.Info("request body exceeded the configured limit",
			append(attrs, "limit_bytes", tooLarge.Limit)...)
		httpapi.WritePayloadTooLarge(w)
		return
	}

	if isTimeout(err) {
		p.logger.Warn("upstream did not answer in time", append(attrs, "error", err)...)
		httpapi.WriteUpstreamTimeout(w)
		return
	}

	p.logger.Error("upstream request failed", append(attrs, "error", err)...)
	httpapi.WriteUpstreamUnavailable(w)
}

// isTimeout reports whether a round-trip error was a deadline.
//
// Two shapes have to be caught: the deadlineTransport's own context expiring,
// which surfaces as context.DeadlineExceeded, and the transport's
// ResponseHeaderTimeout, which surfaces as a net.Error with Timeout() true.
// Checking only one of them would map half the timeouts to 502.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Routes describes the configured upstreams, sorted, for a startup log line
// and for tests.
//
// It returns strings rather than routes on purpose: a caller cannot reach a
// route through it, and therefore cannot serve traffic from one without going
// through Serve.
func (p *Proxy) Routes() []string {
	out := make([]string, 0, len(p.routes))
	for k, rt := range p.routes {
		out = append(out, fmt.Sprintf("%s/%s -> %s", k.product, k.stage, rt.target))
	}
	sort.Strings(out)
	return out
}
