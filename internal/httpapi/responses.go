// Package httpapi holds the Gateway's HTTP handlers, middleware, and response
// contract.
package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// ErrorCode is the machine-readable identifier in an error response.
//
// The set is closed and deliberately small. Clients branch on these values, so
// adding one is an API change (architecture doc §69).
type ErrorCode string

// Error codes.
const (
	ErrUnknownTenant       ErrorCode = "unknown_tenant"
	ErrDisabledTenant      ErrorCode = "disabled_tenant"
	ErrRegistryUnavailable ErrorCode = "registry_unavailable"
	ErrUpstreamUnavailable ErrorCode = "upstream_unavailable"
	ErrRateLimited         ErrorCode = "rate_limited"
	ErrPayloadTooLarge     ErrorCode = "payload_too_large"
	ErrHeadersTooLarge     ErrorCode = "headers_too_large"
	ErrBadRequest          ErrorCode = "bad_request"
	ErrUnauthorized        ErrorCode = "unauthorized"
	ErrForbidden           ErrorCode = "forbidden"
	ErrConflict            ErrorCode = "conflict"
	ErrNotFound            ErrorCode = "not_found"
	ErrMethodNotAllowed    ErrorCode = "method_not_allowed"
	ErrInternal            ErrorCode = "internal_error"
)

// ErrorResponse is the uniform error body returned by both listeners.
//
// Messages are platform-level and intentionally generic: no database errors,
// no connection strings, no internal addresses, no stack traces
// (architecture doc §69).
type ErrorResponse struct {
	Error   ErrorCode `json:"error"`
	Message string    `json:"message"`
}

// msgUnknownHost is the message used for every unresolvable public hostname.
//
// It is a constant because two different internal conditions must produce a
// byte-identical response. See WriteTenantDisabled.
const msgUnknownHost = "The requested host is not registered."

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Marshalling our own response types cannot fail; if it somehow does,
		// fall back to a hand-written body rather than emitting a partial one.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal_error","message":"An internal error occurred."}`))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// WriteError writes a standard error response.
func WriteError(w http.ResponseWriter, status int, code ErrorCode, message string) {
	WriteJSON(w, status, ErrorResponse{Error: code, Message: message})
}

// WriteTenantNotFound responds 404 for a hostname with no active route.
//
// This covers an unknown tenant, a reserved host, a malformed hostname, and a
// multi-label host. They are indistinguishable to the caller by design: there
// is no default tenant and no legacy fallback (architecture doc §45).
func WriteTenantNotFound(w http.ResponseWriter) {
	WriteError(w, http.StatusNotFound, ErrUnknownTenant, msgUnknownHost)
}

// WriteTenantDisabled responds for a tenant whose route is not ACTIVE.
//
// The response is intentionally byte-identical to WriteTenantNotFound.
// Distinguishing them would confirm to an anonymous prober that a hostname
// belongs to a real business, which is precisely what the 404 choice exists to
// prevent (ADR 0001 decision 5). The distinction is preserved in logs and
// metrics, where it is useful and not attacker-visible.
func WriteTenantDisabled(w http.ResponseWriter) {
	WriteError(w, http.StatusNotFound, ErrUnknownTenant, msgUnknownHost)
}

// WriteRegistryUnavailable responds 503 when the registry cannot answer for an
// uncached route. The Gateway must not guess (architecture doc §46).
func WriteRegistryUnavailable(w http.ResponseWriter) {
	WriteError(w, http.StatusServiceUnavailable, ErrRegistryUnavailable,
		"The service is temporarily unavailable.")
}

// WriteUpstreamUnavailable responds when a product upstream cannot be reached.
//
// This never falls through to another product (architecture doc §47, §48).
func WriteUpstreamUnavailable(w http.ResponseWriter) {
	WriteError(w, http.StatusBadGateway, ErrUpstreamUnavailable,
		"The service is temporarily unavailable.")
}

// WriteUpstreamTimeout responds when a product upstream did not answer in time.
//
// It is a 503 rather than the more precise 504 because §47 documents 502/503
// and this follows the documented contract. Retry-After tells a client that
// the condition is temporary, which a bare 503 does not.
func WriteUpstreamTimeout(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	WriteError(w, http.StatusServiceUnavailable, ErrUpstreamUnavailable,
		"The service is temporarily unavailable.")
}

// WritePayloadTooLarge responds 413 when a request body exceeds
// server.max_body_bytes (plan §8.1 step 3, §15.4).
//
// The code exists so that a client which sent too much is told exactly that.
// Answering 502 instead would blame the product for the client's request and
// invite a retry that can never succeed.
func WritePayloadTooLarge(w http.ResponseWriter) {
	WriteError(w, http.StatusRequestEntityTooLarge, ErrPayloadTooLarge,
		"The request body is too large.")
}

// WriteHeadersTooLarge responds 431 when a request's header block exceeds
// server.max_header_bytes (§81 request size limits).
//
// 431 is the correct status rather than the 413 used for bodies: the two are
// different conditions and a client fixes them differently. Like
// method_not_allowed, this code is an addition to the architecture doc's §69
// table, which lists no status for an oversized header block. The doc's set
// covers what a *request* can be wrong about, and this is a distinct,
// standard HTTP condition.
//
// The message names no limit. A caller that can read the threshold can send
// requests just under it, which is the same reason the rate limiter's response
// does not name its limit.
func WriteHeadersTooLarge(w http.ResponseWriter) {
	WriteError(w, http.StatusRequestHeaderFieldsTooLarge, ErrHeadersTooLarge,
		"The request headers are too large.")
}

