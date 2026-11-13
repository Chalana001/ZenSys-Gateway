package tenant

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// testClock is a manually advanced clock.
//
// It is mutex-guarded rather than a bare time.Time because the cache reads it
// while holding its own lock and the test advances it from the test goroutine.
// With -race enabled, an unguarded shared field here would show up as a race in
// the test harness rather than in the code under test.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// countingReader is a RouteReader that records how often it was asked.
//
// The call counts are the point: several tests assert that the cache did *not*
// go upstream, which is the only way to prove a hit rather than merely observe
// that the answer looked right.
type countingReader struct {
	mu     sync.Mutex
	routes map[string]Route
	err    error
	calls  int
	bySlug map[string]int

	// gate, when non-nil, holds every read until a value is received on it.
	gate chan struct{}

	// entered, when non-nil, receives a signal as each read begins. It must be
	// buffered by the test, since the send is non-blocking.
	entered chan struct{}
}

func newCountingReader(routes ...Route) *countingReader {
	r := &countingReader{
		routes: make(map[string]Route),
		bySlug: make(map[string]int),
	}
	for _, route := range routes {
		r.routes[route.TenantSlug] = route
	}
	return r
}

func (r *countingReader) GetBySlug(ctx context.Context, slug string) (Route, error) {
	r.mu.Lock()
	r.calls++
	r.bySlug[slug]++
	err := r.err
	route, ok := r.routes[slug]
	gate := r.gate
	entered := r.entered
	r.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return Route{}, ctx.Err()
		}
	}

	if err != nil {
		return Route{}, err
	}
	if !ok {
		return Route{}, fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	return route, nil
}

func (r *countingReader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *countingReader) callsFor(slug string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bySlug[slug]
}

func (r *countingReader) setError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *countingReader) put(route Route) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[route.TenantSlug] = route
}

func (r *countingReader) remove(slug string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, slug)
}

func cachedRoute(slug string, product Product, status Status) Route {
	return Route{TenantSlug: slug, Product: product, Status: status}
}

// newTestCache builds a cache with a controllable clock.
func newTestCache(t *testing.T, reader RouteReader, opts CacheOptions) (*Cache, *testClock) {
	t.Helper()

	cache := NewCache(reader, opts)
	clock := newTestClock()
	cache.now = clock.Now
	return cache, clock
}

// --- cold and warm ---------------------------------------------------------

func TestCache_ColdMissThenWarmHit(t *testing.T) {
	reader := newCountingReader(cachedRoute("mangala", ProductPOS, StatusActive))
	cache, _ := newTestCache(t, reader, CacheOptions{})

	first, err := cache.GetBySlug(context.Background(), "mangala")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if first.TenantSlug != "mangala" {
		t.Errorf("TenantSlug = %q, want mangala", first.TenantSlug)
	}
	if got := reader.callCount(); got != 1 {
		t.Fatalf("upstream reads after a cold miss = %d, want 1", got)
	}

	for i := 0; i < 10; i++ {
		if _, err := cache.GetBySlug(context.Background(), "mangala"); err != nil {
			t.Fatalf("warm read %d: %v", i, err)
		}
	}

	if got := reader.callCount(); got != 1 {
		t.Errorf("upstream reads after 10 warm hits = %d, want 1", got)
	}

	stats := cache.Stats()
	if stats.Hits != 10 {
		t.Errorf("Hits = %d, want 10", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("Misses = %d, want 1", stats.Misses)
	}
}

func TestCache_NegativeAnswersAreCached(t *testing.T) {
	reader := newCountingReader()
	cache, _ := newTestCache(t, reader, CacheOptions{})

	for i := 0; i < 5; i++ {
		_, err := cache.GetBySlug(context.Background(), "nobody")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("read %d: error = %v, want ErrNotFound", i, err)
		}
	}

	if got := reader.callCount(); got != 1 {
		t.Errorf("upstream reads for a repeated unknown tenant = %d, want 1", got)
	}
	if got := cache.Stats().NegativeHits; got != 4 {
		t.Errorf("NegativeHits = %d, want 4", got)
	}
}

