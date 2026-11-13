package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/security"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first listed runs outermost.
//
// Chain(h, RequestID, Recover) means a request passes through RequestID, then
// Recover, then reaches h. Ordering is load-bearing: Recover must sit inside
// RequestID so that a recovered panic can still be logged with its request ID.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// ChainOptions describes what the standard middleware chain needs to know.
type ChainOptions struct {
	// RequestIDHeader is proxy.request_id_header.
	RequestIDHeader string

	// TrustedProxies is the parsed proxy.trusted_proxies list. A nil value
	// trusts nothing, which is the correct default rather than an oversight.
	TrustedProxies *routing.TrustedProxies

	// MaxBodyBytes is server.max_body_bytes.
	MaxBodyBytes int64

	// MaxHeaderBytes is server.max_header_bytes, the policy limit on the
	// request's header block. See HeaderSizeLimit for how it relates to the
	// hard cap net/http enforces.
	MaxHeaderBytes int

	// Tier1 is the address-keyed rate limiter for the public listener.
	//
	// A nil limiter — or a disabled one — means the listener is not rate
	// limited, which is what the internal listener passes: its callers are
	// authenticated platform services on a loopback address, and §8.1 places
	// the limiter in the public pipeline. The internal listener therefore has
	// no rate limiting by design and not by omission; if it ever becomes
	// reachable beyond loopback (§40 forbids that) this is the first thing to
	// revisit.
	Tier1 *security.Limiter

	// OperationalPaths are the endpoints that exist for the platform's own
	// operation: liveness, readiness, and metrics.
	//
	// They are one concept with two consequences, which is why they are one
	// list rather than two. They are logged at debug level unless they return
	// a server error, because they are polled continuously and would otherwise
	// drown out real traffic; and they are exempt from the rate limiter,
	// because a liveness probe refused for being too frequent reports the
	// service as dead, and an orchestrator that restarts on a failed probe
	// turns a load spike into an outage. Both consequences follow from the
	// same fact — these endpoints are the platform talking to itself — so a
	// second list would be a second place to keep that fact correct.
	OperationalPaths []string
}

// StandardChain builds the middleware chain both listeners use.
//
// It exists as one function rather than as a list at each call site so that
// tests exercise the same order the process runs. That matters more than usual
// here, because the order is the security policy:
//
//   - RequestState installs the slots the per-request decisions are recorded
//     in, and it must stay outermost. The slots are installed by deriving the
//     context, and a derived context only travels inward, so any middleware
//     listed before it would read a slot from a context that does not have one
//     and would see no decision at all. Add new middleware after it.
//   - TrustedProxy decides what the connection is and strips the headers the
//     Gateway will not carry forward. It must run before RequestID, which
//     reads that decision, and before anything reads the request — including
//     the rate limiter, which keys on the address it resolves.
//   - RequestID must sit outside AccessLog so every log line carries an ID,
//     and inside TrustedProxy so an inbound ID is only honoured when it
//     arrived through a trusted proxy.
//   - AccessLog sits outside Recover so a recovered panic is still recorded
//     as a 500 rather than vanishing because the handler never returned, and
//     outside both limits so a 413 or a 429 is logged like any other response.
//   - MaxBodyBytes sits outside Tier1RateLimit, which is the §8.1 step 3
//     before step 4 ordering. See the note below.
//   - HeaderSizeLimit sits outside MaxBodyBytes, so the three size and abuse
//     checks run cheapest first: a comparison over the parsed header map, then
//     a comparison against a declared Content-Length, then a stateful token
//     bucket. The first two hold no state, so a request they refuse does not
//     spend an allowance — and the plan's step 3 before step 4 ordering is
//     preserved unchanged.
func StandardChain(h http.Handler, logger *slog.Logger, opts ChainOptions) http.Handler {
	return Chain(
		h,
		RequestState(),
		TrustedProxy(opts.TrustedProxies, opts.RequestIDHeader),
		RequestID(opts.RequestIDHeader),
		AccessLog(logger, opts.OperationalPaths...),
		Recover(logger),
		HeaderSizeLimit(opts.MaxHeaderBytes),
		MaxBodyBytes(opts.MaxBodyBytes),
		Tier1RateLimit(opts.Tier1, opts.OperationalPaths...),
	)
}

