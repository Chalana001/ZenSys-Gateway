package tenant

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Service applies the tenant lifecycle rules on top of a Repository.
//
// The Repository knows how to store routes; the Service knows what changes are
// legal, in what order, and what to record about them. Keeping the two apart
// means the state machine can be tested exhaustively without a database.
//
// The Service reads through the Repository and never through the route cache.
// A lifecycle decision must be made on the truth, not on a copy that may be up
// to route_ttl old; the cache exists to keep the *public* path fast, and the
// two consumers deliberately have different consistency needs.
type Service struct {
	repo        Repository
	logger      *slog.Logger
	invalidator Invalidator
}

// NewService builds a lifecycle service.
func NewService(repo Repository, logger *slog.Logger) *Service {
	return &Service{repo: repo, logger: logger}
}

// WithInvalidator returns a copy of the Service that drops the cached route
// after every successful mutation.
//
// It returns a copy rather than mutating in place so that a Service cannot be
// reconfigured while requests are in flight. A nil invalidator is accepted and
// means "no cache to invalidate", which is the right behaviour for tests and
// for a deployment running without a cache.
//
// Without this hook a lifecycle change would take up to route_ttl to become
// visible on the public path — so a newly activated tenant would keep returning
// 404 for a minute, and a disabled one would keep serving (§33).
func (s *Service) WithInvalidator(inv Invalidator) *Service {
	clone := *s
	clone.invalidator = inv
	return &clone
}

// invalidate drops the cached route for slug, if there is a cache.
//
// It is called only after a mutation has committed. Invalidating before the
// write would leave a window in which another request repopulates the cache
// from the pre-mutation row.
func (s *Service) invalidate(slug string) {
	if s.invalidator == nil {
		return
	}
	s.invalidator.Invalidate(slug)
}

// trimSlug removes surrounding whitespace without altering case.
//
// Trimming is input hygiene. Lowercasing would be a semantic change, and it is
// deliberately not done here: a product that sends "Mangala" has a bug in its
// own slug handling, and silently storing "mangala" would leave the product
// and the Gateway disagreeing about what the tenant is called. Case is
// rejected, not corrected.
//
// The host resolver takes the opposite approach for hostnames, and correctly
// so — DNS hostnames are case-insensitive, so MANGALA.zensys.lk and
// mangala.zensys.lk really are the same host. It lowercases before calling in.
func trimSlug(slug string) string {
	return strings.TrimSpace(slug)
}

// Get returns the route for a slug.
//
// The slug is validated for *format* only. A reserved slug that somehow
// reached the registry must remain readable and releasable; rejecting it here
// would make it impossible to clean up.
func (s *Service) Get(ctx context.Context, slug string) (Route, error) {
	slug = trimSlug(slug)
	if err := ValidateSlugFormat(slug); err != nil {
		return Route{}, err
	}
	return s.repo.GetBySlug(ctx, slug)
}

// List returns routes matching the filter.
func (s *Service) List(ctx context.Context, filter ListFilter) ([]Route, error) {
	return s.repo.List(ctx, filter)
}

// Reserve claims a slug for a product, creating the route in PROVISIONING.
//
// Reserving does not make a tenant routable. The product must still provision
// its own tenant and then request activation (§34, §62).
func (s *Service) Reserve(ctx context.Context, slug string, product Product, actor Actor) (Route, error) {
	slug = trimSlug(slug)

	// Full validation, including the reserved-host check: this is the one
	// operation that can turn a name into a tenant.
	if err := ValidateSlug(slug); err != nil {
		return Route{}, err
	}
	if !product.Valid() {
		return Route{}, fmt.Errorf("%w: %q", ErrInvalidProduct, product)
	}

	route := Route{
		TenantSlug: slug,
		Product:    product,
		Status:     StatusProvisioning,
	}

	event := Event{
		TenantSlug: slug,
		Action:     ActionReserve,
		To:         StatusProvisioning,
		Product:    product,
		Actor:      actor,
	}

	stored, err := s.repo.Reserve(ctx, route, event)
	if err != nil {
		return Route{}, err
	}

	// Invalidate even though the slug is new. A negative cache entry may
	// already exist for it: anyone probing {slug}.zensys.lk before the
	// reservation would have cached the absence, and without this the tenant
	// would keep returning 404 for up to negative_ttl after being reserved.
	s.invalidate(slug)

	return stored, nil
}

