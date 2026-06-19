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
	// flushFn lets a test control which copies land (return value = the landed
	// (work_unit, volunteer) pairs) and observe/stall the flush. Per-copy dispatch:
	// two records for the SAME unit but DISTINCT volunteers can both land.
	flushFn func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error)
	// markSpotCheckFn / reserveCopyFn back the deferred spot-check write path (a
	// spot-check copy now lands as a RESERVED copy row via ReserveCopy).
	markSpotCheckFn func(id types.ID) error
	reserveCopyFn   func(wuID, vol types.ID, reservedUntil time.Time, deadlineSeconds int) (*workunit.Copy, error)
	// countFn backs the reconcile.
	countFn func() (map[types.ID]int, error)
	// releaseFn backs ReleaseStaleBufferedCopies (the buffer reconcile); releaseCalls
	// counts invocations so a test can assert it was (or was not) run.
	releaseFn    func(vol types.ID, held []types.ID, olderThan time.Time) ([]types.ID, error)
	releaseCalls int
	// dispatchFn backs FindDispatchableBatch AND ClaimDispatchableBatch (the refill);
	// it observes the leaf scope.
	dispatchFn func(limit int, excludeIDs, leafIDs []types.ID) ([]workunit.DispatchCandidate, error)

	flushedBatches   int
	reserveCopyCalls int

	// --- Layer 3 (claim-on-refill) observation fields ---
	claimCalls          int
	clearExpiredCalls   int
	lastClaimHeadID     types.ID
	lastClaimLease      time.Duration
	lastFlushHeadID     types.ID
	lastFlushClaimLease time.Duration

	// hrClasses records homogeneous-redundancy pins (first-writer-wins) so HR dispatch
	// tests can assert which class a unit was pinned to.
	hrClasses map[types.ID]string
}

