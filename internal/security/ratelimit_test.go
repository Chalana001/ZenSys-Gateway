package security

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock is the time seam.
//
// x/time/rate's Limiter takes the time as an argument on AllowN and TokensAt,
// so a bucket can be driven through a full refill without the test sleeping.
// That matters more than it looks: a limiter test written with real sleeps is
// either slow or flaky, and the property most worth asserting — that a key
// reappears after its refill horizon — is exactly the one a short sleep would
// get wrong.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestLimiter builds a limiter whose tiers read the given clock.
//
// The clock must be installed before any goroutine starts using the limiter,
// which is why this returns it rather than mutating it later.
func newTestLimiter(t *testing.T, cfg LimiterConfig) (*Limiter, *fakeClock) {
	t.Helper()

	l, err := NewLimiter(cfg)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}

	clk := &fakeClock{t: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)}
	if l.ip != nil {
		l.ip.now = clk.now
	}
	if l.tenant != nil {
		l.tenant.now = clk.now
	}
	return l, clk
}

// fastSpec is a bucket with a one-millisecond refill horizon, so a test can
// cross the horizon with a clock advance rather than a sleep.
func fastSpec(burst int) BucketSpec {
	return BucketSpec{RPS: 1000, Burst: burst}
}

func TestNewLimiter_RejectsMalformedSpecs(t *testing.T) {
	cases := []struct {
		name string
		cfg  LimiterConfig
	}{
		{"zero rps", LimiterConfig{Enabled: true, IP: BucketSpec{RPS: 0, Burst: 1}, Tenant: fastSpec(1)}},
		{"negative rps", LimiterConfig{Enabled: true, IP: BucketSpec{RPS: -1, Burst: 1}, Tenant: fastSpec(1)}},
		{"zero burst", LimiterConfig{Enabled: true, IP: BucketSpec{RPS: 1, Burst: 0}, Tenant: fastSpec(1)}},
		{"negative burst", LimiterConfig{Enabled: true, IP: BucketSpec{RPS: 1, Burst: -1}, Tenant: fastSpec(1)}},
		{"tenant rps zero", LimiterConfig{Enabled: true, IP: fastSpec(1), Tenant: BucketSpec{RPS: 0, Burst: 1}}},
		{"tenant burst zero", LimiterConfig{Enabled: true, IP: fastSpec(1), Tenant: BucketSpec{RPS: 1, Burst: 0}}},
		{"negative max entries", LimiterConfig{Enabled: true, IP: fastSpec(1), Tenant: fastSpec(1), MaxEntries: -1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, err := NewLimiter(tc.cfg)
			if err == nil {
				t.Fatalf("NewLimiter accepted %+v; a malformed limit must be refused at construction rather than producing a limiter that behaves in some unspecified way", tc.cfg)
			}
			if l != nil {
				t.Errorf("NewLimiter returned a limiter alongside an error; a caller that ignores the error would then hold a partially built one")
			}
		})
	}
}

func TestNewLimiter_DisabledNeedsNoSpecs(t *testing.T) {
	// A disabled limiter builds nothing from the specs, so zero values are
	// fine. Refusing them would force every deployment that does not want rate
	// limiting to write plausible-looking numbers it does not use.
	l, err := NewLimiter(LimiterConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewLimiter(disabled): %v", err)
	}
	if l.Enabled() {
		t.Error("a disabled limiter reports itself enabled")
	}
	if d := l.AllowIP("1.2.3.4"); !d.Allowed {
		t.Error("a disabled limiter refused a request; disabled must mean pass-through")
	}
	if d := l.AllowTenant("mangala"); !d.Allowed {
		t.Error("a disabled limiter refused a tenant; disabled must mean pass-through")
	}
}

func TestLimiter_NilAllowsEverything(t *testing.T) {
	// A nil limiter is what a caller that forgot to wire one holds. It allows,
	// because this is an availability control and not an authorisation one —
	// and because the alternative turns a wiring mistake into a total outage.
	var l *Limiter
	if !l.AllowIP("1.2.3.4").Allowed {
		t.Error("a nil limiter refused a request")
	}
	if !l.AllowTenant("mangala").Allowed {
		t.Error("a nil limiter refused a tenant")
	}
	if l.Enabled() {
		t.Error("a nil limiter reports itself enabled")
	}
	if total, _ := l.trackedKeys(TierIP); total != 0 {
		t.Error("a nil limiter reported tracked keys")
	}
}

