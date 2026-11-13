// Command gateway is the ZenSys Gateway: a platform-level edge service that
// resolves a public tenant hostname to a product and reverse-proxies the
// request to that product's frontend or backend.
//
// It is deliberately a wiring layer. Configuration validation, host
// resolution, the tenant registry, and the proxy live in internal packages so
// that each can be tested without starting a process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/zensys/gateway/internal/config"
	"github.com/zensys/gateway/internal/httpapi"
	"github.com/zensys/gateway/internal/observability"
	"github.com/zensys/gateway/internal/proxy"
	"github.com/zensys/gateway/internal/routing"
	"github.com/zensys/gateway/internal/security"
	"github.com/zensys/gateway/internal/tenant"
)

const serviceName = "zensys-gateway"

func main() {
	if err := run(); err != nil {
		// Startup failures are written to stderr rather than through the
		// logger, because a configuration failure may happen before a logger
		// exists.
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "configs/gateway.dev.yaml", "path to the configuration file")
	flag.Parse()

	// Load validates the whole configuration and resolves secrets before
	// anything else happens. If the process reaches the next line, the
	// configuration is known to be complete and internally consistent.
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger, err := observability.NewLogger(cfg.Observability.LogLevel, cfg.Observability.LogFormat)
	if err != nil {
		return err
	}

	// The trusted-proxy set is parsed once and shared. Load has already
	// validated the same list, so a failure here is unreachable in practice;
	// it is returned rather than ignored so that the two can never disagree
	// about what "valid" means.
	trustedProxies, err := routing.ParseTrustedProxies(cfg.Proxy.TrustedProxies)
	if err != nil {
		return err
	}

	// A Gateway that trusts nothing is a legitimate deployment, but it is also
	// what a typo in the list looks like, and the consequence — every request
	// reported as coming from the proxy — is invisible until someone reads a
	// product's logs. Saying so at startup makes the state deliberate.
	if trustedProxies.Len() == 0 {
		logger.Warn("no trusted proxies are configured; forwarded headers will be ignored and every request will be attributed to its immediate peer")
	}

	logger.Info("starting "+serviceName,
		"root_domain", cfg.Platform.RootDomain,
		"public_addr", cfg.Server.PublicAddr,
		"internal_addr", cfg.Server.InternalAddr,
		"apex_behavior", cfg.Platform.ApexBehavior,
		"products", len(cfg.Products),
	)

	// Registry.
	//
	// OpenMySQL does not connect, so a failure here means the DSN is malformed,
	// not that the database is down. That distinction matters: a malformed DSN
	// is a configuration bug and should stop the process, while an unreachable
	// database is an operational condition the Gateway is designed to survive.
	registryDSN, err := tenant.NormalizeDSN(cfg.Registry.DSN)
	if err != nil {
		return err
	}

	db, err := tenant.OpenMySQL(registryDSN, tenant.MySQLOptions{
		MaxOpenConns:    cfg.Registry.MaxOpenConns,
		MaxIdleConns:    cfg.Registry.MaxIdleConns,
		ConnMaxLifetime: cfg.Registry.ConnMaxLifetime.Std(),
		QueryTimeout:    cfg.Registry.QueryTimeout.Std(),
	})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	registry := tenant.NewMySQLRepository(db, tenant.MySQLOptions{
		QueryTimeout: cfg.Registry.QueryTimeout.Std(),
	})

	// Probe once so a misconfigured or unreachable registry is visible in the
	// logs immediately rather than on the first request.
	//
	// A failure is logged, not fatal. Crash-looping on a registry outage would
	// turn a partial degradation — uncached tenants get 503, cached routes keep
	// serving — into a total outage (§46, §57). Readiness reports the real
	// state so an orchestrator can act on it.
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 3*time.Second)
	if err := registry.Ping(probeCtx); err != nil {
		logger.Warn("registry is not reachable; the Gateway will report not-ready until it recovers",
			"error", err)
	} else {
		logger.Info("registry reachable")
	}
	cancelProbe()

	// The public path reads through the cache; the lifecycle API (M6) reads
	// through the repository and invalidates the cache on every mutation.
	// Those two consumers deliberately have different consistency needs: a
	// routing decision tolerates a route that is up to route_ttl old, a
	// lifecycle decision does not.
	routeCache := tenant.NewCache(registry, tenant.CacheOptions{
		RouteTTL:    cfg.Cache.RouteTTL.Std(),
		NegativeTTL: cfg.Cache.NegativeTTL.Std(),
		MaxEntries:  cfg.Cache.MaxEntries,
	})

	upstream, err := proxy.New(cfg, logger)
	if err != nil {
		return err
	}

	// Log the upstreams once, at startup. Which address a product resolves to
	// is the single most useful thing to know when a tenant answers 502, and
	// it is otherwise only discoverable by reading the configuration file.
	logger.Info("upstreams configured", "routes", upstream.Routes())

	// The lifecycle service reads through the repository, never through the
	// cache, and drops the cached route after every successful mutation. A
	// lifecycle decision has to be made on the truth; a newly activated tenant
	// must become routable on the next request, not a route_ttl later (§33).
	lifecycle := tenant.NewService(registry, logger).WithInvalidator(routeCache)

	authenticator, err := security.NewTokenAuthenticator(serviceTokens(cfg))
	if err != nil {
		return err
	}

	// The two-tier rate limiter (M7, plan §8.2). A disabled limiter is a
	// genuine pass-through rather than a limiter with enormous limits, so the
	// startup log below distinguishes the two states explicitly.
	rateLimiter, err := security.NewLimiter(security.LimiterConfig{
		Enabled:    cfg.RateLimit.Enabled,
		IP:         security.BucketSpec{RPS: cfg.RateLimit.IP.RPS, Burst: cfg.RateLimit.IP.Burst},
		Tenant:     security.BucketSpec{RPS: cfg.RateLimit.Tenant.RPS, Burst: cfg.RateLimit.Tenant.Burst},
		MaxEntries: cfg.RateLimit.MaxEntries,
	})
	if err != nil {
		return err
	}

	logRateLimit(logger, rateLimiter)

	logger.Info("internal lifecycle API configured",
		"auth_mode", cfg.InternalAuth.Mode,
		"services", configuredServices(cfg),
	)

	publicSrv := newServer(cfg.Server.PublicAddr, cfg, logger,
		publicHandler(cfg, logger, registry, routeCache, trustedProxies, upstream, rateLimiter))
	internalSrv := newServer(cfg.Server.InternalAddr, cfg, logger,
		internalHandler(cfg, logger, registry, trustedProxies, authenticator, lifecycle))

	// SIGTERM is how a container runtime or service manager asks for a
	// graceful stop; SIGINT is Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return serve(gctx, publicSrv, logger, "public", cfg.Server.ShutdownGrace.Std())
	})
	g.Go(func() error {
		return serve(gctx, internalSrv, logger, "internal", cfg.Server.ShutdownGrace.Std())
	})

	if err := g.Wait(); err != nil {
		return err
	}

	logger.Info("shutdown complete")
	return nil
}