func (f *fakeWURepo) EnsureWorkUnitHRClass(_ context.Context, id types.ID, class string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hrClasses == nil {
		f.hrClasses = map[types.ID]string{}
	}
	if existing, ok := f.hrClasses[id]; ok {
		return existing, nil
	}
	f.hrClasses[id] = class
	return class, nil
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

// ClaimDispatchableBatch (Layer 3) reuses the same dispatchFn seam as
// FindDispatchableBatch so existing fakes need no change; it records the claim
// (headID, lease) the cache passed so scale-out tests can assert on it.
func (f *fakeWURepo) ClaimDispatchableBatch(_ context.Context, headID types.ID, lease time.Duration, limit int, excludeIDs, leafIDs []types.ID) ([]workunit.DispatchCandidate, error) {
	f.mu.Lock()
	f.claimCalls++
	f.lastClaimHeadID = headID
	f.lastClaimLease = lease
	fn := f.dispatchFn
	f.mu.Unlock()
	if fn != nil {
		return fn(limit, excludeIDs, leafIDs)
	}
	return nil, nil
}

func (f *fakeWURepo) ClearExpiredDispatchClaims(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearExpiredCalls++
	return 0, nil
}

func (f *fakeWURepo) FlushReservations(_ context.Context, recs []workunit.FlushReservation, headID types.ID, claimLease time.Duration) ([]workunit.FlushedCopy, error) {
	f.mu.Lock()
	f.flushedBatches++
	f.lastFlushHeadID = headID
	f.lastFlushClaimLease = claimLease
	fn := f.flushFn
	f.mu.Unlock()
	if fn != nil {
		return fn(recs)
	}
	// Default: every copy lands (one FlushedCopy per input rec).
	out := make([]workunit.FlushedCopy, len(recs))
	for i, r := range recs {
		out[i] = workunit.FlushedCopy{WorkUnitID: r.WorkUnitID, VolunteerID: r.VolunteerID}
	}
	return out, nil
}

func (f *fakeWURepo) MarkSpotCheck(_ context.Context, id types.ID) error {
	if f.markSpotCheckFn != nil {
		return f.markSpotCheckFn(id)
	}
	return nil
}

// ReserveCopy backs the deferred spot-check landing (a spot-check hold lands as a
// RESERVED copy row). It counts calls so the spot-check tests can assert the copy
// was written.
func (f *fakeWURepo) ReserveCopy(_ context.Context, wuID, vol types.ID, reservedUntil time.Time, deadlineSeconds int) (*workunit.Copy, error) {
	f.mu.Lock()
	f.reserveCopyCalls++
	fn := f.reserveCopyFn
	f.mu.Unlock()
	if fn != nil {
		return fn(wuID, vol, reservedUntil, deadlineSeconds)
	}
	return &workunit.Copy{ID: types.NewID(), WorkUnitID: wuID, VolunteerID: vol, DeadlineSeconds: deadlineSeconds}, nil
}

func (f *fakeWURepo) CountActiveByVolunteer(_ context.Context) (map[types.ID]int, error) {
	if f.countFn != nil {
		return f.countFn()
	}
	return map[types.ID]int{}, nil
}

func (f *fakeWURepo) ReleaseStaleBufferedCopies(_ context.Context, vol types.ID, held []types.ID, olderThan time.Time) ([]types.ID, error) {
	f.mu.Lock()
	f.releaseCalls++
	fn := f.releaseFn
	f.mu.Unlock()
	if fn != nil {
		return fn(vol, held, olderThan)
	}
	return nil, nil
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
	c.leafCache[lf.ID] = &cachedLeaf{leaf: lf, fetchedAt: c.now()}
	c.leafMu.Unlock()
}

// --- tests --------------------------------------------------------------------

// hrOpts returns capable AssignmentOptions tagged with a hardware class.
func hrOpts(vol types.ID, class string) workunit.AssignmentOptions {
	o := capableOpts(vol, 0)
	o.HRClass = class
	return o
}

// TestHandOut_HomogeneousRedundancy_PinsAndFiltersByClass verifies HR: the first holder
// pins the unit to its hardware class, a DIFFERENT-class volunteer is then filtered out,
// and a SAME-class volunteer gets the second redundant copy.
func TestHandOut_HomogeneousRedundancy_PinsAndFiltersByClass(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 2, false, 0)
	lf.ValidationConfig.HomogeneousRedundancy = true
	c.warm(lf, leafRepo)

	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	const intel = "GenuineIntel/linux/amd64"
	const amd = "AuthenticAMD/linux/amd64"

	// First holder (Intel) takes a copy and pins the unit to its class.
	volA := types.NewID()
	resA, _ := c.HandOut(volA, hrOpts(volA, intel), 1)
	if len(resA) != 1 {
		t.Fatalf("volA hand-out = %d results, want 1", len(resA))
	}
	wuRepo.mu.Lock()
	pinned := wuRepo.hrClasses[unitID]
	wuRepo.mu.Unlock()
	if pinned != intel {
		t.Fatalf("durable hr_class pin = %q, want %q", pinned, intel)
	}

	// Different-class volunteer (AMD) is filtered out — gets nothing.
	volB := types.NewID()
	resB, _ := c.HandOut(volB, hrOpts(volB, amd), 1)
	if len(resB) != 0 {
		t.Fatalf("volB (different class) hand-out = %d results, want 0 (HR filtered)", len(resB))
	}

	// Same-class volunteer (Intel) gets the second redundant copy.
	volC := types.NewID()
	resC, _ := c.HandOut(volC, hrOpts(volC, intel), 1)
	if len(resC) != 1 {
		t.Fatalf("volC (same class) hand-out = %d results, want 1 (second copy)", len(resC))
	}
}

