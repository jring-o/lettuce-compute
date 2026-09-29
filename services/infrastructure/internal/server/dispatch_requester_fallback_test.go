package server

import (
	"errors"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// The per-requester database fallback. The ready pool is stocked oldest-first without
// regard to who asks, so an account whose results fill the oldest units of a leaf can find
// every staged candidate refusing it (it already has a result on each, and each waits for a
// second account) while the database still holds units it has never touched. These tests pin
// that such a requester is served from the database directly, that the shared pool is left
// exactly as it was, that ALREADY_CONTRIBUTED ("every task") is said only once the database
// agrees, and that a request the pool can serve never reaches the database.

// freshUnits returns n QUEUED units of leafID that nobody has touched.
func freshUnits(leafID types.ID, n int) []*workunit.WorkUnit {
	out := make([]*workunit.WorkUnit, n)
	for i := range out {
		out[i] = &workunit.WorkUnit{
			ID:              types.NewID(),
			LeafID:          leafID,
			State:           workunit.WorkUnitStateQueued,
			Priority:        workunit.WorkUnitPriorityNormal,
			DeadlineSeconds: 3600,
		}
	}
	return out
}

// ownContributionsPool builds a cache whose ready pool is full (100 of 100) of leafN units
// that each already carry vol's result and wait for a second account.
func ownContributionsPool(t *testing.T) (c *dispatchCache, wuRepo *fakeWURepo, leafN, vol types.ID, staged []types.ID) {
	t.Helper()
	wuRepo = &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c = newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	leafN = types.NewID()
	c.warm(nativeLeaf(leafN, 2, false, 0), leafRepo)
	vol = types.NewID()
	staged = make([]types.ID, c.cfg.readyPoolSize)
	for i := range staged {
		staged[i] = types.NewID()
		c.stageUnitSets(staged[i], leafN, 2, 1, []types.ID{vol}, nil)
	}
	return c, wuRepo, leafN, vol, staged
}

func stagedIDs(c *dispatchCache) []types.ID {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]types.ID, len(c.ready))
	for i := range c.ready {
		out[i] = c.ready[i].unit.ID
	}
	return out
}

func leafOpts(vol, leafID types.ID, maxInflight int) workunit.AssignmentOptions {
	opts := capableOpts(vol, maxInflight)
	opts.LeafIDs = []types.ID{leafID}
	return opts
}

