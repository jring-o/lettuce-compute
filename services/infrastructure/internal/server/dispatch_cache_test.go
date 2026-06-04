package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- test fakes ---------------------------------------------------------------
//
// The dispatch cache's dependency surface (dispatchDeps) holds the full
// workunit.WorkUnitRepository / leaf.Repository / assignment.Repository
// interfaces, but the cache only calls a handful of methods. Each fake embeds the
// interface (so the type satisfies it) and overrides only the methods exercised
// here; any un-overridden method panics if the cache ever calls it unexpectedly.

type fakeWURepo struct {
	workunit.WorkUnitRepository

	mu sync.Mutex
	// flushFn lets a test control which reservations land (return value = landed
	// ids) and observe/stall the flush.
	flushFn func(recs []workunit.FlushReservation) ([]types.ID, error)
	// markSpotCheckFn / stampFn back the deferred spot-check write path.
	markSpotCheckFn func(id types.ID) error
	stampFn         func(id, vol types.ID, lease time.Duration) (*workunit.WorkUnit, error)
	// countFn backs the reconcile.
	countFn func() (map[types.ID]int, error)
	// dispatchFn backs FindDispatchableBatch (the refill); it observes the leaf scope.
	dispatchFn func(limit int, excludeIDs, leafIDs []types.ID) ([]workunit.DispatchCandidate, error)

	flushedBatches int
}

func (f *fakeWURepo) FindDispatchableBatch(_ context.Context, limit int, excludeIDs, leafIDs []types.ID) ([]workunit.DispatchCandidate, error) {
	f.mu.Lock()
	fn := f.dispatchFn
	f.mu.Unlock()
	if fn != nil {
		return fn(limit, excludeIDs, leafIDs)
	}
	return nil, nil
}

func (f *fakeWURepo) FlushReservations(_ context.Context, recs []workunit.FlushReservation) ([]types.ID, error) {
	f.mu.Lock()
	f.flushedBatches++
	fn := f.flushFn
	f.mu.Unlock()
	if fn != nil {
		return fn(recs)
	}
	// Default: every reservation lands.
	ids := make([]types.ID, len(recs))
	for i, r := range recs {
		ids[i] = r.WorkUnitID
	}
	return ids, nil
}

func (f *fakeWURepo) MarkSpotCheck(_ context.Context, id types.ID) error {
	if f.markSpotCheckFn != nil {
		return f.markSpotCheckFn(id)
	}
	return nil
}

func (f *fakeWURepo) StampReservation(_ context.Context, id, vol types.ID, lease time.Duration) (*workunit.WorkUnit, error) {
	if f.stampFn != nil {
		return f.stampFn(id, vol, lease)
	}
	return &workunit.WorkUnit{ID: id}, nil
}

func (f *fakeWURepo) CountActiveByVolunteer(_ context.Context) (map[types.ID]int, error) {
	if f.countFn != nil {
		return f.countFn()
	}
	return map[types.ID]int{}, nil
}

type fakeLeafRepo struct {
	leaf.Repository
	mu     sync.Mutex
	leafs  map[types.ID]*leaf.Leaf
	getErr error
}

func (f *fakeLeafRepo) GetByID(_ context.Context, id types.ID) (*leaf.Leaf, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.leafs[id], nil
}

type fakeAssignRepo struct {
	assignment.Repository
	mu      sync.Mutex
	created []*assignment.AssignmentHistoryEntry
}

func (f *fakeAssignRepo) Create(_ context.Context, entry *assignment.AssignmentHistoryEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, entry)
	return nil
}

func (f *fakeAssignRepo) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

type fakeVolunteerRepo struct {
	volunteer.Repository
	mu       sync.Mutex
	vols     map[types.ID]*volunteer.Volunteer
	getCalls int
	getErr   error
}

func (f *fakeVolunteerRepo) GetByID(_ context.Context, id types.ID) (*volunteer.Volunteer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	v, ok := f.vols[id]
	if !ok {
		return nil, apierror.NotFound("volunteer", id.String())
	}
	return v, nil
}

func (f *fakeVolunteerRepo) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls
}