// --- TTL -------------------------------------------------------------------

func TestCache_PositiveEntryExpires(t *testing.T) {
	reader := newCountingReader(cachedRoute("mangala", ProductPOS, StatusActive))
	cache, clock := newTestCache(t, reader, CacheOptions{RouteTTL: 60 * time.Second})

	if _, err := cache.GetBySlug(context.Background(), "mangala"); err != nil {
		t.Fatalf("first read: %v", err)
	}

	clock.Advance(59 * time.Second)
	if _, err := cache.GetBySlug(context.Background(), "mangala"); err != nil {
		t.Fatalf("read before expiry: %v", err)
	}
	if got := reader.callCount(); got != 1 {
		t.Fatalf("the entry expired early: %d upstream reads, want 1", got)
	}

	clock.Advance(2 * time.Second)
	if _, err := cache.GetBySlug(context.Background(), "mangala"); err != nil {
		t.Fatalf("read after expiry: %v", err)
	}
	if got := reader.callCount(); got != 2 {
		t.Errorf("upstream reads after expiry = %d, want 2", got)
	}
}

// TestCache_ActivatedTenantBecomesRoutableWithinNegativeTTL is the §32 exit
// criterion.
//
// It models the worst case: a tenant is activated in the registry but the
// cache is never told. The stale absence must expire on its own within
// negative_ttl rather than persisting for the full positive TTL.
func TestCache_ActivatedTenantBecomesRoutableWithinNegativeTTL(t *testing.T) {
	reader := newCountingReader()
	cache, clock := newTestCache(t, reader, CacheOptions{
		RouteTTL:    60 * time.Second,
		NegativeTTL: 30 * time.Second,
	})

	ctx := context.Background()

	if _, err := cache.GetBySlug(ctx, "newco"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}

	// The tenant is created in the registry, and the cache is not invalidated
	// — as would happen if the mutation were made by another instance.
	reader.put(cachedRoute("newco", ProductPOS, StatusActive))

	if _, err := cache.GetBySlug(ctx, "newco"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the cached absence should still be served before it expires; got %v", err)
	}

	clock.Advance(31 * time.Second)

	route, err := cache.GetBySlug(ctx, "newco")
	if err != nil {
		t.Fatalf("a tenant activated more than negative_ttl ago is still unroutable: %v", err)
	}
	if route.Status != StatusActive {
		t.Errorf("Status = %s, want ACTIVE", route.Status)
	}
}

// TestCache_InvalidationIsImmediate is the §33 counterpart: when the mutation
// *does* come through this instance, the change must be visible on the very
// next request rather than a TTL later.
func TestCache_InvalidationIsImmediate(t *testing.T) {
	reader := newCountingReader()
	cache, _ := newTestCache(t, reader, CacheOptions{NegativeTTL: 30 * time.Second})

	ctx := context.Background()

	if _, err := cache.GetBySlug(ctx, "newco"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}

	reader.put(cachedRoute("newco", ProductPOS, StatusActive))
	cache.Invalidate("newco")

	route, err := cache.GetBySlug(ctx, "newco")
	if err != nil {
		t.Fatalf("invalidation did not take effect immediately: %v", err)
	}
	if route.Status != StatusActive {
		t.Errorf("Status = %s, want ACTIVE", route.Status)
	}
}

// --- singleflight ----------------------------------------------------------

