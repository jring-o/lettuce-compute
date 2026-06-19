package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// WP-DISPATCH (Layer 3, claim-on-refill) unit tests. They prove the per-head
// dispatch claim makes a unit one replica's cache stages invisible to every other
// replica's refill (no cross-replica double-hand), that the flush renews the claim,
// and that a crashed replica's expired claim is re-claimable by a survivor.

// newTestCacheWithHead builds a test cache whose refill stamps claims owned by
// headID with the given lease (scale-out enabled). Background goroutines are not
// started; tests drive refill/flush directly.
func newTestCacheWithHead(wuRepo workunit.WorkUnitRepository, leafRepo *fakeLeafRepo, assignRepo *fakeAssignRepo, headID types.ID, lease time.Duration) *dispatchCache {
	c := newDispatchCache(dispatchCacheConfig{
		readyPoolSize:           100,
		lowWatermark:            10,
		refillBatchSize:         50,
		admissionCap:            4,
		flushInterval:           time.Hour,
		flushBatchSize:          200,
		leaseSeconds:            900,
		maxInflightPerVolunteer: 0,
		headID:                  headID,
		claimLease:              lease,
	}, dispatchDeps{
		wuRepo:     wuRepo,
		leafRepo:   leafRepo,
		assignRepo: assignRepo,
	}, testLogger())
	return c
}

// --- shared claim-modeling fake repo ------------------------------------------
//
// claimRepo models JUST enough of the work_units claim semantics for a multi-cache
// test: a per-unit (claimedBy, expiresAt) plus the atomic ClaimDispatchableBatch
// exclude/re-claim rule the SQL enforces. Two dispatchCache instances pointed at one
// claimRepo behave like two replicas against one Postgres.
type claimUnit struct {
	id        types.ID
	leafID    types.ID
	claimedBy types.ID
	expiresAt time.Time
}

type claimRepo struct {
	workunit.WorkUnitRepository

	mu    sync.Mutex
	now   func() time.Time
	units []*claimUnit
	// claimCallsByHead counts how many units each head has claimed across all calls.
	claimedByHead map[types.ID]int
}

func newClaimRepo(now func() time.Time) *claimRepo {
	return &claimRepo{now: now, claimedByHead: map[types.ID]int{}}
}

func (r *claimRepo) addUnit(id, leafID types.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.units = append(r.units, &claimUnit{id: id, leafID: leafID})
}

// ClaimDispatchableBatch mirrors the pgx UPDATE...RETURNING claim rule: a unit is
// claimable iff it is unclaimed, its claim expired, or it is already this head's;
// claiming stamps (headID, now+lease) atomically (the mutex stands in for the row
// lock + atomic UPDATE).
func (r *claimRepo) ClaimDispatchableBatch(_ context.Context, headID types.ID, lease time.Duration, limit int, excludeIDs, leafIDs []types.ID) ([]workunit.DispatchCandidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	exclude := map[types.ID]struct{}{}
	for _, id := range excludeIDs {
		exclude[id] = struct{}{}
	}
	leafFilter := map[types.ID]struct{}{}
	for _, id := range leafIDs {
		leafFilter[id] = struct{}{}
	}
	var out []workunit.DispatchCandidate
	for _, u := range r.units {
		if len(out) >= limit {
			break
		}
		if _, ex := exclude[u.id]; ex {
			continue
		}
		if len(leafFilter) > 0 {
			if _, ok := leafFilter[u.leafID]; !ok {
				continue
			}
		}
		live := u.claimedBy != (types.ID{}) && u.expiresAt.After(now)
		ownedByOther := live && u.claimedBy != headID
		if ownedByOther {
			continue // another replica's LIVE claim hides the unit.
		}
		// Claimable: stamp this head's claim atomically.
		u.claimedBy = headID
		u.expiresAt = now.Add(lease)
		r.claimedByHead[headID]++
		uid := u.id
		out = append(out, workunit.DispatchCandidate{
			WorkUnit:          &workunit.WorkUnit{ID: uid, LeafID: u.leafID, State: workunit.WorkUnitStateQueued},
			LeafID:            u.leafID,
			RedundancyFactor:  1,
			ActiveAssignments: 0,
			Runtime:           leaf.RuntimeNative,
		})
	}
	return out, nil
}