// Activate makes a route serve public traffic.
//
// product, when non-nil, is the product the caller believes owns the slug. It
// is checked against the stored value so that a product cannot activate a
// tenant belonging to another product by guessing a slug (§42).
//
// Activating an already-ACTIVE route is a successful no-op. A provisioning job
// that retries after a lost response should not have to special-case that.
func (s *Service) Activate(ctx context.Context, slug string, product *Product, actor Actor) (Route, error) {
	slug = trimSlug(slug)
	if err := ValidateSlugFormat(slug); err != nil {
		return Route{}, err
	}

	current, err := s.repo.GetBySlug(ctx, slug)
	if err != nil {
		return Route{}, err
	}

	if product != nil && *product != current.Product {
		return Route{}, fmt.Errorf("%w: %q belongs to %s, not %s",
			ErrConflict, slug, current.Product, *product)
	}

	if current.Status == StatusActive {
		return current, nil
	}

	if !CanTransition(current.Status, StatusActive) {
		return Route{}, fmt.Errorf("%w: %q cannot move from %s to %s",
			ErrInvalidTransition, slug, current.Status, StatusActive)
	}

	event := Event{
		TenantSlug: slug,
		Action:     ActionActivate,
		From:       current.Status,
		To:         StatusActive,
		Product:    current.Product,
		Actor:      actor,
	}

	stored, err := s.repo.Transition(ctx, slug, current.Status, StatusActive, event)
	if err != nil {
		return Route{}, err
	}

	s.invalidate(slug)
	return stored, nil
}

// Disable stops a route from reaching its product.
//
// There is no product parameter: the product is read from the registry and
// authorization is checked against that. Taking the product from the caller
// would invite a caller to name a product that does not match the stored
// route, which is a whole class of bug removed by construction.
func (s *Service) Disable(ctx context.Context, slug string, actor Actor) (Route, error) {
	slug = trimSlug(slug)
	if err := ValidateSlugFormat(slug); err != nil {
		return Route{}, err
	}

	current, err := s.repo.GetBySlug(ctx, slug)
	if err != nil {
		return Route{}, err
	}

	if current.Status == StatusDisabled {
		return current, nil
	}

	if !CanTransition(current.Status, StatusDisabled) {
		return Route{}, fmt.Errorf("%w: %q cannot move from %s to %s",
			ErrInvalidTransition, slug, current.Status, StatusDisabled)
	}

	event := Event{
		TenantSlug: slug,
		Action:     ActionDisable,
		From:       current.Status,
		To:         StatusDisabled,
		Product:    current.Product,
		Actor:      actor,
	}

	stored, err := s.repo.Transition(ctx, slug, current.Status, StatusDisabled, event)
	if err != nil {
		return Route{}, err
	}

	// Disabling must take effect on the next public request, not a TTL later.
	// Until this lands, a disabled tenant keeps being proxied to its product.
	s.invalidate(slug)

	return stored, nil
}

// Release deletes a route outright.
//
// This is the one destructive operation in the lifecycle, and it is
// deliberately protected at the API layer by a confirmation field (§38). The
// service records the release as an event, because the routing row itself is
// about to stop existing.
func (s *Service) Release(ctx context.Context, slug string, actor Actor) (Route, error) {
	slug = trimSlug(slug)
	if err := ValidateSlugFormat(slug); err != nil {
		return Route{}, err
	}

	current, err := s.repo.GetBySlug(ctx, slug)
	if err != nil {
		return Route{}, err
	}

	// Releasing an ACTIVE tenant takes a live site off the internet. That is
	// sometimes exactly what is wanted, so it is allowed — but it is never
	// silent.
	if current.Status == StatusActive {
		s.logger.Warn("releasing an ACTIVE tenant route; this will stop routing immediately",
			"tenant", slug,
			"product", current.Product.String(),
			"actor_service", actor.Service,
			"request_id", actor.RequestID,
		)
	}

	event := Event{
		TenantSlug: slug,
		Action:     ActionRelease,
		From:       current.Status,
		Product:    current.Product,
		Actor:      actor,
	}

	if err := s.repo.Release(ctx, slug, event); err != nil {
		return Route{}, err
	}

	// The row is gone, so any cached copy of it is now wrong in the worst
	// direction: it would keep routing traffic to a tenant that has been
	// released.
	s.invalidate(slug)

	return current, nil
}

// SlugPolicy describes the platform slug rules, for exposure to products so
// they validate identically rather than reimplementing the rules and drifting.
type SlugPolicy struct {
	Pattern       string   `json:"pattern"`
	MinLength     int      `json:"min_length"`
	MaxLength     int      `json:"max_length"`
	ReservedHosts []string `json:"reserved_hosts"`
}

// Policy returns the authoritative slug and reserved-host policy.
func Policy() SlugPolicy {
	return SlugPolicy{
		Pattern:       SlugPattern,
		MinLength:     SlugMinLength,
		MaxLength:     SlugMaxLength,
		ReservedHosts: ReservedHosts(),
	}
}

// CheckSlug reports whether a candidate slug is acceptable, returning the
// reason when it is not. It is the same validation Reserve applies, exposed so
// a product can pre-validate before starting provisioning.
func CheckSlug(slug string) error {
	return ValidateSlug(strings.TrimSpace(slug))
}