// --- test helpers -------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestCache builds a cache wired to the given fakes with small, deterministic
// tunables. Background goroutines are NOT started; tests drive HandOut / flushOnce
// directly.
func newTestCache(wuRepo *fakeWURepo, leafRepo *fakeLeafRepo, assignRepo *fakeAssignRepo) *dispatchCache {
	c := newDispatchCache(dispatchCacheConfig{
		readyPoolSize:           100,
		lowWatermark:            10,
		refillBatchSize:         50,
		admissionCap:            4,
		flushInterval:           time.Hour, // never auto-fires in unit tests
		flushBatchSize:          200,
		leaseSeconds:            900,
		maxInflightPerVolunteer: 0,
	}, dispatchDeps{
		wuRepo:     wuRepo,
		leafRepo:   leafRepo,
		assignRepo: assignRepo,
	}, testLogger())
	return c
}

// nativeLeaf builds a NATIVE-runtime leaf that any capable volunteer matches.
func nativeLeaf(id types.ID, redundancy int, spotEnabled bool, spotPct float64) *leaf.Leaf {
	return &leaf.Leaf{
		ID:    id,
		State: leaf.StateActive,
		ExecutionConfig: leaf.ExecutionConfig{
			Runtime:     leaf.RuntimeNative,
			MaxMemoryMB: 512,
		},
		ValidationConfig: leaf.ValidationConfig{
			RedundancyFactor:    redundancy,
			SpotCheckEnabled:    spotEnabled,
			SpotCheckPercentage: spotPct,
		},
		ResourceRequirements: leaf.ResourceRequirements{
			MinCPUCores: 1,
			MinDiskMB:   1,
		},
	}
}

// stageUnit appends a ready candidate for a unit on leafID with the given
// redundancy / active-count. Caller must have warmed the leaf in leafRepo + cache.
func (c *dispatchCache) stageUnit(unitID, leafID types.ID, redundancy, dbActive int) {
	c.mu.Lock()
	c.ready = append(c.ready, candidate{
		unit:                &workunit.WorkUnit{ID: unitID, LeafID: leafID, State: workunit.WorkUnitStateQueued},
		effectiveRedundancy: redundancy,
		dbActiveCount:       dbActive,
	})
	c.mu.Unlock()
}

// capableOpts returns AssignmentOptions for a volunteer that matches nativeLeaf.
func capableOpts(vol types.ID, maxInflight int) workunit.AssignmentOptions {
	return workunit.AssignmentOptions{
		VolunteerID:             vol,
		MaxCPUCores:             8,
		MaxMemoryMB:             4096,
		MaxDiskMB:               100000,
		AvailableRuntimes:       []string{leaf.RuntimeNative},
		MaxInflightPerVolunteer: maxInflight,
	}
}

// warm caches the leaf in both the leaf repo and the cache's metadata map.
func (c *dispatchCache) warm(lf *leaf.Leaf, leafRepo *fakeLeafRepo) {
	leafRepo.mu.Lock()
	if leafRepo.leafs == nil {
		leafRepo.leafs = map[types.ID]*leaf.Leaf{}
	}
	leafRepo.leafs[lf.ID] = lf
	leafRepo.mu.Unlock()
	c.leafMu.Lock()
	c.leafCache[lf.ID] = lf
	c.leafMu.Unlock()
}

// --- tests --------------------------------------------------------------------

// TestHandOutBasic: a single capable volunteer gets a reservation with a window,
// and the unit leaves the ready pool (redundancy 1).
func TestHandOutBasic(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 1, false, 0)
	c.warm(lf, leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 1, 0)

	vol := types.NewID()
	results, _ := c.HandOut(vol, capableOpts(vol, 0), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 hand-out, got %d", len(results))
	}
	if results[0].unit.ID != unitID {
		t.Fatalf("handed out wrong unit")
	}
	if results[0].unit.ReservedUntil == nil || results[0].unit.ReservedVolunteerID == nil {
		t.Fatalf("hand-out missing reservation window")
	}
	if *results[0].unit.ReservedVolunteerID != vol {
		t.Fatalf("reservation volunteer mismatch")
	}
	if got := c.readyLen(); got != 0 {
		t.Fatalf("redundancy-1 unit should leave the ready pool, ready=%d", got)
	}
	if got := c.pendingWriteCount(); got != 1 {
		t.Fatalf("expected 1 pending reservation write, got %d", got)
	}
}

