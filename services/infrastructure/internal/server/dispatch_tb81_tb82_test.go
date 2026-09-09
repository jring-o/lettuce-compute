package server

// TB-81 / TB-82 regression tests — the in-memory half of two head defects the
// infra.scios.tech operator's report of 2026-09-07 traced to the same close path.
//
// TB-81: an ABANDONED copy that never started (the client could not even begin the
// unit — no runtime, an unreachable container engine, a prepare error) benched its
// volunteer nowhere and dented its reliability nowhere, while every such abandon spent
// the unit's copy budget. One machine with a dead container engine re-took the same
// unit every ten minutes, spent its nine-copy budget in 80 minutes, and dead-lettered
// 22 healthy units in 30 hours, each holding a good result from another volunteer.
// The #59 exemption behind it ("a graceful return of un-started buffered work is not
// a reliability signal") predates the give-back flag that now marks graceful returns
// RETURNED; an un-started ABANDONED is a failure by the client's own account.
//
// TB-82: the fault monitor's timeout sweep closed copies without telling the dispatch
// cache. onCopyClosed — the only function that drops a volunteer's in-memory hold —
// was called from the abandon RPC alone, and the transitioner's eviction hook fires
// only on a unit STATE change, which a closed copy usually does not produce. The
// fossil hold kept the unit excluded from refill and its staged candidate counting a
// holder that no longer existed, so two dispatchable units were offered to nobody for
// 30 days until a restart rebuilt the pool.
//
// Red evidence (2026-09-09, pre-fix tree, these tests in place):
//   TestAbandonUnstarted_BenchesVolunteerOnStagedCandidate — "hand-out inside the
//     abandon cooldown = 1 results, want 0" (the un-started abandon benched nothing).
//   TestAbandon_DentsHostReliabilityForAbandonedNotReturned — "RecordOutcome calls
//     after an un-started ABANDONED close = 0, want 1" (no reliability writer on the
//     abandon path).
//   TestFaultMonitor_ReaperClose_ReleasesHoldAndBenchesHolder — "excludedIDsLocked
//     still lists the unit after the reaper closed its only copy" and "fresh volunteer X
//     refused after the reaper closed V's copy" (the hold outlived the copy).
//   TestReconcile_ReleasesHoldWhoseLeaseLapsedUnannounced — "hold still present ...
//     past the lease + grace" (no backstop existed).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// engineDownReason is the reason the volunteer client sends for a prepare-time
// container-engine failure — the shape that produced the operator report's 188
// un-started abandons.
const engineDownReason = "docker is not available: docker ping: Cannot connect to the Docker daemon at unix:///run/user/1000/podman/podman.sock"

// TestAbandonUnstarted_BenchesVolunteerOnStagedCandidate pins TB-81's bench half in
// memory: a plain (non-give-back) abandon of an un-started copy must bench the
// volunteer on the still-staged candidate for the deadline window, exactly like a
// started abandon — the SQL gate now refuses it, so a bare release would hand the
// volunteer a phantom (TB-40). The pool-exhausted fallback still re-admits it once the
// unit has sat uncovered past the grace, so a one-volunteer pool cannot strand.
func TestAbandonUnstarted_BenchesVolunteerOnStagedCandidate(t *testing.T) {
	svc, c, ctx, volID, unitID, advance := tb40Service(t, 18000,
		workunit.ClosedCopy{Outcome: "ABANDONED"})

	if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 1 {
		t.Fatalf("initial hand-out = %d results, want 1", len(res))
	}

	if _, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  unitID.String(),
		VolunteerId: volID.String(),
		Reason:      engineDownReason,
	}); err != nil {
		t.Fatalf("AbandonWorkUnit(un-started): %v", err)
	}

	// The client's runtime breaker retries ten minutes later (runtimeAbandonCooldown);
	// the volunteer's next poll for this unit must be refused in memory.
	if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 0 {
		t.Fatalf("hand-out inside the abandon cooldown = %d results, want 0: an un-started ABANDONED close must bench the volunteer on the still-staged candidate (TB-81)", len(res))
	}

	// Past the pool-exhausted fallback grace with the unit still uncovered: the bench
	// yields (PB-9), the same escape every other benching close has.
	advance(3 * time.Minute)
	c.refreshStaleLeafSnapshots(context.Background())
	if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 1 {
		t.Fatal("volunteer still refused past the pool-exhausted fallback grace with the unit uncovered: the un-started-abandon bench must carry the outcome time and yield like the SQL cooldown's fallback, or a small pool strands (PB-9)")
	}
}