// The body limit runs before the rate limiter, and the reason is worth stating
// because the opposite order looks equally reasonable.
//
// §8.1 numbers the body limit as step 3 and tier-1 rate limiting as step 4, so
// a request that violates both is told 413 rather than 429. The two checks are
// not equivalent in cost: the body limit's early rejection is a comparison
// against a declared Content-Length and holds no state, while the rate limiter
// consults a bucket and spends a token. A request that cannot be accepted on
// its own terms should therefore be refused on its own terms, before any
// stateful check is consulted.
//
// What the ordering does *not* affect is whether a body is read. MaxBodyBytes
// only replaces the request's reader; it reads nothing itself, and the limiter
// listed after it still runs before the handler that would read the body. So
// for a chunked request — which declares no length and cannot be refused up
// front — the limiter still refuses before a single body byte is consumed, in
// either order.
//
// The one consequence to know: a client that is both over its limit and over
// the body limit never sees a 429 for that request, so a client that reacts
// only to 429 will not back off. It will get a 413, which is a permanent
// condition it must fix rather than retry, so the missing back-off signal
// costs nothing.

// Tier1RateLimit applies the coarse, address-keyed rate limit (§8.1 step 4).
//
// The key is the address routing.Edge resolved, not r.RemoteAddr. Behind the
// real edge chain every request arrives from the Nginx hop, so keying on the
// socket address would put every client on the internet into one bucket: the
// first busy one would exhaust it and everyone else would be refused. ResolveEdge
// has already applied the trusted-proxy policy to work out which address the
// Gateway can actually vouch for, and that is the only defensible key.
//
// When no Edge is present — which means the chain omitted TrustedProxy — the
// connection address is used directly. That is the same value the trusted-proxy
// policy would have produced for an untrusted request, so the limiter keeps
// limiting rather than either exempting the request or putting every request
// into one shared bucket. A missing middleware must not become a way to bypass
// the limit, and it must not become a self-inflicted outage either.
//
// Operational paths are exempt; see ChainOptions.OperationalPaths.
func Tier1RateLimit(limiter *security.Limiter, exempt ...string) Middleware {
	if !limiter.Enabled() {
		// A no-op rather than a wrapper that checks and falls through: the
		// disabled case should cost nothing at all.
		return func(next http.Handler) http.Handler { return next }
	}

	skip := make(map[string]bool, len(exempt))
	for _, p := range exempt {
		skip[p] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}

			key := routing.ClientAddr(r.RemoteAddr)
			if edge, ok := routing.EdgeFrom(r.Context()); ok {
				key = edge.ClientIP
			}

			if decision := limiter.AllowIP(key); !decision.Allowed {
				WriteRateLimited(w, decision.RetryAfterSeconds())
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequestState installs the per-request slots that the request's own handler
// fills in.
//
// There are two, and they exist for the same reason. The routing pipeline
// records its decision in one, and the internal API's authentication wrapper
// records the calling service in the other. Both are read by consumers on both
// sides of the handler that writes them: the proxy reads the routing decision
// inward and the access log reads it outward; a lifecycle handler reads the
// identity inward and the access log reads it outward. A context value written
// by the handler cannot travel outward, so the slots are created here and
// filled in there (see routing.State and identitySlot).
//
// This is why the middleware must stay outermost. A slot is reachable only
// through the request context, and a derived context travels inward, so
// anything listed before this one would install nothing and see nothing.
func RequestState() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := routing.WithState(r.Context(), &routing.State{})
			ctx = withIdentitySlot(ctx, &identitySlot{})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// TrustedProxy establishes the connection facts for a request and applies the
// inbound half of the header contract.
//
// Both jobs belong to one middleware because they are one decision: whether
// the request arrived through a proxy the operator named. Splitting them would
// mean making that decision twice, in two places, and eventually disagreeing.
func TrustedProxy(tp *routing.TrustedProxies, requestIDHeader string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			edge := routing.ResolveEdge(r, tp)
			routing.SanitizeInbound(r.Header, edge, requestIDHeader)
			next.ServeHTTP(w, r.WithContext(routing.WithEdge(r.Context(), edge)))
		})
	}
}

type ctxKey int

const requestIDKey ctxKey = 0

// RequestIDFrom returns the request ID carried by ctx, or "" if absent.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// newRequestID returns a 128-bit random identifier as lowercase hex.
//
// crypto/rand is used rather than a counter or timestamp because request IDs
// are forwarded to products and appear in their logs; a guessable ID would let
// one tenant's activity be correlated with another's by anyone who can read
// either log stream.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read does not fail on any platform the Gateway
		// supports. If it somehow does, fall back to a timestamp rather than
		// leaving a request without an identifier at all.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// maxRequestIDLength bounds an inbound request ID the Gateway will adopt.