// TestRequesterFallback_PoolOfOwnContributions_HandsOutUntouchedUnits is the field shape:
// every staged candidate of the requested leaf refuses the requester (its own result), three
// units it has never touched are QUEUED in the database, and it asks for 64. It must be handed
// those three, straight from the database, with the pool untouched.
func TestRequesterFallback_PoolOfOwnContributions_HandsOutUntouchedUnits(t *testing.T) {
	c, wuRepo, leafN, vol, staged := ownContributionsPool(t)
	fresh := freshUnits(leafN, 3)
	// The volunteer-agnostic refill would return them too, but the pool is full of the
	// demanded leaf and nothing may be evicted for it.
	wuRepo.dispatchFn = func(int, []types.ID, []types.ID) ([]workunit.DispatchCandidate, error) {
		out := make([]workunit.DispatchCandidate, len(fresh))
		for i, u := range fresh {
			cp := *u
			out[i] = workunit.DispatchCandidate{WorkUnit: &cp, LeafID: leafN, RedundancyFactor: 2, Runtime: leaf.RuntimeNative}
		}
		return out, nil
	}
	var gotOpts workunit.AssignmentOptions
	var gotLimit int
	var gotExcluded []types.ID
	wuRepo.assignableFn = func(opts workunit.AssignmentOptions, limit int, excluded []types.ID, _ types.ID, _ time.Duration) ([]*workunit.WorkUnit, error) {
		gotOpts, gotLimit, gotExcluded = opts, limit, excluded
		out := make([]*workunit.WorkUnit, len(fresh))
		for i, u := range fresh {
			cp := *u
			out[i] = &cp
		}
		return out, nil
	}
	var landed []workunit.FlushReservation
	wuRepo.flushFn = func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error) {
		landed = append(landed, recs...)
		out := make([]workunit.FlushedCopy, len(recs))
		for i, r := range recs {
			out[i] = workunit.FlushedCopy{WorkUnitID: r.WorkUnitID, VolunteerID: r.VolunteerID}
		}
		return out, nil
	}

	res, _, noWork := c.HandOutWithReason(vol, leafOpts(vol, leafN, 0), 64)
	if len(res) != len(fresh) {
		t.Fatalf("handed %d units, reason %v; want the %d untouched units from the database", len(res), noWork.reason, len(fresh))
	}
	for i, r := range res {
		if r.unit.ID != fresh[i].ID {
			t.Fatalf("hand-out %d is %s, want %s (the database's order)", i, r.unit.ID, fresh[i].ID)
		}
		if r.unit.ReservedVolunteerID == nil || *r.unit.ReservedVolunteerID != vol || r.unit.ReservedUntil == nil {
			t.Fatalf("hand-out %d carries no reservation echo for the requester", i)
		}
	}
	if noWork.reason != lettucev1.NoWorkReason_NO_WORK_REASON_UNSPECIFIED {
		t.Fatalf("a reply carrying work carries reason %v", noWork.reason)
	}

	// The shared pool is exactly as it was: same candidates, same order, nothing staged.
	after := stagedIDs(c)
	if len(after) != len(staged) {
		t.Fatalf("ready pool holds %d candidates after the fallback, want the same %d", len(after), len(staged))
	}
	for i := range staged {
		if after[i] != staged[i] {
			t.Fatalf("ready pool changed at %d", i)
		}
	}

	// The query was the requester's own: its leaf, its ask, skipping what the cache holds.
	if len(gotOpts.LeafIDs) != 1 || gotOpts.LeafIDs[0] != leafN || gotOpts.VolunteerID != vol {
		t.Fatalf("fallback query options = %+v, want the request's own", gotOpts)
	}
	if gotLimit != 64 {
		t.Fatalf("fallback asked for %d, want the request's 64", gotLimit)
	}
	skip := make(map[types.ID]bool, len(gotExcluded))
	for _, id := range gotExcluded {
		skip[id] = true
	}
	for _, id := range staged {
		if !skip[id] {
			t.Fatalf("fallback query did not skip staged unit %s", id)
		}
	}

	// The copies landed synchronously through the shared landing write, and the requester
	// holds them like any hand-out: counted in flight, nothing left queued for the flusher.
	if len(landed) != len(fresh) {
		t.Fatalf("landing write saw %d copies, want %d", len(landed), len(fresh))
	}
	for i, rec := range landed {
		if rec.WorkUnitID != fresh[i].ID || rec.VolunteerID != vol || rec.DeadlineSeconds != 3600 {
			t.Fatalf("landed copy %d = %+v", i, rec)
		}
	}
	for _, u := range fresh {
		if !c.hasInMemReservation(u.ID, vol) {
			t.Fatalf("requester holds no reservation on handed unit %s", u.ID)
		}
	}
	if got := c.inflightFor(vol); got != len(fresh) {
		t.Fatalf("in-flight = %d, want %d", got, len(fresh))
	}
	if n := c.pendingWriteCount(); n != 0 {
		t.Fatalf("%d reservation writes left queued; the fallback lands its own", n)
	}
}