// TestCache_ConcurrentMissesForOneSlugProduceOneRead is the stampede requirement.
func TestCache_ConcurrentMissesForOneSlugProduceOneRead(t *testing.T) {
	const goroutines = 50

	reader := newCountingReader(cachedRoute("mangala", ProductPOS, StatusActive))
	reader.gate = make(chan struct{})
	reader.entered = make(chan struct{}, goroutines)

	cache, _ := newTestCache(t, reader, CacheOptions{})

	var wg sync.WaitGroup
	results := make([]Route, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = cache.GetBySlug(context.Background(), "mangala")
		}(i)
	}

	// Wait until the first read is in flight, then let it finish. Any
	// goroutine that arrives after the store is served from the cache, so the
	// assertion below holds either way — the gate exists to make the overlap
	// real rather than incidental.
	<-reader.entered
	close(reader.gate)
	wg.Wait()

	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i].TenantSlug != "mangala" {
			t.Fatalf("goroutine %d got tenant %q, want mangala", i, results[i].TenantSlug)
		}
	}

	if got := reader.callCount(); got != 1 {
		t.Errorf("%d concurrent misses for one cold slug produced %d registry reads, want 1", goroutines, got)
	}
}

// TestCache_SingleflightIsKeyedBySlug proves a load is never shared across
// tenants.
//
// If the group were keyed by anything coarser — or not keyed at all — a request
// for one tenant could receive another tenant's route. That is the §31 failure
// mode, and it would be a cross-tenant data leak rather than a mere slowdown.
func TestCache_SingleflightIsKeyedBySlug(t *testing.T) {
	const perSlug = 50

	reader := newCountingReader(
		cachedRoute("mangala", ProductPOS, StatusActive),
		cachedRoute("garage1", ProductGMS, StatusActive),
	)
	reader.gate = make(chan struct{})
	reader.entered = make(chan struct{}, 2*perSlug)

	cache, _ := newTestCache(t, reader, CacheOptions{})

	type answer struct {
		slug    string
		product Product
		err     error
	}

	answers := make(chan answer, 2*perSlug)

	var wg sync.WaitGroup
	for _, slug := range []string{"mangala", "garage1"} {
		for i := 0; i < perSlug; i++ {
			wg.Add(1)
			go func(slug string) {
				defer wg.Done()
				route, err := cache.GetBySlug(context.Background(), slug)
				answers <- answer{slug: slug, product: route.Product, err: err}
			}(slug)
		}
	}

	<-reader.entered
	close(reader.gate)
	wg.Wait()
	close(answers)

	for a := range answers {
		if a.err != nil {
			t.Fatalf("%s: %v", a.slug, a.err)
		}

		want := ProductPOS
		if a.slug == "garage1" {
			want = ProductGMS
		}
		if a.product != want {
			t.Fatalf("%s received product %q, want %q — a load leaked across tenants",
				a.slug, a.product, want)
		}
	}

	if got := reader.callsFor("mangala"); got != 1 {
		t.Errorf("mangala upstream reads = %d, want 1", got)
	}
	if got := reader.callsFor("garage1"); got != 1 {
		t.Errorf("garage1 upstream reads = %d, want 1", got)
	}
}

// TestCache_CancelledLeaderDoesNotFailWaiters covers the reason the load runs
// with a detached context.
//
// singleflight.Do runs the load in the first caller's goroutine using that
// caller's context. If that client disconnects, every request waiting on the
// same slug would inherit its cancellation — one dropped connection would fail
// a whole cohort of unrelated requests. Detaching the load means the read
// completes for everyone, while each waiter still gives up when its own request
// goes away.
func TestCache_CancelledLeaderDoesNotFailWaiters(t *testing.T) {
	reader := newCountingReader(cachedRoute("mangala", ProductPOS, StatusActive))
	reader.gate = make(chan struct{})
	reader.entered = make(chan struct{}, 4)

	cache, _ := newTestCache(t, reader, CacheOptions{})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())

	leaderDone := make(chan error, 1)
	go func() {
		_, err := cache.GetBySlug(leaderCtx, "mangala")
		leaderDone <- err
	}()

	// The leader is now inside the gated upstream read.
	<-reader.entered

	waiterDone := make(chan error, 1)
	waiterRoute := make(chan Route, 1)
	go func() {
		route, err := cache.GetBySlug(context.Background(), "mangala")
		waiterRoute <- route
		waiterDone <- err
	}()

	// Give the waiter a moment to join the in-flight call, then drop the
	// leader.
	time.Sleep(50 * time.Millisecond)
	cancelLeader()

	close(reader.gate)

	// The leader's own request must fail: it was cancelled, and the caller
	// asked to stop.
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Errorf("leader error = %v, want context.Canceled", err)
	}

	// The waiter must succeed despite the leader having gone away.
	if err := <-waiterDone; err != nil {
		t.Fatalf("a cancelled leader failed an unrelated waiter: %v", err)
	}
	if route := <-waiterRoute; route.TenantSlug != "mangala" {
		t.Errorf("waiter got tenant %q, want mangala", route.TenantSlug)
	}
}