func (r *claimRepo) FlushReservations(_ context.Context, recs []workunit.FlushReservation, headID types.ID, claimLease time.Duration) ([]workunit.FlushedCopy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	landed := make([]workunit.FlushedCopy, 0, len(recs))
	for _, rec := range recs {
		for _, u := range r.units {
			if u.id == rec.WorkUnitID {
				// Renew only this head's own claim (the equality guard), mirroring SQL.
				if u.claimedBy == headID {
					u.expiresAt = now.Add(claimLease)
				}
				landed = append(landed, workunit.FlushedCopy{WorkUnitID: rec.WorkUnitID, VolunteerID: rec.VolunteerID})
			}
		}
	}
	return landed, nil
}

func (r *claimRepo) CountActiveByVolunteer(_ context.Context) (map[types.ID]int, error) {
	return map[types.ID]int{}, nil
}

// claimOwner returns the current claim owner of a unit (for assertions).
func (r *claimRepo) claimOwner(id types.ID) (types.ID, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, u := range r.units {
		if u.id == id {
			return u.claimedBy, u.expiresAt
		}
	}
	return types.ID{}, time.Time{}
}

// --- tests --------------------------------------------------------------------

// TestRefillUsesClaimWhenScaleOutEnabled: with a head id configured the refill goes
// through ClaimDispatchableBatch carrying that head id + lease; with no head id it
// uses the claim-free FindDispatchableBatch path (Layer-2 behavior preserved).
func TestRefillUsesClaimWhenScaleOutEnabled(t *testing.T) {
	leafID := types.NewID()
	mkCands := func() func(int, []types.ID, []types.ID) ([]workunit.DispatchCandidate, error) {
		return func(limit int, _, _ []types.ID) ([]workunit.DispatchCandidate, error) {
			return []workunit.DispatchCandidate{{
				WorkUnit:         &workunit.WorkUnit{ID: types.NewID(), LeafID: leafID, State: workunit.WorkUnitStateQueued},
				LeafID:           leafID,
				RedundancyFactor: 1,
				Runtime:          leaf.RuntimeNative,
			}}, nil
		}
	}

	t.Run("scale-out enabled uses ClaimDispatchableBatch", func(t *testing.T) {
		wuRepo := &fakeWURepo{dispatchFn: mkCands()}
		leafRepo := &fakeLeafRepo{}
		assignRepo := &fakeAssignRepo{}
		headID := types.NewID()
		lease := 90 * time.Second
		c := newTestCacheWithHead(wuRepo, leafRepo, assignRepo, headID, lease)
		c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)

		c.refillOnce(context.Background())

		wuRepo.mu.Lock()
		defer wuRepo.mu.Unlock()
		if wuRepo.claimCalls != 1 {
			t.Fatalf("expected 1 ClaimDispatchableBatch call, got %d", wuRepo.claimCalls)
		}
		if wuRepo.lastClaimHeadID != headID {
			t.Fatalf("claim head id mismatch: got %s want %s", wuRepo.lastClaimHeadID, headID)
		}
		if wuRepo.lastClaimLease != lease {
			t.Fatalf("claim lease mismatch: got %s want %s", wuRepo.lastClaimLease, lease)
		}
	})

	t.Run("scale-out disabled uses FindDispatchableBatch", func(t *testing.T) {
		wuRepo := &fakeWURepo{dispatchFn: mkCands()}
		leafRepo := &fakeLeafRepo{}
		assignRepo := &fakeAssignRepo{}
		c := newTestCache(wuRepo, leafRepo, assignRepo) // no head id => claim-free
		c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)

		c.refillOnce(context.Background())

		wuRepo.mu.Lock()
		defer wuRepo.mu.Unlock()
		if wuRepo.claimCalls != 0 {
			t.Fatalf("claim-free cache must NOT call ClaimDispatchableBatch, got %d", wuRepo.claimCalls)
		}
	})
}