// TestNoDoubleReserveConcurrent: many goroutines hammer HandOut for distinct
// volunteers against a pool of redundancy-1 units WITH THE FLUSHER STALLED. No unit
// may be handed to two volunteers. This is the core no-double-hand property.
func TestNoDoubleReserveConcurrent(t *testing.T) {
	wuRepo := &fakeWURepo{
		// Stall the flush entirely: in-memory bookkeeping alone must prevent
		// double-reserve.
		flushFn: func(recs []workunit.FlushReservation) ([]types.ID, error) {
			return nil, context.DeadlineExceeded
		},
	}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 1, false, 0)
	c.warm(lf, leafRepo)

	const nUnits = 200
	for i := 0; i < nUnits; i++ {
		c.stageUnit(types.NewID(), leafID, 1, 0)
	}

	var mu sync.Mutex
	handedTo := make(map[types.ID]types.ID) // unit -> volunteer

	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vol := types.NewID()
			for i := 0; i < 50; i++ {
				results, _ := c.HandOut(vol, capableOpts(vol, 0), 4)
				for _, r := range results {
					mu.Lock()
					if prev, ok := handedTo[r.unit.ID]; ok {
						mu.Unlock()
						t.Errorf("unit %s double-handed: %s and %s", r.unit.ID, prev, vol)
						return
					}
					handedTo[r.unit.ID] = vol
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(handedTo) > nUnits {
		t.Fatalf("handed out more than the %d available units: %d", nUnits, len(handedTo))
	}
}

// TestRedundancyTwoDistinctHolders: a redundancy-2 NORMAL unit is handed to TWO
// distinct volunteers, but — mirroring the DB's columns-only "one live NORMAL
// reservation at a time" model (reservation_test.go) — only ONE holder is staged
// in memory at a time. The SECOND distinct holder is reached the Layer-1 way: after
// the first holder run-starts (onRunStart, which corresponds to Assign freeing the
// reserved_volunteer_id column in the DB), the unit re-enters the ready pool with one
// active history row and headroom for the second volunteer. Staging both holders from
// one snapshot would make BOTH flush into the single column and the WorkUnitID-keyed
// void-check could not tell which lost, leaking a phantom in-memory holder (the
// blocker this corrects).
func TestRedundancyTwoDistinctHolders(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 2, false, 0)
	c.warm(lf, leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	volA, volB, volC := types.NewID(), types.NewID(), types.NewID()

	rA, _ := c.HandOut(volA, capableOpts(volA, 0), 1)
	if len(rA) != 1 {
		t.Fatalf("volA should get the unit, got %d", len(rA))
	}
	// A NORMAL unit is capped at ONE concurrent in-memory holder: after volA reserves
	// it, it leaves the ready pool (the DB column is now volA's; a second concurrent
	// NORMAL reservation could not land).
	if c.readyLen() != 0 {
		t.Fatalf("NORMAL redundancy-2 unit must leave ready after 1 holder, ready=%d", c.readyLen())
	}
	// volB cannot get it from the cache while volA's reservation is live.
	rBearly, _ := c.HandOut(volB, capableOpts(volB, 0), 1)
	if len(rBearly) != 0 {
		t.Fatalf("volB must NOT get a concurrent 2nd in-memory hold of a NORMAL unit, got %d", len(rBearly))
	}
	// Same volunteer cannot take it again (self-exclusion).
	rAdup, _ := c.HandOut(volA, capableOpts(volA, 0), 1)
	if len(rAdup) != 0 {
		t.Fatalf("self-exclusion violated: volA got the unit twice")
	}

	// volA run-starts: the in-memory reservation becomes an active history row and the
	// DB column is freed. The refiller now legitimately re-stages the still-under-
	// redundancy unit (1 active history row < redundancy 2) for the next volunteer.
	c.onRunStart(unitID, volA)
	c.stageUnit(unitID, leafID, 2, 1) // dbActiveCount=1 (volA's run-started row)

	rB, _ := c.HandOut(volB, capableOpts(volB, 0), 1)
	if len(rB) != 1 {
		t.Fatalf("volB (2nd distinct holder) should get the re-staged unit, got %d", len(rB))
	}
	// Now redundancy is exhausted: 1 active row + 1 in-memory holder == 2.
	if c.readyLen() != 0 {
		t.Fatalf("redundancy-2 unit should be exhausted after 2 holders, ready=%d", c.readyLen())
	}
	rC, _ := c.HandOut(volC, capableOpts(volC, 0), 1)
	if len(rC) != 0 {
		t.Fatalf("redundancy exceeded: volC got a 3rd hand-out")
	}
}

// TestInflightCap: a volunteer at its per-volunteer inflight cap is handed no more.
func TestInflightCap(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 1, false, 0)
	c.warm(lf, leafRepo)
	for i := 0; i < 5; i++ {
		c.stageUnit(types.NewID(), leafID, 1, 0)
	}

	vol := types.NewID()
	results, _ := c.HandOut(vol, capableOpts(vol, 2), 10) // cap 2, request 10
	if len(results) != 2 {
		t.Fatalf("inflight cap should bound hand-outs to 2, got %d", len(results))
	}
	// A second request still returns nothing while at cap.
	more, _ := c.HandOut(vol, capableOpts(vol, 2), 10)
	if len(more) != 0 {
		t.Fatalf("volunteer at inflight cap should get nothing, got %d", len(more))
	}
}