// newServer builds an http.Server with the shared timeouts and a logger that
// routes net/http's internal error output into the structured log.
func newServer(addr string, cfg *config.Config, logger *slog.Logger, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// serve runs srv until ctx is cancelled, then drains in-flight requests.
func serve(ctx context.Context, srv *http.Server, logger *slog.Logger, name string, grace time.Duration) error {
	// Addr is assigned by the caller through srv.Addr.
	errCh := make(chan error, 1)

	go func() {
		logger.Info("listener started", "listener", name, "addr", srv.Addr)

		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("%s listener on %s: %w", name, srv.Addr, err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		// The listener stopped on its own. A non-nil error here is a bind
		// failure or an unexpected accept error, and both are fatal.
		return err

	case <-ctx.Done():
		logger.Info("draining listener", "listener", name, "grace", grace.String())
	}

	// Use a fresh context: ctx is already cancelled, and Shutdown needs a live
	// deadline of its own to bound the drain.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("%s listener shutdown: %w", name, err)
	}

	logger.Info("listener stopped", "listener", name)
	return nil
}

// publicHandler builds the public listener's handler chain.
//
// The middleware order lives in httpapi.StandardChain rather than here, so
// that the tests drive the same chain the process runs.
//
// registry must not be nil: it backs the readiness probe. routeCache must not
// be nil either: it is the public path's only route source. rateLimiter may be
// nil, which leaves both tiers disabled.
func publicHandler(
	cfg *config.Config,
	logger *slog.Logger,
	registry *tenant.MySQLRepository,
	routeCache *tenant.Cache,
	trustedProxies *routing.TrustedProxies,
	upstream httpapi.Upstream,
	rateLimiter *security.Limiter,
) http.Handler {
	mux := http.NewServeMux()

	// Health is off the public listener by default. The public listener is a
	// host-based catch-all, so serving /healthz there would shadow a path a
	// product may legitimately own on {tenant}.zensys.lk.
	//
	// When it is enabled, the pattern "GET /healthz" is more specific than the
	// "/" catch-all below and therefore still wins.
	if cfg.Observability.HealthOnPublic {
		httpapi.NewHealthHandler(logger, readinessChecks(cfg, registry)).Register(mux)
	}

	// The whole public listener is one host-based handler: host resolution,
	// tenant lookup through the route cache, the frontend/backend split, and
	// the proxy to the product.
	apex := httpapi.NewApexHandler(cfg.Platform.ApexBehavior, cfg.Platform.LandingDir, logger)
	resolver := routing.NewResolver(routeCache)

	// Tier 2 is attached to the routing handler rather than to the chain,
	// because it is keyed by the tenant and the tenant is not known until the
	// handler has resolved it (plan §8.1 step 13).
	mux.Handle("/", httpapi.NewPublicHandler(cfg, logger, resolver, apex, upstream).
		WithTenantRateLimit(rateLimiter))

	return httpapi.StandardChain(mux, logger, chainOptions(cfg, trustedProxies, rateLimiter))
}

// internalHandler builds the internal listener's handler chain.
//
// This listener carries the tenant lifecycle API and must never be reachable
// from the public one (architecture doc §40). The two are separate http.Server
// values on separate addresses, not two muxes on one server, so there is no
// configuration mistake that can merge them.
func internalHandler(
	cfg *config.Config,
	logger *slog.Logger,
	registry *tenant.MySQLRepository,
	trustedProxies *routing.TrustedProxies,
	authenticator security.Authenticator,
	lifecycle httpapi.Lifecycle,
) http.Handler {
	mux := http.NewServeMux()

	httpapi.NewHealthHandler(logger, readinessChecks(cfg, registry)).Register(mux)

	// The tenant lifecycle API (§39). It is mounted on this listener and only
	// this one; the public listener's mux has no /internal/ pattern, so there
	// is no public address at which it can be reached (architecture doc §40).
	httpapi.NewInternalHandler(logger, authenticator, lifecycle).Register(mux)

	// Anything else on this listener is a 404 in the standard error contract
	// rather than Go's default plain-text response.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.ErrNotFound, "No such endpoint.")
	}))

	// The same chain as the public listener, deliberately. The header contract
	// is one policy, and the internal listener's callers are loopback services
	// that are trusted by the same rule — which is what lets POS and GMS pass
	// a correlation ID through when they drive the lifecycle API.
	//
	// The rate limiter is nil here, so this listener has no rate limiting.
	// That is a decision rather than an omission: §8.1 places the limiter in
	// the public pipeline, the callers here are authenticated platform
	// services on a loopback address, and a limit would turn a legitimate bulk
	// provisioning run into what looks like a bug. The public listener's own
	// health endpoints are exempt for the same reason in reverse — see
	// ChainOptions.OperationalPaths.
	return httpapi.StandardChain(mux, logger, chainOptions(cfg, trustedProxies, nil))
}