// TestFlushPassesClaimRenewal: the flusher hands headID + claimLease to
// FlushReservations so a held unit's claim is renewed off the hot path.
func TestFlushPassesClaimRenewal(t *testing.T) {
	leafID := types.NewID()
	wuRepo := &fakeWURepo{
		dispatchFn: func(int, []types.ID, []types.ID) ([]workunit.DispatchCandidate, error) {
			return []workunit.DispatchCandidate{{
				WorkUnit:         &workunit.WorkUnit{ID: types.NewID(), LeafID: leafID, State: workunit.WorkUnitStateQueued},
				LeafID:           leafID,
				RedundancyFactor: 1,
				Runtime:          leaf.RuntimeNative,
			}}, nil
		},
	}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	headID := types.NewID()
	lease := 77 * time.Second
	c := newTestCacheWithHead(wuRepo, leafRepo, assignRepo, headID, lease)
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	c.refillOnce(context.Background())

	vol := types.NewID()
	if results, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(results) != 1 {
		t.Fatalf("expected 1 hand-out, got %d", len(results))
	}
	c.flushOnce(context.Background())

	wuRepo.mu.Lock()
	defer wuRepo.mu.Unlock()
	if wuRepo.lastFlushHeadID != headID {
		t.Fatalf("flush head id mismatch: got %s want %s", wuRepo.lastFlushHeadID, headID)
	}
	if wuRepo.lastFlushClaimLease != lease {
		t.Fatalf("flush claim lease mismatch: got %s want %s", wuRepo.lastFlushClaimLease, lease)
	}
}

// TestTwoReplicasNoDoubleStage is the core WP-DISPATCH proof: two dispatchCache
// instances (two replicas) refill from ONE shared claim-modeling repo. The per-head
// claim must guarantee NO unit is staged in BOTH caches' ready pools — i.e. no
// cross-replica double-hand can ever occur, because the unit one replica claims is
// invisible to the other's refill.
func TestTwoReplicasNoDoubleStage(t *testing.T) {
	now := time.Now()
	repo := newClaimRepo(func() time.Time { return now })
	leafID := types.NewID()
	const nUnits = 60
	ids := make([]types.ID, nUnits)
	for i := range ids {
		ids[i] = types.NewID()
		repo.addUnit(ids[i], leafID)
	}

	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	lf := nativeLeaf(leafID, 1, false, 0)
	lease := 120 * time.Second

	headA, headB := types.NewID(), types.NewID()
	cacheA := newTestCacheWithHead(repo, leafRepo, assignRepo, headA, lease)
	cacheB := newTestCacheWithHead(repo, leafRepo, assignRepo, headB, lease)
	cacheA.warm(lf, leafRepo)
	cacheB.warm(lf, leafRepo)

	// Drive both replicas' refills concurrently and repeatedly (the contended case).
	var wg sync.WaitGroup
	for _, c := range []*dispatchCache{cacheA, cacheB} {
		wg.Add(1)
		go func(c *dispatchCache) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				c.refillOnce(context.Background())
			}
		}(c)
	}
	wg.Wait()

	// Collect each cache's staged ids.
	staged := map[types.ID]int{}
	for _, c := range []*dispatchCache{cacheA, cacheB} {
		c.mu.Lock()
		for i := range c.ready {
			staged[c.ready[i].unit.ID]++
		}
		c.mu.Unlock()
	}
	for id, n := range staged {
		if n > 1 {
			t.Fatalf("unit %s staged in %d replicas' ready pools (cross-replica double-stage)", id, n)
		}
	}

	// Every staged unit's DB claim must be owned by the replica that staged it.
	check := func(c *dispatchCache, head types.ID) {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i := range c.ready {
			owner, _ := repo.claimOwner(c.ready[i].unit.ID)
			if owner != head {
				t.Fatalf("unit %s staged by head %s but claimed by %s", c.ready[i].unit.ID, head, owner)
			}
		}
	}
	check(cacheA, headA)
	check(cacheB, headB)
}