// TestCache_WaiterStopsWhenItsOwnRequestIsCancelled is the other half: a waiter
// must not be held hostage by a slow load it no longer cares about.
func TestCache_WaiterStopsWhenItsOwnRequestIsCancelled(t *testing.T) {
	reader := newCountingReader(cachedRoute("mangala", ProductPOS, StatusActive))
	reader.gate = make(chan struct{})
	reader.entered = make(chan struct{}, 4)

	cache, _ := newTestCache(t, reader, CacheOptions{})

	go func() { _, _ = cache.GetBySlug(context.Background(), "mangala") }()
	<-reader.entered

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := cache.GetBySlug(ctx, "mangala")
		waiterDone <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter kept waiting after its own request was cancelled")
	}

	close(reader.gate)
}

// --- the cache-aside race --------------------------------------------------

// TestCache_WriteInvalidatedDuringLoadIsDiscarded covers the interleaving that
// makes cache-aside subtly wrong.
//
// A request that misses reads the registry. If the route changes and is
// invalidated while that read is in flight, the answer it eventually writes
// describes a state that no longer exists. Storing it would serve stale data
// for a full TTL — a newly activated tenant would stay unroutable for up to
// route_ttl, which looks exactly like a cache bug and is miserable to
// reproduce.
func TestCache_WriteInvalidatedDuringLoadIsDiscarded(t *testing.T) {
	reader := newCountingReader(cachedRoute("mangala", ProductPOS, StatusProvisioning))
	reader.gate = make(chan struct{})
	reader.entered = make(chan struct{}, 4)

	cache, _ := newTestCache(t, reader, CacheOptions{})

	ctx := context.Background()

	// Start a read that will report PROVISIONING.
	firstDone := make(chan Route, 1)
	go func() {
		route, _ := cache.GetBySlug(ctx, "mangala")
		firstDone <- route
	}()

	<-reader.entered

	// The route is activated and invalidated while that read is in flight.
	reader.put(cachedRoute("mangala", ProductPOS, StatusActive))
	cache.Invalidate("mangala")

	// Now let the original read complete.
	close(reader.gate)
	stale := <-firstDone
	if stale.Status != StatusProvisioning {
		t.Fatalf("the in-flight read returned %s, want PROVISIONING", stale.Status)
	}

	// The stale answer must not have been cached.
	route, err := cache.GetBySlug(ctx, "mangala")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if route.Status != StatusActive {
		t.Errorf("the cache served the pre-invalidation answer %s; the write should have been discarded", route.Status)
	}
	if got := reader.callCount(); got != 2 {
		t.Errorf("upstream reads = %d, want 2 (the stale write must not satisfy the second read)", got)
	}
	if got := cache.Stats().StaleWrites; got != 1 {
		t.Errorf("StaleWrites = %d, want 1", got)
	}
}

// --- failure handling ------------------------------------------------------

