package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/security"
	"github.com/zensys/gateway/internal/tenant"
)

// Upstream is the terminal step of the public routing pipeline: it forwards a
// request whose tenant, product, and stage have already been decided.
//
// It takes the whole decision rather than the route alone so that there is
// exactly one place where it is computed. The same value is recorded in the
// request context for the access log, and it is passed by value here rather
// than looked up there, so the two cannot disagree.
type Upstream interface {
	Serve(w http.ResponseWriter, r *http.Request, info routing.Info)
}

// PublicHandler implements the public listener's routing pipeline.
//
// It is the only place that decides what a public hostname means. The listener
// itself is a host-based catch-all, so there is no per-host mux and no way for
// a route to exist that this handler did not authorise.
type PublicHandler struct {
	rootDomain string
	logger     *slog.Logger
	resolver   *routing.Resolver
	selector   *routing.Selector
	apex       *ApexHandler
	upstream   Upstream

	// tenantLimiter is tier 2 (plan §8.2), keyed by the tenant slug. It is nil
	// until WithTenantRateLimit is called, and a nil limiter allows everything.
	tenantLimiter *security.Limiter

	// extraReserved holds platform.reserved_hosts_extra. The authoritative
	// list lives in internal/tenant and is compiled in; configuration may only
	// add to it, never replace or empty it.
	extraReserved map[string]struct{}
}

// NewPublicHandler builds the public pipeline.
func NewPublicHandler(
	cfg *config.Config,
	logger *slog.Logger,
	resolver *routing.Resolver,
	apex *ApexHandler,
	upstream Upstream,
) *PublicHandler {
	extra := make(map[string]struct{}, len(cfg.Platform.ReservedHostsExtra))
	for _, h := range cfg.Platform.ReservedHostsExtra {
		extra[h] = struct{}{}
	}

	return &PublicHandler{
		rootDomain:    cfg.Platform.RootDomain,
		logger:        logger,
		resolver:      resolver,
		selector:      routing.NewSelector(cfg.Routing.BackendPathPrefix, cfg.Routing.PathPrefixCaseSensitive),
		apex:          apex,
		upstream:      upstream,
		extraReserved: extra,
	}
}

// WithTenantRateLimit attaches the tenant-keyed rate limiter (tier 2).
//
// It is a separate step rather than a constructor argument, following
// tenant.Service.WithInvalidator: the callers that do not care about rate
// limiting — which is every test that is about routing rather than about
// limiting — keep building a handler without one, and a nil limiter leaves
// tier 2 disabled rather than making it a required dependency of the pipeline.
func (h *PublicHandler) WithTenantRateLimit(limiter *security.Limiter) *PublicHandler {
	h.tenantLimiter = limiter
	return h
}

