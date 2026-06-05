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
// surface (Incr + PExpire). It is sufficient to exercise the shared fixed-window
// limiter without a live Redis. When failAfter > 0, Incr returns an error once the
// call count exceeds it (to test fail-open). It also models per-key TTLs so a test
// can assert a window key is never left WITHOUT a TTL, and can be told to fail the
// first PExpire to exercise the self-healing re-assert path.
type fakeRateLimitRedis struct {
	mu        sync.Mutex
	counts    map[string]int64
	ttls      map[string]time.Duration // last PEXPIRE per key; absent = no TTL set
	failErr   error
	failAfter int
	calls     int

	// pexpireErr, when set, is returned by the first pexpireFailFor PExpire calls
	// (0 = never fail). pexpireCalls counts PExpire invocations.
	pexpireErr     error
	pexpireFailFor int
	pexpireCalls   int
}

func newFakeRateLimitRedis() *fakeRateLimitRedis {
	return &fakeRateLimitRedis{
		counts: make(map[string]int64),
		ttls:   make(map[string]time.Duration),
	}
}

func (f *fakeRateLimitRedis) Incr(_ context.Context, key string) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	cmd := redis.NewIntCmd(context.Background())
	if f.failErr != nil && (f.failAfter == 0 || f.calls > f.failAfter) {
		cmd.SetErr(f.failErr)
		return cmd
	}
	f.counts[key]++
	cmd.SetVal(f.counts[key])
	return cmd
}

func (f *fakeRateLimitRedis) PExpire(_ context.Context, key string, d time.Duration) *redis.BoolCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pexpireCalls++
	cmd := redis.NewBoolCmd(context.Background())
	if f.pexpireErr != nil && f.pexpireCalls <= f.pexpireFailFor {
		// Simulate an EXPIRE that did not land: the key keeps whatever TTL it had
		// (none, on the very first hit) and the command reports the error.
		cmd.SetErr(f.pexpireErr)
		return cmd
	}
	f.ttls[key] = d
	cmd.SetVal(true)
	return cmd
}

// hasTTL reports whether the given key currently has a TTL recorded. A key that
// has been INCR'd but has no TTL is the unbounded-block hazard this guards against.
func (f *fakeRateLimitRedis) hasTTL(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.ttls[key]
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

	now := time.Now()
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
	fake.failErr = errors.New("simulated redis outage")
	b := newSharedBucket(fake, "grpc:1.2.3.4", 1, time.Minute)

	allowed, _, _ := b.allow(time.Now())
	if !allowed {
		t.Fatal("shared rate limiter must fail OPEN (admit) on a store error")
	}
}

// TestSharedBucket_FirstExpireFailureSelfHeals is the HARDENING 2 guard: if the
// VERY FIRST PEXPIRE fails (the count==1 hit), the window key would, under the old
// "EXPIRE only on first hit" logic, be left with NO TTL forever — the counter would
// climb past the limit and the client would be blocked PERMANENTLY because the
// window never resets. The fix re-asserts PEXPIRE on EVERY call, so the second call
// repairs the missing TTL. This test fails the first PExpire and asserts (a) the
// key gains a TTL on the next call and (b) the client is NOT permanently blocked.
func TestSharedBucket_FirstExpireFailureSelfHeals(t *testing.T) {
	fake := newFakeRateLimitRedis()
	fake.pexpireErr = errors.New("simulated EXPIRE failure")
	fake.pexpireFailFor = 1 // only the FIRST PExpire fails

	clientKey := "grpc:9.9.9.9"
	redisKey := redisRateLimitKeyPrefix + clientKey
	b := newSharedBucket(fake, clientKey, 3, time.Minute)

	now := time.Now()

	// First hit: INCR -> count 1, PEXPIRE FAILS. Request is still admitted (the
	// expire error is non-fatal) but the key currently has no TTL.
	if ok, _, _ := b.allow(now); !ok {
		t.Fatal("first request should be admitted even when the initial PEXPIRE fails")
	}
	if fake.hasTTL(redisKey) {
		t.Fatal("precondition: first PEXPIRE was supposed to fail, leaving no TTL")
	}

	// Second hit: PEXPIRE now succeeds and MUST repair the missing TTL. Without the
	// always-re-assert fix the key would stay TTL-less forever.
	if ok, _, _ := b.allow(now); !ok {
		t.Fatal("second request should be admitted")
	}
	if !fake.hasTTL(redisKey) {
		t.Fatal("window key still has NO TTL after a second call: PEXPIRE is not being re-asserted — client can be blocked indefinitely")
	}

	// Drive well past the limit; the limiter must REJECT (a healthy fixed window),
	// not the indefinite block we are guarding against vs. the never-resetting key.
	// The point is that the window key is bounded by a TTL so it CAN reset.
	for i := 0; i < 10; i++ {
		b.allow(now)
	}
	if !fake.hasTTL(redisKey) {
		t.Fatal("window key lost its TTL under sustained traffic; a stranded key blocks the client permanently")
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