// serviceTokens maps the configured service tokens into the shape the
// security package expects.
//
// The mapping is explicit rather than a type alias so that the configuration
// model and the authentication model can diverge without one dragging the
// other. Both product values are parsed here; Validate has already rejected
// anything that is not a configured product, so a failure is unreachable and
// is returned rather than ignored.
func serviceTokens(cfg *config.Config) []security.TokenSpec {
	specs := make([]security.TokenSpec, 0, len(cfg.InternalAuth.Tokens))
	for _, token := range cfg.InternalAuth.Tokens {
		products := make([]tenant.Product, 0, len(token.MayManageProducts))
		for _, name := range token.MayManageProducts {
			products = append(products, tenant.Product(name))
		}
		specs = append(specs, security.TokenSpec{
			Service:  token.Service,
			Token:    token.Token,
			Products: products,
		})
	}
	return specs
}

// configuredServices returns the names of the services that hold a token, for
// the startup log.
//
// Names only. A token is never logged, in whole or in part, at any level.
func configuredServices(cfg *config.Config) []string {
	services := make([]string, 0, len(cfg.InternalAuth.Tokens))
	for _, token := range cfg.InternalAuth.Tokens {
		services = append(services, token.Service)
	}
	return services
}

// operationalPaths are the endpoints that exist for the platform's own
// operation.
//
// One list with two consequences: they are logged at debug unless they fail,
// and they are exempt from tier-1 rate limiting. Both follow from the same
// fact — these endpoints are the platform talking to itself — which is why
// they are not two lists. See ChainOptions.OperationalPaths.
var operationalPaths = []string{"/healthz", "/readyz", "/metrics"}