// TestSkipBlockedAndIncapable: blocked-leaf, runtime-mismatch, memory-budget, and
// leaf-id filters all skip a candidate in memory.
func TestSkipBlockedAndIncapable(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 1, false, 0)
	c.warm(lf, leafRepo)
	vol := types.NewID()

	// Blocked leaf.
	c.stageUnit(types.NewID(), leafID, 1, 0)
	blocked := capableOpts(vol, 0)
	blocked.BlockedLeafIDs = []types.ID{leafID}
	if r, _ := c.HandOut(vol, blocked, 1); len(r) != 0 {
		t.Fatalf("blocked-leaf unit was handed out")
	}

	// Runtime mismatch (volunteer only runs WASM, leaf is NATIVE).
	rtMismatch := capableOpts(vol, 0)
	rtMismatch.AvailableRuntimes = []string{leaf.RuntimeWasm}
	if r, _ := c.HandOut(vol, rtMismatch, 1); len(r) != 0 {
		t.Fatalf("runtime-mismatch unit was handed out")
	}

	// Memory budget too small (leaf needs 512 MB).
	lowMem := capableOpts(vol, 0)
	lowMem.MaxMemoryMB = 128
	if r, _ := c.HandOut(vol, lowMem, 1); len(r) != 0 {
		t.Fatalf("over-budget-memory unit was handed out")
	}

	// Leaf-id preference filter excludes a non-listed leaf.
	pref := capableOpts(vol, 0)
	pref.LeafIDs = []types.ID{types.NewID()} // some other leaf
	if r, _ := c.HandOut(vol, pref, 1); len(r) != 0 {
		t.Fatalf("leaf not in preferred set was handed out")
	}

	// The capable volunteer eventually gets it (proving the unit was never voided).
	if r, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(r) != 1 {
		t.Fatalf("capable volunteer should still get the staged unit, got %d", len(r))
	}
}

// TestSpotCheckDeferredKeepsEligible: a spot-check-eligible (100%) redundancy-1
// leaf routes its FIRST hand-out to the deferred spot-check queue, marks the unit
// spot_check with effective redundancy 2, and keeps it staged for a 2nd
// corroborating volunteer.
func TestSpotCheckDeferredKeepsEligible(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 1, true, 100) // 100% spot-check => always spot-checked
	c.warm(lf, leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 1, 0)

	volA := types.NewID()
	rA, _ := c.HandOut(volA, capableOpts(volA, 0), 1)
	if len(rA) != 1 {
		t.Fatalf("volA should get the spot-checked unit")
	}
	// Routed to the spot-check queue, NOT the normal reservation queue.
	if c.pendingWriteCount() != 0 {
		t.Fatalf("spot-check hand-out should not enqueue a NORMAL reservation write")
	}
	c.mu.Lock()
	nSpot := len(c.pendingSpotChecks)
	c.mu.Unlock()
	if nSpot != 1 {
		t.Fatalf("expected 1 deferred spot-check write, got %d", nSpot)
	}
	// Stays staged for a 2nd corroborating volunteer (effective redundancy 2).
	if c.readyLen() != 1 {
		t.Fatalf("spot-checked unit should stay eligible for a 2nd volunteer, ready=%d", c.readyLen())
	}
	volB := types.NewID()
	rB, _ := c.HandOut(volB, capableOpts(volB, 0), 1)
	if len(rB) != 1 {
		t.Fatalf("2nd corroborating volunteer should get the spot-checked unit")
	}

	// Flushing the spot-check queue marks it + stamps a reservation + writes a row.
	c.flushSpotChecksOnce(context.Background())
	if assignRepo.createdCount() == 0 {
		t.Fatalf("spot-check flush should write an assignment_history row")
	}
}