// TestHandOut_NonHR_NoClassFilter verifies a non-HR leaf is unaffected: copies go to
// volunteers of any class (no pin, no filter).
func TestHandOut_NonHR_NoClassFilter(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 2, false, 0) // HomogeneousRedundancy defaults false
	c.warm(lf, leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	volA := types.NewID()
	if resA, _ := c.HandOut(volA, hrOpts(volA, "GenuineIntel/linux/amd64"), 1); len(resA) != 1 {
		t.Fatalf("volA hand-out = %d, want 1", len(resA))
	}
	// Different class still gets the second copy (no HR pin on a non-HR leaf).
	volB := types.NewID()
	if resB, _ := c.HandOut(volB, hrOpts(volB, "AuthenticAMD/darwin/arm64"), 1); len(resB) != 1 {
		t.Fatalf("volB (different class, non-HR leaf) = %d, want 1 (no class filter)", len(resB))
	}
	wuRepo.mu.Lock()
	_, pinned := wuRepo.hrClasses[unitID]
	wuRepo.mu.Unlock()
	if pinned {
		t.Fatal("non-HR leaf should not pin hr_class")
	}
}

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
		flushFn: func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error) {
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

// TestRedundancyTwoParallelHolders: per-copy dispatch (migration 00006) makes a
// redundancy-2 NORMAL unit dispatch its TWO copies IN PARALLEL to TWO DISTINCT
// volunteers from the SAME ready snapshot (property 7). Each copy lands as its own
// work_unit_assignment_history row, so the cache stages up to effectiveRedundancy
// concurrent in-memory holders — it no longer caps a NORMAL unit at one holder and no
// longer needs the first holder to run-start before the second can be dispatched. The
// unit stays QUEUED while both copies run; it leaves the ready pool only once its
// redundancy headroom is fully covered. (This INVERTS the old serial-redundancy
// assertion that a second concurrent distinct volunteer got nothing.)
func TestRedundancyTwoParallelHolders(t *testing.T) {
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
		t.Fatalf("volA should get a copy, got %d", len(rA))
	}
	// The unit STAYS staged: it still has redundancy headroom for a 2nd parallel copy
	// to a distinct volunteer (0 active + 1 holder < redundancy 2).
	if c.readyLen() != 1 {
		t.Fatalf("redundancy-2 unit must stay staged for its 2nd parallel copy, ready=%d", c.readyLen())
	}
	// Same volunteer cannot take it again (self-exclusion) even though headroom exists.
	rAdup, _ := c.HandOut(volA, capableOpts(volA, 0), 1)
	if len(rAdup) != 0 {
		t.Fatalf("self-exclusion violated: volA got the same unit twice")
	}
	if c.readyLen() != 1 {
		t.Fatalf("self-excluded hand-out must leave the unit staged, ready=%d", c.readyLen())
	}
	// volB (a DISTINCT volunteer) gets the 2nd copy IN PARALLEL from the same snapshot —
	// no run-start of volA's copy required.
	rB, _ := c.HandOut(volB, capableOpts(volB, 0), 1)
	if len(rB) != 1 {
		t.Fatalf("volB should get the 2nd parallel copy, got %d", len(rB))
	}
	// Redundancy is now covered (2 distinct live copies): the unit leaves the ready pool.
	if c.readyLen() != 0 {
		t.Fatalf("redundancy-2 unit should leave ready after both copies are out, ready=%d", c.readyLen())
	}
	// A third distinct volunteer gets nothing: redundancy exhausted.
	rC, _ := c.HandOut(volC, capableOpts(volC, 0), 1)
	if len(rC) != 0 {
		t.Fatalf("redundancy exceeded: volC got a 3rd copy")
	}
	// Both distinct holders are tracked in memory (the parallel-copy invariant).
	c.mu.Lock()
	holders := c.reservedInMem[unitID]
	c.mu.Unlock()
	if len(holders) != 2 {
		t.Fatalf("expected 2 distinct in-memory holders (parallel copies), got %d", len(holders))
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

	// Flushing the spot-check queue marks it + lands its RESERVED copy row(s) via
	// ReserveCopy (the per-copy spot-check landing).
	c.flushSpotChecksOnce(context.Background())
	wuRepo.mu.Lock()
	reserveCalls := wuRepo.reserveCopyCalls
	wuRepo.mu.Unlock()
	if reserveCalls == 0 {
		t.Fatalf("spot-check flush should reserve a copy row")
	}
}

// TestFlushConflictVoidsHandOut: when FlushReservations reports a reservation did
// NOT land (a conflict), the cache voids that in-memory hand-out — releasing the
// holder and decrementing the volunteer's inflight count.
func TestFlushConflictVoidsHandOut(t *testing.T) {
	var conflictID types.ID
	wuRepo := &fakeWURepo{
		flushFn: func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error) {
			// Land every (unit, volunteer) copy EXCEPT conflictID's.
			var landed []workunit.FlushedCopy
			for _, r := range recs {
				if r.WorkUnitID != conflictID {
					landed = append(landed, workunit.FlushedCopy{WorkUnitID: r.WorkUnitID, VolunteerID: r.VolunteerID})
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

// TestFlushVoidsPerCopyConflict guards the pair-keyed void accounting under per-copy
// dispatch (migration 00006). A flush batch CAN legitimately carry two records for the
// SAME work_unit_id with DISTINCT volunteers (the parallel-copy case), and each lands
// as its OWN copy row — so the void check is keyed on the exact (work_unit, volunteer)
// pair, NOT the unit id. Here volA's copy lands but volB's conflicts (e.g. volB already
// held a live copy, or redundancy was already met): the cache must VOID exactly volB's
// hold and KEEP volA's, never leaving a phantom holder + inflight that reconcile cannot
// clear.
func TestFlushVoidsPerCopyConflict(t *testing.T) {
	sharedID := types.NewID()
	volA, volB := types.NewID(), types.NewID()
	wuRepo := &fakeWURepo{
		flushFn: func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error) {
			// Only volA's copy lands; volB's is a per-copy conflict (not returned).
			return []workunit.FlushedCopy{{WorkUnitID: sharedID, VolunteerID: volA}}, nil
		},
	}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 2, false, 0)
	c.warm(lf, leafRepo)

	// Two distinct in-memory holders of the same unit, both queued for the NORMAL flush
	// (the parallel-copy staging the cache now legitimately produces).
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
	// volA's copy survives; volB's is voided (pair-keyed).
	holders := c.reservedInMem[sharedID]
	if len(holders) != 1 {
		t.Fatalf("exactly volA's holder should survive the conflict, got %d holders", len(holders))
	}
	if _, ok := holders[volA]; !ok {
		t.Fatalf("the landed copy (volA) should remain held")
	}
	if _, ok := holders[volB]; ok {
		t.Fatalf("the conflicted copy (volB) should be voided")
	}
	if c.inflight[volA] != 1 {
		t.Fatalf("volA's inflight should remain 1, got %d", c.inflight[volA])
	}
	if c.inflight[volB] != 0 {
		t.Fatalf("voided volB's inflight should be released to 0, got %d", c.inflight[volB])
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
		flushFn: func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error) {
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
		flushFn: func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error) {
			mu.Lock()
			defer mu.Unlock()
			out := make([]workunit.FlushedCopy, 0, len(recs))
			for _, r := range recs {
				landed[r.WorkUnitID] = true
				out = append(out, workunit.FlushedCopy{WorkUnitID: r.WorkUnitID, VolunteerID: r.VolunteerID})
			}
			return out, nil
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

// --- FIX 1: HandOut early-exit / tail-splice ----------------------------------

// TestHandOutEarlyExitStopsScanning stages a large all-eligible ready pool and asks
// for n=1. The early-exit must stop scanning right after the single accepted
// candidate (it visits the accepted unit, then breaks at the top of the next
// iteration), leaving the rest of the pool intact and in FIFO order. This is the
// FIX-1 latency-cliff guard: a hand-out is no longer O(pool).
func TestHandOutEarlyExitStopsScanning(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)
	c.cfg.readyPoolSize = 4000
	c.cfg.lowWatermark = 1

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	const poolSize = 2000
	frontID := types.NewID()
	c.stageUnit(frontID, leafID, 1, 0) // FIFO front
	for i := 1; i < poolSize; i++ {
		c.stageUnit(types.NewID(), leafID, 1, 0)
	}

	vol := types.NewID()
	results, _ := c.HandOut(vol, capableOpts(vol, 0), 1)
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 hand-out, got %d", len(results))
	}
	// FIFO: the front (first-staged) unit is handed out.
	if results[0].unit.ID != frontID {
		t.Fatalf("FIFO priority broken: expected front unit handed out")
	}
	// Early-exit: only ONE candidate was visited (the accepted front unit); the loop
	// broke at the top of the next iteration without scanning the rest.
	if c.scanCount != 1 {
		t.Fatalf("FIX-1 early-exit: expected scanCount==1 (n=1), got %d", c.scanCount)
	}
	// The unscanned tail is spliced back intact: pool drops by exactly the 1 taken.
	if got := c.readyLen(); got != poolSize-1 {
		t.Fatalf("expected ready pool %d after handing out 1, got %d", poolSize-1, got)
	}
}

// TestHandOutLeafFilteredScansFully documents the accepted O(pool) corner: a request
// tightly filtered to a leaf absent from the pool finds <n eligible candidates and
// scans the WHOLE pool. Per the operator directive this is acceptable/rare; we do
// NOT index by leaf.
func TestHandOutLeafFilteredScansFully(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafA := types.NewID()
	leafB := types.NewID()
	c.warm(nativeLeaf(leafA, 1, false, 0), leafRepo)
	c.warm(nativeLeaf(leafB, 1, false, 0), leafRepo)
	const poolSize = 50
	for i := 0; i < poolSize; i++ {
		c.stageUnit(types.NewID(), leafA, 1, 0)
	}

	vol := types.NewID()
	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{leafB} // none of the pool matches
	results, _ := c.HandOut(vol, opts, 1)
	if len(results) != 0 {
		t.Fatalf("leaf-B request should get nothing from a leaf-A pool, got %d", len(results))
	}
	// Found <n, so the loop ran to the end: the whole pool was visited.
	if c.scanCount != poolSize {
		t.Fatalf("a no-match leaf-filtered request should scan the whole pool, scanCount=%d want %d", c.scanCount, poolSize)
	}
	if c.readyLen() != poolSize {
		t.Fatalf("no unit handed out: pool must be intact, got %d", c.readyLen())
	}
}

// TestHandOutEarlyExitMultiTake asserts the splice is correct for n>1: it hands out
// exactly n, visits n candidates (all eligible front-to-back), and the tail of
// untouched candidates is spliced back so the pool count is exact.
func TestHandOutEarlyExitMultiTake(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	const poolSize = 100
	staged := make([]types.ID, 0, poolSize)
	for i := 0; i < poolSize; i++ {
		id := types.NewID()
		staged = append(staged, id)
		c.stageUnit(id, leafID, 1, 0)
	}

	vol := types.NewID()
	const n = 5
	results, _ := c.HandOut(vol, capableOpts(vol, 0), n)
	if len(results) != n {
		t.Fatalf("expected %d hand-outs, got %d", n, len(results))
	}
	// The first n (FIFO) units were handed out.
	for i := 0; i < n; i++ {
		if results[i].unit.ID != staged[i] {
			t.Fatalf("FIFO broken at %d", i)
		}
	}
	if c.scanCount != n {
		t.Fatalf("expected to visit exactly n=%d candidates, got %d", n, c.scanCount)
	}
	if got := c.readyLen(); got != poolSize-n {
		t.Fatalf("expected pool %d after handing out %d, got %d", poolSize-n, n, got)
	}
}

// BenchmarkHandOutN1 measures the cost of HandOut(n=1) as the ready-pool size grows.
// FIX-1 stops SCANNING (running eligibleLocked / the accept branch) after the single
// accepted candidate, and splices the unscanned tail back in ONE bulk copy instead of
// the old per-element append-the-tail loop. A FIFO front hand-out still shifts the
// tail by `taken` positions (a single memmove — unavoidable without a ring-buffer
// redesign, which the directive forbids), so per-op cost is not perfectly flat; the
// win is the much lower constant factor of one memmove vs N per-element appends under
// the global lock, which is what reduces the concurrency latency cliff. Run:
// go test -bench BenchmarkHandOutN1 -benchmem.
func BenchmarkHandOutN1(b *testing.B) {
	for _, poolSize := range []int{100, 1000, 4000} {
		poolSize := poolSize
		b.Run("pool="+itoa(poolSize), func(b *testing.B) {
			wuRepo := &fakeWURepo{}
			leafRepo := &fakeLeafRepo{}
			assignRepo := &fakeAssignRepo{}
			c := newTestCache(wuRepo, leafRepo, assignRepo)
			c.cfg.readyPoolSize = poolSize * 2
			c.cfg.lowWatermark = 1
			leafID := types.NewID()
			c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				// Re-stage to a full pool each iteration so every op pays the scan cost
				// against a pool of `poolSize` (HandOut removes the unit it takes).
				c.mu.Lock()
				c.ready = c.ready[:0]
				for j := 0; j < poolSize; j++ {
					c.ready = append(c.ready, candidate{
						unit:                &workunit.WorkUnit{ID: types.NewID(), LeafID: leafID, State: workunit.WorkUnitStateQueued},
						effectiveRedundancy: 1,
					})
				}
				c.mu.Unlock()
				vol := types.NewID()
				b.StartTimer()
				c.HandOut(vol, capableOpts(vol, 0), 1)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// --- FIX 4: maintenance admission budget --------------------------------------

// TestMaintenanceBudgetNotStarvedByClient asserts that a FULLY saturated CLIENT
// admission budget does NOT block the refiller's fetchAndStage: it acquires a
// maintenance slot and stages units (the ready pool grows). This is the FIX-4
// structural guarantee.
func TestMaintenanceBudgetNotStarvedByClient(t *testing.T) {
	leafID := types.NewID()
	stagedID := types.NewID()
	wuRepo := &fakeWURepo{
		dispatchFn: func(limit int, excludeIDs, leafIDs []types.ID) ([]workunit.DispatchCandidate, error) {
			return []workunit.DispatchCandidate{{
				WorkUnit:          &workunit.WorkUnit{ID: stagedID, LeafID: leafID, State: workunit.WorkUnitStateQueued},
				LeafID:            leafID,
				RedundancyFactor:  1,
				ActiveAssignments: 0,
			}}, nil
		},
	}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)

	// Saturate the CLIENT admission budget entirely (a write storm holding all slots).
	for i := 0; i < cap(c.admission); i++ {
		c.admission <- struct{}{}
	}
	if !c.admissionSaturated() {
		t.Fatalf("client admission should be saturated")
	}

	// Refill must still succeed via the SEPARATE maintenance budget.
	c.refillOnce(context.Background())
	if !c.readyContainsLockedTest(stagedID) {
		t.Fatalf("FIX 4: refill must stage units even when the client admission budget is fully held")
	}
}

// TestSpotCheckFlushUsesMaintenanceBudget asserts a fully-saturated CLIENT admission
// budget does NOT block a spot-check flush from landing (MarkSpotCheck +
// StampReservation + history row) — it runs on the maintenance budget (FIX 4). This
// guards against the spot-check-deferral regression FIX 3 would otherwise introduce.
func TestSpotCheckFlushUsesMaintenanceBudget(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, true, 100), leafRepo) // 100% spot-check
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 1, 0)

	vol := types.NewID()
	if r, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(r) != 1 {
		t.Fatalf("spot-check hand-out failed")
	}
	c.mu.Lock()
	nSpot := len(c.pendingSpotChecks)
	c.mu.Unlock()
	if nSpot != 1 {
		t.Fatalf("expected 1 deferred spot-check, got %d", nSpot)
	}

	// Saturate the CLIENT admission budget; the spot-check flush must still land.
	for i := 0; i < cap(c.admission); i++ {
		c.admission <- struct{}{}
	}
	c.flushSpotChecksOnce(context.Background())
	wuRepo.mu.Lock()
	reserveCalls := wuRepo.reserveCopyCalls
	wuRepo.mu.Unlock()
	if reserveCalls == 0 {
		t.Fatalf("FIX 4: spot-check flush must land via the maintenance budget even with client admission full")
	}
}

// TestMaintenanceCapDefaultDerives asserts newDispatchCache derives the maintenance
// budget as max(1, admissionCap/4) when not explicitly set, and is always >= 1.
func TestMaintenanceCapDefaultDerives(t *testing.T) {
	c := newDispatchCache(dispatchCacheConfig{admissionCap: 12}, dispatchDeps{}, testLogger())
	if got := cap(c.maintenanceAdmission); got != 3 {
		t.Fatalf("admissionCap 12 should derive maintenance cap 3, got %d", got)
	}
	c2 := newDispatchCache(dispatchCacheConfig{admissionCap: 1}, dispatchDeps{}, testLogger())
	if got := cap(c2.maintenanceAdmission); got != 1 {
		t.Fatalf("admissionCap 1 should derive maintenance cap 1 (floor), got %d", got)
	}
	c3 := newDispatchCache(dispatchCacheConfig{admissionCap: 12, maintenanceAdmissionCap: 5}, dispatchDeps{}, testLogger())
	if got := cap(c3.maintenanceAdmission); got != 5 {
		t.Fatalf("explicit maintenance cap 5 should be honored, got %d", got)
	}
}

// --- MINOR: purge pending writes on in-memory void ----------------------------

// TestReleaseInMemPurgesPending asserts that voiding an in-memory hold also purges a
// still-queued pendingWrite / pendingSpotCheck for that (unit, volunteer) — so a late
// flush cannot re-stamp a reservation onto a unit whose hold was just dropped.
func TestReleaseInMemPurgesPending(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	assignRepo := &fakeAssignRepo{}
	c := newTestCache(wuRepo, leafRepo, assignRepo)

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 1, 0)

	vol := types.NewID()
	if r, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(r) != 1 {
		t.Fatalf("hand-out failed")
	}
	if c.pendingWriteCount() != 1 {
		t.Fatalf("expected a queued reservation write, got %d", c.pendingWriteCount())
	}

	// Void the hold (e.g. the buffered-abandon path): the queued write must be purged.
	c.releaseInMem(unitID, vol)
	if c.pendingWriteCount() != 0 {
		t.Fatalf("MINOR: voiding the hold must purge the queued reservation write, got %d", c.pendingWriteCount())
	}
	c.mu.Lock()
	nSpot := len(c.pendingSpotChecks)
	c.mu.Unlock()
	if nSpot != 0 {
		t.Fatalf("no spot-check entry should survive for the voided pair, got %d", nSpot)
	}

	// A second still-queued entry for a DIFFERENT volunteer must NOT be purged.
	otherVol := types.NewID()
	until := c.now().UTC().Add(time.Hour)
	c.mu.Lock()
	c.pendingWrites = append(c.pendingWrites,
		workunit.FlushReservation{WorkUnitID: unitID, VolunteerID: otherVol, ReservedUntil: until},
		workunit.FlushReservation{WorkUnitID: types.NewID(), VolunteerID: vol, ReservedUntil: until},
	)
	c.reservedInMem[unitID] = map[types.ID]time.Time{otherVol: until}
	c.inflight[otherVol] = 1
	c.mu.Unlock()
	c.releaseInMem(unitID, otherVol) // purges only (unitID, otherVol)
	if c.pendingWriteCount() != 1 {
		t.Fatalf("purge must drop ONLY the matching pair, leaving the unrelated write, got %d", c.pendingWriteCount())
	}
}

// TestReconcileBuffers_ReleasesUnheldBufferedReservations verifies the buffer
// reconcile: a volunteer reports holding only a subset of the units the cache has
// reserved for it, and the reconcile releases the dropped one (with the grace cutoff)
// while leaving the held one — clearing the released unit's in-memory hold so it can
// redispatch.
func TestReconcileBuffers_ReleasesUnheldBufferedReservations(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	now := time.Now()
	c.now = func() time.Time { return now }

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)

	vol := types.NewID()
	kept := types.NewID()
	dropped := types.NewID()
	c.stageUnit(kept, leafID, 2, 0)
	c.stageUnit(dropped, leafID, 2, 0)

	// Hand both units to vol so the cache holds an in-memory reservation for each.
	if r, _ := c.HandOut(vol, capableOpts(vol, 0), 2); len(r) != 2 {
		t.Fatalf("expected 2 hand-outs, got %d", len(r))
	}

	// vol reports holding only `kept` — it dropped `dropped` from its buffer.
	c.NoteVolunteerHeld(vol, []types.ID{kept})

	var gotVol types.ID
	var gotHeld []types.ID
	var gotCutoff time.Time
	wuRepo.releaseFn = func(v types.ID, held []types.ID, olderThan time.Time) ([]types.ID, error) {
		gotVol, gotHeld, gotCutoff = v, held, olderThan
		return []types.ID{dropped}, nil
	}

	c.reconcileBuffers(context.Background())

	if gotVol != vol {
		t.Fatalf("release called for %v, want %v", gotVol, vol)
	}
	if len(gotHeld) != 1 || gotHeld[0] != kept {
		t.Fatalf("release held set = %v, want [%v]", gotHeld, kept)
	}
	if !gotCutoff.Equal(now.Add(-reconcileGracePeriod)) {
		t.Fatalf("release cutoff = %v, want now-grace %v", gotCutoff, now.Add(-reconcileGracePeriod))
	}

	c.mu.Lock()
	_, droppedHeld := c.reservedInMem[dropped][vol]
	_, keptHeld := c.reservedInMem[kept][vol]
	c.mu.Unlock()
	if droppedHeld {
		t.Errorf("dropped unit must be cleared from the in-memory ledger after release")
	}
	if !keptHeld {
		t.Errorf("kept unit must remain reserved (volunteer still holds it)")
	}
}

// TestReconcileBuffers_CutoffBoundedByReportTime verifies the reap cutoff is bounded by
// the volunteer's last report time, not just now-grace. A full client that stopped
// requesting goes quiet; the batch that filled its buffer was created AFTER its last
// report, so it must never be reaped even once it ages past the grace window.
func TestReconcileBuffers_CutoffBoundedByReportTime(t *testing.T) {
	wuRepo := &fakeWURepo{}
	c := newTestCache(wuRepo, &fakeLeafRepo{}, &fakeAssignRepo{})

	base := time.Now()
	// Report recorded 70s ago (still fresh: < heldReportFreshness), then the client went
	// quiet (buffer full). now-grace would be base-60s, but the report is older than that.
	c.now = func() time.Time { return base.Add(-70 * time.Second) }
	vol := types.NewID()
	c.NoteVolunteerHeld(vol, []types.ID{types.NewID()})
	c.now = func() time.Time { return base }

	var gotCutoff time.Time
	wuRepo.releaseFn = func(_ types.ID, _ []types.ID, olderThan time.Time) ([]types.ID, error) {
		gotCutoff = olderThan
		return nil, nil
	}
	c.reconcileBuffers(context.Background())

	want := base.Add(-70 * time.Second) // bounded by the report time, not now-grace (base-60s)
	if !gotCutoff.Equal(want) {
		t.Fatalf("cutoff = %v, want report time %v (must not reap copies created after the last report)", gotCutoff, want)
	}
}

// TestReconcileBuffers_SkipsStaleReports verifies a volunteer whose last held-set
// report is older than the freshness window is NOT reconciled (its buffered copies
// ride the deadline instead), and its stale report is pruned.
func TestReconcileBuffers_SkipsStaleReports(t *testing.T) {
	wuRepo := &fakeWURepo{}
	c := newTestCache(wuRepo, &fakeLeafRepo{}, &fakeAssignRepo{})

	now := time.Now()
	c.now = func() time.Time { return now }

	vol := types.NewID()
	// Record a report and then advance the clock past the freshness window.
	c.NoteVolunteerHeld(vol, []types.ID{types.NewID()})
	c.now = func() time.Time { return now.Add(heldReportFreshness + time.Second) }

	c.reconcileBuffers(context.Background())

	if wuRepo.releaseCalls != 0 {
		t.Fatalf("stale report must not trigger a DB release, got %d calls", wuRepo.releaseCalls)
	}
	c.heldMu.Lock()
	_, present := c.heldReports[vol]
	c.heldMu.Unlock()
	if present {
		t.Errorf("stale report must be pruned")
	}
}
