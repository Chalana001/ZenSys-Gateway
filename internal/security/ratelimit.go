package security

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Two-tier rate limiting (architecture doc §81, plan §8.2).
//
// The two tiers exist because a single one cannot do both jobs. A limiter
// keyed by tenant sits after tenant resolution, so an attacker hammering
// unknown hostnames reaches the registry on every request — the negative cache
// blunts that but does not remove it. A limiter keyed only by address lets one
// noisy tenant behind a shared NAT starve its neighbours, because every one of
// them arrives from the same address.
//
// So tier 1 is keyed by client address and runs before any lookup, and tier 2
// is keyed by tenant and runs after the tenant is known. Neither is a security
// boundary in the sense that authentication is: rate limiting protects
// availability, and it is deliberately the weakest control in this package.
//
// # The bound
//
// The obvious implementation of a keyed limiter — a map from key to token
// bucket — grows without limit, and its keys are chosen by whoever is sending
// requests. That is the same defect as an unbounded metric label: it converts
// a request stream into unbounded memory. Every keyed table here is therefore
// bounded, and the eviction rule is chosen so that the bound does not quietly
// break the limiter for legitimate clients.
//
// This is the load-bearing property of this file. A test pushes a hundred
// thousand distinct keys through one limiter and asserts the table never
// exceeds its cap (TestLimiter_KeySpaceIsBounded).

// Tier names. They appear in the rate-limit decision and in the access log, so
// they are constants rather than literals at each use site.
const (
	// TierIP is the coarse tier, keyed by client address.
	TierIP = "ip"

	// TierTenant is the fairness tier, keyed by tenant slug.
	TierTenant = "tenant"
)

// numShards is how many independently locked tables the key space is split
// into.
//
// A single mutex around one map would serialise every request in the process
// through one lock, which on the public listener is the hottest path there is.
// Sharding by key makes contention depend on how many *distinct* keys are in
// flight rather than on total request rate, so a burst against one tenant does
// not queue behind a burst against another.
//
// 16 is chosen because it is a power of two (so the index is a mask rather
// than a modulo) and comfortably more than the number of cores this service is
// expected to run on. It is not tuned, and it does not need to be: the lock is
// held for a map lookup and a few float operations.
const numShards = 16

// BucketSpec describes one token bucket.
//
// It is defined here rather than reusing config.LimitConfig for the same
// reason TokenSpec does not reuse config.TokenConfig: the configuration model
// and the runtime model are allowed to diverge, and a shared type would make
// that a breaking change instead of a local one.
type BucketSpec struct {
	// RPS is the sustained rate in requests per second.
	RPS float64

	// Burst is the number of requests that may arrive at once.
	Burst int
}

// Valid reports whether a limiter can be built from the spec.
func (s BucketSpec) Valid() bool {
	return s.RPS > 0 && !math.IsInf(s.RPS, 0) && !math.IsNaN(s.RPS) && s.Burst >= 1
}

// LimiterConfig is the configuration for the two-tier limiter.
type LimiterConfig struct {
	// Enabled false builds a limiter that allows everything. It is not the
	// same as a limiter with enormous limits: a disabled limiter allocates
	// nothing and costs one nil check per request.
	Enabled bool

	IP     BucketSpec
	Tenant BucketSpec

	// MaxEntries bounds the number of keys each tier remembers. When it is
	// zero a default is used; it is never treated as "unbounded".
	MaxEntries int
}

// defaultMaxEntries bounds each tier's table when the caller does not say.
//
// 4096 keys per tier is a few hundred kilobytes at most and is far more than
// the number of clients or tenants that will be active within one refill
// period. A Gateway serving more distinct clients than this in a refill window
// is either under a distributed attack or sized for a much larger MaxEntries,
// and in both cases the table being full is the correct behaviour rather than
// a silent expansion.
const defaultMaxEntries = 4096

// Decision is the outcome of one rate-limit check.
type Decision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool

	// Tier names which limiter refused, and is empty when Allowed is true.
	Tier string

	// RetryAfter is how long until the request would be permitted. It is zero
	// when Allowed is true.
	RetryAfter time.Duration
}