// TestStalledFlusherClaimNotStolenWhileHeld is the Major #2 guard: the
// claim-expiry-vs-unflushed-reservation race. Replica A claims a unit, hands it out
// in memory (so it has a LIVE in-memory reservation), but its DURABLE reservation
// flush is "stalled" — the reservation row never lands. Meanwhile wall-clock time
// advances PAST the original claim lease. The ONLY thing keeping the claim alive is
// the async reservation flush RENEWING it (FlushReservations stamps now+claimLease
// for this head's own units). We model exactly that flush-renews-claim path: A
// flushes on a cadence shorter than the lease as the clock advances, so its claim
// window keeps moving forward even though the unit's reservation is still in memory
// only. Throughout, replica B's refill must NEVER re-stage the still-held unit — a
// re-stage would be a cross-replica double-hand of a reservation A still holds.
//
// The contrast case (renewal omitted) is exercised by
// TestCrashedReplicaClaimExpiryReclaim: when A stops renewing, the claim DOES expire
// and B reclaims. Here renewal is present, so the unit stays A's.
func TestStalledFlusherClaimNotStolenWhileHeld(t *testing.T) {
	clock := time.Now()
	nowFn := func() time.Time { return clock }
	repo := newClaimRepo(nowFn)
	leafID := types.NewID()
	unitID := types.NewID()
	repo.addUnit(unitID, leafID)

	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	lf := nativeLeaf(leafID, 1, false, 0)
	lease := 30 * time.Second

	headA, headB := types.NewID(), types.NewID()
	cacheA := newTestCacheWithHead(repo, leafRepo, assignRepo, headA, lease)
	cacheB := newTestCacheWithHead(repo, leafRepo, assignRepo, headB, lease)
	// Both caches read the SAME fake clock so reservedUntil math and claim expiry
	// advance together.
	cacheA.now = nowFn
	cacheB.now = nowFn
	cacheA.warm(lf, leafRepo)
	cacheB.warm(lf, leafRepo)

	// Replica A claims + hands out the unit, taking a LIVE in-memory reservation.
	cacheA.refillOnce(context.Background())
	vol := types.NewID()
	if results, _ := cacheA.HandOut(vol, capableOpts(vol, 0), 1); len(results) != 1 {
		t.Fatalf("expected replica A to hand out the unit, got %d", len(results))
	}
	if !cacheA.hasInMemReservation(unitID, vol) {
		t.Fatal("replica A should hold a live in-memory reservation on the unit")
	}
	if owner, _ := repo.claimOwner(unitID); owner != headA {
		t.Fatalf("expected head A to own the claim, got %s", owner)
	}

	// Advance well past the ORIGINAL lease in sub-lease steps. At each step we model
	// the async reservation flush RENEWING A's claim (the flush-renews-claim path):
	// the unit's reservation stays in A's flush stream (we re-enqueue it so the flush
	// carries it), and FlushReservations re-stamps A's claim to now+claimLease for its
	// own units. The DURABLE reservation never reaches a terminal run-start (the unit
	// is "held but unflushed" w.r.t. completion) — yet because A keeps renewing, the
	// claim window keeps moving forward. After every step, replica B must still see the
	// unit as owned by A and refuse to re-stage it: a re-stage would be a cross-replica
	// double-hand of a reservation A still holds (Major #2).
	enqueuePending := func() {
		cacheA.mu.Lock()
		cacheA.pendingWrites = append(cacheA.pendingWrites, workunit.FlushReservation{
			WorkUnitID:    unitID,
			VolunteerID:   vol,
			ReservedUntil: clock.Add(time.Duration(cacheA.cfg.leaseSeconds) * time.Second),
		})
		cacheA.mu.Unlock()
	}
	step := lease / 2
	for elapsed := time.Duration(0); elapsed < 3*lease; elapsed += step {
		clock = clock.Add(step)

		// flush-renews-claim: keep the held unit in A's flush stream and flush, which
		// re-stamps A's claim to now+claimLease (claimRepo.FlushReservations renews only
		// this head's own claims, mirroring the SQL equality guard).
		enqueuePending()
		cacheA.flushOnce(context.Background())

		// Replica B tries to refill: the live, A-owned claim must hide the unit.
		cacheB.refillOnce(context.Background())
		if cacheB.readyLen() != 0 {
			t.Fatalf("replica B re-staged a unit still held + claim-renewed by replica A (Major #2 race) at elapsed=%s", elapsed+step)
		}
		if owner, expiresAt := repo.claimOwner(unitID); owner != headA {
			t.Fatalf("claim of a held+renewed unit must stay with head A, got %s at elapsed=%s", owner, elapsed+step)
		} else if !expiresAt.After(clock) {
			t.Fatalf("renewal should keep A's claim live (expires %s) past now (%s) at elapsed=%s", expiresAt, clock, elapsed+step)
		}
		// A still holds the in-memory reservation throughout.
		if !cacheA.hasInMemReservation(unitID, vol) {
			t.Fatalf("replica A lost its in-memory reservation at elapsed=%s", elapsed+step)
		}
	}
}