// TestFlushConflictVoidsHandOut: when FlushReservations reports a reservation did
// NOT land (a conflict), the cache voids that in-memory hand-out — releasing the
// holder and decrementing the volunteer's inflight count.
func TestFlushConflictVoidsHandOut(t *testing.T) {
	var conflictID types.ID
	wuRepo := &fakeWURepo{
		flushFn: func(recs []workunit.FlushReservation) ([]types.ID, error) {
			// Land everything EXCEPT conflictID.
			var landed []types.ID
			for _, r := range recs {
				if r.WorkUnitID != conflictID {
					landed = append(landed, r.WorkUnitID)
				}
			}
			return landed, nil
		},
	}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 1, false, 0)
	c.warm(lf, leafRepo)
	conflictID = types.NewID()
	okID := types.NewID()
	c.stageUnit(conflictID, leafID, 1, 0)
	c.stageUnit(okID, leafID, 1, 0)

	vol := types.NewID()
	results, _ := c.HandOut(vol, capableOpts(vol, 0), 2)
	if len(results) != 2 {
		t.Fatalf("expected 2 hand-outs, got %d", len(results))
	}
	// Both are held in memory pre-flush.
	c.mu.Lock()
	if c.inflight[vol] != 2 {
		c.mu.Unlock()
		t.Fatalf("expected inflight 2 before flush, got %d", c.inflight[vol])
	}
	c.mu.Unlock()

	c.flushOnce(context.Background())

	// The conflicted hand-out is voided: its holder released, inflight back to 1.
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, held := c.reservedInMem[conflictID]; held {
		t.Fatalf("conflicted reservation should be voided in memory")
	}
	if _, held := c.reservedInMem[okID]; !held {
		t.Fatalf("landed reservation should remain held")
	}
	if c.inflight[vol] != 1 {
		t.Fatalf("inflight should be decremented to 1 after voiding the conflict, got %d", c.inflight[vol])
	}
}

// TestFlushSameUnitOnlyFirstLands guards the void-by-occurrence accounting: if a
// single flush batch ever carried TWO records for the SAME work_unit_id (distinct
// volunteers) — which the staging cap now prevents for NORMAL units, but which the
// multi-row UPDATE's single reserved_volunteer_id column can only ever satisfy for
// ONE of — the cache must credit the single landed RETURNING id to exactly ONE record
// and VOID the other, never leaving a phantom in-memory holder + inflight that
// reconcile cannot clear (the leak the redundancy blocker described).
func TestFlushSameUnitOnlyFirstLands(t *testing.T) {
	sharedID := types.NewID()
	wuRepo := &fakeWURepo{
		flushFn: func(recs []workunit.FlushReservation) ([]types.ID, error) {
			// The real multi-row UPDATE on a single column returns the unit id ONCE
			// even if two records target it: simulate exactly one landed occurrence.
			return []types.ID{sharedID}, nil
		},
	}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 2, false, 0)
	c.warm(lf, leafRepo)

	volA, volB := types.NewID(), types.NewID()
	// Force the adversarial state directly: two distinct in-memory holders of the same
	// unit, both queued for the NORMAL flush. (HandOut's cap prevents producing this,
	// so we construct it to prove the void path is robust regardless.)
	until := c.now().UTC().Add(time.Hour)
	c.mu.Lock()
	c.reservedInMem[sharedID] = map[types.ID]time.Time{volA: until, volB: until}
	c.inflight[volA] = 1
	c.inflight[volB] = 1
	c.pendingWrites = []workunit.FlushReservation{
		{WorkUnitID: sharedID, VolunteerID: volA, ReservedUntil: until},
		{WorkUnitID: sharedID, VolunteerID: volB, ReservedUntil: until},
	}
	c.mu.Unlock()

	c.flushOnce(context.Background())

	c.mu.Lock()
	defer c.mu.Unlock()
	// Exactly ONE holder survives (the one credited with the landed row); the other is
	// voided. We do not care which, only that the count collapsed from 2 to 1 and the
	// voided volunteer's inflight was released.
	holders := c.reservedInMem[sharedID]
	if len(holders) != 1 {
		t.Fatalf("exactly one holder should survive a single landed row, got %d", len(holders))
	}
	total := c.inflight[volA] + c.inflight[volB]
	if total != 1 {
		t.Fatalf("voided holder's inflight should be released (total inflight want 1, got %d)", total)
	}
}

