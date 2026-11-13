package security

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zensys/gateway/internal/tenant"
)

// testTokens are long enough to look like real ones; length is not enforced
// here (config.Load enforces it), but a test that used "a" and "b" would not
// exercise anything about how tokens are compared.
const (
	posToken = "pos-0123456789abcdef0123456789abcdef"
	gmsToken = "gms-fedcba9876543210fedcba9876543210"
)

func testAuthenticator(t *testing.T) Authenticator {
	t.Helper()

	auth, err := NewTokenAuthenticator([]TokenSpec{
		{Service: "pos", Token: posToken, Products: []tenant.Product{tenant.ProductPOS}},
		{Service: "gms", Token: gmsToken, Products: []tenant.Product{tenant.ProductGMS}},
	})
	if err != nil {
		t.Fatalf("NewTokenAuthenticator: %v", err)
	}
	return auth
}

func requestWithAuthorization(values ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/internal/v1/tenant-routes/reserve", nil)
	for _, v := range values {
		r.Header.Add("Authorization", v)
	}
	return r
}

// TestNewTokenAuthenticator_RejectsMalformedSpecs covers the fail-fast rules.
// Each of these would otherwise produce a process that starts and then
// misbehaves at request time, which is the failure mode the whole
// configuration layer exists to prevent.
func TestNewTokenAuthenticator_RejectsMalformedSpecs(t *testing.T) {
	tests := []struct {
		name    string
		specs   []TokenSpec
		wantSub string
	}{
		{
			name:    "no tokens at all",
			specs:   nil,
			wantSub: "at least one service token",
		},
		{
			name:    "empty service name",
			specs:   []TokenSpec{{Service: "", Token: posToken, Products: []tenant.Product{tenant.ProductPOS}}},
			wantSub: "service is required",
		},
		{
			name:    "empty token",
			specs:   []TokenSpec{{Service: "pos", Token: "", Products: []tenant.Product{tenant.ProductPOS}}},
			wantSub: "empty token",
		},
		{
			name:    "no products",
			specs:   []TokenSpec{{Service: "pos", Token: posToken}},
			wantSub: "may manage no products",
		},
		{
			name:    "unknown product",
			specs:   []TokenSpec{{Service: "pos", Token: posToken, Products: []tenant.Product{"ERP"}}},
			wantSub: "not a configured product",
		},
		{
			name: "two services share a token",
			specs: []TokenSpec{
				{Service: "pos", Token: posToken, Products: []tenant.Product{tenant.ProductPOS}},
				{Service: "gms", Token: posToken, Products: []tenant.Product{tenant.ProductGMS}},
			},
			wantSub: "same token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth, err := NewTokenAuthenticator(tt.specs)
			if err == nil {
				t.Fatalf("expected an error containing %q, got a working authenticator", tt.wantSub)
			}
			if auth != nil {
				t.Error("expected a nil authenticator alongside the error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

// TestTokenAuthenticator_Identify covers the authentication outcomes.
//
// Every rejection produces the same ErrUnauthenticated. The distinction is for
// the log, and the table records it here so a future change that starts
// returning a different sentinel per case is visible.
func TestTokenAuthenticator_Identify(t *testing.T) {
	auth := testAuthenticator(t)

	tests := []struct {
		name        string
		headers     []string
		wantService string
		wantErr     bool
	}{
		{name: "pos token", headers: []string{"Bearer " + posToken}, wantService: "pos"},
		{name: "gms token", headers: []string{"Bearer " + gmsToken}, wantService: "gms"},

		// The scheme is case-insensitive (RFC 9110 §11.1), and a client that
		// sends it in another case is not making an authentication mistake.
		{name: "lowercase scheme", headers: []string{"bearer " + posToken}, wantService: "pos"},
		{name: "uppercase scheme", headers: []string{"BEARER " + posToken}, wantService: "pos"},

		// Whitespace around the token is hygiene, not a semantic change.
		{name: "surrounding whitespace", headers: []string{"Bearer   " + posToken + "  "}, wantService: "pos"},

		{name: "no header", headers: nil, wantErr: true},
		{name: "empty header", headers: []string{""}, wantErr: true},
		{name: "wrong scheme", headers: []string{"Basic " + posToken}, wantErr: true},
		{name: "scheme with no token", headers: []string{"Bearer "}, wantErr: true},
		{name: "scheme and spaces only", headers: []string{"Bearer    "}, wantErr: true},
		{name: "token with no scheme", headers: []string{posToken}, wantErr: true},
		{name: "unknown token", headers: []string{"Bearer " + "not-a-configured-token-0000000000000"}, wantErr: true},

		// A prefix of a real token must not authenticate. This is the case a
		// length-insensitive comparison that ignored trailing bytes would get
		// wrong.
		{name: "prefix of a real token", headers: []string{"Bearer " + posToken[:len(posToken)-1]}, wantErr: true},

		// A superset of a real token, for the same reason in the other
		// direction.
		{name: "real token plus a suffix", headers: []string{"Bearer " + posToken + "x"}, wantErr: true},

		// Two Authorization headers are refused rather than resolved. `Get`
		// would return the first and silently drop the second, so the Gateway
		// and any other reader of the same request could disagree about which
		// token was presented.
		{
			name:    "two authorization headers",
			headers: []string{"Bearer " + posToken, "Bearer " + gmsToken},
			wantErr: true,
		},
		{
			name:    "two headers, one valid",
			headers: []string{"Bearer " + posToken, "Bearer nope"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity, err := auth.Identify(requestWithAuthorization(tt.headers...))

			if tt.wantErr {
				if !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("expected ErrUnauthenticated, got %v", err)
				}
				if identity.Service != "" {
					t.Errorf("expected no identity alongside the error, got %q", identity.Service)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if identity.Service != tt.wantService {
				t.Errorf("service: got %q, want %q", identity.Service, tt.wantService)
			}
		})
	}
}

// TestTokenAuthenticator_DoesNotShortCircuitOnMatch is the behavioural proof
// that Identify compares against *every* configured token.
//
// NewTokenAuthenticator refuses duplicate tokens, so this state is unreachable
// in production — which is exactly what makes it usable as evidence. The index
// is selected arithmetically from every comparison's result, so a duplicate
// makes the *last* matching entry win. An implementation that returned as soon
// as it found a match would report the first, and the test would fail.
//
// Without this, "the comparison is constant-time" would be a claim about the
// comparison function only, and the loop around it could still leak the
// position of a token through an early return.
func TestTokenAuthenticator_DoesNotShortCircuitOnMatch(t *testing.T) {
	digest := sha256.Sum256([]byte(posToken))

	auth := &tokenAuth{entries: []tokenEntry{
		{service: "first", products: []tenant.Product{tenant.ProductPOS}, digest: digest},
		{service: "second", products: []tenant.Product{tenant.ProductGMS}, digest: digest},
	}}

	identity, err := auth.Identify(requestWithAuthorization("Bearer " + posToken))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if identity.Service != "second" {
		t.Fatalf("got service %q; an implementation that returned on the first match would report %q, "+
			"so this failure means Identify stopped comparing before the end of the token list",
			identity.Service, "first")
	}

	// The identity must also carry the *matching entry's* products, not the
	// first entry's. A loop that found the right index but built the identity
	// from the wrong entry would grant GMS access to a POS service.
	if !identity.CanManage(tenant.ProductGMS) || identity.CanManage(tenant.ProductPOS) {
		t.Errorf("identity carries the wrong product set: %v", identity.ManagedProducts())
	}
}

// TestTokenAuthenticator_IdentityIsNotShareableState proves that the identity
// returned to a handler owns its product set.
//
// If Identify returned a slice into the authenticator's own entry, a handler
// that appended to it would widen the permissions of every later request.
func TestTokenAuthenticator_IdentityIsNotShareableState(t *testing.T) {
	auth := testAuthenticator(t)

	first, err := auth.Identify(requestWithAuthorization("Bearer " + posToken))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Mutate what the caller was given.
	managed := first.ManagedProducts()
	if len(managed) != 1 {
		t.Fatalf("expected one managed product, got %v", managed)
	}
	managed[0] = tenant.ProductGMS

	second, err := auth.Identify(requestWithAuthorization("Bearer " + posToken))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if second.CanManage(tenant.ProductGMS) {
		t.Error("a caller was able to widen its own permissions by mutating the slice it was given")
	}
	if !second.CanManage(tenant.ProductPOS) {
		t.Error("the pos identity lost its own product")
	}
}

// TestServiceIdentity_AuthorizeProduct covers the §42 rule.
func TestServiceIdentity_AuthorizeProduct(t *testing.T) {
	pos := NewServiceIdentity("pos", tenant.ProductPOS)
	both := NewServiceIdentity("platform", tenant.ProductPOS, tenant.ProductGMS)

	tests := []struct {
		name     string
		identity ServiceIdentity
		product  tenant.Product
		wantErr  bool
	}{
		{name: "pos manages pos", identity: pos, product: tenant.ProductPOS},
		{name: "pos does not manage gms", identity: pos, product: tenant.ProductGMS, wantErr: true},
		{name: "platform manages pos", identity: both, product: tenant.ProductPOS},
		{name: "platform manages gms", identity: both, product: tenant.ProductGMS},
		{name: "empty identity manages nothing", identity: ServiceIdentity{}, product: tenant.ProductPOS, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.identity.AuthorizeProduct(tt.product)
			if tt.wantErr {
				if !errors.Is(err, ErrForbidden) {
					t.Fatalf("expected ErrForbidden, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestServiceIdentity_AuthorizeList covers the narrowing rule.
func TestServiceIdentity_AuthorizeList(t *testing.T) {
	pos := NewServiceIdentity("pos", tenant.ProductPOS)
	both := NewServiceIdentity("platform", tenant.ProductPOS, tenant.ProductGMS)
	gms := tenant.ProductGMS

	t.Run("a single-product caller is narrowed to its own product", func(t *testing.T) {
		filter, err := pos.AuthorizeList(nil, tenant.ListFilter{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filter.Product == nil {
			t.Fatal("expected the filter to be narrowed to POS, got no product filter")
		}
		if *filter.Product != tenant.ProductPOS {
			t.Errorf("got product filter %q, want POS", *filter.Product)
		}
	})

	t.Run("asking for a product it does not manage is forbidden", func(t *testing.T) {
		if _, err := pos.AuthorizeList(&gms, tenant.ListFilter{}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("expected ErrForbidden, got %v", err)
		}
	})

	t.Run("asking for its own product is allowed", func(t *testing.T) {
		own := tenant.ProductPOS
		filter, err := pos.AuthorizeList(&own, tenant.ListFilter{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filter.Product == nil || *filter.Product != tenant.ProductPOS {
			t.Errorf("got %v, want POS", filter.Product)
		}
	})

	t.Run("a multi-product caller is not narrowed", func(t *testing.T) {
		filter, err := both.AuthorizeList(nil, tenant.ListFilter{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filter.Product != nil {
			t.Errorf("expected no product restriction, got %q", *filter.Product)
		}
	})

	t.Run("narrowing preserves the other filter fields", func(t *testing.T) {
		status := tenant.StatusActive
		in := tenant.ListFilter{After: "mangala", Limit: 7, Status: &status}
		out, err := pos.AuthorizeList(nil, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.After != "mangala" || out.Limit != 7 || out.Status == nil || *out.Status != tenant.StatusActive {
			t.Errorf("narrowing changed unrelated filter fields: %+v", out)
		}
	})
}

// TestServiceIdentity_ManagedProductsIsACopy proves the accessor does not hand
// out a handle to the permission set.
func TestServiceIdentity_ManagedProductsIsACopy(t *testing.T) {
	identity := NewServiceIdentity("pos", tenant.ProductPOS)

	managed := identity.ManagedProducts()
	managed = append(managed, tenant.ProductGMS)
	managed[0] = tenant.ProductGMS

	if identity.CanManage(tenant.ProductGMS) {
		t.Error("appending to the returned slice widened the identity's permissions")
	}
	if !identity.CanManage(tenant.ProductPOS) {
		t.Error("mutating the returned slice changed the identity's own permissions")
	}
}
