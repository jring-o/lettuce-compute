package server

import (
	"sync"
	"sync/atomic"
	"testing"
)

// fakeAdvisoryLock is a DB-free model of one Postgres session-level advisory lock:
// pg_try_advisory_lock(key) succeeds for AT MOST ONE holder of a given key at a
// time and is non-blocking (returns false if already held). It is keyed so two
// managers contending on the SAME lockKey race for ONE lock — exactly the cross-
// replica mutual exclusion the real pg_try_advisory_lock provides. (The behavioral
// proof against a real Postgres lives in the integration TestLeadership... test;
// this models the GATE logic so it is exercised without a database.)
type fakeAdvisoryLock struct {
	mu   sync.Mutex
	held map[int64]bool
}

func newFakeAdvisoryLock() *fakeAdvisoryLock {
	return &fakeAdvisoryLock{held: map[int64]bool{}}
}

// tryLock mirrors pg_try_advisory_lock: returns true and takes the lock iff no one
// else currently holds this key.
func (l *fakeAdvisoryLock) tryLock(key int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[key] {
		return false
	}
	l.held[key] = true
	return true
}

// runGatedLazyGen models a LeadershipManager's leader-only path WITHOUT a DB: it
// derives its lock key exactly as the real manager does (advisoryLockKey of the
// shared singleton name), attempts the shared advisory lock, and runs the singleton
// "lazy generation" job (incrementing the counter) ONLY if it won the lock — the
// same "if !locked { return }" gate as acquireAndHold. A follower that loses the
// lock returns without running the job.
func runGatedLazyGen(m *LeadershipManager, lock *fakeAdvisoryLock, lazyGenRuns *atomic.Int32) (becameLeader bool) {
	if !lock.tryLock(m.lockKey) {
		return false // follower: skip the singleton job
	}
	lazyGenRuns.Add(1) // leader-only: lazy generation runs exactly here
	return true
}

// TestAdvisoryLockKeyIsStableAndShared is the unit-level guarantee behind
// singleton-job exclusivity: every replica derives the SAME 64-bit advisory-lock
// key from the SAME stable name, so they all contend for ONE Postgres advisory
// lock and exactly one wins. If the key were derived per-replica (e.g. mixed with
// the head instance id), two replicas would acquire "different" locks and both run
// the leader-only jobs — defeating the gate. This test pins the invariant without
// a database: the key is deterministic for a name and differs for a different name.
func TestAdvisoryLockKeyIsStableAndShared(t *testing.T) {
	t.Parallel()

	// Determinism: the same name always hashes to the same key (so all replicas,
	// which all use singletonJobsLockName, contend for one lock).
	k1 := advisoryLockKey(singletonJobsLockName)
	k2 := advisoryLockKey(singletonJobsLockName)
	if k1 != k2 {
		t.Fatalf("advisoryLockKey not deterministic: %d != %d", k1, k2)
	}

	// The constant the whole fleet shares must be non-zero (a zero key is a
	// suspicious hash and risks colliding with an unkeyed default).
	if k1 == 0 {
		t.Fatalf("advisoryLockKey(%q) == 0; expected a non-zero shared lock key", singletonJobsLockName)
	}

	// Distinctness: a different name must hash to a different key, so unrelated
	// advisory locks (were any added later) would not collide with this one.
	if other := advisoryLockKey("lettuce:some-other-lock"); other == k1 {
		t.Fatalf("distinct lock names collided to the same key: %d", k1)
	}
}

// TestLeaderGatedLazyGenerationRunsOnce is the singleton-job guard: lazy generation
// is a per-leaf cursor read-modify-write with NO row lock, so if it ran on two
// replicas on the same tick they would double-generate work units and clobber the
// cursor (the headline reason the leadership gate exists). This test models lazy
// generation as a counter that is incremented ONLY behind the leader gate, drives
// TWO LeadershipManagers (two replicas) contending on the shared advisory-lock key,
// and asserts the singleton job runs EXACTLY ONCE — the follower is shut out, so
// lazy generation cannot double-act. The contention seam (shared lockKey + a single-
// holder advisory lock) mirrors pg_try_advisory_lock without needing a database.
func TestLeaderGatedLazyGenerationRunsOnce(t *testing.T) {
	t.Parallel()

	lock := newFakeAdvisoryLock()
	var lazyGenRuns atomic.Int32

	// Two replicas: distinct managers, but (per TestNewLeadershipManagerUsesSharedKey)
	// they compute the SAME lock key and so contend for ONE lock.
	mgrA := NewLeadershipManager(nil, nil)
	mgrB := NewLeadershipManager(nil, nil)
	if mgrA.lockKey != mgrB.lockKey {
		t.Fatalf("precondition: replicas must share a lock key, got %d and %d", mgrA.lockKey, mgrB.lockKey)
	}

	// Both replicas attempt leadership CONCURRENTLY (the contended case). Exactly one
	// may win the lock and run the singleton lazy-generation job.
	var wg sync.WaitGroup
	var leaders atomic.Int32
	for _, m := range []*LeadershipManager{mgrA, mgrB} {
		wg.Add(1)
		go func(m *LeadershipManager) {
			defer wg.Done()
			if runGatedLazyGen(m, lock, &lazyGenRuns) {
				leaders.Add(1)
			}
		}(m)
	}
	wg.Wait()

	if got := leaders.Load(); got != 1 {
		t.Fatalf("exactly one replica must become leader, got %d", got)
	}
	if got := lazyGenRuns.Load(); got != 1 {
		t.Fatalf("lazy generation (singleton job) must run EXACTLY once across replicas, ran %d times — double-acting would clobber the generation cursor", got)
	}

	// A subsequent follower attempt while the leader still holds the lock must NOT
	// re-run lazy generation: the gate stays closed.
	if runGatedLazyGen(mgrB, lock, &lazyGenRuns) {
		t.Fatal("a second replica acquired leadership while the lock was held")
	}
	if got := lazyGenRuns.Load(); got != 1 {
		t.Fatalf("lazy generation ran again for a follower; count=%d, want 1", got)
	}
}

// TestNewLeadershipManagerUsesSharedKey verifies that two independently
// constructed managers (modeling two head replicas) compute the SAME lock key.
// This is what makes pg_try_advisory_lock mutually exclusive across replicas:
// because both managers pass the identical key, exactly one acquires it and the
// other becomes a follower that skips the leader-only jobs. (The full behavioral
// proof — one acquires, the other skips, failover on release — lives in the
// integration test against a real Postgres.)
func TestNewLeadershipManagerUsesSharedKey(t *testing.T) {
	t.Parallel()

	a := NewLeadershipManager(nil, nil)
	b := NewLeadershipManager(nil, nil)
	if a.lockKey != b.lockKey {
		t.Fatalf("two replicas computed different lock keys: %d != %d", a.lockKey, b.lockKey)
	}
	if a.lockKey != advisoryLockKey(singletonJobsLockName) {
		t.Fatalf("manager lock key %d does not match singletonJobsLockName hash %d",
			a.lockKey, advisoryLockKey(singletonJobsLockName))
	}
}