// TestOnRunStartDropsReservationKeepsInflight: run-start converts a holder's
// reservation into a DB history row — the cache drops the reservation hold but
// keeps the volunteer's inflight count (the slot is still occupied) and removes a
// now-ASSIGNED redundancy-1 unit from ready.
func TestOnRunStartDropsReservationKeepsInflight(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 1, false, 0)
	c.warm(lf, leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 1, 0)

	vol := types.NewID()
	if r, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(r) != 1 {
		t.Fatalf("hand-out failed")
	}
	c.onRunStart(unitID, vol)

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, held := c.reservedInMem[unitID]; held {
		t.Fatalf("run-start should drop the in-memory reservation hold")
	}
	if c.inflight[vol] != 1 {
		t.Fatalf("run-start should KEEP the inflight count (slot still occupied), got %d", c.inflight[vol])
	}
}

// TestOnUnitDoneEvicts: completion evicts a unit from the ledger and ready pool and
// releases every holder's inflight count.
func TestOnUnitDoneEvicts(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 2, false, 0)
	c.warm(lf, leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	volA, volB := types.NewID(), types.NewID()
	c.HandOut(volA, capableOpts(volA, 0), 1)
	c.HandOut(volB, capableOpts(volB, 0), 1)

	c.onUnitDone(unitID)

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, held := c.reservedInMem[unitID]; held {
		t.Fatalf("completed unit should be fully evicted from the ledger")
	}
	if len(c.inflight) != 0 {
		t.Fatalf("all holders' inflight should be released, got %v", c.inflight)
	}
	if c.readyContainsLocked(unitID) {
		t.Fatalf("completed unit should be removed from ready")
	}
}

// TestReconcileRebuildsInflightFromDB: the reconcile replaces the in-memory
// inflight counters with the authoritative DB count plus not-yet-flushed pending
// reservations.
func TestReconcileRebuildsInflightFromDB(t *testing.T) {
	volDB := types.NewID()
	wuRepo := &fakeWURepo{
		countFn: func() (map[types.ID]int, error) {
			return map[types.ID]int{volDB: 3}, nil
		},
	}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	// Seed a stale/drifted in-memory counter and one not-yet-flushed pending write.
	c.mu.Lock()
	c.inflight[volDB] = 99 // drifted
	c.pendingWrites = append(c.pendingWrites, workunit.FlushReservation{
		WorkUnitID: types.NewID(), VolunteerID: volDB, ReservedUntil: time.Now().Add(time.Hour),
	})
	c.mu.Unlock()

	c.reconcileOnce(context.Background())

	c.mu.Lock()
	defer c.mu.Unlock()
	// DB authoritative 3 + 1 pending = 4 (the drifted 99 is discarded).
	if c.inflight[volDB] != 4 {
		t.Fatalf("reconcile should set inflight to DB(3)+pending(1)=4, got %d", c.inflight[volDB])
	}
}

// TestRefillExcludesInMemoryHeld: the refill exclude-set covers every id the cache
// currently holds in ready or reservedInMem (the DB-level backstop).
func TestRefillExcludesInMemoryHeld(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 1, false, 0)
	c.warm(lf, leafRepo)
	readyID := types.NewID()
	c.stageUnit(readyID, leafID, 1, 0)

	// Hand one out so it moves to reservedInMem.
	heldID := types.NewID()
	c.stageUnit(heldID, leafID, 1, 0)
	vol := types.NewID()
	c.HandOut(vol, capableOpts(vol, 0), 1)

	c.mu.Lock()
	excluded := c.excludedIDsLocked()
	c.mu.Unlock()

	hasReady, hasHeld := false, false
	for _, id := range excluded {
		if id == readyID {
			hasReady = true
		}
		if id == heldID {
			hasHeld = true
		}
	}
	if !hasReady || !hasHeld {
		t.Fatalf("exclude set must cover both ready and reserved ids (ready=%v held=%v)", hasReady, hasHeld)
	}
}

// --- Blocker 1: volunteer-identity snapshot off the hot path ------------------

