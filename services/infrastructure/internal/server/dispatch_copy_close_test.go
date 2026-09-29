package server

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// A copy the held-copy reconcile releases is closed in the database like a give-back (an
// un-started copy closes RETURNED, a started one ABANDONED), and the SQL landing then refuses
// the same volunteer that unit for the matching cooldown. The cache must learn the same bench,
// or it re-offers the unit to that machine on its next poll and every such hand-out is voided
// at the landing.

// pinnedOpts is a request naming leafID, so the requester is served whatever the age of the
// cache's leaf snapshot (these tests move the clock past its freshness window).
func pinnedOpts(vol, leafID types.ID, host *types.ID) workunit.AssignmentOptions {
	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{leafID}
	opts.HostID = host
	return opts
}

// reconcileRelease hands unitID to vol on host, lands the copy, then has the reconcile release
// it because the machine's next report no longer lists it. It returns the release time.
func reconcileRelease(t *testing.T, c *dispatchCache, wuRepo *fakeWURepo, clock *time.Time, leafID, unitID, vol, host types.ID, started bool) time.Time {
	t.Helper()
	opts := pinnedOpts(vol, leafID, &host)
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 1 || res[0].unit.ID != unitID {
		t.Fatalf("setup: vol was not handed the unit")
	}
	c.flushOnce(context.Background())
	if started {
		c.onRunStart(unitID, vol)
	}
	c.NoteVolunteerHeld(vol, host, nil)
	wuRepo.releaseFn = func(h types.ID, _ []types.ID, _ time.Time) ([]workunit.ReleasedCopy, error) {
		if h != host {
			return nil, nil
		}
		return []workunit.ReleasedCopy{{WorkUnitID: unitID, Started: started}}, nil
	}
	*clock = clock.Add(reconcileGracePeriod + time.Second)
	c.reconcileHeldCopies(context.Background())
	if wuRepo.releaseCalls != 1 {
		t.Fatalf("setup: reconcile made %d release calls, want 1", wuRepo.releaseCalls)
	}
	return *clock
}

// TestHeldCopyReconcile_ReleasedHolderIsNotReofferedInsideTheGiveBackWindow is the field
// shape: a redundancy unit stays staged for its other copies after vol's copy is released,
// so without the bench vol is handed it again at once.
func TestHeldCopyReconcile_ReleasedHolderIsNotReofferedInsideTheGiveBackWindow(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	clock := time.Now()
	c.now = func() time.Time { return clock }
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 3, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 3, 0)
	vol, host := types.NewID(), types.NewID()

	// Another volunteer holds a copy too, so the unit is not "uncovered" and the
	// pool-exhausted fallback cannot re-admit vol early: only the give-back window decides.
	other := types.NewID()
	if res, _ := c.HandOut(other, pinnedOpts(other, leafID, nil), 1); len(res) != 1 {
		t.Fatalf("setup: other volunteer was not handed the unit")
	}
	released := reconcileRelease(t, c, wuRepo, &clock, leafID, unitID, vol, host, false)

	opts := pinnedOpts(vol, leafID, &host)
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 0 {
		t.Fatalf("the machine whose copy the reconcile just released was handed the unit again " +
			"inside the give-back cooldown; the landing write would refuse it")
	}
	c.mu.Lock()
	var cand candidate
	for _, cd := range c.ready {
		if cd.unit.ID == unitID {
			cand = cd
		}
	}
	_, reason := c.eligibleLocked(vol, host, opts, cand)
	c.mu.Unlock()
	if reason != rejectBenched {
		t.Fatalf("refusal reason %v, want %v (the post-failure cooldown arm)", reason, rejectBenched)
	}

	clock = released.Add(workunit.ReturnedReofferCooldownSeconds*time.Second - time.Second)
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 0 {
		t.Fatalf("re-offered one second before the give-back window ends")
	}
	clock = released.Add(workunit.ReturnedReofferCooldownSeconds*time.Second + time.Second)
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 1 {
		t.Fatalf("not re-offered once the give-back window ended")
	}
}

// TestHeldCopyReconcile_ReleasedRunningCopyBenchesForTheDeadline: a released RUNNING copy is
// closed ABANDONED, which the landing refuses the holder for about one deadline.
func TestHeldCopyReconcile_ReleasedRunningCopyBenchesForTheDeadline(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	clock := time.Now()
	c.now = func() time.Time { return clock }
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 3, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 3, 0)
	c.mu.Lock()
	c.ready[0].unit.DeadlineSeconds = 3600
	c.mu.Unlock()
	vol, host := types.NewID(), types.NewID()

	released := reconcileRelease(t, c, wuRepo, &clock, leafID, unitID, vol, host, true)

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cd := range c.ready {
		if cd.unit.ID != unitID {
			continue
		}
		e, ok := cd.benched[vol]
		if !ok {
			t.Fatalf("released running copy left no bench for its holder")
		}
		if want := released.Add(time.Hour); !e.until.Equal(want) {
			t.Fatalf("bench until %v, want %v (one deadline after the release)", e.until, want)
		}
		return
	}
	t.Fatalf("unit no longer staged")
}
