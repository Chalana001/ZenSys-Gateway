// Package security authenticates and authorizes callers of the internal
// lifecycle API.
//
// The Gateway has two kinds of caller and they are authenticated in completely
// different ways, which is why this package exists separately from anything
// public:
//
//   - Public traffic is anonymous. The Gateway never authenticates a product
//     user and never issues or validates a product JWT (architecture doc §26,
//     §93). A public request is authorised by the hostname it arrives on, and
//     nothing else.
//   - Internal lifecycle traffic is a *service* call. It comes from POS or GMS
//     provisioning jobs over the private listener, and the caller is a service
//     with a name and a set of products it may manage (§39, §41, §42).
//
// Nothing here is used on the public listener. A public request carries no
// service identity, and the code path that would read one does not exist.
//
// The package is split in two files along the line the architecture doc draws:
// serviceauth.go establishes *who* is calling, authz.go decides *what they may
// do*. Authentication can be swapped for mTLS without touching authorization;
// authorization can gain a rule without touching either transport.
package security

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/zensys/gateway/internal/tenant"
)

// ErrUnauthenticated means the request carried no token the Gateway
// recognises. It maps to 401 (§69).
//
// It is deliberately a single value with no cause attached at the point of
// return: which of "no header", "wrong scheme", and "unrecognised token"
// happened is recorded in the log, and telling the caller would let an
// unauthenticated prober enumerate tokens a character at a time.
var ErrUnauthenticated = errors.New("security: caller is not authenticated")

// Authenticator identifies the service that sent a request.
//
// It is an interface with one method so that the transport used to establish
// the identity can be replaced without touching a handler or a single
// authorization check. `tokenAuth` implements it for V1; an mTLS implementation
// can be added alongside it in V1.1. That seam is what "mTLS-ready" means
// concretely — not a stub, just a boundary that is already load-bearing.
type Authenticator interface {
	// Identify returns the calling service, or an error wrapping
	// ErrUnauthenticated.
	Identify(r *http.Request) (ServiceIdentity, error)
}

// TokenSpec is one configured service token, already resolved from its
// environment variable.
//
// It is deliberately not config.TokenConfig: this package should not depend on
// the configuration model, and the two are free to diverge. The mapping
// happens once, in the wiring layer.
type TokenSpec struct {
	// Service is the caller's name, such as "pos".
	Service string

	// Token is the resolved secret. It is never logged, never returned, and
	// never compared with ==.
	Token string

	// Products is the may_manage_products set.
	Products []tenant.Product
}

// tokenEntry is a TokenSpec reduced to what a comparison needs.
type tokenEntry struct {
	service  string
	products []tenant.Product

	// digest is sha256(token). Comparing digests rather than tokens gives
	// every comparison the same length, so the comparison is constant-time
	// with respect to the *contents and the length* of both values.
	//
	// subtle.ConstantTimeCompare returns 0 immediately when the lengths
	// differ, which is correct but leaks the length of the stored token to
	// anyone who can measure it. Hashing first removes that leak as well as
	// keeping the raw secret out of the long-lived process heap in a form a
	// core dump would expose directly.
	digest [sha256.Size]byte
}

// tokenAuth authenticates a bearer token against a fixed set of configured
// service tokens.
type tokenAuth struct {
	entries []tokenEntry
}