// TestResolveIdentityWarmedNoDB asserts a warmed identity snapshot resolves entirely
// in memory with NO volunteer-repo DB touch (the hot-path property).
func TestResolveIdentityWarmedNoDB(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)
	volRepo := &fakeVolunteerRepo{vols: map[types.ID]*volunteer.Volunteer{}}
	c.deps.volunteerRepo = volRepo

	volID := types.NewID()
	pk := make([]byte, 32)
	pk[0] = 7
	c.putIdentity(&volunteer.Volunteer{
		ID:                   volID,
		PublicKey:            pk,
		AvailableRuntimes:    []string{leaf.RuntimeNative},
		HardwareCapabilities: volunteer.HardwareCapabilities{MaxCPUCores: 4},
	})

	ident, notFound, shed := c.resolveIdentity(volID)
	if notFound || shed || ident == nil {
		t.Fatalf("warmed identity should resolve (notFound=%v shed=%v nil=%v)", notFound, shed, ident == nil)
	}
	if ident.hardware.MaxCPUCores != 4 || len(ident.availableRuntimes) != 1 {
		t.Fatalf("identity snapshot fields not carried")
	}
	if volRepo.calls() != 0 {
		t.Fatalf("warmed identity must not touch the volunteer repo, got %d calls", volRepo.calls())
	}
}

// TestResolveIdentityLazyMissThenCached asserts a cold miss fetches from the repo
// once, then caches so subsequent resolves are in-memory.
func TestResolveIdentityLazyMissThenCached(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	volID := types.NewID()
	pk := make([]byte, 32)
	volRepo := &fakeVolunteerRepo{vols: map[types.ID]*volunteer.Volunteer{
		volID: {ID: volID, PublicKey: pk, AvailableRuntimes: []string{leaf.RuntimeNative}},
	}}
	c.deps.volunteerRepo = volRepo

	if _, nf, shed := c.resolveIdentity(volID); nf || shed {
		t.Fatalf("cold resolve should succeed (notFound=%v shed=%v)", nf, shed)
	}
	if _, nf, shed := c.resolveIdentity(volID); nf || shed {
		t.Fatalf("second resolve should succeed")
	}
	if volRepo.calls() != 1 {
		t.Fatalf("expected exactly ONE repo fetch (then cached), got %d", volRepo.calls())
	}
}

// TestResolveIdentityNotFoundAndShed asserts a 404 maps to notFound, and a saturated
// admission semaphore maps to shed (not a DB-pool collapse).
func TestResolveIdentityNotFoundAndShed(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)
	c.deps.volunteerRepo = &fakeVolunteerRepo{vols: map[types.ID]*volunteer.Volunteer{}}

	if _, nf, _ := c.resolveIdentity(types.NewID()); !nf {
		t.Fatalf("unknown volunteer should resolve notFound")
	}

	// Saturate the admission semaphore so the lazy DB read cannot be admitted.
	for i := 0; i < cap(c.admission); i++ {
		c.admission <- struct{}{}
	}
	_, nf, shed := c.resolveIdentity(types.NewID())
	if nf {
		t.Fatalf("saturated admission should shed, not 404")
	}
	if !shed {
		t.Fatalf("saturated admission should shed")
	}
}

// --- Blocker 2: leaf-filtered starvation triggers a leaf-scoped refill ---------

// TestLeafFilteredStarvationRequestsLeafRefill asserts that a requester filtered to
// leaf B that gets nothing (the pool holds only leaf A's units) records a pending
// leaf-scoped refill for B.
func TestLeafFilteredStarvationRequestsLeafRefill(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafA := types.NewID()
	leafB := types.NewID()
	c.warm(nativeLeaf(leafA, 1, false, 0), leafRepo)
	c.warm(nativeLeaf(leafB, 1, false, 0), leafRepo)
	// Pool holds only leaf A's units.
	for i := 0; i < 5; i++ {
		c.stageUnit(types.NewID(), leafA, 1, 0)
	}

	vol := types.NewID()
	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{leafB} // requester wants leaf B only
	results, _ := c.HandOut(vol, opts, 4)
	if len(results) != 0 {
		t.Fatalf("leaf-B requester should get nothing from a leaf-A pool, got %d", len(results))
	}
	pending := c.drainLeafRefills()
	if len(pending) != 1 || pending[0] != leafB {
		t.Fatalf("expected a pending leaf-scoped refill for leaf B, got %v", pending)
	}
}