// chainOptions gathers what the standard chain needs from configuration.
//
// tier1 is nil for the internal listener, which is what disables rate limiting
// there. Passing nil rather than a disabled limiter is deliberate: the two are
// equivalent to the chain, and nil states "this listener has no limiter" at
// the call site where the decision belongs.
func chainOptions(cfg *config.Config, trustedProxies *routing.TrustedProxies, tier1 *security.Limiter) httpapi.ChainOptions {
	return httpapi.ChainOptions{
		RequestIDHeader:  cfg.Proxy.RequestIDHeader,
		TrustedProxies:   trustedProxies,
		MaxBodyBytes:     cfg.Server.MaxBodyBytes,
		MaxHeaderBytes:   cfg.Server.MaxHeaderBytes,
		Tier1:            tier1,
		OperationalPaths: operationalPaths,
	}
}

// logRateLimit states the rate-limit configuration at startup.
//
// It is logged even when the limiter is disabled, because "no rate limiting"
// is the state an operator most needs to be told about and least likely to
// discover from the configuration file they just wrote. A disabled limiter and
// a limiter that is silently failing open look identical from the outside.
func logRateLimit(logger *slog.Logger, limiter *security.Limiter) {
	if limiter == nil || !limiter.Enabled() {
		logger.Warn("rate limiting is disabled; every request is accepted at whatever rate it arrives")
		return
	}

	ip, tenantLimit := limiter.IPSpec(), limiter.TenantSpec()
	logger.Info("rate limiting configured",
		"tier1_tier", security.TierIP,
		"tier1_rps", ip.RPS,
		"tier1_burst", ip.Burst,
		"tier2_tier", security.TierTenant,
		"tier2_rps", tenantLimit.RPS,
		"tier2_burst", tenantLimit.Burst,
	)
}

// readinessChecks returns the dependencies that must be usable for the Gateway
// to serve traffic.
//
// A failing check makes /readyz report 503 and log the cause. It never makes
// the process exit: an orchestrator that restarts on a failed readiness probe
// would turn a registry outage into a Gateway outage, and the Gateway is
// specifically designed to keep serving cached routes through one (§46, §57).
func readinessChecks(cfg *config.Config, registry *tenant.MySQLRepository) []httpapi.Check {
	checks := []httpapi.Check{
		{
			// Configuration was already validated at load time, so reaching
			// this point means it is valid. The check exists so that /readyz
			// reports it explicitly rather than leaving its absence ambiguous.
			Name: "config",
			Run:  func(context.Context) error { return nil },
		},
		{
			Name: "upstreams",
			Run: func(context.Context) error {
				for _, name := range config.RequiredProducts {
					p, ok := cfg.Product(name)
					if !ok {
						return fmt.Errorf("product %s is not configured", name)
					}
					if p.FrontendUpstream == "" || p.BackendUpstream == "" {
						return fmt.Errorf("product %s is missing an upstream", name)
					}
				}
				return nil
			},
		},
	}

	// A nil registry means the caller has no registry to check, which is only
	// true in tests. Skipping the check then keeps /readyz from reporting a
	// failure that says nothing about the process being tested.
	if registry != nil {
		checks = append(checks, httpapi.Check{
			Name: "registry",
			Run:  registry.Ping,
		})
	}

	return checks
}
