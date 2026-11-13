package tenant

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// RouteReader reads a single route. It is the slice of Repository the cache
// needs, and it is satisfied by *MySQLRepository.
//
// Declaring it here rather than taking a whole Repository keeps the cache
// honest about what it does: it reads, and it caches reads. It cannot mutate,
// so it can never be the thing that makes the registry disagree with itself.
type RouteReader interface {
	GetBySlug(ctx context.Context, slug string) (Route, error)
}

// Invalidator drops a cached route.
//
// The lifecycle Service holds one of these so that a mutation cannot leave a
// stale route behind (§33). It is a one-method interface so the Service does
// not depend on the cache's configuration or on the cache existing at all.
type Invalidator interface {
	Invalidate(slug string)
}

// Cache defaults.
const (
	DefaultRouteTTL    = 60 * time.Second
	DefaultNegativeTTL = 30 * time.Second
	DefaultMaxEntries  = 10000

	// DefaultLoadTimeout bounds a single upstream read. It exists because the
	// load runs detached from the caller's cancellation (see load), so without
	// a bound of its own a hung registry could leave a singleflight call
	// running forever and every later request for that slug would queue behind
	// it.
	DefaultLoadTimeout = 5 * time.Second
)

// CacheOptions configures the route cache.
//
// Zero values fall back to the defaults above, so a caller that only cares
// about one field does not have to restate the others.
type CacheOptions struct {
	RouteTTL    time.Duration
	NegativeTTL time.Duration
	MaxEntries  int
	LoadTimeout time.Duration
}

// cacheEntry is one cached answer.
type cacheEntry struct {
	route     Route
	expiresAt time.Time

	// negative records that the registry answered "no such tenant". It is a
	// separate flag rather than a sentinel Route value so that a zero Route
	// can never be mistaken for a cached absence.
	negative bool
}

// Cache is a TTL cache in front of a route reader.
//
// It is deliberately never authoritative. Deleting every entry at any moment
// must leave the Gateway fully correct, only slower (§92). Nothing outside
// this type may assume an entry exists.
//
// Every field that a request touches is either immutable after construction or
// guarded by mu. There is no package-level mutable tenant state anywhere in
// the Gateway (§31) — the only per-request state is the context, and the only
// shared state is this map, which holds routes for *all* tenants and is
// indexed by slug.
type Cache struct {
	upstream    RouteReader
	routeTTL    time.Duration
	negativeTTL time.Duration
	maxEntries  int
	loadTimeout time.Duration

	// now is injectable so TTL expiry can be tested without sleeping. It is
	// never changed after construction outside of tests.
	now func() time.Time

	// mu guards entries, generation, and every counter below.
	//
	// Counters are plain int64 rather than atomics because they are only ever
	// read or written while mu is held. Mixing atomics with a mutex in one
	// type invites the reader to wonder which of the two actually protects
	// what.
	mu      sync.RWMutex
	entries map[string]cacheEntry

	// generation is bumped by every Invalidate and Flush.
	//
	// It closes a real cache-aside race: a request that missed can read the
	// registry, and by the time it writes its answer the route may already
	// have changed and been invalidated. Without this, that stale answer would
	// be written *after* the invalidation and served for a full TTL — a newly
	// activated tenant would stay unroutable for up to route_ttl, which looks
	// exactly like a cache bug and is miserable to reproduce.
	//
	// A single global counter, rather than one per slug, keeps memory bounded:
	// a per-slug generation map would grow without limit as unknown hostnames
	// are probed. The cost is that an unrelated invalidation discards an
	// in-flight write, which is only a wasted cache fill, never a wrong answer.
	generation uint64

	hits         int64
	misses       int64
	negativeHits int64
	overflowed   int64
	staleWrites  int64

	// group collapses concurrent misses for one slug into a single upstream
	// read. It is keyed by slug, so a load for one tenant is never shared with
	// another.
	group singleflight.Group
}

// NewCache builds a route cache in front of upstream.
func NewCache(upstream RouteReader, opts CacheOptions) *Cache {
	routeTTL := opts.RouteTTL
	if routeTTL <= 0 {
		routeTTL = DefaultRouteTTL
	}
	negativeTTL := opts.NegativeTTL
	if negativeTTL <= 0 {
		negativeTTL = DefaultNegativeTTL
	}
	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	loadTimeout := opts.LoadTimeout
	if loadTimeout <= 0 {
		loadTimeout = DefaultLoadTimeout
	}

	return &Cache{
		upstream:    upstream,
		routeTTL:    routeTTL,
		negativeTTL: negativeTTL,
		maxEntries:  maxEntries,
		loadTimeout: loadTimeout,
		now:         time.Now,
		entries:     make(map[string]cacheEntry),
	}
}

// GetBySlug implements RouteReader.
//
// A registry failure is never cached. "I could not ask" is not an answer, and
// caching it would turn a transient outage into a negative TTL of wrong
// answers — which is precisely the failure mode the 503 path exists to avoid
// (architecture doc §46).
func (c *Cache) GetBySlug(ctx context.Context, slug string) (Route, error) {
	if entry, ok := c.get(slug); ok {
		if entry.negative {
			c.mu.Lock()
			c.negativeHits++
			c.mu.Unlock()
			return Route{}, fmt.Errorf("%w: %q", ErrNotFound, slug)
		}

		c.mu.Lock()
		c.hits++
		c.mu.Unlock()
		return entry.route, nil
	}

	c.mu.Lock()
	c.misses++
	c.mu.Unlock()

	route, err := c.load(ctx, slug)
	if err != nil {
		return Route{}, err
	}
	return route, nil
}

