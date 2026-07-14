package transition

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// fakeRecoveryStore returns fixed candidate id sets (and optional query errors) and records how
// many times each finder was called.
type fakeRecoveryStore struct {
	shape1, shape2, residue          []types.ID
	shape1Err, shape2Err, residueErr error
	shape1Calls, shape2Calls         int
	residueCalls                     int
}

func (f *fakeRecoveryStore) FindStalledFinalizationUnits(context.Context, time.Duration, int) ([]types.ID, error) {
	f.shape1Calls++
	return f.shape1, f.shape1Err
}
func (f *fakeRecoveryStore) FindStalledQueuedAtQuorum(context.Context, time.Duration, int) ([]types.ID, error) {
	f.shape2Calls++
	return f.shape2, f.shape2Err
}
func (f *fakeRecoveryStore) FindFinalizationResidueUnits(context.Context, int) ([]types.ID, error) {
	f.residueCalls++
	return f.residue, f.residueErr
}

// fakeEvaluator records the ids passed to Evaluate and can inject a per-id error.
type fakeEvaluator struct {
	evaluated []types.ID
	errFor    map[types.ID]error
}

func (f *fakeEvaluator) Evaluate(_ context.Context, id types.ID) (Outcome, error) {
	f.evaluated = append(f.evaluated, id)
	if e := f.errFor[id]; e != nil {
		return OutcomeNoop, e
	}
	return OutcomeValidated, nil
}

func newSweeperForTest(store RecoveryStore, ev Evaluator) *RecoverySweeper {
	return NewRecoverySweeper(store, ev, time.Minute, time.Minute, 100, nil)
}

// TestRecoverySweeper_FunnelsToEvaluate: shape-1 then shape-2 candidate ids are all handed to
// Evaluate, in order; residue units are never evaluated.
func TestRecoverySweeper_FunnelsToEvaluate(t *testing.T) {
	a, b, c := types.NewID(), types.NewID(), types.NewID()
	store := &fakeRecoveryStore{shape1: []types.ID{a, b}, shape2: []types.ID{c}}
	ev := &fakeEvaluator{}
	newSweeperForTest(store, ev).RunOnce(context.Background())

	if len(ev.evaluated) != 3 || ev.evaluated[0] != a || ev.evaluated[1] != b || ev.evaluated[2] != c {
		t.Fatalf("evaluated = %v, want [%v %v %v] (shape1 then shape2)", ev.evaluated, a, b, c)
	}
}

// TestRecoverySweeper_PerUnitErrorDoesNotAbort: an Evaluate error on one unit is logged and the
// sweep continues to the remaining units (the next tick retries the failed one).
func TestRecoverySweeper_PerUnitErrorDoesNotAbort(t *testing.T) {
	a, b, c := types.NewID(), types.NewID(), types.NewID()
	store := &fakeRecoveryStore{shape1: []types.ID{a, b, c}}
	ev := &fakeEvaluator{errFor: map[types.ID]error{b: errors.New("boom")}}
	newSweeperForTest(store, ev).RunOnce(context.Background())

	if len(ev.evaluated) != 3 {
		t.Fatalf("evaluated %d units, want 3 (an error must not abort the sweep): %v", len(ev.evaluated), ev.evaluated)
	}
}

// TestRecoverySweeper_QueryErrorDoesNotAbort: a failed candidate query is logged, and the other
// shape's candidates are still driven.
func TestRecoverySweeper_QueryErrorDoesNotAbort(t *testing.T) {
	c := types.NewID()
	store := &fakeRecoveryStore{shape1Err: errors.New("shape1 query failed"), shape2: []types.ID{c}}
	ev := &fakeEvaluator{}
	newSweeperForTest(store, ev).RunOnce(context.Background())

	if len(ev.evaluated) != 1 || ev.evaluated[0] != c {
		t.Fatalf("evaluated = %v, want [%v] (shape-2 driven despite shape-1 query error)", ev.evaluated, c)
	}
}

// TestRecoverySweeper_ResidueWarnsNeverEvaluates: residue units are reported (the query is run)
// but never handed to Evaluate — re-evaluation cannot repair them.
func TestRecoverySweeper_ResidueWarnsNeverEvaluates(t *testing.T) {
	r1, r2 := types.NewID(), types.NewID()
	store := &fakeRecoveryStore{residue: []types.ID{r1, r2}}
	ev := &fakeEvaluator{}
	newSweeperForTest(store, ev).RunOnce(context.Background())

	if store.residueCalls != 1 {
		t.Errorf("residue query called %d times, want 1", store.residueCalls)
	}
	if len(ev.evaluated) != 0 {
		t.Fatalf("residue units were evaluated (%v); they must only be reported", ev.evaluated)
	}
}
