package workunit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- fake DemoterRepo ---

type refundCall struct {
	maxTotal int
	maxError int
}

// fakeDemoterRepo models the work_units row the demoter drives. Transitions mutate the
// in-memory unit exactly as the guarded SQL would, so the walk asserts real resume
// semantics rather than a scripted sequence.
type fakeDemoterRepo struct {
	unit *WorkUnit

	// Modeled copy tallies behind RefundCopyBudget's inlined count subqueries — the fake
	// reproduces the SQL's absolute-write CASE contract off these.
	countTotal int
	countError int

	getByIDCalls     int
	expireCalls      int
	updateStateCalls int
	refundCalls      []refundCall
	reassignCalls    int

	// updateStateConflict models a concurrent pass having already demoted the unit: the
	// optimistic guard misses (409) and the DB now sits at REJECTED.
	updateStateConflict bool
	refundErr           error
	reassignErr         error
}

func (f *fakeDemoterRepo) GetByID(_ context.Context, _ types.ID) (*WorkUnit, error) {
	f.getByIDCalls++
	if f.unit == nil {
		return nil, apierror.NotFound("work_unit", "missing")
	}
	cp := *f.unit
	return &cp, nil
}

func (f *fakeDemoterRepo) ExpireLiveCopies(_ context.Context, _ types.ID, _ string) (int, error) {
	f.expireCalls++
	return 0, nil
}

func (f *fakeDemoterRepo) UpdateState(_ context.Context, _ types.ID, from, to WorkUnitState) (*WorkUnit, error) {
	f.updateStateCalls++
	if f.updateStateConflict {
		f.unit.State = WorkUnitStateRejected
		return nil, apierror.Conflict("work unit state changed concurrently",
			map[string]string{"code": "STATE_MISMATCH"})
	}
	if f.unit.State != from {
		return nil, apierror.Conflict("state mismatch", map[string]string{"code": "STATE_MISMATCH"})
	}
	f.unit.State = to
	cp := *f.unit
	return &cp, nil
}

func (f *fakeDemoterRepo) RefundCopyBudget(_ context.Context, _ types.ID, maxTotal, maxError int) (bool, error) {
	f.refundCalls = append(f.refundCalls, refundCall{maxTotal: maxTotal, maxError: maxError})
	if f.refundErr != nil {
		return false, f.refundErr
	}
	// Model the WHERE state = 'REJECTED' guard.
	if f.unit.State != WorkUnitStateRejected {
		return false, nil
	}
	// Absolute write (audit H3): count + fresh ceiling, never a bare increment.
	f.unit.MaxTotalCopies = f.countTotal + maxTotal
	// CASE: a 0 (unlimited) resolved ceiling leaves max_error_copies untouched.
	if maxError > 0 {
		f.unit.MaxErrorCopies = f.countError + maxError
	}
	return true, nil
}

func (f *fakeDemoterRepo) Reassign(_ context.Context, _ types.ID) (*WorkUnit, bool, error) {
	f.reassignCalls++
	if f.reassignErr != nil {
		return nil, false, f.reassignErr
	}
	if f.unit.State != WorkUnitStateExpired && f.unit.State != WorkUnitStateRejected {
		return nil, false, apierror.Conflict("cannot reassign",
			map[string]string{"code": "INVALID_REASSIGNMENT_STATE"})
	}
	f.unit.State = WorkUnitStateQueued
	cp := *f.unit
	return &cp, true, nil
}

// --- helpers ---

