package security

import (
	"errors"
	"fmt"

	"github.com/zensys/gateway/internal/tenant"
)

// ErrForbidden means the caller is authenticated but may not manage the
// product the operation concerns. It maps to 403 (§69).
//
// The distinction from ErrUnauthenticated is load-bearing and not cosmetic:
// 401 means "I do not know who you are", 403 means "I know exactly who you are
// and you may not do this". A caller that gets them the wrong way round cannot
// tell a misconfigured token from a missing permission, and the two are fixed
// by different people.
var ErrForbidden = errors.New("security: caller may not manage this product")

// ServiceIdentity is an authenticated internal caller: which service it is,
// and which products it is allowed to manage.
//
// The product set is unexported so that it cannot be widened after the
// identity has been established. An identity whose permissions could be edited
// in place would be a permission check that a later line of code can undo.
type ServiceIdentity struct {
	// Service is the authenticated service's name, such as "pos".
	Service string

	// products is the configured may_manage_products set (§41).
	products []tenant.Product
}

// NewServiceIdentity builds an identity for a service that may manage the
// named products.
//
// It is variadic because the common case is one product per service, and a
// call site that reads `NewServiceIdentity("pos", tenant.ProductPOS)` says
// what it means without a slice literal in the middle of it.
func NewServiceIdentity(service string, products ...tenant.Product) ServiceIdentity {
	return ServiceIdentity{
		Service:  service,
		products: append([]tenant.Product(nil), products...),
	}
}

// ManagedProducts returns a copy of the products the caller may manage.
//
// A copy rather than the slice itself: the caller gets the answer, not a
// handle that lets it append to the identity's permission set.
func (i ServiceIdentity) ManagedProducts() []tenant.Product {
	return append([]tenant.Product(nil), i.products...)
}

// CanManage reports whether the caller may manage routes for p.
func (i ServiceIdentity) CanManage(p tenant.Product) bool {
	for _, allowed := range i.products {
		if allowed == p {
			return true
		}
	}
	return false
}

// AuthorizeProduct returns an error wrapping ErrForbidden unless the caller
// may manage p.
//
// This is the §42 check: a valid POS token must not be able to touch a GMS
// tenant. Note that it is a check on a *product*, not on a tenant — a service
// may manage every tenant of its own product and none of any other, and there
// is deliberately no per-tenant ACL to get wrong.
func (i ServiceIdentity) AuthorizeProduct(p tenant.Product) error {
	if i.CanManage(p) {
		return nil
	}
	return fmt.Errorf("%w: service %q may not manage %s", ErrForbidden, i.Service, p)
}

// AuthorizeList narrows a list query to what the caller may see, and returns
// an error wrapping ErrForbidden when the query explicitly asks for a product
// the caller does not manage.
//
// requested is the `?product=` query parameter, empty when absent.
//
// The returned filter is the caller's alone: a service that manages exactly one
// product sees only that product's routes, and does not have to remember to
// pass a filter to avoid learning about another product's tenants. A service
// that manages every product gets no restriction, which is the correct
// behaviour for a platform-level operator token and is why this is expressed
// as "narrow to what you may manage" rather than as a fixed product.
//
// A list is a read, and §42 is written about management, so this is stricter
// than the architecture doc strictly requires. It is stricter in the safe
// direction and costs nothing: no V1 caller has a reason to enumerate another
// product's tenants, and a future one that does can be given a token that
// manages both.
func (i ServiceIdentity) AuthorizeList(requested *tenant.Product, filter tenant.ListFilter) (tenant.ListFilter, error) {
	if requested != nil {
		if err := i.AuthorizeProduct(*requested); err != nil {
			return tenant.ListFilter{}, err
		}
		filter.Product = requested
		return filter, nil
	}

	if len(i.products) == 1 {
		only := i.products[0]
		filter.Product = &only
	}

	return filter, nil
}