const maxRequestIDLength = 128

// validRequestID returns id if it is safe to adopt, and "" otherwise.
//
// The accepted character set is what identifiers are actually made of: hex,
// UUIDs, and W3C traceparent-derived values. Anything else is refused. The
// value is copied into every log line and every upstream request, so a
// whitespace or a comma in it would let it be read as two values by a
// downstream consumer that splits on the separator, and a newline would be an
// attempt at log injection.
func validRequestID(id string) string {
	if id == "" || len(id) > maxRequestIDLength {
		return ""
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':', c == '+':
		default:
			return ""
		}
	}
	return id
}

// RequestID ensures every request carries an identifier.
//
// An inbound value is adopted only when the request arrived through a trusted
// proxy (architecture doc §43, §50, plan §15.3). Otherwise a fresh 128-bit ID
// is generated: a client-supplied ID would otherwise be copied into the
// Gateway's logs and the products', letting anyone forge or collide entries
// across both.
//
// The trust check is deliberately independent of TrustedProxy's sanitising.
// TrustedProxy has already deleted the header for an untrusted request, so
// this check is redundant in the standard chain — and that is the point. If
// the trusted-proxy middleware is ever missing or reordered, EdgeFrom reports
// false, every ID is generated, and the failure is a lost correlation ID
// rather than a forged one.
func RequestID(header string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := ""
			if edge, ok := routing.EdgeFrom(r.Context()); ok && edge.Trusted {
				id = validRequestID(r.Header.Get(header))
			}
			if id == "" {
				id = newRequestID()
			}

			// Canonicalise in place. The outbound request is built from this
			// header map, so writing the accepted value back means the
			// forwarded value and the echoed value cannot disagree — and a
			// trusted proxy that sent several values is left with one.
			r.Header.Set(header, id)
			w.Header().Set(header, id)

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
		})
	}
}