// TestRequesterFallback_AlreadyContributedOnlyWhenTheDatabaseAgrees pins the reason: "every
// task" may be said only once the per-requester query has also found nothing.
func TestRequesterFallback_AlreadyContributedOnlyWhenTheDatabaseAgrees(t *testing.T) {
	contributed := lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED
	unspecified := lettucev1.NoWorkReason_NO_WORK_REASON_UNSPECIFIED

	t.Run("the database has nothing either", func(t *testing.T) {
		c, wuRepo, leafN, vol, _ := ownContributionsPool(t)
		res, _, noWork := c.HandOutWithReason(vol, leafOpts(vol, leafN, 0), 64)
		if len(res) != 0 || noWork.reason != contributed {
			t.Fatalf("handed %d, reason %v; want nothing and ALREADY_CONTRIBUTED", len(res), noWork.reason)
		}
		if wuRepo.assignableCalls != 1 {
			t.Fatalf("per-requester query ran %d times, want 1", wuRepo.assignableCalls)
		}
	})

	t.Run("the database could not be read", func(t *testing.T) {
		c, wuRepo, leafN, vol, _ := ownContributionsPool(t)
		wuRepo.assignableFn = func(workunit.AssignmentOptions, int, []types.ID, types.ID, time.Duration) ([]*workunit.WorkUnit, error) {
			return nil, errors.New("connection reset")
		}
		res, _, noWork := c.HandOutWithReason(vol, leafOpts(vol, leafN, 0), 64)
		if len(res) != 0 || noWork.reason != unspecified {
			t.Fatalf("handed %d, reason %v; an unverified claim must not be sent", len(res), noWork.reason)
		}
	})

	t.Run("the maintenance budget is saturated", func(t *testing.T) {
		c, wuRepo, leafN, vol, _ := ownContributionsPool(t)
		for i := 0; i < cap(c.maintenanceAdmission); i++ {
			c.maintenanceAdmission <- struct{}{}
		}
		res, _, noWork := c.HandOutWithReason(vol, leafOpts(vol, leafN, 0), 64)
		if len(res) != 0 || noWork.reason != unspecified {
			t.Fatalf("handed %d, reason %v; an unverified claim must not be sent", len(res), noWork.reason)
		}
		if wuRepo.assignableCalls != 0 {
			t.Fatalf("per-requester query ran %d times under a saturated budget", wuRepo.assignableCalls)
		}
	})

	t.Run("the machine is at its in-flight cap", func(t *testing.T) {
		c, wuRepo, leafN, vol, _ := ownContributionsPool(t)
		c.mu.Lock()
		c.inflight[vol] = 5
		c.mu.Unlock()
		res, _, noWork := c.HandOutWithReason(vol, leafOpts(vol, leafN, 5), 64)
		if len(res) != 0 || noWork.reason != unspecified {
			t.Fatalf("handed %d, reason %v; want nothing and no unverified claim", len(res), noWork.reason)
		}
		if wuRepo.assignableCalls != 0 {
			t.Fatalf("per-requester query ran %d times for a machine with no room", wuRepo.assignableCalls)
		}
	})
}

// TestRequesterFallback_EmptyAnswerStandsForItsWindow: one empty answer covers that machine and
// request scope for a few minutes, so a starved machine polling every few seconds costs one
// query per window, and the reply keeps saying ALREADY_CONTRIBUTED meanwhile. Another scope is
// its own question, and once the window has passed the database is asked again.
func TestRequesterFallback_EmptyAnswerStandsForItsWindow(t *testing.T) {
	c, wuRepo, leafN, vol, _ := ownContributionsPool(t)
	clock := time.Now()
	c.now = func() time.Time { return clock }
	ask := func(opts workunit.AssignmentOptions) lettucev1.NoWorkReason {
		t.Helper()
		res, _, noWork := c.HandOutWithReason(vol, opts, 64)
		if len(res) != 0 {
			t.Fatalf("handed %d units from an empty database", len(res))
		}
		return noWork.reason
	}
	contributed := lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED
	// The refiller keeps staged leaves' snapshots fresh in production; a reason is only
	// given for a leaf whose snapshot is.
	advance := func(d time.Duration) {
		clock = clock.Add(d)
		c.leafMu.Lock()
		c.leafCache[leafN].fetchedAt = clock
		c.leafMu.Unlock()
	}

	if r := ask(leafOpts(vol, leafN, 0)); r != contributed || wuRepo.assignableCalls != 1 {
		t.Fatalf("first ask: reason %v, %d queries", r, wuRepo.assignableCalls)
	}
	advance(time.Minute)
	if r := ask(leafOpts(vol, leafN, 0)); r != contributed || wuRepo.assignableCalls != 1 {
		t.Fatalf("ask inside the window: reason %v, %d queries (want the standing answer, no query)", r, wuRepo.assignableCalls)
	}
	// A request for another scope (here: this leaf plus one the pool has nothing of) is a new
	// question.
	wider := leafOpts(vol, leafN, 0)
	wider.LeafIDs = append(wider.LeafIDs, types.NewID())
	ask(wider)
	if wuRepo.assignableCalls != 2 {
		t.Fatalf("another scope reused the verdict: %d queries, want 2", wuRepo.assignableCalls)
	}
	advance(time.Hour)
	if r := ask(leafOpts(vol, leafN, 0)); r != contributed || wuRepo.assignableCalls != 3 {
		t.Fatalf("ask after the window: reason %v, %d queries (want a fresh query)", r, wuRepo.assignableCalls)
	}
}

