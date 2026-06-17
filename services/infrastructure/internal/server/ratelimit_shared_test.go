package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeRateLimitRedis is an in-memory fake implementing the narrow rateLimitCmd
// surface (a single EVAL). It models the fixedWindowScript faithfully, INCLUDING
// time-based key expiry against a virtual clock the test advances. Modeling real
// expiry is the whole point: the previous fake recorded only the last TTL value and
// never expired keys, so it structurally could not catch the "TTL re-stamped every
// call ⇒ window never closes ⇒ permanent lockout" bug. With a virtual clock, a test
// can prove the counter actually resets when the window elapses.
//
// When evalErr is set, Eval returns it once the call count exceeds evalFailAfter
// (0 = fail from the first call) so the fail-open path is testable.
type fakeRateLimitRedis struct {
	mu     sync.Mutex
	counts map[string]int64
	expiry map[string]time.Time // absolute virtual expiry; absent = no TTL set
	now    time.Time            // virtual clock; advance() moves it forward

	evalErr       error
	evalFailAfter int
	calls         int
}

func newFakeRateLimitRedis() *fakeRateLimitRedis {
	return &fakeRateLimitRedis{
		counts: make(map[string]int64),
		expiry: make(map[string]time.Time),
		// A fixed, non-zero base so tests are deterministic and clock math is obvious.
		now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// advance moves the virtual clock forward (simulating wall-clock passing between
// requests). The next Eval lazily expires any key whose TTL has elapsed.
func (f *fakeRateLimitRedis) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Eval replicates fixedWindowScript: lazy-expire the key, INCR, then set the TTL
// only on the first request of a window (count == 1) or to repair a key with no TTL.
func (f *fakeRateLimitRedis) Eval(_ context.Context, _ string, keys []string, args ...interface{}) *redis.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	cmd := redis.NewCmd(context.Background())

	if f.evalErr != nil && (f.evalFailAfter == 0 || f.calls > f.evalFailAfter) {
		cmd.SetErr(f.evalErr)
		return cmd
	}

	key := keys[0]
	window := time.Duration(args[0].(int64)) * time.Millisecond

	// Lazy expiry: a real Redis would have dropped the key once its TTL elapsed.
	if exp, ok := f.expiry[key]; ok && !f.now.Before(exp) {
		delete(f.counts, key)
		delete(f.expiry, key)
	}

	f.counts[key]++
	c := f.counts[key]
	if _, hasTTL := f.expiry[key]; c == 1 || !hasTTL {
		f.expiry[key] = f.now.Add(window)
	}

	cmd.SetVal(c)
	return cmd
}

// hasTTL reports whether the given key currently has a (virtual) TTL set. A key that
// has been INCR'd but has no TTL is the stranded, never-resetting hazard.
func (f *fakeRateLimitRedis) hasTTL(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.expiry[key]
	return ok
}

// TestSharedBucket_FixedWindowGlobal verifies the shared bucket grants exactly the
// configured budget across the window and rejects beyond it — crucially, two
// sharedBuckets sharing ONE fake (two replicas, same key) share ONE global counter,
// so the client gets its intended budget regardless of replica count.
func TestSharedBucket_FixedWindowGlobal(t *testing.T) {
	fake := newFakeRateLimitRedis()
	limit := 5
	replicaA := newSharedBucket(fake, "grpc:1.2.3.4", limit, time.Minute)
	replicaB := newSharedBucket(fake, "grpc:1.2.3.4", limit, time.Minute)

	now := fake.now
	allowedCount := 0
	// Alternate across the two replicas; the shared counter must cap the TOTAL.
	for i := 0; i < limit*2; i++ {
		b := replicaA
		if i%2 == 1 {
			b = replicaB
		}
		if ok, _, _ := b.allow(now); ok {
			allowedCount++
		}
	}
	if allowedCount != limit {
		t.Fatalf("shared global budget should allow exactly %d across replicas, allowed %d", limit, allowedCount)
	}
}

// TestSharedBucket_FailOpenOnError verifies a Redis error admits the request
// (fail-open) rather than throttling the whole fleet.
func TestSharedBucket_FailOpenOnError(t *testing.T) {
	fake := newFakeRateLimitRedis()
	fake.evalErr = errors.New("simulated redis outage")
	b := newSharedBucket(fake, "grpc:1.2.3.4", 1, time.Minute)

	allowed, _, _ := b.allow(fake.now)
	if !allowed {
		t.Fatal("shared rate limiter must fail OPEN (admit) on a store error")
	}
}

// TestSharedBucket_WindowResetsUnderSustainedTraffic is the core regression for the
// permanent-lockout bug. The window must close a fixed `window` after its FIRST
// request, regardless of intervening traffic. The old implementation re-stamped the
// TTL on EVERY call, so a steady trickle of requests (each < window apart) rolled the
// expiry forward forever: the counter never reset and the client was rejected
// permanently once it crossed the limit. This test drives traffic that stays under
// the per-window limit overall yet keeps the key continuously busy, and asserts the
// counter resets at the fixed boundary — which only holds when the TTL is set once.
func TestSharedBucket_WindowResetsUnderSustainedTraffic(t *testing.T) {
	fake := newFakeRateLimitRedis()
	limit := 3
	window := time.Minute
	b := newSharedBucket(fake, "grpc:7.7.7.7", limit, window)

	// Fill the window: limit allows, then reject.
	for i := 0; i < limit; i++ {
		if ok, _, _ := b.allow(fake.now); !ok {
			t.Fatalf("request %d within the budget should be allowed", i+1)
		}
	}
	if ok, _, _ := b.allow(fake.now); ok {
		t.Fatal("request over the per-window limit should be rejected")
	}

	// Keep the key continuously busy with a sub-window trickle. Under the OLD
	// "re-stamp TTL every call" code each of these would push the expiry forward and
	// the window would never close. With the TTL set once, the expiry stays pinned to
	// (first request + window).
	b.allow(fake.now)        // still in window 1 → rejected, must NOT extend the TTL
	fake.advance(30 * time.Second)
	b.allow(fake.now)        // t+30s, still window 1 → rejected, must NOT extend the TTL

	// Cross the original window boundary. The key must have expired and the counter
	// reset, so a fresh request is admitted again.
	fake.advance(31 * time.Second) // t+61s, past the fixed window that began at t0
	if ok, _, _ := b.allow(fake.now); !ok {
		t.Fatal("window never reset under sustained traffic: the TTL is being rolled forward on every call (permanent-lockout bug)")
	}
}

// TestSharedBucket_RepairsStrandedTTL guards the self-heal path: if a key is ever
// left with a count but NO TTL (e.g. a crash between a prior INCR and its EXPIRE),
// the next call must (re)set a TTL via the PTTL == -1 branch so the key can still
// expire and reset — it must never become a permanently-stranded, never-resetting
// counter.
func TestSharedBucket_RepairsStrandedTTL(t *testing.T) {
	fake := newFakeRateLimitRedis()
	clientKey := "grpc:9.9.9.9"
	redisKey := redisRateLimitKeyPrefix + clientKey
	limit := 3
	b := newSharedBucket(fake, clientKey, limit, time.Minute)

	// Force a stranded state: a counter already over the limit with NO TTL recorded.
	fake.mu.Lock()
	fake.counts[redisKey] = 5
	delete(fake.expiry, redisKey)
	fake.mu.Unlock()
	if fake.hasTTL(redisKey) {
		t.Fatal("precondition: key should start with no TTL")
	}

	// The next call increments to 6 (rejected) but MUST repair the missing TTL so the
	// key can expire later instead of blocking the client forever.
	if ok, _, _ := b.allow(fake.now); ok {
		t.Fatal("a count over the limit should be rejected")
	}
	if !fake.hasTTL(redisKey) {
		t.Fatal("stranded TTL-less key was not repaired: client could be blocked indefinitely")
	}

	// And it does expire: after the window the counter resets and traffic flows again.
	fake.advance(61 * time.Second)
	if ok, _, _ := b.allow(fake.now); !ok {
		t.Fatal("repaired key should expire after the window and admit a fresh request")
	}
}

// TestRateLimitStore_UsesSharedWhenRedisSet verifies getBucket returns a shared
// limiter (not the in-process token bucket) when a redis client is installed on the
// store.
func TestRateLimitStore_UsesSharedWhenRedisSet(t *testing.T) {
	fake := newFakeRateLimitRedis()
	store := newRateLimitStore()
	store.redisClient = fake

	lim := store.getBucket("grpc:5.6.7.8", 3)
	if _, ok := lim.(*sharedBucket); !ok {
		t.Fatalf("expected *sharedBucket when redis is set, got %T", lim)
	}

	// With no redis, the in-process token bucket is used.
	store2 := newRateLimitStore()
	lim2 := store2.getBucket("grpc:5.6.7.8", 3)
	if _, ok := lim2.(*tokenBucket); !ok {
		t.Fatalf("expected *tokenBucket without redis, got %T", lim2)
	}
}