// tb81ReliabilityRepo records every RecordOutcome the abandon path makes.
type tb81ReliabilityRepo struct {
	reliability.Repository
	mu    sync.Mutex
	calls []tb81ReliabilityCall
}

type tb81ReliabilityCall struct {
	host types.ID
	good bool
}

func (r *tb81ReliabilityRepo) RecordOutcome(_ context.Context, host types.ID, good bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, tb81ReliabilityCall{host: host, good: good})
	return nil
}

func (r *tb81ReliabilityRepo) recorded() []tb81ReliabilityCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tb81ReliabilityCall(nil), r.calls...)
}

// TestAbandon_DentsHostReliabilityForAbandonedNotReturned pins TB-81's reliability
// half: an ABANDONED close records one bad outcome for the machine the copy was
// charged to (the copy row's host_id, folding onto the account when none was
// reported) — the signal the fault monitor records for a timeout, which the abandon
// path never recorded (the operator report's host had 188 abandons and no
// host_reliability row at all). A RETURNED give-back records nothing: returning
// un-run buffered work promptly is cooperative (TB-35).
func TestAbandon_DentsHostReliabilityForAbandonedNotReturned(t *testing.T) {
	host := types.NewID()

	t.Run("abandoned, host reported", func(t *testing.T) {
		rel := &tb81ReliabilityRepo{}
		svc, c, ctx, volID, unitID, _ := tb40Service(t, 18000,
			workunit.ClosedCopy{Outcome: "ABANDONED", HostID: &host})
		svc.reliabilityRepo = rel
		if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 1 {
			t.Fatalf("initial hand-out = %d results, want 1", len(res))
		}
		if _, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
			WorkUnitId: unitID.String(), VolunteerId: volID.String(), Reason: engineDownReason,
		}); err != nil {
			t.Fatalf("AbandonWorkUnit: %v", err)
		}
		calls := rel.recorded()
		if len(calls) != 1 {
			t.Fatalf("RecordOutcome calls after an un-started ABANDONED close = %d, want 1 (TB-81: the abandon path must dent the holder's reliability like the deadline sweep does)", len(calls))
		}
		if calls[0].host != host || calls[0].good {
			t.Fatalf("RecordOutcome(%v, good=%v), want (%v, good=false): the dent is charged to the copy's host", calls[0].host, calls[0].good, host)
		}
	})

	t.Run("abandoned, no host reported folds onto the account", func(t *testing.T) {
		rel := &tb81ReliabilityRepo{}
		svc, c, ctx, volID, unitID, _ := tb40Service(t, 18000,
			workunit.ClosedCopy{Outcome: "ABANDONED"})
		svc.reliabilityRepo = rel
		if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 1 {
			t.Fatalf("initial hand-out = %d results, want 1", len(res))
		}
		if _, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
			WorkUnitId: unitID.String(), VolunteerId: volID.String(), Reason: "execution failed: exit status 1",
		}); err != nil {
			t.Fatalf("AbandonWorkUnit: %v", err)
		}
		calls := rel.recorded()
		if len(calls) != 1 || calls[0].host != volID || calls[0].good {
			t.Fatalf("RecordOutcome calls = %+v, want exactly one bad outcome keyed on the account %v (COALESCE(host_id, volunteer_id))", calls, volID)
		}
	})

	t.Run("returned give-back records nothing", func(t *testing.T) {
		rel := &tb81ReliabilityRepo{}
		svc, c, ctx, volID, unitID, _ := tb40Service(t, 18000,
			workunit.ClosedCopy{Outcome: "RETURNED", HostID: &host})
		svc.reliabilityRepo = rel
		if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 1 {
			t.Fatalf("initial hand-out = %d results, want 1", len(res))
		}
		if _, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
			WorkUnitId: unitID.String(), VolunteerId: volID.String(),
			Reason: "work buffer full (over the hours target)", UnrunGiveback: true,
		}); err != nil {
			t.Fatalf("AbandonWorkUnit(giveback): %v", err)
		}
		if calls := rel.recorded(); len(calls) != 0 {
			t.Fatalf("RecordOutcome calls after a RETURNED give-back = %+v, want none (a give-back is not a reliability signal, TB-35)", calls)
		}
	})
}