// TestRequesterFallback_BoundedByTheMachinesRoom: the fallback never asks for more than the
// machine's remaining in-flight room, and its hand-outs count against the cap.
func TestRequesterFallback_BoundedByTheMachinesRoom(t *testing.T) {
	c, wuRepo, leafN, vol, _ := ownContributionsPool(t)
	c.mu.Lock()
	c.inflight[vol] = 3
	c.mu.Unlock()
	var gotLimit int
	wuRepo.assignableFn = func(_ workunit.AssignmentOptions, limit int, _ []types.ID, _ types.ID, _ time.Duration) ([]*workunit.WorkUnit, error) {
		gotLimit = limit
		return freshUnits(leafN, limit), nil
	}
	res, _, _ := c.HandOutWithReason(vol, leafOpts(vol, leafN, 5), 64)
	if gotLimit != 2 || len(res) != 2 {
		t.Fatalf("asked for %d and handed %d; want 2 of 2 (cap 5, 3 held)", gotLimit, len(res))
	}
	if got := c.inflightFor(vol); got != 5 {
		t.Fatalf("in-flight = %d, want 5", got)
	}
}

// TestRequesterFallback_OnlyLandedCopiesAreHandedOut: a unit the landing write refuses (the
// database moved between the query and the landing) is not handed out and leaves no hold.
func TestRequesterFallback_OnlyLandedCopiesAreHandedOut(t *testing.T) {
	c, wuRepo, leafN, vol, _ := ownContributionsPool(t)
	fresh := freshUnits(leafN, 3)
	wuRepo.assignableFn = func(workunit.AssignmentOptions, int, []types.ID, types.ID, time.Duration) ([]*workunit.WorkUnit, error) {
		return fresh, nil
	}
	wuRepo.flushFn = func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error) {
		return []workunit.FlushedCopy{{WorkUnitID: recs[0].WorkUnitID, VolunteerID: recs[0].VolunteerID}}, nil
	}
	res, _, _ := c.HandOutWithReason(vol, leafOpts(vol, leafN, 0), 64)
	if len(res) != 1 || res[0].unit.ID != fresh[0].ID {
		t.Fatalf("handed %d units, want only the one that landed", len(res))
	}
	for _, u := range fresh[1:] {
		if c.hasInMemReservation(u.ID, vol) {
			t.Fatalf("a refused copy left a hold on %s", u.ID)
		}
	}
	if got := c.inflightFor(vol); got != 1 {
		t.Fatalf("in-flight = %d, want 1", got)
	}
}

// TestRequesterFallback_CooldownRefusalsQualify: a pool whose candidates refuse the requester
// for its own recent failed copies is the same per-account shape.
func TestRequesterFallback_CooldownRefusalsQualify(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	leafN := types.NewID()
	c.warm(nativeLeaf(leafN, 2, false, 0), leafRepo)
	vol := types.NewID()
	c.stageUnitSets(types.NewID(), leafN, 2, 0, nil, []types.ID{vol})
	wuRepo.assignableFn = func(workunit.AssignmentOptions, int, []types.ID, types.ID, time.Duration) ([]*workunit.WorkUnit, error) {
		return freshUnits(leafN, 1), nil
	}
	if res, _, _ := c.HandOutWithReason(vol, leafOpts(vol, leafN, 0), 8); len(res) != 1 {
		t.Fatalf("handed %d; a cooldown-only pool must reach the per-requester query", len(res))
	}
}