// get returns a live entry, or false on a miss.
func (c *Cache) get(slug string) (cacheEntry, bool) {
	c.mu.RLock()
	entry, ok := c.entries[slug]
	now := c.now()
	c.mu.RUnlock()

	if !ok {
		return cacheEntry{}, false
	}
	if !now.Before(entry.expiresAt) {
		c.expire(slug)
		return cacheEntry{}, false
	}
	return entry, true
}

// expire removes an entry that has passed its deadline.
//
// The deadline is re-checked under the write lock. Between the caller's read
// and this upgrade another goroutine may have replaced the entry with a fresh
// one, and deleting that would throw away a valid answer.
func (c *Cache) expire(slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[slug]
	if ok && !c.now().Before(entry.expiresAt) {
		delete(c.entries, slug)
	}
}

// load reads the route from upstream, collapsing concurrent callers.
func (c *Cache) load(ctx context.Context, slug string) (Route, error) {
	// DoChan with a detached context, rather than Do with the caller's.
	//
	// Do runs the load in the *first caller's* goroutine using that caller's
	// context, so if the leader disconnects every waiter inherits its
	// cancellation and one dropped client fails an entire cohort of unrelated
	// requests. Detaching means the read completes for everyone; each waiter
	// still stops waiting when its own request goes away, via the select
	// below.
	//
	// The detached context has no deadline of its own, so loadTimeout supplies
	// one. Without it a hung registry would leave the singleflight call
	// running forever and every later request for that slug would queue behind
	// it.
	ch := c.group.DoChan(slug, func() (any, error) {
		// Re-check the cache inside the singleflight call.
		//
		// singleflight deletes a key the moment its function returns, so the
		// window between a caller's cache miss and its arrival here is not
		// atomic with the store. A caller that missed just before the previous
		// call finished can therefore arrive *after* the key was removed and
		// would otherwise start a second upstream read for the same slug.
		//
		// Re-checking closes the gap: the previous call stores its answer
		// before returning, and the key is only removed after that, so an
		// entry that is visible here is one the previous call just wrote.
		// Without this, a burst of concurrent requests for one cold tenant
		// produces two registry reads instead of one.
		if entry, ok := c.get(slug); ok {
			if entry.negative {
				return Route{}, fmt.Errorf("%w: %q", ErrNotFound, slug)
			}
			return entry.route, nil
		}

		// The generation is read before the upstream call, so the store can
		// tell whether anything was invalidated while the read was in flight.
		generation := c.currentGeneration()

		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.loadTimeout)
		defer cancel()

		route, err := c.upstream.GetBySlug(loadCtx, slug)

		// Only a definite answer is cached. An absence is an answer; a failure
		// to answer is not.
		if err == nil || errors.Is(err, ErrNotFound) {
			c.store(slug, route, errors.Is(err, ErrNotFound), generation)
		}

		return route, err
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return Route{}, res.Err
		}
		route, ok := res.Val.(Route)
		if !ok {
			return Route{}, fmt.Errorf("tenant cache: upstream returned an unexpected %T", res.Val)
		}
		return route, nil

	case <-ctx.Done():
		return Route{}, ctx.Err()
	}
}

// store writes an answer, unless it has been invalidated in the meantime.
func (c *Cache) store(slug string, route Route, negative bool, generation uint64) {
	ttl := c.routeTTL
	if negative {
		ttl = c.negativeTTL
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.generation != generation {
		c.staleWrites++
		return
	}

	// Refuse to grow past the cap rather than evict a live route.
	//
	// Evicting to make room would cause the very stampede the cache exists to
	// prevent: the entry chosen for eviction is one that is being actively
	// requested. Degrading a *new* slug to an uncached read is bounded and
	// self-correcting; evicting is not.
	if len(c.entries) >= c.maxEntries {
		c.sweepExpiredLocked()

		if _, exists := c.entries[slug]; !exists && len(c.entries) >= c.maxEntries {
			c.overflowed++
			return
		}
	}

	c.entries[slug] = cacheEntry{
		route:     route,
		expiresAt: c.now().Add(ttl),
		negative:  negative,
	}
}

// sweepExpiredLocked drops every entry past its deadline. Callers must hold mu.
func (c *Cache) sweepExpiredLocked() {
	now := c.now()
	for slug, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, slug)
		}
	}
}

// currentGeneration reads the invalidation generation.
func (c *Cache) currentGeneration() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generation
}

// Invalidate drops the cached route for one slug.
//
// It is called after every successful lifecycle mutation, so that a change
// made through the internal API is visible to the next public request rather
// than up to a full TTL later (§33).
func (c *Cache) Invalidate(slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, slug)
	c.generation++
}

// Flush drops every cached route.
//
// Nothing may depend on the cache for correctness, so this must be safe to
// call at any moment, including under load (§92).
func (c *Cache) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]cacheEntry)
	c.generation++
}

// Len reports how many entries are currently stored, including entries that
// have expired but not yet been swept.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// CacheStats is a snapshot of cache counters.
type CacheStats struct {
	Entries      int
	Hits         int64
	Misses       int64
	NegativeHits int64
	Overflowed   int64
	StaleWrites  int64
}

// Stats returns a snapshot of the cache counters.
func (c *Cache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return CacheStats{
		Entries:      len(c.entries),
		Hits:         c.hits,
		Misses:       c.misses,
		NegativeHits: c.negativeHits,
		Overflowed:   c.overflowed,
		StaleWrites:  c.staleWrites,
	}
}