func TestLimiter_AllowsUpToBurstThenRefuses(t *testing.T) {
	const burst = 5
	l, _ := newTestLimiter(t, LimiterConfig{
		Enabled: true, IP: BucketSpec{RPS: 1, Burst: burst}, Tenant: fastSpec(1),
	})

	for i := 1; i <= burst; i++ {
		if d := l.AllowIP("203.0.113.7"); !d.Allowed {
			t.Fatalf("request %d of %d refused; a new bucket starts full, so the first `burst` requests must be allowed", i, burst)
		}
	}

	d := l.AllowIP("203.0.113.7")
	if d.Allowed {
		t.Fatalf("request %d was allowed; the bucket held %d tokens and all were spent", burst+1, burst)
	}
	if d.Tier != TierIP {
		t.Errorf("refusal names tier %q, want %q: the log needs to say which tier refused", d.Tier, TierIP)
	}
	if d.RetryAfter <= 0 {
		t.Error("refusal reported no retry delay, which would produce a Retry-After of 0 — an instruction to retry immediately")
	}
}

func TestLimiter_RefillsOverTime(t *testing.T) {
	l, clk := newTestLimiter(t, LimiterConfig{
		Enabled: true, IP: BucketSpec{RPS: 1, Burst: 1}, Tenant: fastSpec(1),
	})

	if !l.AllowIP("203.0.113.7").Allowed {
		t.Fatal("the first request was refused; a new bucket starts full")
	}
	if l.AllowIP("203.0.113.7").Allowed {
		t.Fatal("the second request was allowed; the bucket held one token")
	}

	// One token per second, so half a second is not enough and a whole one is.
	clk.advance(500 * time.Millisecond)
	if l.AllowIP("203.0.113.7").Allowed {
		t.Error("a request was allowed after half a refill; at rps=1 half a second earns half a token")
	}

	clk.advance(600 * time.Millisecond)
	if !l.AllowIP("203.0.113.7").Allowed {
		t.Error("a request was refused after a full refill; the bucket should have regained its token")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(t, LimiterConfig{
		Enabled: true, IP: BucketSpec{RPS: 1, Burst: 1}, Tenant: fastSpec(1),
	})

	if !l.AllowIP("203.0.113.7").Allowed {
		t.Fatal("first request for the first key was refused")
	}
	if l.AllowIP("203.0.113.7").Allowed {
		t.Fatal("second request for the first key was allowed; its bucket held one token")
	}

	// A different key must be unaffected. This is the whole point of keying
	// the tier: one abusive address must not consume another's allowance.
	if !l.AllowIP("203.0.113.8").Allowed {
		t.Error("a second key was refused because the first key had spent its token; the buckets are not independent")
	}
}

func TestLimiter_EmptyKeySharesOneBucketRatherThanEscaping(t *testing.T) {
	// An unattributable request must be limited, not exempt. If the empty key
	// were skipped, whatever causes the address to be missing would also be a
	// way to bypass tier 1 entirely.
	l, _ := newTestLimiter(t, LimiterConfig{
		Enabled: true, IP: BucketSpec{RPS: 1, Burst: 2}, Tenant: fastSpec(1),
	})

	if !l.AllowIP("").Allowed {
		t.Fatal("the first unattributable request was refused; a new bucket starts full")
	}
	if !l.AllowIP("").Allowed {
		t.Fatal("the second unattributable request was refused; the bucket held two tokens")
	}
	if l.AllowIP("").Allowed {
		t.Error("the third unattributable request was allowed; an empty key must share one bucket, not bypass the tier")
	}
}

func TestLimiter_RetryAfterSecondsIsNeverZero(t *testing.T) {
	cases := []struct {
		name string
		d    Decision
		want int
	}{
		{"allowed reports nothing", Decision{Allowed: true, RetryAfter: time.Second}, 0},
		{"zero delay rounds to one", Decision{RetryAfter: 0}, 1},
		{"a negative delay rounds to one", Decision{RetryAfter: -time.Second}, 1},
		{"a sub-second delay rounds up to one", Decision{RetryAfter: time.Millisecond}, 1},
		{"exactly one second stays one", Decision{RetryAfter: time.Second}, 1},
		{"a hair over one second rounds up to two", Decision{RetryAfter: time.Second + time.Nanosecond}, 2},
		{"ten and a half seconds rounds up to eleven", Decision{RetryAfter: 10500 * time.Millisecond}, 11},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.RetryAfterSeconds(); got != tc.want {
				t.Errorf("RetryAfterSeconds() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLimiter_TenantTierIsSeparateFromTheAddressTier(t *testing.T) {
	l, _ := newTestLimiter(t, LimiterConfig{
		Enabled: true,
		IP:      BucketSpec{RPS: 1, Burst: 1},
		Tenant:  BucketSpec{RPS: 1, Burst: 2},
	})

	if !l.AllowIP("203.0.113.7").Allowed {
		t.Fatal("tier 1 refused its first request")
	}
	if l.AllowIP("203.0.113.7").Allowed {
		t.Fatal("tier 1 allowed a second request; it held one token")
	}

	// Tier 2 must be untouched by tier 1's exhaustion, and must have its own
	// burst rather than inheriting tier 1's.
	if !l.AllowTenant("mangala").Allowed {
		t.Fatal("tier 2 refused its first request because tier 1 was exhausted; the tiers share state")
	}
	if !l.AllowTenant("mangala").Allowed {
		t.Fatal("tier 2 refused its second request; its burst is 2")
	}
	if l.AllowTenant("mangala").Allowed {
		t.Error("tier 2 allowed a third request; its burst is 2")
	}

	// And the refusal must name the tier that made it, or an operator cannot
	// tell a noisy tenant from a noisy address.
	d := l.AllowTenant("mangala")
	if d.Tier != TierTenant {
		t.Errorf("refusal names tier %q, want %q", d.Tier, TierTenant)
	}
}

// TestLimiter_KeySpaceIsBounded is the load-bearing test of this file.
//
// A keyed limiter whose table grows with the number of distinct keys it has
// seen is a memory exhaustion vector, and its keys are chosen by whoever is
// sending requests. This drives far more distinct keys through one tier than
// its cap allows and asserts the table never exceeds it.
//
// The clock is frozen deliberately. With time passing, the sweep would free
// entries and the table would stay small for the wrong reason — the test would
// pass even if the cap did nothing. A frozen clock means every entry is live,
// so the only thing that can hold the table down is the cap itself.
func TestLimiter_KeySpaceIsBounded(t *testing.T) {
	const maxEntries = 64
	l, _ := newTestLimiter(t, LimiterConfig{
		Enabled: true, IP: fastSpec(4), Tenant: fastSpec(4), MaxEntries: maxEntries,
	})

	perShard := l.perShardCap(TierIP)
	if perShard*numShards < maxEntries {
		t.Fatalf("per-shard cap %d × %d shards = %d is below the configured bound %d; the cap must be at least the bound, or the bound is a lie",
			perShard, numShards, perShard*numShards, maxEntries)
	}

	const distinctKeys = 100000
	for i := 0; i < distinctKeys; i++ {
		l.AllowIP(fmt.Sprintf("203.0.113.%d", i))

		if i%1000 == 0 {
			total, largest := l.trackedKeys(TierIP)
			if total > perShard*numShards {
				t.Fatalf("after %d distinct keys the tier is holding %d entries, above its cap of %d: the table grows with the key space",
					i+1, total, perShard*numShards)
			}
			if largest > perShard {
				t.Fatalf("after %d distinct keys a single shard is holding %d entries, above its cap of %d",
					i+1, largest, perShard)
			}
		}
	}

	total, largest := l.trackedKeys(TierIP)
	if total > perShard*numShards {
		t.Errorf("final table holds %d entries, above the cap of %d", total, perShard*numShards)
	}
	if largest > perShard {
		t.Errorf("largest shard holds %d entries, above the per-shard cap of %d", largest, perShard)
	}
	if total == 0 {
		t.Error("the table is empty after 100000 keys, which means the sweep is removing live entries rather than the cap holding the table down")
	}
}

// shardKeys returns the keys a tier's shard is currently holding.
//
// The test reaches into the shard directly because the property under test —
// which entries a sweep removed — is not observable through AllowIP, which
// answers only yes or no.
func shardKeys(l *Limiter, s int) map[string]bool {
	sh := &l.ip.shards[s]
	sh.mu.Lock()
	defer sh.mu.Unlock()

	out := make(map[string]bool, len(sh.entries))
	for k := range sh.entries {
		out[k] = true
	}
	return out
}

// TestLimiter_EvictsRefilledEntriesBeforeLiveOnes pins the eviction rule.
//
// An entry whose bucket has fully refilled is indistinguishable from one that
// does not exist, so it is the right thing to drop. If eviction were purely
// "oldest first", a saturated tier would discard live buckets while refilled
// ones sat in the table — and the difference only shows up under load, which
// is the worst time to discover it.
//
// The test also pins the *scope* of a sweep, which is deliberate: a sweep
// covers only the shard being inserted into, not the whole tier. Sweeping the
// tier would make the cost of one insertion proportional to the whole table,
// so instead refilled entries in untouched shards are reclaimed lazily, the
// next time that shard is written to. Memory is bounded either way — by the
// cap, which TestLimiter_KeySpaceIsBounded asserts — so the choice is purely
// about what an insertion costs.
func TestLimiter_EvictsRefilledEntriesBeforeLiveOnes(t *testing.T) {
	// 32 entries over 16 shards gives a per-shard cap of 2, which is the
	// smallest cap that can distinguish "swept the refilled entries" (the
	// shard ends with 1) from "evicted the single oldest" (it ends with 2).
	const maxEntries = 32
	l, clk := newTestLimiter(t, LimiterConfig{
		Enabled: true, IP: fastSpec(1), Tenant: fastSpec(1), MaxEntries: maxEntries,
	})

	perShard := l.perShardCap(TierIP)
	if perShard != 2 {
		t.Fatalf("per-shard cap is %d, want 2; the assertions below depend on a cap of exactly 2", perShard)
	}

	// Collect perShard+1 keys for every shard, so each shard can be filled to
	// its cap and then given one more key that lands in the same shard.
	byShard := make([][]string, numShards)
	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("198.51.100.%d", i)
		s := shardIndex(key)
		if len(byShard[s]) <= perShard {
			byShard[s] = append(byShard[s], key)
		}
	}
	for s := range byShard {
		if len(byShard[s]) <= perShard {
			t.Fatalf("could not find %d keys for shard %d; the hash is not distributing", perShard+1, s)
		}
	}

	// Fill every shard to its cap. rps=1000 with burst=1 gives a
	// one-millisecond refill horizon, and the clock is still.
	for s := 0; s < numShards; s++ {
		for i := 0; i < perShard; i++ {
			l.AllowIP(byShard[s][i])
		}
		if got := len(shardKeys(l, s)); got != perShard {
			t.Fatalf("shard %d holds %d entries after filling it to %d", s, got, perShard)
		}
	}

	// Cross the refill horizon, so every entry currently in the table is
	// indistinguishable from an absent one.
	clk.advance(10 * time.Millisecond)

	// Write to every shard except shard 0. Each write finds its shard full,
	// sweeps, and should end up holding only the key just written.
	for s := 1; s < numShards; s++ {
		newKey := byShard[s][perShard]
		l.AllowIP(newKey)

		keys := shardKeys(l, s)
		if len(keys) != 1 {
			t.Errorf("shard %d holds %d entries, want 1: the two refilled entries should have been swept rather than one of them being chosen as the oldest to evict", s, len(keys))
		}
		if !keys[newKey] {
			t.Errorf("shard %d does not hold the key just written to it; the sweep removed the wrong entry", s)
		}
	}

	// Shard 0 was not written to, so its refilled entries are still there.
	// This is the lazy half of the rule, asserted rather than left implicit:
	// the memory is still bounded by the cap, but the reclaim waits for the
	// next write to that shard.
	if got := len(shardKeys(l, 0)); got != perShard {
		t.Errorf("untouched shard 0 holds %d entries, want %d: a sweep covers only the shard being inserted into, so an untouched shard keeps its refilled entries until it is written to", got, perShard)
	}

	// And writing to it reclaims them.
	l.AllowIP(byShard[0][perShard])
	if got := len(shardKeys(l, 0)); got != 1 {
		t.Errorf("shard 0 holds %d entries after being written to, want 1: the sweep should have reclaimed its refilled entries on the next write", got)
	}
}

// TestLimiter_ForcedEvictionDropsTheStalestLiveEntry exercises the second half
// of the eviction rule, which the sweep does not reach.
//
// When every entry in a saturated shard is live, the sweep frees nothing and
// one entry must be chosen and dropped. This is the case that decides whether
// the choice is by staleness or by recency, and it is only reachable when
// nothing is refilled — which is why the other eviction tests, where the sweep
// always fires first, cannot assert it. Confirmed by breaking the comparison
// to pick the newest entry: this test fails and they do not.
func TestLimiter_ForcedEvictionDropsTheStalestLiveEntry(t *testing.T) {
	const maxEntries = 32
	l, clk := newTestLimiter(t, LimiterConfig{
		// rps=1 with burst=1 gives a one-second horizon, so entries stay live
		// across the half-second step below.
		Enabled: true, IP: BucketSpec{RPS: 1, Burst: 1}, Tenant: fastSpec(1),
		MaxEntries: maxEntries,
	})

	if got := l.perShardCap(TierIP); got != 2 {
		t.Fatalf("per-shard cap is %d, want 2", got)
	}

	target := shardIndex("203.0.113.7")
	keys := []string{}
	for i := 0; i < 10000 && len(keys) < 3; i++ {
		key := fmt.Sprintf("198.51.100.%d", i)
		if shardIndex(key) == target {
			keys = append(keys, key)
		}
	}
	if len(keys) < 3 {
		t.Fatalf("could not find three keys in shard %d", target)
	}
	stalest, newer, newcomer := keys[0], keys[1], keys[2]

	if !l.AllowIP(stalest).Allowed {
		t.Fatal("the first request for the stalest key was refused")
	}
	clk.advance(500 * time.Millisecond)
	if !l.AllowIP(newer).Allowed {
		t.Fatal("the first request for the newer key was refused")
	}

	// Half a second has passed, so neither entry has refilled and the sweep
	// below will free nothing. Writing a third key therefore forces the
	// oldest entry to be chosen and dropped.
	l.AllowIP(newcomer)

	held := shardKeys(l, target)
	if len(held) != 2 {
		t.Fatalf("shard %d holds %d entries, want 2", target, len(held))
	}
	if held[stalest] {
		t.Error("the stalest entry survived a forced eviction; with nothing refilled the choice must be the entry seen longest ago")
	}
	if !held[newer] {
		t.Error("the newer entry was dropped in preference to the staler one; the choice is being made by recency")
	}

	// The surviving entry's spent token must have survived with it.
	if d := l.AllowIP(newer); d.Allowed {
		t.Error("the surviving entry was recreated rather than kept: its spent token was forgotten")
	}
}

// TestLimiter_LiveEntrySurvivesWhenARefilledNeighbourIsEvicted pins the
// sweep's preference between the two kinds of entry.
//
// Be precise about what this establishes. With one bucket spec per tier the
// refill horizon is identical for every key, so a refilled entry is
// *necessarily* the older of any refilled/live pair — which means this test
// cannot distinguish "sweep the refilled entries" from "evict the single
// oldest", because both drop the same entry here. The sweep's distinct
// behaviour, dropping *every* refilled entry rather than one, is asserted by
// TestLimiter_EvictsRefilledEntriesBeforeLiveOnes, and the choice between two
// live entries is asserted by
// TestLimiter_ForcedEvictionDropsTheStalestLiveEntry.
//
// What this one adds is the mixed case in isolation: a key that has fully
// refilled is the one sacrificed, and a key under active load keeps both its
// entry and its spent allowance.
func TestLimiter_LiveEntrySurvivesWhenARefilledNeighbourIsEvicted(t *testing.T) {
	const maxEntries = 32
	l, clk := newTestLimiter(t, LimiterConfig{
		Enabled: true, IP: BucketSpec{RPS: 1, Burst: 1}, Tenant: fastSpec(1),
		MaxEntries: maxEntries,
	})

	if got := l.perShardCap(TierIP); got != 2 {
		t.Fatalf("per-shard cap is %d, want 2", got)
	}

	target := shardIndex("203.0.113.7")
	keys := []string{}
	for i := 0; i < 10000 && len(keys) < 3; i++ {
		key := fmt.Sprintf("198.51.100.%d", i)
		if shardIndex(key) == target {
			keys = append(keys, key)
		}
	}
	if len(keys) < 3 {
		t.Fatalf("could not find three keys in shard %d", target)
	}
	refillable, live, newcomer := keys[0], keys[1], keys[2]

	if !l.AllowIP(refillable).Allowed {
		t.Fatal("the first request for the refillable key was refused")
	}

	// Cross the horizon, so the refillable key is indistinguishable from
	// absent, then create the live key — newer, and holding no tokens.
	clk.advance(2 * time.Second)
	if !l.AllowIP(live).Allowed {
		t.Fatal("the first request for the live key was refused")
	}
	if got := len(shardKeys(l, target)); got != 2 {
		t.Fatalf("shard %d holds %d entries, want 2 (the cap)", target, got)
	}

	l.AllowIP(newcomer)

	held := shardKeys(l, target)
	if len(held) != 2 {
		t.Fatalf("shard %d holds %d entries after the eviction, want 2", target, len(held))
	}
	if held[refillable] {
		t.Error("the refilled entry survived; it is indistinguishable from an absent entry and is the entry to drop")
	}
	if !held[live] {
		t.Error("the live entry was dropped while a refilled one was kept")
	}

	// The live entry's spent allowance must have survived with it. Had the
	// entry been dropped and recreated, this would be allowed.
	if d := l.AllowIP(live); d.Allowed {
		t.Error("the live entry was recreated rather than kept: its spent token was forgotten and it was handed its whole burst again")
	}
}

func TestLimiter_ShardIndexIsStableAndInRange(t *testing.T) {
	// The index is a mask, so it is in range by construction; what this pins
	// is determinism. A key must always land in the same shard, or its bucket
	// would be created in one shard and looked up in another and the limiter
	// would allow roughly `numShards` times its burst.
	for _, key := range []string{"", "203.0.113.7", "mangala", "garage1", "a", "::1"} {
		first := shardIndex(key)
		if first < 0 || first >= numShards {
			t.Fatalf("shardIndex(%q) = %d, outside [0,%d)", key, first, numShards)
		}
		for i := 0; i < 10; i++ {
			if got := shardIndex(key); got != first {
				t.Fatalf("shardIndex(%q) returned %d then %d; the index must be a pure function of the key", key, first, got)
			}
		}
	}

	// And distinct keys should not all collapse into one shard, or the
	// sharding would buy nothing.
	seen := make(map[int]bool)
	for i := 0; i < 200; i++ {
		seen[shardIndex(fmt.Sprintf("203.0.113.%d", i))] = true
	}
	if len(seen) < numShards/2 {
		t.Errorf("200 distinct keys occupied only %d of %d shards; the hash is clustering and the lock is not being shared", len(seen), numShards)
	}
}

func TestLimiter_ConcurrentRequestsOnOneKeySpendExactlyTheBurst(t *testing.T) {
	// This asserts the atomicity of lookup-insert-check. If two goroutines
	// could each create a bucket for the same unseen key, the second would
	// overwrite the first and the key would get up to two bursts — so the
	// allowed count would exceed the burst rather than equal it.
	const (
		burst      = 100
		goroutines = 50
		perRoutine = 20
	)

	l, _ := newTestLimiter(t, LimiterConfig{
		// rps=1 and a frozen clock: nothing refills during the test, so the
		// number of allowed requests is exactly the burst.
		Enabled: true, IP: BucketSpec{RPS: 1, Burst: burst}, Tenant: fastSpec(1),
	})

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	start := make(chan struct{})

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perRoutine; i++ {
				if l.AllowIP("203.0.113.7").Allowed {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	if allowed != burst {
		t.Errorf("%d of %d concurrent requests on one key were allowed, want exactly %d: a bucket must be created once per key, and its burst is the only allowance there is",
			allowed, goroutines*perRoutine, burst)
	}
}

func TestLimiter_ConcurrentUseAcrossManyKeysIsSafe(t *testing.T) {
	// Exercises the shards in parallel under the race detector. The assertion
	// is deliberately weak — every key gets its own full burst, so the exact
	// count is not the point; the point is that nothing corrupts.
	const (
		keys       = 200
		burst      = 5
		goroutines = 8
	)

	l, _ := newTestLimiter(t, LimiterConfig{
		Enabled: true,
		IP:      BucketSpec{RPS: 1, Burst: burst},
		Tenant:  BucketSpec{RPS: 1, Burst: burst},
	})

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < keys; i++ {
				l.AllowIP(fmt.Sprintf("203.0.113.%d", i))
				l.AllowTenant(fmt.Sprintf("tenant-%d", i))
			}
		}()
	}
	wg.Wait()

	// Every key's first request must have been allowed regardless of
	// contention, so each key's bucket was created and starts full.
	if !l.AllowIP("198.51.100.99").Allowed {
		t.Error("a fresh key was refused after concurrent use; a new bucket starts full")
	}
}

func TestLimiter_MaxEntriesDefaultsRatherThanBeingUnbounded(t *testing.T) {
	// Zero must not mean unlimited. It is the value a caller gets by leaving
	// the field out, and "unlimited" is the one thing this table must never
	// be.
	l, err := NewLimiter(LimiterConfig{
		Enabled: true, IP: fastSpec(1), Tenant: fastSpec(1),
	})
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	if got := l.perShardCap(TierIP) * numShards; got < defaultMaxEntries {
		t.Errorf("default capacity is %d, below defaultMaxEntries %d; a zero MaxEntries must resolve to the default, not to nothing", got, defaultMaxEntries)
	}
}