// TestCache_RegistryFailureIsNotCached is the §46 requirement at the cache
// layer.
//
// "I could not ask" is not an answer. Caching it would turn a transient outage
// into a negative TTL of wrong answers, and would keep serving them after the
// registry recovered.
func TestCache_RegistryFailureIsNotCached(t *testing.T) {
	reader := newCountingReader()
	outage := errors.New("dial tcp 10.0.0.5:3306: connection refused")
	reader.setError(outage)

	cache, _ := newTestCache(t, reader, CacheOptions{})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := cache.GetBySlug(ctx, "mangala")
		if !errors.Is(err, outage) {
			t.Fatalf("read %d: error = %v, want the registry failure", i, err)
		}
	}

	if got := reader.callCount(); got != 3 {
		t.Errorf("upstream reads = %d, want 3: a failure must be retried, not cached", got)
	}

	// Recovery must be visible on the next request.
	reader.setError(nil)
	reader.put(cachedRoute("mangala", ProductPOS, StatusActive))

	route, err := cache.GetBySlug(ctx, "mangala")
	if err != nil {
		t.Fatalf("read after recovery: %v", err)
	}
	if route.Status != StatusActive {
		t.Errorf("Status = %s, want ACTIVE", route.Status)
	}
}

// TestCache_NotFoundIsCachedButFailureIsNot separates the two error cases that
// are easiest to conflate.
func TestCache_NotFoundIsCachedButFailureIsNot(t *testing.T) {
	reader := newCountingReader()
	cache, _ := newTestCache(t, reader, CacheOptions{})
	ctx := context.Background()

	// An absence: cached.
	if _, err := cache.GetBySlug(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if _, err := cache.GetBySlug(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if got := reader.callsFor("absent"); got != 1 {
		t.Errorf("an absence was not cached: %d upstream reads", got)
	}

	// A failure: not cached.
	reader.setError(errors.New("boom"))
	if _, err := cache.GetBySlug(ctx, "broken"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := cache.GetBySlug(ctx, "broken"); err == nil {
		t.Fatal("expected an error")
	}
	if got := reader.callsFor("broken"); got != 2 {
		t.Errorf("a failure was cached: %d upstream reads, want 2", got)
	}
}

// --- capacity --------------------------------------------------------------

// TestCache_RefusesToEvictLiveEntries covers the capacity policy.
//
// Evicting a live entry to make room would cause the very stampede the cache
// exists to prevent, because the entry chosen for eviction is one that is
// being actively requested. The cache degrades instead: the new slug is served
// uncached, which is slower but never wrong.
func TestCache_RefusesToEvictLiveEntries(t *testing.T) {
	reader := newCountingReader(
		cachedRoute("aaa", ProductPOS, StatusActive),
		cachedRoute("bbb", ProductPOS, StatusActive),
		cachedRoute("ccc", ProductPOS, StatusActive),
	)
	cache, _ := newTestCache(t, reader, CacheOptions{MaxEntries: 2})
	ctx := context.Background()

	if _, err := cache.GetBySlug(ctx, "aaa"); err != nil {
		t.Fatalf("aaa: %v", err)
	}
	if _, err := cache.GetBySlug(ctx, "bbb"); err != nil {
		t.Fatalf("bbb: %v", err)
	}

	if got := cache.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}

	// The cache is full of live entries. The third must still be answered.
	route, err := cache.GetBySlug(ctx, "ccc")
	if err != nil {
		t.Fatalf("ccc: %v", err)
	}
	if route.TenantSlug != "ccc" {
		t.Errorf("TenantSlug = %q, want ccc", route.TenantSlug)
	}

	if got := cache.Len(); got != 2 {
		t.Errorf("Len = %d, want 2: a live entry was evicted", got)
	}
	if got := cache.Stats().Overflowed; got != 1 {
		t.Errorf("Overflowed = %d, want 1", got)
	}

	// The live entries are untouched...
	for _, slug := range []string{"aaa", "bbb"} {
		if _, err := cache.GetBySlug(ctx, slug); err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		if got := reader.callsFor(slug); got != 1 {
			t.Errorf("%s was re-read: %d upstream reads, want 1", slug, got)
		}
	}

	// ...and the overflowed slug is served uncached rather than wrongly.
	if _, err := cache.GetBySlug(ctx, "ccc"); err != nil {
		t.Fatalf("ccc: %v", err)
	}
	if got := reader.callsFor("ccc"); got != 2 {
		t.Errorf("ccc upstream reads = %d, want 2 (it is not cached)", got)
	}
}

// TestCache_ExpiredEntriesAreSweptToMakeRoom proves the capacity limit does not
// permanently disable caching once the map has ever been full.
func TestCache_ExpiredEntriesAreSweptToMakeRoom(t *testing.T) {
	reader := newCountingReader(
		cachedRoute("aaa", ProductPOS, StatusActive),
		cachedRoute("bbb", ProductPOS, StatusActive),
		cachedRoute("ccc", ProductPOS, StatusActive),
	)
	cache, clock := newTestCache(t, reader, CacheOptions{
		RouteTTL:   60 * time.Second,
		MaxEntries: 2,
	})
	ctx := context.Background()

	if _, err := cache.GetBySlug(ctx, "aaa"); err != nil {
		t.Fatalf("aaa: %v", err)
	}
	if _, err := cache.GetBySlug(ctx, "bbb"); err != nil {
		t.Fatalf("bbb: %v", err)
	}

	clock.Advance(61 * time.Second)

	if _, err := cache.GetBySlug(ctx, "ccc"); err != nil {
		t.Fatalf("ccc: %v", err)
	}

	if got := cache.Stats().Overflowed; got != 0 {
		t.Errorf("Overflowed = %d, want 0: expired entries should have made room", got)
	}
	if got := cache.Len(); got != 1 {
		t.Errorf("Len = %d, want 1 after sweeping two expired entries", got)
	}
}

// --- Flush -----------------------------------------------------------------

// TestCache_FlushLeavesTheGatewayCorrect is the §92 authority requirement: the
// cache is never authoritative, so discarding everything must change nothing
// except the hit rate.
func TestCache_FlushLeavesTheGatewayCorrect(t *testing.T) {
	reader := newCountingReader(
		cachedRoute("mangala", ProductPOS, StatusActive),
		cachedRoute("garage1", ProductGMS, StatusActive),
	)
	cache, _ := newTestCache(t, reader, CacheOptions{})
	ctx := context.Background()

	if _, err := cache.GetBySlug(ctx, "mangala"); err != nil {
		t.Fatalf("mangala: %v", err)
	}
	if _, err := cache.GetBySlug(ctx, "garage1"); err != nil {
		t.Fatalf("garage1: %v", err)
	}

	cache.Flush()

	if got := cache.Len(); got != 0 {
		t.Fatalf("Len after Flush = %d, want 0", got)
	}

	route, err := cache.GetBySlug(ctx, "mangala")
	if err != nil {
		t.Fatalf("read after Flush: %v", err)
	}
	if route.Product != ProductPOS {
		t.Errorf("Product = %q, want POS", route.Product)
	}

	route, err = cache.GetBySlug(ctx, "garage1")
	if err != nil {
		t.Fatalf("read after Flush: %v", err)
	}
	if route.Product != ProductGMS {
		t.Errorf("Product = %q, want GMS", route.Product)
	}
}

// TestCache_FlushDuringLoadIsSafe exercises Flush under concurrency rather than
// in a quiet moment.
func TestCache_FlushDuringLoadIsSafe(t *testing.T) {
	reader := newCountingReader(cachedRoute("mangala", ProductPOS, StatusActive))
	cache, _ := newTestCache(t, reader, CacheOptions{})
	ctx := context.Background()

	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cache.GetBySlug(ctx, "mangala"); err != nil {
				t.Errorf("read: %v", err)
			}
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.Flush()
		}()
	}

	wg.Wait()

	route, err := cache.GetBySlug(ctx, "mangala")
	if err != nil {
		t.Fatalf("read after concurrent Flush: %v", err)
	}
	if route.TenantSlug != "mangala" {
		t.Errorf("TenantSlug = %q, want mangala", route.TenantSlug)
	}
}

