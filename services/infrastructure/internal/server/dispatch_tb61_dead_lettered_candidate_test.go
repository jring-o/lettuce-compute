package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// TB-61 regression tests: a unit dead-lettered (FAILED) by the transitioner or the fault
// monitor stayed staged in the dispatch cache's ready pool — nothing evicted it and
// nothing re-read staged candidates from the database — so the one volunteer whose
// refill-time snapshot still admitted it was handed the same dead unit every 60 s
// (the flush-conflict void bench), refused at the SQL landing each time, forever. On
// infra.scios.tech one host received nothing but one FAILED unit for two weeks, ~1,200
// refused cycles a day.
//
// Two layers close it, each pinned here: the flush's refused-copy state probe evicts a
// candidate whose unit is no longer QUEUED instead of benching the volunteer (the
// landing-side backstop for any writer), and the fault monitor's dead-letter reaches the
// cache through the transitioner's dispatch hook (the writer-side fix).

// TestFlush_RefusedCopyOfNotQueuedUnit_EvictsCandidate_NoReofferLoop is the filing's
// reproduction as a named test: one candidate on a redundancy-3 leaf whose unit is
// FAILED in the database (the flush lands nothing; the state probe reads FAILED); one
// volunteer polling every 70 s — the 60 s void bench plus a poll gap — for twenty polls,
// with the leaf snapshot re-warmed each cycle as the live refiller does. Red before the
// fix: 20 offers in 20 polls, candidate still staged at the end. After: at most one
// offer, and the candidate is gone the moment the first refused flush is processed.
func TestFlush_RefusedCopyOfNotQueuedUnit_EvictsCandidate_NoReofferLoop(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	now := time.Now()
	c.now = func() time.Time { return now }

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 3, false, 0)
	c.warm(lf, leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 3, 0)

	wuRepo.mu.Lock()
	// FlushReservations for a unit that is no longer QUEUED: no pair lands.
	wuRepo.flushFn = func(_ []workunit.FlushReservation) ([]workunit.FlushedCopy, error) { return nil, nil }
	// The state probe reads the truth the landing refused on.
	wuRepo.getByIDFn = func(id types.ID) (*workunit.WorkUnit, error) {
		return &workunit.WorkUnit{ID: id, LeafID: leafID, State: workunit.WorkUnitStateFailed}, nil
	}
	wuRepo.mu.Unlock()

	vol := types.NewID()
	offers := 0
	for i := 0; i < 20; i++ {
		res, _ := c.HandOut(vol, capableOpts(vol, 0), 1)
		offers += len(res)
		c.flushOnce(context.Background())
		if i == 0 {
			c.mu.Lock()
			staged := c.readyContainsLocked(unitID)
			_, held := c.reservedInMem[unitID]
			c.mu.Unlock()
			if staged {
				t.Fatalf("candidate still staged after the first refused flush of a FAILED unit: the void bench lapses in 60 s and the same volunteer is re-offered it (TB-61)")
			}
			if held {
				t.Fatalf("in-memory hold survived the eviction of a FAILED unit's candidate")
			}
		}
		now = now.Add(70 * time.Second) // the void bench has lapsed
		c.warm(lf, leafRepo)            // the live refiller keeps staged leaf snapshots fresh
	}
	if offers != 1 {
		t.Fatalf("offers of the FAILED unit to the same volunteer over 20 polls = %d, want 1: the first hand-out is the stale snapshot's; the refused flush must evict, not bench for 60 s", offers)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readyContainsLocked(unitID) {
		t.Fatalf("FAILED unit still staged after 20 polls")
	}
}

// TestFlush_RefusedCopyOfDeletedUnit_EvictsCandidate: a unit the probe cannot find at all
// (deleted with its leaf, say) can land nothing either — gone is gone; evict.
func TestFlush_RefusedCopyOfDeletedUnit_EvictsCandidate(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	wuRepo.mu.Lock()
	wuRepo.flushFn = func(_ []workunit.FlushReservation) ([]workunit.FlushedCopy, error) { return nil, nil }
	wuRepo.getByIDFn = func(id types.ID) (*workunit.WorkUnit, error) {
		return nil, apierror.NotFound("work unit", id.String())
	}
	wuRepo.mu.Unlock()

	vol := types.NewID()
	if res, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(res) != 1 {
		t.Fatalf("first hand-out = %d, want 1", len(res))
	}
	c.flushOnce(context.Background())
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readyContainsLocked(unitID) {
		t.Fatalf("a unit the database no longer has must not stay staged")
	}
}