// WriteRateLimited responds 429 when a request exceeds its rate limit
// (§81, plan §8.2).
//
// Retry-After carries the limiter's own estimate of when the bucket refills,
// rather than a fixed constant. A client that honours the header then retries
// when it will actually succeed; a constant would be either too short, so the
// client retries into another refusal, or too long, so the client waits for
// nothing.
//
// The value is floored at one second here as well as in the limiter, because
// the two must not be able to disagree: a 429 with "Retry-After: 0" is an
// instruction to retry immediately, which is the opposite of what a rate limit
// is for. Setting it in one place and trusting the caller is how that value
// eventually reaches a client.
//
// Retry-After is the only hint given. The response deliberately does not say
// which tier refused or what the limit is: a caller that can read the limit
// can time its traffic to sit just under it.
//
// The tier is not lost, though. It is recoverable from the access log without
// a field of its own, because the two tiers sit on opposite sides of tenant
// resolution: a 429 whose log line carries a `tenant` is tier 2, and one that
// does not is tier 1, refused before the hostname was even parsed. That is
// structural rather than conventional — tier 1 runs in the middleware chain
// and tier 2 runs inside the routing handler — so it cannot drift.
//
// It rests on one line, though: PublicHandler.ServeHTTP calls routing.SetInfo
// before its tier-2 check rather than after it. Move that call below the check
// and a tier-2 refusal becomes an anonymous 429 that no log query can separate
// from a tier-1 refusal. TestTier2RateLimit_RefusalIsDistinguishableInTheLog
// pins it.
func WriteRateLimited(w http.ResponseWriter, retryAfterSeconds int) {
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	WriteError(w, http.StatusTooManyRequests, ErrRateLimited,
		"Too many requests. Please retry later.")
}

// WriteInternalError responds 500 without disclosing the cause.
func WriteInternalError(w http.ResponseWriter) {
	WriteError(w, http.StatusInternalServerError, ErrInternal,
		"An internal error occurred.")
}

// The internal lifecycle API's responses.
//
// These are separate helpers from the public ones even where the status code
// matches, because the *messages* are governed by a different rule. On the
// public listener a message must reveal nothing, so every unresolvable
// hostname shares one string. On the internal listener the caller is an
// authenticated platform service that supplied the value being complained
// about, and a message that says which field is wrong is worth more than one
// that hides it. What is never disclosed on either listener is an internal
// cause: no driver error, no DSN, no address, no stack trace (§69).

// WriteBadRequest responds 400 for a malformed or incomplete request.
//
// The message is written by the caller, which is responsible for making it
// describe the *request* and not the Gateway's internals.
func WriteBadRequest(w http.ResponseWriter, message string) {
	WriteError(w, http.StatusBadRequest, ErrBadRequest, message)
}

// WriteUnauthorized responds 401 when no service token was recognised.
//
// One message covers a missing header, a wrong scheme, and an unknown token.
// Distinguishing them would let an unauthenticated prober enumerate tokens a
// character at a time; the distinction is in the log instead.
func WriteUnauthorized(w http.ResponseWriter) {
	WriteError(w, http.StatusUnauthorized, ErrUnauthorized,
		"A valid service token is required.")
}

// WriteForbidden responds 403 when the caller is authenticated but may not
// manage the product the request concerns (§42).
func WriteForbidden(w http.ResponseWriter) {
	WriteError(w, http.StatusForbidden, ErrForbidden,
		"The service token is not permitted to manage this product.")
}

// WriteConflict responds 409 for a request that is well-formed but conflicts
// with stored state: a slug already reserved, a product mismatch, or an
// illegal lifecycle transition.
func WriteConflict(w http.ResponseWriter, message string) {
	WriteError(w, http.StatusConflict, ErrConflict, message)
}

// WriteRouteNotFound responds 404 when a tenant route does not exist.
//
// Unlike the public listener's 404, this one is distinguishable from a
// disabled tenant: the caller is authenticated, so confirming that a route
// exists is not a disclosure it is not entitled to.
func WriteRouteNotFound(w http.ResponseWriter) {
	WriteError(w, http.StatusNotFound, ErrNotFound, "No such tenant route.")
}

// WriteMethodNotAllowed responds 405 when the path names a real endpoint but
// the method is wrong.
//
// The caller is expected to set the Allow header first. A 404 here would be
// actively misleading — it says the endpoint does not exist when it does and
// the client used the wrong verb — and `Allow` is the standard way to say
// which verb is right.
//
// This code is an addition to the architecture doc's §69 table, which does not
// list it. The reason is that the doc's set covers what a *request* can be
// wrong about, and a wrong method is a distinct, standard HTTP condition that
// the doc's 404 would report incorrectly. It is documented as a deviation.
func WriteMethodNotAllowed(w http.ResponseWriter) {
	WriteError(w, http.StatusMethodNotAllowed, ErrMethodNotAllowed,
		"The method is not allowed for this endpoint.")
}