// TestLeafRefillOnceStagesScopedUnits asserts leafRefillOnce fetches leaf-scoped
// candidates (even though the pool is above the watermark) and stages them, passing
// the requested leaf scope to the repo.
func TestLeafRefillOnceStagesScopedUnits(t *testing.T) {
	leafA := types.NewID()
	leafB := types.NewID()
	bUnit := types.NewID()

	var gotLeafScope []types.ID
	wuRepo := &fakeWURepo{
		dispatchFn: func(limit int, excludeIDs, leafIDs []types.ID) ([]workunit.DispatchCandidate, error) {
			gotLeafScope = leafIDs
			return []workunit.DispatchCandidate{{
				WorkUnit:          &workunit.WorkUnit{ID: bUnit, LeafID: leafB, State: workunit.WorkUnitStateQueued},
				LeafID:            leafB,
				RedundancyFactor:  1,
				ActiveAssignments: 0,
			}}, nil
		},
	}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)
	c.warm(nativeLeaf(leafA, 1, false, 0), leafRepo)
	c.warm(nativeLeaf(leafB, 1, false, 0), leafRepo)
	// Pool above the watermark with leaf A units (so refillOnce would do nothing).
	for i := 0; i < 20; i++ {
		c.stageUnit(types.NewID(), leafA, 1, 0)
	}

	c.requestLeafRefill([]types.ID{leafB})
	c.leafRefillOnce(context.Background())

	if len(gotLeafScope) != 1 || gotLeafScope[0] != leafB {
		t.Fatalf("leafRefillOnce did not pass the leaf scope to the repo, got %v", gotLeafScope)
	}
	if !c.readyContainsLockedTest(bUnit) {
		t.Fatalf("leaf-scoped refill did not stage leaf B's unit")
	}
}

func (c *dispatchCache) readyContainsLockedTest(id types.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readyContainsLocked(id)
}

// --- Major 3: StartWork flush-race (in-memory reservation) --------------------

// TestHasInMemReservationFlushRace asserts the cache reports an in-memory reservation
// for a freshly handed-out unit whose flush has not yet landed, and stops reporting it
// after the hold is released (a flush conflict / run-start).
func TestHasInMemReservationFlushRace(t *testing.T) {
	wuRepo := &fakeWURepo{
		// Stall the flush so the reservation stays in-memory-only.
		flushFn: func(recs []workunit.FlushReservation) ([]types.ID, error) {
			return nil, context.DeadlineExceeded
		},
	}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 1, 0)

	vol := types.NewID()
	results, _ := c.HandOut(vol, capableOpts(vol, 0), 1)
	if len(results) != 1 {
		t.Fatalf("expected a hand-out")
	}
	// Flush has NOT landed: the DB reserved_volunteer_id would still be NULL, but the
	// cache holds the reservation in memory — StartWork keys off exactly this.
	if !c.hasInMemReservation(unitID, vol) {
		t.Fatalf("cache should report the in-memory reservation during the flush window")
	}
	if c.hasInMemReservation(unitID, types.NewID()) {
		t.Fatalf("cache must not report a reservation for a different volunteer")
	}

	// Run-start clears the in-memory hold; the reservation is no longer reported.
	c.onRunStart(unitID, vol)
	if c.hasInMemReservation(unitID, vol) {
		t.Fatalf("in-memory reservation should be cleared after run-start")
	}
}

// TestFlushAllPendingHeldDoesNotAcquireAdmission asserts the StartWork-side flush
// drains the pending queue WITHOUT acquiring the admission semaphore (the caller
// already holds a slot), so it does not self-deadlock when admissionCap == 1.
func TestFlushAllPendingHeldDoesNotAcquireAdmission(t *testing.T) {
	landed := map[types.ID]bool{}
	var mu sync.Mutex
	wuRepo := &fakeWURepo{
		flushFn: func(recs []workunit.FlushReservation) ([]types.ID, error) {
			mu.Lock()
			defer mu.Unlock()
			ids := make([]types.ID, 0, len(recs))
			for _, r := range recs {
				landed[r.WorkUnitID] = true
				ids = append(ids, r.WorkUnitID)
			}
			return ids, nil
		},
	}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)
	// admissionCap 1, and the caller "holds" the only slot.
	c.admission = make(chan struct{}, 1)
	c.admission <- struct{}{}

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 1, 0)
	vol := types.NewID()
	c.HandOut(vol, capableOpts(vol, 0), 1)
	if c.pendingWriteCount() != 1 {
		t.Fatalf("expected one pending write")
	}

	done := make(chan struct{})
	go func() {
		c.flushAllPendingHeld(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("flushAllPendingHeld deadlocked while the caller held the only admission slot")
	}
	mu.Lock()
	ok := landed[unitID]
	mu.Unlock()
	if !ok {
		t.Fatalf("flushAllPendingHeld did not persist the reservation")
	}
	if c.pendingWriteCount() != 0 {
		t.Fatalf("pending queue should be drained, got %d", c.pendingWriteCount())
	}
}