// NewTokenAuthenticator builds a bearer-token authenticator.
//
// It returns an error rather than panicking on a malformed spec, because the
// specs come from configuration and a configuration mistake should stop the
// process at startup with a message naming the problem — which is the same
// fail-fast rule the rest of the configuration follows.
func NewTokenAuthenticator(specs []TokenSpec) (Authenticator, error) {
	if len(specs) == 0 {
		return nil, errors.New("security: at least one service token is required; the lifecycle API would otherwise be unauthenticated")
	}

	entries := make([]tokenEntry, 0, len(specs))
	seen := make(map[[sha256.Size]byte]string, len(specs))

	for i, spec := range specs {
		label := spec.Service
		if label == "" {
			label = fmt.Sprintf("internal_auth.tokens[%d]", i)
		}

		if spec.Service == "" {
			return nil, fmt.Errorf("%s: service is required", label)
		}
		if spec.Token == "" {
			return nil, fmt.Errorf("security: service %q has an empty token", label)
		}
		if len(spec.Products) == 0 {
			return nil, fmt.Errorf("security: service %q may manage no products; a token that authorises nothing should not be configured", label)
		}
		for _, p := range spec.Products {
			if !p.Valid() {
				return nil, fmt.Errorf("security: service %q lists %q, which is not a configured product", label, p)
			}
		}

		digest := sha256.Sum256([]byte(spec.Token))

		// Two services sharing a token is not a permission escalation — both
		// would still be limited to their own products — but it makes the
		// audit trail ambiguous, because an event's actor would depend on the
		// order the tokens were configured in. Refusing is cheaper than
		// discovering that during an incident.
		if previous, dup := seen[digest]; dup {
			return nil, fmt.Errorf("security: services %q and %q are configured with the same token", previous, spec.Service)
		}
		seen[digest] = spec.Service

		entries = append(entries, tokenEntry{
			service:  spec.Service,
			products: append([]tenant.Product(nil), spec.Products...),
			digest:   digest,
		})
	}

	return &tokenAuth{entries: entries}, nil
}

// Identify implements Authenticator.
func (a *tokenAuth) Identify(r *http.Request) (ServiceIdentity, error) {
	presented, ok := bearerToken(r)
	if !ok {
		return ServiceIdentity{}, ErrUnauthenticated
	}

	digest := sha256.Sum256([]byte(presented))

	// Every configured token is compared, and the loop is never left early.
	//
	// An early `return` on the first match would make the time taken to answer
	// depend on how many entries preceded the one that matched, which is a
	// remote timing oracle over the *position* of a token. The selection of
	// the matching index is therefore arithmetic rather than an `if`, so no
	// branch depends on whether a given entry matched either.
	//
	// The arithmetic form means the *last* match wins. Duplicate tokens are
	// rejected at construction, so in production there is at most one match
	// and the distinction is unobservable; a test constructs a duplicate
	// deliberately and asserts the last entry is the one reported, which is
	// the only available proof that the loop really did run to completion
	// instead of returning on the first hit.
	idx, found := 0, 0
	for i := range a.entries {
		eq := subtle.ConstantTimeCompare(digest[:], a.entries[i].digest[:])
		idx = eq*(i-idx) + idx
		found |= eq
	}

	if found == 0 {
		return ServiceIdentity{}, ErrUnauthenticated
	}

	entry := a.entries[idx]
	return ServiceIdentity{
		Service:  entry.service,
		products: append([]tenant.Product(nil), entry.products...),
	}, nil
}

// bearerPrefix is the authentication scheme the internal API accepts.
const bearerPrefix = "Bearer "

// bearerToken extracts the bearer token from a request, reporting whether one
// was present in a form the Gateway will even look at.
//
// Every rejection here is a 401 with no distinction in the response, so the
// reasons are enumerated for the log and for the tests rather than for the
// caller.
func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")

	// Exactly one Authorization header. `Header.Get` returns the first and
	// silently discards the rest, so a request carrying two would be
	// authenticated against one of them while a downstream reader — a log
	// shipper, a proxy, a WAF — might act on the other. There is no legitimate
	// reason to send two, so the ambiguity is refused rather than resolved.
	if len(values) != 1 {
		return "", false
	}

	header := values[0]

	// The scheme is case-insensitive (RFC 9110 §11.1) and is not a secret, so
	// EqualFold is correct here — this is the one comparison in the file that
	// does not need to be constant-time, and the token itself is not involved
	// in it.
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}

	// Trimmed as input hygiene, the same way a tenant slug is. A client that
	// sends "Bearer  <token>" has a bug in its own header construction, and
	// rejecting the request would report it as an authentication failure,
	// which sends the reader looking at the token rather than at the space.
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" {
		return "", false
	}

	return token, true
}