// TestCrashedReplicaClaimExpiryReclaim: replica A claims a unit, then "crashes"
// (stops renewing). Once the claim expires, replica B's refill re-claims it. This is
// the passive-expiry crash-reclaim guarantee — no active sweep required.
func TestCrashedReplicaClaimExpiryReclaim(t *testing.T) {
	clock := time.Now()
	nowFn := func() time.Time { return clock }
	repo := newClaimRepo(nowFn)
	leafID := types.NewID()
	unitID := types.NewID()
	repo.addUnit(unitID, leafID)

	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	lf := nativeLeaf(leafID, 1, false, 0)
	lease := 30 * time.Second

	headA, headB := types.NewID(), types.NewID()
	cacheA := newTestCacheWithHead(repo, leafRepo, assignRepo, headA, lease)
	cacheB := newTestCacheWithHead(repo, leafRepo, assignRepo, headB, lease)
	cacheA.warm(lf, leafRepo)
	cacheB.warm(lf, leafRepo)

	// Replica A claims the unit.
	cacheA.refillOnce(context.Background())
	if owner, _ := repo.claimOwner(unitID); owner != headA {
		t.Fatalf("expected head A to own the claim, got %s", owner)
	}

	// While A's claim is LIVE, replica B's refill must NOT re-claim it.
	cacheB.refillOnce(context.Background())
	if cacheB.readyLen() != 0 {
		t.Fatalf("replica B re-claimed a unit under replica A's LIVE claim")
	}
	if owner, _ := repo.claimOwner(unitID); owner != headA {
		t.Fatalf("live claim must still belong to head A, got %s", owner)
	}

	// A "crashes": advance the clock past the lease so A's claim expires (A no longer
	// renews it). B's next refill re-claims the now-expired unit.
	clock = clock.Add(lease + time.Second)
	cacheB.refillOnce(context.Background())
	if cacheB.readyLen() != 1 {
		t.Fatalf("replica B should re-claim the expired unit, ready=%d", cacheB.readyLen())
	}
	if owner, _ := repo.claimOwner(unitID); owner != headB {
		t.Fatalf("expired claim should be re-claimed by head B, got %s", owner)
	}
}