// tb82ReaperRepo is the fault monitor's repository for a scan whose only work is the
// lapsed buffered copies it is seeded with: FindExpiredCopies yields them once,
// CloseCopy records the close, and every other sweep ScanOnce runs is empty. The
// embedded nil interface panics on anything else — which this path never calls.
type tb82ReaperRepo struct {
	workunit.WorkUnitRepository
	mu      sync.Mutex
	expired []*workunit.Copy
	closed  []types.ID
}

func (r *tb82ReaperRepo) FindExpiredCopies(context.Context, int) ([]*workunit.Copy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.expired
	r.expired = nil
	return out, nil
}

func (r *tb82ReaperRepo) CloseCopy(_ context.Context, copyID types.ID, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = append(r.closed, copyID)
	return nil
}

func (r *tb82ReaperRepo) FindStuckSpotCheckUnits(context.Context, int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}

func (r *tb82ReaperRepo) ClearExpiredDispatchClaims(context.Context) (int64, error) {
	return 0, nil
}

func (r *tb82ReaperRepo) FindRunningWithStaleCheckpoints(context.Context, int) ([]workunit.StaleCheckpointInfo, error) {
	return nil, nil
}

// tb82Cache builds a test cache with a settable clock and ONE staged candidate on a
// native leaf of the given redundancy, with a real copy deadline so the close-time
// bench window is the deadline (not the 1 s floor).
func tb82Cache(t *testing.T, redundancy int) (c *dispatchCache, unitID types.ID, setNow func(time.Time)) {
	t.Helper()
	leafRepo := &fakeLeafRepo{}
	c = newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})
	now := time.Now().UTC()
	c.now = func() time.Time { return now }
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, redundancy, false, 0), leafRepo)
	unitID = types.NewID()
	c.mu.Lock()
	c.ready = append(c.ready, candidate{
		unit: &workunit.WorkUnit{
			ID:              unitID,
			LeafID:          leafID,
			State:           workunit.WorkUnitStateQueued,
			DeadlineSeconds: 18000,
		},
		effectiveRedundancy: redundancy,
	})
	c.mu.Unlock()
	return c, unitID, func(at time.Time) { now = at }
}

// excludedFromRefill reports whether the cache would exclude unitID from its next
// refill (a fossil hold's most damaging effect: the unit is never re-staged).
func excludedFromRefill(c *dispatchCache, unitID types.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return containsID(c.excludedIDsLocked(), unitID)
}