func fixedBudgets(freshTotal, errorCeiling int) func(context.Context, *WorkUnit) (int, int, error) {
	return func(context.Context, *WorkUnit) (int, int, error) {
		return freshTotal, errorCeiling, nil
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- tests ---

func TestDemoteAndRequeue_FullWalk(t *testing.T) {
	repo := &fakeDemoterRepo{
		unit:       &WorkUnit{ID: types.NewID(), State: WorkUnitStateValidated},
		countTotal: 5,
		countError: 3,
	}
	// Fresh total 8 (= target 2 + margin 6), error ceiling 0 (unlimited).
	d := NewEnforcementDemoter(repo, fixedBudgets(8, 0), discardLogger())

	if err := d.DemoteAndRequeue(context.Background(), repo.unit.ID); err != nil {
		t.Fatalf("DemoteAndRequeue: %v", err)
	}

	if repo.expireCalls != 1 {
		t.Errorf("expireCalls = %d, want 1", repo.expireCalls)
	}
	if repo.updateStateCalls != 1 {
		t.Errorf("updateStateCalls = %d, want 1", repo.updateStateCalls)
	}
	if len(repo.refundCalls) != 1 {
		t.Fatalf("refundCalls = %d, want 1", len(repo.refundCalls))
	}
	if got := repo.refundCalls[0]; got.maxTotal != 8 || got.maxError != 0 {
		t.Errorf("refund args = %+v, want {maxTotal:8 maxError:0}", got)
	}
	if repo.reassignCalls != 1 {
		t.Errorf("reassignCalls = %d, want 1", repo.reassignCalls)
	}
	if repo.unit.State != WorkUnitStateQueued {
		t.Errorf("final state = %s, want QUEUED", repo.unit.State)
	}
	// H3: absolute ceiling = copies already consumed + fresh budget (5 + 8), NOT a
	// bare += of the fraud set that would dead-letter.
	if repo.unit.MaxTotalCopies != 13 {
		t.Errorf("MaxTotalCopies = %d, want 13 (countTotal 5 + fresh 8)", repo.unit.MaxTotalCopies)
	}
	// Unlimited error ceiling (0) stays untouched.
	if repo.unit.MaxErrorCopies != 0 {
		t.Errorf("MaxErrorCopies = %d, want 0 (unlimited, untouched by the CASE)", repo.unit.MaxErrorCopies)
	}
}

func TestDemoteAndRequeue_ResumeFromRejected(t *testing.T) {
	repo := &fakeDemoterRepo{
		unit:       &WorkUnit{ID: types.NewID(), State: WorkUnitStateRejected},
		countTotal: 5,
		countError: 3,
	}
	d := NewEnforcementDemoter(repo, fixedBudgets(8, 0), discardLogger())

	if err := d.DemoteAndRequeue(context.Background(), repo.unit.ID); err != nil {
		t.Fatalf("DemoteAndRequeue: %v", err)
	}

	// A REJECTED unit dispatches nothing and was already demoted: no expire, no demote.
	if repo.expireCalls != 0 {
		t.Errorf("expireCalls = %d, want 0 (resume skips straggler abandon)", repo.expireCalls)
	}
	if repo.updateStateCalls != 0 {
		t.Errorf("updateStateCalls = %d, want 0 (already demoted)", repo.updateStateCalls)
	}
	if len(repo.refundCalls) != 1 {
		t.Errorf("refundCalls = %d, want 1", len(repo.refundCalls))
	}
	if repo.reassignCalls != 1 {
		t.Errorf("reassignCalls = %d, want 1", repo.reassignCalls)
	}
	if repo.unit.State != WorkUnitStateQueued {
		t.Errorf("final state = %s, want QUEUED", repo.unit.State)
	}
}

func TestDemoteAndRequeue_AlreadyQueuedNoop(t *testing.T) {
	repo := &fakeDemoterRepo{unit: &WorkUnit{ID: types.NewID(), State: WorkUnitStateQueued}}
	d := NewEnforcementDemoter(repo, fixedBudgets(8, 0), discardLogger())

	if err := d.DemoteAndRequeue(context.Background(), repo.unit.ID); err != nil {
		t.Fatalf("DemoteAndRequeue: %v", err)
	}

	if repo.expireCalls != 0 || repo.updateStateCalls != 0 || len(repo.refundCalls) != 0 || repo.reassignCalls != 0 {
		t.Errorf("expected a pure no-op, got expire=%d update=%d refund=%d reassign=%d",
			repo.expireCalls, repo.updateStateCalls, len(repo.refundCalls), repo.reassignCalls)
	}
	if repo.unit.State != WorkUnitStateQueued {
		t.Errorf("final state = %s, want QUEUED (unchanged)", repo.unit.State)
	}
}

func TestDemoteAndRequeue_RefundCaseSemantics(t *testing.T) {
	t.Run("unlimited error ceiling (0) leaves max_error_copies untouched", func(t *testing.T) {
		repo := &fakeDemoterRepo{
			unit:       &WorkUnit{ID: types.NewID(), State: WorkUnitStateRejected, MaxErrorCopies: 0},
			countTotal: 4,
			countError: 2,
		}
		d := NewEnforcementDemoter(repo, fixedBudgets(9, 0), discardLogger())
		if err := d.DemoteAndRequeue(context.Background(), repo.unit.ID); err != nil {
			t.Fatalf("DemoteAndRequeue: %v", err)
		}
		if repo.refundCalls[0].maxError != 0 {
			t.Errorf("refund maxError = %d, want 0 (repo receives the unlimited sentinel)", repo.refundCalls[0].maxError)
		}
		if repo.unit.MaxErrorCopies != 0 {
			t.Errorf("MaxErrorCopies = %d, want 0 (CASE untouched)", repo.unit.MaxErrorCopies)
		}
		if repo.unit.MaxTotalCopies != 13 {
			t.Errorf("MaxTotalCopies = %d, want 13 (countTotal 4 + fresh 9)", repo.unit.MaxTotalCopies)
		}
	})

	t.Run("bounded error ceiling (>0) materializes count + ceiling", func(t *testing.T) {
		repo := &fakeDemoterRepo{
			unit:       &WorkUnit{ID: types.NewID(), State: WorkUnitStateRejected},
			countTotal: 4,
			countError: 2,
		}
		d := NewEnforcementDemoter(repo, fixedBudgets(9, 5), discardLogger())
		if err := d.DemoteAndRequeue(context.Background(), repo.unit.ID); err != nil {
			t.Fatalf("DemoteAndRequeue: %v", err)
		}
		if repo.refundCalls[0].maxError != 5 {
			t.Errorf("refund maxError = %d, want 5", repo.refundCalls[0].maxError)
		}
		// count 2 + ceiling 5.
		if repo.unit.MaxErrorCopies != 7 {
			t.Errorf("MaxErrorCopies = %d, want 7 (countError 2 + ceiling 5)", repo.unit.MaxErrorCopies)
		}
	})
}

func TestDemoteAndRequeue_ResolveBudgetsErrorPropagates(t *testing.T) {
	repo := &fakeDemoterRepo{unit: &WorkUnit{ID: types.NewID(), State: WorkUnitStateValidated}}
	boom := errors.New("resolve budgets failed")
	d := NewEnforcementDemoter(repo, func(context.Context, *WorkUnit) (int, int, error) {
		return 0, 0, boom
	}, discardLogger())

	err := d.DemoteAndRequeue(context.Background(), repo.unit.ID)
	if !errors.Is(err, boom) {
		t.Fatalf("DemoteAndRequeue error = %v, want %v", err, boom)
	}
	// The unit is demoted (resumable) but the refund + requeue never ran.
	if len(repo.refundCalls) != 0 {
		t.Errorf("refundCalls = %d, want 0 (resolve failed first)", len(repo.refundCalls))
	}
	if repo.reassignCalls != 0 {
		t.Errorf("reassignCalls = %d, want 0", repo.reassignCalls)
	}
	if repo.unit.State != WorkUnitStateRejected {
		t.Errorf("final state = %s, want REJECTED (left resumable)", repo.unit.State)
	}
}

func TestDemoteAndRequeue_DemotionConflictResumes(t *testing.T) {
	// A concurrent pass demotes the unit first: our UpdateState 409s, we re-load, find it
	// REJECTED, and finish the refund + requeue.
	repo := &fakeDemoterRepo{
		unit:                &WorkUnit{ID: types.NewID(), State: WorkUnitStateValidated},
		countTotal:          6,
		updateStateConflict: true,
	}
	d := NewEnforcementDemoter(repo, fixedBudgets(8, 0), discardLogger())

	if err := d.DemoteAndRequeue(context.Background(), repo.unit.ID); err != nil {
		t.Fatalf("DemoteAndRequeue: %v", err)
	}

	if repo.updateStateCalls != 1 {
		t.Errorf("updateStateCalls = %d, want 1 (the conflicting attempt)", repo.updateStateCalls)
	}
	if repo.getByIDCalls != 2 {
		t.Errorf("getByIDCalls = %d, want 2 (initial + post-conflict reload)", repo.getByIDCalls)
	}
	if len(repo.refundCalls) != 1 {
		t.Errorf("refundCalls = %d, want 1", len(repo.refundCalls))
	}
	if repo.reassignCalls != 1 {
		t.Errorf("reassignCalls = %d, want 1", repo.reassignCalls)
	}
	if repo.unit.State != WorkUnitStateQueued {
		t.Errorf("final state = %s, want QUEUED", repo.unit.State)
	}
	if repo.unit.MaxTotalCopies != 14 {
		t.Errorf("MaxTotalCopies = %d, want 14 (countTotal 6 + fresh 8)", repo.unit.MaxTotalCopies)
	}
}
