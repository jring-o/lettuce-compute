package server

// PB-15 / PB-9 regression tests (Phase 3 local campaign).
//
// These are DIFFERENTIAL regression tests: they are written against test seams that
// existed BEFORE the fixes (newTestCache, stageUnit, warm, HandOut, flushOnce,
// hasInMemReservation, flushPendingFor, voidNonLandedCopy, pendingSpotCheckCount,
// the fakeWURepo flush/reserve hooks), so this file compiles on the pre-fix tree
// and demonstrably FAILS there:
//
//   - TestFlushPendingFor_WaitsOutInFlightTickerBatch — PB-15's pinned mechanism:
//     the ticker flush removes a batch from pendingWrites BEFORE its DB write lands,
//     so the StartWork guard's forced flush (which only drained the queue) returned
//     with the racing reservation in neither the queue nor the DB, and Assign denied
//     the run-start ("work unit no longer reserved for this volunteer").
//   - TestFlushPendingFor_LandsSpotCheckReservation — PB-15's spot-check half: the
//     forced flush never drained pendingSpotChecks at all, so a warm-cache
//     volunteer's first StartWork on a spot-checked unit was denied unconditionally.
//   - TestHandOut_VoidBench_ExpiresInsteadOfStranding — PB-9's stale-set defect: a
//     flush-conflict bench on a staged candidate never expired while the candidate
//     sat in the ready pool, so the benched volunteer was refused forever — a
//     one-volunteer pool stranded permanently.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// TestFlushPendingFor_WaitsOutInFlightTickerBatch reproduces the warm-cache
// fast-StartWork race (PB-15) at the cache layer: a hand-out's reservation write is
// snapshotted into a ticker flush batch whose DB write is still in flight when the
// forced flush runs. The forced flush must not return until that reservation is
// durable (or voided) — that is the exact contract StartWork's Major-3 guard relies
// on before calling Assign.
func TestFlushPendingFor_WaitsOutInFlightTickerBatch(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}

	gateEntered := make(chan struct{})
	gateRelease := make(chan struct{})
	var entered sync.Once
	var landed atomic.Bool
	wuRepo.flushFn = func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error) {
		entered.Do(func() { close(gateEntered) })
		// Simulate the DB round-trip in flight: the batch has left pendingWrites but
		// nothing is durable until this returns.
		<-gateRelease
		out := make([]workunit.FlushedCopy, len(recs))
		for i, r := range recs {
			out[i] = workunit.FlushedCopy{WorkUnitID: r.WorkUnitID, VolunteerID: r.VolunteerID}
		}
		landed.Store(true)
		return out, nil
	}

	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	vol := types.NewID()
	if res, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(res) != 1 {
		t.Fatalf("hand-out = %d results, want 1", len(res))
	}

	// The 100ms ticker fires: it snapshots the batch out of pendingWrites and blocks
	// mid-write (in production: on the maintenance semaphore the burst-triggered
	// refill holds, then the DB round-trip).
	go c.flushOnce(context.Background())
	<-gateEntered

	// The write completes a little later — well after a warm-cache volunteer's
	// StartWork (observed live at hand-out + ~5ms) has arrived.
	go func() {
		time.Sleep(75 * time.Millisecond)
		close(gateRelease)
	}()

	// StartWork's guard sequence: the in-memory hold is present, so the guard forces
	// the flush and then calls Assign, which reads the copy row from Postgres.
	if !c.hasInMemReservation(unitID, vol) {
		t.Fatal("in-memory reservation missing right after hand-out")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.flushPendingFor(ctx)

	if !landed.Load() {
		t.Fatal("flushPendingFor returned while the hand-out's reservation was still in an in-flight flush batch (neither durable nor voided): StartWork's Assign would find no copy row and deny the run-start — the PB-15 warm-cache first-StartWork denial")
	}
}

// TestFlushPendingFor_LandsSpotCheckReservation reproduces PB-15's spot-check half:
// a spot-checked hand-out queues its copy write on the SEPARATE spot-check queue,
// which the forced flush never drained — so the copy row could only land via the
// ticker and every immediate StartWork on a spot-checked unit was denied.
func TestFlushPendingFor_LandsSpotCheckReservation(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	// Redundancy-1 leaf with spot-check at 100%: the first hold marks the unit
	// spot-check and routes its write to pendingSpotChecks.
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, true, 100), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 1, 0)

	vol := types.NewID()
	if res, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(res) != 1 {
		t.Fatalf("hand-out = %d results, want 1", len(res))
	}
	if n := c.pendingSpotCheckCount(); n != 1 {
		t.Fatalf("pending spot-check writes = %d, want 1 (the hand-out must have routed to the spot-check queue)", n)
	}
	if !c.hasInMemReservation(unitID, vol) {
		t.Fatal("in-memory reservation missing right after hand-out")
	}

	// StartWork's forced flush must land the spot-check copy row (MarkSpotCheck +
	// ReserveCopy) before Assign reads it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.flushPendingFor(ctx)

	wuRepo.mu.Lock()
	reserveCalls := wuRepo.reserveCopyCalls
	wuRepo.mu.Unlock()
	if reserveCalls != 1 {
		t.Fatalf("spot-check copy landings after forced flush = %d, want 1: the copy row is not durable, so StartWork's Assign would deny the run-start (PB-15, spot-check variant)", reserveCalls)
	}
}

// TestHandOut_VoidBench_ExpiresInsteadOfStranding reproduces PB-9's stale-set
// defect: a volunteer benched on a staged candidate by a flush conflict
// (voidNonLandedCopy) stayed benched for as long as the candidate sat in the ready
// pool — the set refreshed only on re-stage, which never happens while the unit
// stays staged — so the only volunteer of a small pool was refused the unit forever
// (observed live: benched_cooldown rejects 9+ minutes after the SQL cooldown had
// lapsed; E11). The in-memory bench is a hand-out throttle over the authoritative
// SQL gates, so it must expire and re-offer.
func TestHandOut_VoidBench_ExpiresInsteadOfStranding(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	base := time.Now().UTC()
	now := base
	c.now = func() time.Time { return now }

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	vol := types.NewID()
	if res, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(res) != 1 {
		t.Fatalf("initial hand-out = %d results, want 1", len(res))
	}
	// The DB flush refuses the copy (e.g. the volunteer is in SQL cooldown): the
	// hold is voided and the volunteer benched on the staged candidate.
	c.voidNonLandedCopy(unitID, vol)

	// Immediately after the void the bench must still refuse (the livelock damper).
	if res, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(res) != 0 {
		t.Fatalf("hand-out right after void = %d results, want 0 (bench must throttle the re-offer)", len(res))
	}

	// Two minutes later — far past the void-bench throttle, with the candidate
	// STILL staged (never re-staged) — the volunteer must be offered the unit
	// again; the SQL landing remains the authoritative gate.
	now = base.Add(2 * time.Minute)
	if res, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(res) != 1 {
		t.Fatal("volunteer still benched on the staged candidate long after the bench window: the stale in-memory bench out-lives the SQL cooldown and permanently strands a one-volunteer pool (PB-9)")
	}
}