// RetryAfterSeconds returns RetryAfter as a whole number of seconds suitable
// for a Retry-After header.
//
// Two rules, both of which are response rules rather than arithmetic, and both
// of which have been got wrong by someone:
//
//   - It rounds *up*, and never reports zero for a refusal. A Retry-After of 0
//     tells the client to retry immediately, which is an instruction to spin —
//     so a refused request that happens to become admissible within the
//     current second still reports 1.
//   - It is at least 1 even when the bucket refills in a millisecond, because
//     the header has one-second resolution and "less than a second" cannot be
//     expressed in it.
//
// A caller must not compute this itself; the two rules are easy to omit and
// the result is a client that hammers harder the more it is throttled. The
// zero return is reserved for the allowed case, where the caller must not set
// the header at all.
func (d Decision) RetryAfterSeconds() int {
	if d.Allowed {
		return 0
	}
	secs := int(math.Ceil(d.RetryAfter.Seconds()))
	if secs < 1 {
		return 1
	}
	return secs
}

// Limiter is the two-tier rate limiter.
//
// The zero value allows everything, and so does a nil *Limiter. That direction
// is deliberate: this is an availability control, not an authorisation one, so
// a caller that fails to wire a limiter gets an unlimited Gateway rather than
// one that refuses every request. The startup log always states the configured
// limits, so "no limiting" is visible rather than inferred.
type Limiter struct {
	ip     *keyedLimiter
	tenant *keyedLimiter

	ipSpec     BucketSpec
	tenantSpec BucketSpec
}