// TestRequesterFallback_ScaleOutTakesTheHeadsClaim: with a head id configured, the fallback's
// query carries it (so it skips other replicas' claims and claims what it returns), and the
// landing renews this head's claim like any flush.
func TestRequesterFallback_ScaleOutTakesTheHeadsClaim(t *testing.T) {
	headID := types.NewID()
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCacheWithHead(wuRepo, leafRepo, &fakeAssignRepo{}, headID, 90*time.Second)
	leafN := types.NewID()
	c.warm(nativeLeaf(leafN, 2, false, 0), leafRepo)
	vol := types.NewID()
	c.stageUnitSets(types.NewID(), leafN, 2, 1, []types.ID{vol}, nil)
	var gotHead types.ID
	var gotLease time.Duration
	wuRepo.assignableFn = func(_ workunit.AssignmentOptions, _ int, _ []types.ID, head types.ID, lease time.Duration) ([]*workunit.WorkUnit, error) {
		gotHead, gotLease = head, lease
		return freshUnits(leafN, 1), nil
	}
	if res, _, _ := c.HandOutWithReason(vol, leafOpts(vol, leafN, 0), 8); len(res) != 1 {
		t.Fatalf("handed %d, want 1", len(res))
	}
	if gotHead != headID || gotLease != 90*time.Second {
		t.Fatalf("fallback query claimed for head %s lease %v, want %s / 90s", gotHead, gotLease, headID)
	}
	if wuRepo.lastFlushHeadID != headID || wuRepo.lastFlushClaimLease != 90*time.Second {
		t.Fatalf("landing ran without this head's claim renewal")
	}
}

// TestRequesterFallback_NotOnTheOrdinaryPath: a request the pool serves never touches the
// database (no query, no synchronous landing), and neither does an empty answer that is not
// the requester's own doing.
func TestRequesterFallback_NotOnTheOrdinaryPath(t *testing.T) {
	t.Run("the pool serves the request", func(t *testing.T) {
		wuRepo := &fakeWURepo{}
		leafRepo := &fakeLeafRepo{}
		c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
		leafN := types.NewID()
		c.warm(nativeLeaf(leafN, 2, false, 0), leafRepo)
		vol := types.NewID()
		c.stageUnitSets(types.NewID(), leafN, 2, 1, []types.ID{vol}, nil) // refuses vol
		c.stageUnit(types.NewID(), leafN, 2, 0)                           // serves vol
		if res, _, _ := c.HandOutWithReason(vol, leafOpts(vol, leafN, 0), 64); len(res) != 1 {
			t.Fatalf("handed %d, want the one eligible staged unit", len(res))
		}
		if wuRepo.assignableCalls != 0 || wuRepo.flushedBatches != 0 {
			t.Fatalf("a served request made %d queries and %d landing writes, want none", wuRepo.assignableCalls, wuRepo.flushedBatches)
		}
	})

	t.Run("nothing fits the machine", func(t *testing.T) {
		wuRepo := &fakeWURepo{}
		leafRepo := &fakeLeafRepo{}
		c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
		gpuLeaf := nativeLeaf(types.NewID(), 2, false, 0)
		gpuLeaf.ResourceRequirements.GPURequired = true
		c.warm(gpuLeaf, leafRepo)
		c.stageUnit(types.NewID(), gpuLeaf.ID, 2, 0)
		vol := types.NewID()
		if res, _, _ := c.HandOutWithReason(vol, capableOpts(vol, 0), 64); len(res) != 0 {
			t.Fatalf("a CPU-only machine was handed a GPU unit")
		}
		if wuRepo.assignableCalls != 0 {
			t.Fatalf("an empty answer that is not the account's own reached the database")
		}
	})
}