// Recover converts a panic into a 500 without leaking the panic value or a
// stack trace to the client.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}

				// http.ErrAbortHandler is the documented sentinel for
				// aborting a handler deliberately. Re-panic so net/http
				// handles it and does not log a spurious stack trace.
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}

				logger.Error("panic recovered",
					"request_id", RequestIDFrom(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"panic", recovered,
					"stack", string(debug.Stack()),
				)

				// If the handler already started writing, the status line is
				// committed and this becomes a no-op. There is nothing
				// useful left to do except log, which we have.
				if !rec.wrote {
					WriteInternalError(rec)
				}
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

// MaxBodyBytes caps the request body.
//
// A declared length over the limit is refused here, before the request is
// routed and before any upstream connection is opened. There is no reason to
// resolve a tenant or dial a product for a request that cannot be accepted,
// and the plan's pipeline puts the body limit ahead of both (§8.1 step 3).
//
// A body with no declared length — a chunked upload — cannot be checked up
// front, so the limit is also enforced on the reader. That failure surfaces
// during the round trip rather than here, and the proxy maps it to the same
// 413 (see internal/proxy).
//
// Handlers that read a body must still detect *http.MaxBytesError and respond
// 413; this middleware only enforces the limit.
func MaxBodyBytes(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limit > 0 && r.ContentLength > limit {
				WritePayloadTooLarge(w)
				return
			}

			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// maxLoggedPathLength bounds the path written to the access log.
//
// The path is the one field in a log line whose length the client chooses, and
// it is bounded only by the header-block limit — which is 64 KiB by default.
// Without this, one client sending 64 KiB paths at a few hundred requests per
// second produces megabytes per second of log, and the requests are refused by
// the size checks before the rate limiter is reached, so nothing throttles it.
//
// The truncation is marked rather than silent: a reader who sees "…" knows the
// path was longer, and a log line that quietly shows a shorter path than the
// request carried would be worse than no line at all — it would look like the
// request itself was short.
const maxLoggedPathLength = 512

// truncatePath returns path, shortened to maxLoggedPathLength with a marker.
//
// The marker is ASCII "..." rather than a Unicode ellipsis so that the value
// survives any log pipeline that is not careful about encoding, and so that a
// reader grepping for it does not need to type a multi-byte character.
func truncatePath(path string) string {
	if len(path) <= maxLoggedPathLength {
		return path
	}
	return path[:maxLoggedPathLength] + "..."
}

// HeaderSizeLimit caps the request's header block.
//
// net/http enforces a limit of its own, and it is not this one.
// http.Server reads up to MaxHeaderBytes + 4096 bytes before refusing —
// net/http's own source calls the extra 4096 "bufio slop" — and it answers a
// bare text 431 written straight to the connection, before any handler,
// middleware, or access-log entry exists. So a client can exceed
// max_header_bytes by up to 4096 bytes and be refused in a way the Gateway
// neither shapes nor records.
//
// This middleware closes that band: it measures the decoded header block and
// answers in the error contract, which means the refusal also reaches the
// access log.
//
// Two limits must be stated precisely, because they are not the same thing:
//
//   - max_header_bytes is the policy limit, enforced here.
//   - max_header_bytes + 4096 is the hard cap, enforced by net/http. Above it
//     the refusal is net/http's and the Gateway logs nothing at all. This is
//     not fixable from inside the process: net/http must read the block before
//     anything can judge it too large, so there is no point at which Gateway
//     code could observe that request, let alone respond to it.
//
// The measurement approximates the wire size — the request line plus each
// field as name, value, and separators — because the decoded form is what a
// handler can see. The request line is included because net/http counts it in
// the same budget: an oversized URI is refused as an oversized header block,
// not as a 414, which is a deviation worth knowing when reading a 431 from
// this listener.
func HeaderSizeLimit(limit int) Middleware {
	if limit <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if headerBlockSize(r) > limit {
				WriteHeadersTooLarge(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// headerBlockSize approximates the size of a request's header block in bytes.
func headerBlockSize(r *http.Request) int {
	requestURI := r.RequestURI
	if requestURI == "" {
		// A client-side request has no RequestURI; a server-side one always
		// does. Falling back keeps the function total rather than returning a
		// number that is wrong by the whole request line.
		requestURI = r.URL.RequestURI()
	}

	// "METHOD SP request-target SP HTTP-version CRLF".
	size := len(r.Method) + 1 + len(requestURI) + 1 + len(r.Proto) + 2

	// "name: value CRLF" per field, repeated per value.
	for name, values := range r.Header {
		for _, value := range values {
			size += len(name) + 2 + len(value) + 2
		}
	}
	return size
}

// AccessLog emits one structured line per request.
//
// Only the URL *path* is logged, never the raw URL: query strings routinely
// carry tokens and reset codes, and a log is a poor place to discover that.
// The path is also truncated to maxLoggedPathLength, because unlike every
// other field in the line its length is chosen by the client.
//
// The routing fields are present only when the request reached a decision.
// A rejected host, a reserved host, or a registry outage has no tenant to
// report, and emitting an empty one would make "not routed" indistinguishable
// from "routed to a tenant with an empty slug".
func AccessLog(logger *slog.Logger, quietPaths ...string) Middleware {
	quiet := make(map[string]bool, len(quietPaths))
	for _, p := range quietPaths {
		quiet[p] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			// Server errors escalate regardless of path. Health and metrics
			// endpoints are polled continuously so they are demoted to debug
			// to avoid drowning out real traffic — but a health endpoint
			// returning 500 is exactly the thing an operator needs to see, so
			// the status check takes precedence over the quiet-path rule.
			level := slog.LevelInfo
			switch {
			case rec.status >= http.StatusInternalServerError:
				level = slog.LevelWarn
			case quiet[r.URL.Path]:
				level = slog.LevelDebug
			}

			attrs := make([]slog.Attr, 0, 9)
			attrs = append(attrs,
				slog.String("request_id", RequestIDFrom(r.Context())),
			)

			if info, ok := routing.InfoFrom(r.Context()); ok {
				attrs = append(attrs,
					slog.String("host", info.Host),
					slog.String("tenant", info.Tenant),
					slog.String("product", string(info.Product())),
				)
			}

			// Internal lifecycle calls have no routing decision — they are not
			// addressed to a tenant — so the actor is what identifies them.
			// Without this an internal mutation logs as an anonymous request
			// with no way to tell which service made it (§51).
			if identity, ok := IdentityFrom(r.Context()); ok {
				attrs = append(attrs, slog.String("service", identity.Service))
			}

			attrs = append(attrs,
				slog.String("method", r.Method),
				slog.String("path", truncatePath(r.URL.Path)),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)

			logger.LogAttrs(r.Context(), level, "request", attrs...)
		})
	}
}

// statusRecorder captures the status code and byte count for logging, and
// records whether the response has been committed.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap exposes the underlying ResponseWriter so that
// http.ResponseController can reach Flush, Hijack, and deadlines.
//
// Without this, streaming responses (server-sent events, long-poll) would
// buffer until the handler returned, because the wrapper would hide the
// Flusher implementation.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