// TestFlush_RefusedCopyStillQueued_BenchesNotEvicts: the per-volunteer refusal (cooldown,
// a live copy already held, redundancy met by others) is the OTHER cause of a refused
// flush, and it must keep the pre-TB-61 answer — bench this volunteer, keep the candidate
// for everyone else. The probe reads QUEUED, so the bench arm runs.
func TestFlush_RefusedCopyStillQueued_BenchesNotEvicts(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	probes := 0
	wuRepo.mu.Lock()
	wuRepo.flushFn = func(_ []workunit.FlushReservation) ([]workunit.FlushedCopy, error) { return nil, nil }
	wuRepo.getByIDFn = func(id types.ID) (*workunit.WorkUnit, error) {
		probes++
		return &workunit.WorkUnit{ID: id, LeafID: leafID, State: workunit.WorkUnitStateQueued}, nil
	}
	wuRepo.mu.Unlock()

	volA := types.NewID()
	if res, _ := c.HandOut(volA, capableOpts(volA, 0), 1); len(res) != 1 {
		t.Fatalf("first hand-out to A = %d, want 1", len(res))
	}
	c.flushOnce(context.Background())
	if probes != 1 {
		t.Fatalf("state probes = %d, want exactly 1 (one point read per refused unit)", probes)
	}
	if res, _ := c.HandOut(volA, capableOpts(volA, 0), 1); len(res) != 0 {
		t.Fatalf("A re-offered inside the void bench: got %d, want 0", len(res))
	}
	volB := types.NewID()
	if res, _ := c.HandOut(volB, capableOpts(volB, 0), 1); len(res) != 1 {
		t.Fatalf("B refused a still-QUEUED unit: got %d, want 1 — a per-volunteer refusal benches A, it must not evict the candidate", len(res))
	}
}

// TestFlush_StateProbeFails_FallsBackToBench: a probe that fails for any reason other than
// not-found leaves the unit's fate unknown, so the flush keeps the old, self-correcting
// answer (bench the volunteer, keep the candidate) and says so once.
func TestFlush_StateProbeFails_FallsBackToBench(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	logger, buf := capturingLogger()
	c.logger = logger

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	wuRepo.mu.Lock()
	wuRepo.flushFn = func(_ []workunit.FlushReservation) ([]workunit.FlushedCopy, error) { return nil, nil }
	wuRepo.getByIDFn = func(id types.ID) (*workunit.WorkUnit, error) { return nil, errors.New("connection reset") }
	wuRepo.mu.Unlock()

	volA := types.NewID()
	if res, _ := c.HandOut(volA, capableOpts(volA, 0), 1); len(res) != 1 {
		t.Fatalf("first hand-out to A = %d, want 1", len(res))
	}
	c.flushOnce(context.Background())
	c.mu.Lock()
	staged := c.readyContainsLocked(unitID)
	c.mu.Unlock()
	if !staged {
		t.Fatalf("candidate evicted on an UNKNOWN state: a failed probe must fall back to the bench")
	}
	if res, _ := c.HandOut(volA, capableOpts(volA, 0), 1); len(res) != 0 {
		t.Fatalf("A re-offered after a refused flush: got %d, want 0 (benched)", len(res))
	}
	if !bytes.Contains(buf.Bytes(), []byte("could not read the state of a refused unit")) {
		t.Fatalf("expected one WARN naming the failed probe; log:\n%s", buf.String())
	}
}

// --- the fault monitor's dead-letter, end to end through the real transitioner --------

// tb61MonitorRepo is the one work-unit store both the fault monitor and the transitioner
// read for a single expired copy whose close exhausts the unit's budget: the monitor sees
// one timed-out copy and closes it; the transitioner then reads a QUEUED unit with no live
// copy, eight copies spent, and a dead-letter probe that says yes. Everything else the
// scan touches is empty. The embedded nil interface panics on anything unexpected.
type tb61MonitorRepo struct {
	workunit.WorkUnitRepository
	unit            *workunit.WorkUnit
	expired         []*workunit.Copy
	closed          []types.ID
	deadLetterCalls int
}

func (r *tb61MonitorRepo) FindExpiredCopies(context.Context, int) ([]*workunit.Copy, error) {
	out := r.expired
	r.expired = nil
	return out, nil
}
func (r *tb61MonitorRepo) CloseCopy(_ context.Context, copyID types.ID, _ string) error {
	r.closed = append(r.closed, copyID)
	return nil
}
func (r *tb61MonitorRepo) FindStuckSpotCheckUnits(context.Context, int) ([]*workunit.WorkUnit, error) {
	return nil, nil
}
func (r *tb61MonitorRepo) ClearExpiredDispatchClaims(context.Context) (int64, error) { return 0, nil }
func (r *tb61MonitorRepo) FindRunningWithStaleCheckpoints(context.Context, int) ([]workunit.StaleCheckpointInfo, error) {
	return nil, nil
}
func (r *tb61MonitorRepo) GetByID(context.Context, types.ID) (*workunit.WorkUnit, error) {
	return r.unit, nil
}
func (r *tb61MonitorRepo) MarkCompleted(context.Context, types.ID) error {
	r.unit.State = workunit.WorkUnitStateCompleted
	return nil
}
func (r *tb61MonitorRepo) CountLiveCopies(context.Context, types.ID) (int, error) { return 0, nil }
func (r *tb61MonitorRepo) CountProbationLiveCopies(context.Context, types.ID) (int, error) {
	return 0, nil
}
func (r *tb61MonitorRepo) CountTotalCopies(context.Context, types.ID) (int, error) { return 8, nil }
func (r *tb61MonitorRepo) CountErrorCopies(context.Context, types.ID) (int, error) { return 0, nil }
func (r *tb61MonitorRepo) DeadLetterIfExhausted(context.Context, types.ID) (bool, error) {
	r.deadLetterCalls++
	r.unit.State = workunit.WorkUnitStateFailed
	return true, nil
}
func (r *tb61MonitorRepo) ExpireLiveCopies(context.Context, types.ID, string) (int, error) {
	return 0, nil
}