// --- Service integration ---------------------------------------------------

// recordingInvalidator records the slugs the lifecycle service invalidated.
type recordingInvalidator struct {
	mu    sync.Mutex
	slugs []string
	calls int
}

func (r *recordingInvalidator) Invalidate(slug string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slugs = append(r.slugs, slug)
	r.calls++
}

func (r *recordingInvalidator) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.slugs))
	copy(out, r.slugs)
	return out
}

// TestService_EverySuccessfulMutationInvalidates is the §33 requirement.
//
// A mutation that does not invalidate leaves the public path serving the old
// answer for up to a TTL — a disabled tenant keeps being proxied to, or a newly
// activated one keeps returning 404.
func TestService_EverySuccessfulMutationInvalidates(t *testing.T) {
	actor := Actor{Service: "test", RequestID: "req-1"}

	t.Run("reserve", func(t *testing.T) {
		repo := newFakeRepository()
		inv := &recordingInvalidator{}
		svc := NewService(repo, testLogger()).WithInvalidator(inv)

		if _, err := svc.Reserve(context.Background(), "newco", ProductPOS, actor); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		assertInvalidated(t, inv, "newco")
	})

	t.Run("activate", func(t *testing.T) {
		repo := newFakeRepository()
		repo.seed(cachedRoute("newco", ProductPOS, StatusProvisioning))

		inv := &recordingInvalidator{}
		svc := NewService(repo, testLogger()).WithInvalidator(inv)

		if _, err := svc.Activate(context.Background(), "newco", nil, actor); err != nil {
			t.Fatalf("activate: %v", err)
		}
		assertInvalidated(t, inv, "newco")
	})

	t.Run("disable", func(t *testing.T) {
		repo := newFakeRepository()
		repo.seed(cachedRoute("newco", ProductPOS, StatusActive))

		inv := &recordingInvalidator{}
		svc := NewService(repo, testLogger()).WithInvalidator(inv)

		if _, err := svc.Disable(context.Background(), "newco", actor); err != nil {
			t.Fatalf("disable: %v", err)
		}
		assertInvalidated(t, inv, "newco")
	})

	t.Run("release", func(t *testing.T) {
		repo := newFakeRepository()
		repo.seed(cachedRoute("newco", ProductPOS, StatusActive))

		inv := &recordingInvalidator{}
		svc := NewService(repo, testLogger()).WithInvalidator(inv)

		if _, err := svc.Release(context.Background(), "newco", actor); err != nil {
			t.Fatalf("release: %v", err)
		}
		assertInvalidated(t, inv, "newco")
	})
}