// NewLimiter builds the two-tier limiter.
//
// A disabled limiter needs no valid specs, because nothing is built from them.
// An enabled limiter requires both specs to be valid and reports every problem
// at once, for the same reason config.Validate aggregates: fixing a
// configuration one error per restart is a miserable loop.
func NewLimiter(cfg LimiterConfig) (*Limiter, error) {
	if !cfg.Enabled {
		return &Limiter{}, nil
	}

	var errs []error
	if !cfg.IP.Valid() {
		errs = append(errs, fmt.Errorf("rate limit tier %s: rps must be a positive number and burst must be at least 1, got rps=%v burst=%d",
			TierIP, cfg.IP.RPS, cfg.IP.Burst))
	}
	if !cfg.Tenant.Valid() {
		errs = append(errs, fmt.Errorf("rate limit tier %s: rps must be a positive number and burst must be at least 1, got rps=%v burst=%d",
			TierTenant, cfg.Tenant.RPS, cfg.Tenant.Burst))
	}
	if cfg.MaxEntries < 0 {
		errs = append(errs, fmt.Errorf("rate limit max entries must not be negative, got %d", cfg.MaxEntries))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	maxEntries := cfg.MaxEntries
	if maxEntries == 0 {
		maxEntries = defaultMaxEntries
	}

	return &Limiter{
		ip:         newKeyedLimiter(cfg.IP, maxEntries),
		tenant:     newKeyedLimiter(cfg.Tenant, maxEntries),
		ipSpec:     cfg.IP,
		tenantSpec: cfg.Tenant,
	}, nil
}

// Enabled reports whether any tier is active.
func (l *Limiter) Enabled() bool {
	return l != nil && l.ip != nil
}

// IPSpec returns the configured tier-1 bucket, for the startup log.
func (l *Limiter) IPSpec() BucketSpec {
	if l == nil {
		return BucketSpec{}
	}
	return l.ipSpec
}

// TenantSpec returns the configured tier-2 bucket, for the startup log.
func (l *Limiter) TenantSpec() BucketSpec {
	if l == nil {
		return BucketSpec{}
	}
	return l.tenantSpec
}

// AllowIP checks tier 1 for a client address.
//
// An empty address is not an error and does not bypass the tier: it is a key
// like any other, so every request the Gateway cannot attribute shares one
// bucket. Failing closed matters here because the alternative — treating an
// unattributable request as unlimited — makes the limiter skippable by
// whatever causes the address to be missing. In practice the address is
// unparseable only if RemoteAddr is malformed, which no supported listener
// produces.
func (l *Limiter) AllowIP(clientIP string) Decision {
	if l == nil || l.ip == nil {
		return Decision{Allowed: true}
	}
	return l.ip.allow(clientIP, TierIP)
}

// AllowTenant checks tier 2 for a tenant slug.
//
// The caller must have resolved the tenant already. A slug that did not
// resolve has no route and is refused by the routing pipeline before this is
// reached, so there is no such thing as a tier-2 check for an unknown tenant.
func (l *Limiter) AllowTenant(slug string) Decision {
	if l == nil || l.tenant == nil {
		return Decision{Allowed: true}
	}
	return l.tenant.allow(slug, TierTenant)
}

// keyedLimiter is one tier: a bounded, sharded table of token buckets.
type keyedLimiter struct {
	rps    rate.Limit
	burst  int
	refill time.Duration
	cap    int
	now    func() time.Time

	shards [numShards]shard
}

// shard is one independently locked partition of a tier's key space.
type shard struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// entry is one key's bucket.
//
// lastSeen is the time of the key's most recent request, and it is what makes
// eviction principled rather than arbitrary: see keyedLimiter.evict.
type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// newKeyedLimiter builds one tier.
func newKeyedLimiter(spec BucketSpec, maxEntries int) *keyedLimiter {
	// Round the per-shard share up, so the total is at least maxEntries even
	// when it is not a multiple of numShards. The bound that matters is the
	// total, and a test asserts it against the total.
	perShard := (maxEntries + numShards - 1) / numShards
	if perShard < 1 {
		perShard = 1
	}

	k := &keyedLimiter{
		rps:    rate.Limit(spec.RPS),
		burst:  spec.Burst,
		refill: refillDuration(spec),
		cap:    perShard,
		now:    time.Now,
	}
	for i := range k.shards {
		k.shards[i].entries = make(map[string]*entry)
	}
	return k
}

// refillDuration is how long a fully drained bucket takes to refill.
//
// It is the eviction horizon: an entry that has not been seen for this long
// has certainly regained every token it had spent, so it is indistinguishable
// from an entry that was never created.
func refillDuration(spec BucketSpec) time.Duration {
	seconds := float64(spec.Burst) / spec.RPS
	if seconds < 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) {
		// Unreachable: NewLimiter rejects non-finite rps and burst < 1. A
		// zero horizon would make every entry immediately evictable, so the
		// safe fallback is to keep entries rather than to churn them.
		return time.Hour
	}
	d := time.Duration(seconds * float64(time.Second))
	if d <= 0 {
		// A sub-nanosecond refill rounds to zero. One nanosecond is enough to
		// keep the entry alive for the duration of a request.
		return time.Nanosecond
	}
	return d
}

// shardIndex picks the shard for a key.
//
// A byte-wise hash rather than hash/fnv: the keys are addresses and slugs, so
// the input is short, and avoiding a dependency for a ten-line function is the
// same trade the rest of this package makes.
func shardIndex(key string) int {
	// FNV-1a, 64-bit, offset basis 14695981039346656037 and prime
	// 1099511628211. Computed over bytes rather than runes: the key space is
	// ASCII, and a byte-wise loop cannot be made to allocate.
	var h uint64 = 14695981039346656037
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return int(h & (numShards - 1))
}