// tb61CheckpointRepo answers the dead-letter's checkpoint cleanup.
type tb61CheckpointRepo struct {
	checkpoint.Repository
	deleted []types.ID
}

func (r *tb61CheckpointRepo) Delete(_ context.Context, id types.ID) error {
	r.deleted = append(r.deleted, id)
	return nil
}

type tb61Results struct{}

func (tb61Results) ListByWorkUnit(context.Context, types.ID) ([]*result.Result, error) {
	return nil, nil
}

// tb61Comparator is never consulted (no pending results), but the transitioner needs one.
type tb61Comparator struct{}

func (tb61Comparator) FilterPending(p []*result.Result) []*result.Result { return p }
func (tb61Comparator) Compare(context.Context, *workunit.WorkUnit, *leaf.Leaf, []*result.Result) ([]*result.Result, error) {
	return nil, nil
}
func (tb61Comparator) ApplyAccept(context.Context, *workunit.WorkUnit, *leaf.Leaf, []*result.Result, []*result.Result, *transition.ComparisonVerdict, transition.RedundancyPolicy, int) error {
	return nil
}
func (tb61Comparator) ApplyReject(context.Context, *workunit.WorkUnit, *leaf.Leaf, []*result.Result, *transition.ComparisonVerdict, transition.RedundancyPolicy, int) error {
	return nil
}

// TestFaultMonitor_DeadLetterEvictsStagedCandidate runs the writer the filing named: the
// fault monitor closes a timed-out copy, the real transitioner dead-letters the unit, and
// — through the dispatch hook, wired to the same late-bound handle main.go binds — the
// unit's staged candidate leaves the cache. Red before the hook: the unit went FAILED,
// the candidate stayed, and the next capable volunteer was handed a dead unit.
func TestFaultMonitor_DeadLetterEvictsStagedCandidate(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	lf := nativeLeaf(leafID, 2, false, 0)
	c.warm(lf, leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 2, 0)

	// The head's late-bound handle, bound to this cache the way StartDispatchCache does.
	ref := NewDispatchCacheRef()
	ref.set(c)

	repo := &tb61MonitorRepo{
		unit: &workunit.WorkUnit{ID: unitID, LeafID: leafID, State: workunit.WorkUnitStateQueued},
		expired: []*workunit.Copy{{
			ID: types.NewID(), WorkUnitID: unitID, VolunteerID: types.NewID(),
			AssignedAt: time.Now().Add(-2 * time.Hour), DeadlineSeconds: 60,
		}},
	}
	tr := transition.NewTransitioner(transition.NoopLocker{}, repo, leafRepo, tb61Results{}, tb61Comparator{}, transition.TrustPolicy{}, nil)
	tr.SetDispatchInvalidator(ref)
	ckpt := &tb61CheckpointRepo{}
	m := &FaultMonitor{
		workUnitRepo:   repo,
		checkpointRepo: ckpt,
		transitioner:   tr,
		logger:         slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		batchSize:      100,
	}

	if err := m.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if len(repo.closed) != 1 {
		t.Fatalf("copies closed = %d, want 1", len(repo.closed))
	}
	if repo.deadLetterCalls != 1 || repo.unit.State != workunit.WorkUnitStateFailed {
		t.Fatalf("dead-letter calls = %d, state = %s; want 1 and FAILED", repo.deadLetterCalls, repo.unit.State)
	}
	if len(ckpt.deleted) != 1 {
		t.Fatalf("checkpoint cleanups = %d, want 1 (the monitor's own dead-letter follow-up must still run)", len(ckpt.deleted))
	}
	c.mu.Lock()
	staged := c.readyContainsLocked(unitID)
	c.mu.Unlock()
	if staged {
		t.Fatalf("the fault monitor dead-lettered the unit but its candidate is still staged: it will be re-offered to the next capable volunteer and refused at the landing every 60 s (TB-61)")
	}
	vol := types.NewID()
	if res, _ := c.HandOut(vol, capableOpts(vol, 0), 1); len(res) != 0 {
		t.Fatalf("hand-out after the dead-letter = %d units, want 0", len(res))
	}
}