// TestService_FailedMutationDoesNotInvalidate proves invalidation is tied to
// the write succeeding rather than to the attempt.
//
// Invalidating after a failed mutation would be harmless on its own, but it
// would hide the case where a caller assumed a change had been made.
func TestService_FailedMutationDoesNotInvalidate(t *testing.T) {
	repo := newFakeRepository()
	repo.reserveErr = errors.New("registry is down")

	inv := &recordingInvalidator{}
	svc := NewService(repo, testLogger()).WithInvalidator(inv)

	_, err := svc.Reserve(context.Background(), "newco", ProductPOS, Actor{Service: "test"})
	if err == nil {
		t.Fatal("expected the reserve to fail")
	}
	if got := inv.recorded(); len(got) != 0 {
		t.Errorf("a failed mutation invalidated %v", got)
	}
}

// TestService_NoInvalidatorIsSafe covers a deployment running without a cache.
func TestService_NoInvalidatorIsSafe(t *testing.T) {
	repo := newFakeRepository()
	svc := NewService(repo, testLogger())

	if _, err := svc.Reserve(context.Background(), "newco", ProductPOS, Actor{Service: "test"}); err != nil {
		t.Fatalf("reserve without an invalidator: %v", err)
	}
}

func assertInvalidated(t *testing.T, inv *recordingInvalidator, slug string) {
	t.Helper()

	got := inv.recorded()
	if len(got) != 1 {
		t.Fatalf("invalidations = %v, want exactly one", got)
	}
	if got[0] != slug {
		t.Errorf("invalidated %q, want %q", got[0], slug)
	}
}