// allow performs one check.
func (k *keyedLimiter) allow(key, tier string) Decision {
	if k == nil {
		return Decision{Allowed: true}
	}

	sh := &k.shards[shardIndex(key)]
	now := k.now()

	// The lock covers the bucket check as well as the map. rate.Limiter has
	// its own mutex, so this is a nested acquisition, which is safe only
	// because rate.Limiter never calls back into this package — it holds its
	// lock for a handful of float operations and releases it. Holding ours
	// across the check is what makes the lookup, the insert, and the token
	// decision one atomic step, so two concurrent requests for the same new
	// key cannot each create a bucket.
	sh.mu.Lock()
	defer sh.mu.Unlock()

	e, ok := sh.entries[key]
	if !ok {
		if len(sh.entries) >= k.cap {
			k.evict(sh, now)
		}
		e = &entry{limiter: rate.NewLimiter(k.rps, k.burst)}
		sh.entries[key] = e
	}
	e.lastSeen = now

	if e.limiter.AllowN(now, 1) {
		return Decision{Allowed: true}
	}

	// Refused. TokensAt reports the bucket's level without consuming, so the
	// delay to the next permitted request is the time to earn the one token
	// that is missing.
	//
	// The alternative — ReserveN and CancelAt — would also work, but it
	// mutates the bucket and then rolls it back, and the rollback is subtle
	// enough that it is worth avoiding when a read-only computation gives the
	// same answer.
	missing := 1 - e.limiter.TokensAt(now)
	if missing <= 0 {
		// Unreachable: AllowN failed, so fewer than one token was available.
		// Reporting a refusal with no delay would produce a Retry-After of
		// zero, so it is rounded to the smallest expressible wait instead.
		missing = 1
	}
	delay := time.Duration(missing / float64(k.rps) * float64(time.Second))

	return Decision{Allowed: false, Tier: tier, RetryAfter: delay}
}

// evict frees space in a saturated shard.
//
// The first rule is the principled one. An entry whose bucket has fully
// refilled is indistinguishable from an entry that does not exist: both would
// answer the next request identically. Dropping those loses nothing, so a
// sweep is preferred to a forced eviction and in ordinary traffic is the only
// rule that ever runs.
//
// When the sweep frees nothing, every entry is live and the oldest is dropped.
// That does discard a real client's state, and it is the honest consequence of
// a bounded table: a caller who can present enough distinct keys can reset
// other keys' buckets. The alternative is an unbounded map, which is a memory
// exhaustion vector requiring no cleverness at all. A bound that degrades
// under sustained key churn is better than a bound that does not exist — and
// churning tier 1's keys requires many distinct source addresses, which is
// exactly the case where a per-address limiter is weakest anyway. This is
// recorded in the plan as a known limitation, not as a property.
//
// The cost is bounded by the shard, not by the tier: at most cap/numShards
// map iterations, and only on the insert that finds the shard full.
func (k *keyedLimiter) evict(sh *shard, now time.Time) {
	for key, e := range sh.entries {
		if now.Sub(e.lastSeen) >= k.refill {
			delete(sh.entries, key)
		}
	}
	if len(sh.entries) < k.cap {
		return
	}

	var (
		oldestKey string
		oldest    time.Time
		found     bool
	)
	for key, e := range sh.entries {
		if !found || e.lastSeen.Before(oldest) {
			oldestKey, oldest, found = key, e.lastSeen, true
		}
	}
	if found {
		delete(sh.entries, oldestKey)
	}
}

// trackedKeys returns how many keys one tier is currently holding, and the
// largest number any single shard of it holds.
//
// It exists for tests. The bound is a memory-safety property, and a property
// that is only asserted through behaviour is a property that is not asserted:
// the test that uses this drives real requests and then measures the table.
func (l *Limiter) trackedKeys(tier string) (total, largestShard int) {
	t := l.tier(tier)
	if t == nil {
		return 0, 0
	}
	for i := range t.shards {
		t.shards[i].mu.Lock()
		n := len(t.shards[i].entries)
		t.shards[i].mu.Unlock()
		total += n
		if n > largestShard {
			largestShard = n
		}
	}
	return total, largestShard
}

// tier returns the keyed limiter for a tier name, or nil.
func (l *Limiter) tier(name string) *keyedLimiter {
	if l == nil {
		return nil
	}
	switch name {
	case TierIP:
		return l.ip
	case TierTenant:
		return l.tenant
	default:
		return nil
	}
}

// perShardCap returns a tier's per-shard cap, for tests.
func (l *Limiter) perShardCap(tier string) int {
	t := l.tier(tier)
	if t == nil {
		return 0
	}
	return t.cap
}