// TestFaultMonitor_ReaperClose_ReleasesHoldAndBenchesHolder is TB-82's filed
// reproduction: stage a unit, hand it to volunteer V (an in-memory hold), close V's
// copy through the FAULT MONITOR's path — the lapsed-reservation sweep, not the
// abandon RPC — and expect the cache to learn of it. Two shapes:
//
//   - redundancy 1 (the operator's f13/f14 units after their last copy): the candidate
//     left the ready pool when V took its only copy, so all the cache holds is V's
//     hold; after the reaper closes that copy the unit must no longer be excluded from
//     refill (pre-fix it was excluded for the life of the process — the 30-day silence).
//   - redundancy 2 (the candidate stays staged): after the reaper's close, W takes a
//     copy and a second fresh volunteer X takes the other — pre-fix X was refused
//     because V's fossil hold still filled one of the two slots — while V itself is
//     benched for the deadline window, like any other benching close.
func TestFaultMonitor_ReaperClose_ReleasesHoldAndBenchesHolder(t *testing.T) {
	past := time.Now().UTC().Add(-time.Minute)

	t.Run("last copy: unit no longer excluded from refill", func(t *testing.T) {
		c, unitID, _ := tb82Cache(t, 1)
		volV := types.NewID()
		if res, _ := c.HandOut(volV, capableOpts(volV, 0), 1); len(res) != 1 {
			t.Fatalf("hand-out to V = %d results, want 1", len(res))
		}
		if !excludedFromRefill(c, unitID) {
			t.Fatal("sanity: a held unit is excluded from refill")
		}

		ref := NewDispatchCacheRef()
		ref.set(c)
		repo := &tb82ReaperRepo{expired: []*workunit.Copy{{
			ID: types.NewID(), WorkUnitID: unitID, VolunteerID: volV,
			ReservedUntil: &past, DeadlineSeconds: 18000,
		}}}
		m := newSpotCheckMonitor(repo, &spyEvaluator{outcome: transition.OutcomeWaiting}).WithDispatchCache(ref)
		if err := m.ScanOnce(context.Background()); err != nil {
			t.Fatalf("ScanOnce: %v", err)
		}
		if len(repo.closed) != 1 {
			t.Fatalf("sanity: the reaper closed %d copies, want 1", len(repo.closed))
		}

		if c.hasInMemReservation(unitID, volV) {
			t.Fatal("V's in-memory hold survived the reaper's close of its copy (TB-82)")
		}
		if excludedFromRefill(c, unitID) {
			t.Fatal("excludedIDsLocked still lists the unit after the reaper closed its only copy: the fossil hold keeps it out of every refill, offered to nobody until a restart (TB-82)")
		}
	})

	t.Run("staged candidate: fresh volunteers get the freed slot, the holder is benched", func(t *testing.T) {
		c, unitID, setNow := tb82Cache(t, 2)
		base := c.now()
		volV, volW, volX := types.NewID(), types.NewID(), types.NewID()
		if res, _ := c.HandOut(volV, capableOpts(volV, 0), 1); len(res) != 1 {
			t.Fatalf("hand-out to V = %d results, want 1", len(res))
		}

		ref := NewDispatchCacheRef()
		ref.set(c)
		repo := &tb82ReaperRepo{expired: []*workunit.Copy{{
			ID: types.NewID(), WorkUnitID: unitID, VolunteerID: volV,
			ReservedUntil: &past, DeadlineSeconds: 18000,
		}}}
		m := newSpotCheckMonitor(repo, &spyEvaluator{outcome: transition.OutcomeWaiting}).WithDispatchCache(ref)
		if err := m.ScanOnce(context.Background()); err != nil {
			t.Fatalf("ScanOnce: %v", err)
		}

		if res, _ := c.HandOut(volW, capableOpts(volW, 0), 1); len(res) != 1 {
			t.Fatalf("hand-out to W after the reaper's close = %d results, want 1", len(res))
		}
		if res, _ := c.HandOut(volX, capableOpts(volX, 0), 1); len(res) != 1 {
			t.Fatal("fresh volunteer X refused after the reaper closed V's copy: V's fossil hold still fills a redundancy slot the copy no longer occupies (TB-82)")
		}
		// V is benched for the deadline window (the SQL gate refuses it; the in-memory
		// mirror must too, TB-40) — inside the window and inside the grace.
		setNow(base.Add(time.Minute))
		if res, _ := c.HandOut(volV, capableOpts(volV, 0), 1); len(res) != 0 {
			t.Fatalf("hand-out to V inside the bench window = %d results, want 0: the reaper's close must bench the holder like the abandon RPC's does", len(res))
		}
	})
}

// TestReconcile_ReleasesHoldWhoseLeaseLapsedUnannounced pins the reconcile-time
// backstop behind TB-82: a hold whose lease (the copy's reserved_until) lapsed more
// than lapsedHoldGrace ago is a fossil by construction — the copy was run-started (the
// hold already converted) or reaped — so the reconciler drops it, healing any close
// this replica was never told about within one tick of the grace. Inside the grace
// the hold stands (the reaper's own close-time release gets there first).
func TestReconcile_ReleasesHoldWhoseLeaseLapsedUnannounced(t *testing.T) {
	c, unitID, setNow := tb82Cache(t, 1)
	base := c.now()
	volV := types.NewID()
	if res, _ := c.HandOut(volV, capableOpts(volV, 0), 1); len(res) != 1 {
		t.Fatalf("hand-out to V = %d results, want 1", len(res))
	}
	lease := 18000 * time.Second // the hold's lease is the unit's deadline

	setNow(base.Add(lease + lapsedHoldGrace - time.Second))
	c.reconcileOnce(context.Background())
	if !c.hasInMemReservation(unitID, volV) {
		t.Fatal("hold dropped inside the grace: the reconciler must leave the close-time paths their window")
	}

	setNow(base.Add(lease + lapsedHoldGrace + time.Second))
	c.reconcileOnce(context.Background())
	if c.hasInMemReservation(unitID, volV) {
		t.Fatal("hold still present past the lease + grace: the reconciler must release a hold whose lease lapsed unannounced (TB-82 backstop)")
	}
	if excludedFromRefill(c, unitID) {
		t.Fatal("unit still excluded from refill after its fossil hold lapsed")
	}
}