// ServeHTTP implements http.Handler.
//
// The steps below are the §8 hostname validation sequence verbatim, plus the
// §65 reserved-host ordering rule. Every rejection path writes the same 404,
// so the reason survives only in the log.
func (h *PublicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, err := routing.ParseHost(r.Host, h.rootDomain)
	if err != nil {
		h.rejectHost(r, err)
		WriteTenantNotFound(w)
		return
	}

	// The apex is a platform hostname, not a tenant, so it leaves the tenant
	// path entirely here rather than being rejected further down.
	if host.Kind == routing.KindApex {
		h.apex.ServeHTTP(w, r)
		return
	}

	// Reserved platform hosts are refused before the registry is consulted
	// (architecture doc §65). The ordering is load-bearing: a reserved host is
	// guaranteed to miss, and consulting the registry first would let anyone
	// generate database load with hostnames that can never be tenants.
	if h.isReserved(host.Slug) {
		h.rejectHost(r, fmt.Errorf("reserved platform host %q", host.Slug))
		WriteTenantNotFound(w)
		return
	}

	// Shape only. Reserved-ness was already handled above, and
	// ValidateSlugFormat deliberately does not re-check it, so that a reserved
	// slug already in the registry stays readable and removable.
	if err := tenant.ValidateSlugFormat(host.Slug); err != nil {
		h.rejectHost(r, err)
		WriteTenantNotFound(w)
		return
	}

	route, err := h.resolver.Resolve(r.Context(), host.Slug)
	if err != nil {
		h.rejectRoute(w, r, host, err)
		return
	}

	// The frontend/backend split (§9.1) is decided here rather than in the
	// proxy, because it is a routing rule and not a property of any upstream.
	// The proxy consumes the decision; it does not make it.
	info := routing.Info{
		Host:   host.Name,
		Tenant: route.TenantSlug,
		Stage:  h.selector.Stage(r.URL.Path),
		Route:  route,
	}

	// Record the decision as soon as it exists — before the tier-2 check
	// below, not after it. The slot is read on both sides of this handler (the
	// proxy inward, the access log outward), so it is written once, here, from
	// the same value that is passed to the upstream.
	//
	// The position of this line is load-bearing, and it is what makes the two
	// tiers distinguishable in the access log. Tier 1 runs in the middleware
	// chain, before this handler and before the hostname is parsed, so a
	// tier-1 refusal can never carry a `tenant` field. Tier 2 runs below,
	// after resolution, so it can. Had the write stayed after the tier-2
	// check, a tier-2 refusal would be an anonymous 429 indistinguishable
	// from a tier-1 one, and the only way to learn which tier refused a
	// request would be to reproduce it. This was not theoretical: the probe
	// found exactly that, and it is why the write moved.
	//
	// Note that Host is the hostname this request was routed on, not the
	// forwarded host a trusted proxy may have asserted. Those are allowed to
	// differ (see routing.ResolveEdge), and only this one is the reason the
	// request reached the tenant it did.
	routing.SetInfo(r.Context(), info)

	// Tier 2 (plan §8.1 step 13): per-tenant fairness, keyed by the tenant
	// that has just resolved.
	//
	// It sits here, and only here, because a limiter keyed by tenant cannot
	// exist before the tenant is known — which is the entire reason there are
	// two tiers rather than one. It is deliberately after the status check
	// rather than before it: a tenant that is not ACTIVE is refused by
	// rejectRoute above, so this spends a token only on traffic that would
	// otherwise have reached a product. A DISABLED tenant cannot consume its
	// own allowance, and cannot be used to consume anyone else's either,
	// because the key is the tenant that resolved.
	//
	// No separate log line is written for a refusal. The access log records
	// the 429 with the tenant that SetInfo just stored, which is the whole
	// reason the two tiers can be told apart without a field of their own.
	if decision := h.tenantLimiter.AllowTenant(route.TenantSlug); !decision.Allowed {
		WriteRateLimited(w, decision.RetryAfterSeconds())
		return
	}

	h.upstream.Serve(w, r, info)
}

// isReserved reports whether slug is reserved, from either the compiled-in
// authoritative list or the configured additions.
func (h *PublicHandler) isReserved(slug string) bool {
	if tenant.IsReserved(slug) {
		return true
	}
	_, ok := h.extraReserved[slug]
	return ok
}

// rejectHost logs why a hostname was refused.
//
// Every caller responds with the same 404, so this log line is the only place
// the distinction exists. It is logged at debug: rejected hostnames are
// overwhelmingly probes and typos, and logging each one at info would let
// anyone inflate the log with a loop over random subdomains.
func (h *PublicHandler) rejectHost(r *http.Request, reason error) {
	h.logger.Debug("host rejected",
		"request_id", RequestIDFrom(r.Context()),
		"host", r.Host,
		"reason", reason.Error(),
	)
}

// rejectRoute maps a resolution failure to a response.
//
// Unknown and inactive tenants are deliberately indistinguishable in the
// response and distinguishable only in the log. An unknown tenant is usually a
// typo or a probe; an inactive one is a provisioning or billing event. Both
// are 404 with an identical body, because distinguishing them would confirm to
// an anonymous prober that a hostname belongs to a real business
// (ADR 0001 decision 5).
//
// The field naming is deliberate. `tenant` means a slug that resolved to a
// route, and only the disabled case can claim that; the other two use
// `host_label`, which is what the leftmost label of a hostname is until the
// registry says otherwise. Using one name for both would make a log query for
// tenant=X match hostnames that were never tenants, which is the sort of thing
// that costs an hour at three in the morning.
func (h *PublicHandler) rejectRoute(w http.ResponseWriter, r *http.Request, host routing.Host, err error) {
	switch {
	case errors.Is(err, routing.ErrRegistryDown):
		// Not a 404. The Gateway cannot tell whether this tenant exists, and
		// guessing would either break a working tenant or hide an outage
		// (architecture doc §46). It reports its own inability instead.
		h.logger.Error("registry unavailable while resolving tenant",
			"request_id", RequestIDFrom(r.Context()),
			"host", host.Name,
			"host_label", host.Slug,
			"error", err,
		)
		WriteRegistryUnavailable(w)
		return

	case errors.Is(err, routing.ErrTenantDisabled):
		h.logger.Warn("tenant is not active",
			"request_id", RequestIDFrom(r.Context()),
			"host", host.Name,
			"tenant", host.Slug,
			"error", err,
		)
		WriteTenantDisabled(w)
		return
	}

	h.logger.Debug("unknown tenant",
		"request_id", RequestIDFrom(r.Context()),
		"host", host.Name,
		"host_label", host.Slug,
	)
	WriteTenantNotFound(w)
}
