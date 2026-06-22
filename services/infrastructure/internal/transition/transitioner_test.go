package transition

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- fakes ---

type fakeWUS struct {
	wu          *workunit.WorkUnit
	live, total int
	errCopies   int
	deadLetter  bool // what DeadLetterIfExhausted returns

	markCompletedCalls int
	expireCalls        []string // outcomes passed to ExpireLiveCopies
	deadLetterCalls    int
}

func (f *fakeWUS) GetByID(context.Context, types.ID) (*workunit.WorkUnit, error) { return f.wu, nil }
func (f *fakeWUS) MarkCompleted(context.Context, types.ID) error {
	f.markCompletedCalls++
	f.wu.State = workunit.WorkUnitStateCompleted
	return nil
}
func (f *fakeWUS) CountLiveCopies(context.Context, types.ID) (int, error)  { return f.live, nil }
func (f *fakeWUS) CountTotalCopies(context.Context, types.ID) (int, error) { return f.total, nil }
func (f *fakeWUS) CountErrorCopies(context.Context, types.ID) (int, error) { return f.errCopies, nil }
func (f *fakeWUS) DeadLetterIfExhausted(context.Context, types.ID) (bool, error) {
	f.deadLetterCalls++
	return f.deadLetter, nil
}
func (f *fakeWUS) ExpireLiveCopies(_ context.Context, _ types.ID, outcome string) (int, error) {
	f.expireCalls = append(f.expireCalls, outcome)
	return f.live, nil
}

type fakeLeaf struct{ lf *leaf.Leaf }

func (f fakeLeaf) GetByID(context.Context, types.ID) (*leaf.Leaf, error) { return f.lf, nil }

type fakeResults struct{ results []*result.Result }

func (f fakeResults) ListByWorkUnit(context.Context, types.ID) ([]*result.Result, error) {
	return f.results, nil
}

type fakeComparator struct {
	majority      []*result.Result
	compareErr    error
	acceptCalls   int
	rejectCalls   int
}

func (f fakeComparator) FilterPending(p []*result.Result) []*result.Result { return p }
func (f fakeComparator) Compare(context.Context, *workunit.WorkUnit, *leaf.Leaf, []*result.Result) ([]*result.Result, error) {
	return f.majority, f.compareErr
}
func (f *fakeComparator) ApplyAccept(context.Context, *workunit.WorkUnit, *leaf.Leaf, []*result.Result, []*result.Result) error {
	f.acceptCalls++
	return nil
}
func (f *fakeComparator) ApplyReject(context.Context, *workunit.WorkUnit, []*result.Result) error {
	f.rejectCalls++
	return nil
}

func pendingResults(n int) []*result.Result {
	out := make([]*result.Result, n)
	for i := range out {
		out[i] = &result.Result{ID: types.NewID(), VolunteerID: types.NewID(), ValidationStatus: result.ValidationPending}
	}
	return out
}

func runEval(t *testing.T, wus *fakeWUS, lf *leaf.Leaf, results []*result.Result, cmp Comparator) Outcome {
	t.Helper()
	tr := NewTransitioner(NoopLocker{}, wus, fakeLeaf{lf}, fakeResults{results}, cmp, nil)
	out, err := tr.Evaluate(context.Background(), wus.wu.ID)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return out
}

// TestTransitioner_ValidateSupersedesExtras: a target>quorum unit that reaches quorum agreement
// validates AND closes its remaining in-flight copies SUPERSEDED (over-dispatch hygiene).
func TestTransitioner_ValidateSupersedesExtras(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2, TargetCopies: 3, MinQuorum: 2})
	pend := pendingResults(2) // quorum reached
	wus := &fakeWUS{
		wu:    &workunit.WorkUnit{ID: types.NewID(), LeafID: lf.ID, State: workunit.WorkUnitStateQueued},
		live:  1, // one extra copy still running
		total: 3,
	}
	cmp := &fakeComparator{majority: pend} // all agree -> ratio 1.0 >= threshold
	out := runEval(t, wus, lf, pend, cmp)

	if out != OutcomeValidated {
		t.Fatalf("outcome = %v, want VALIDATED", out)
	}
	if cmp.acceptCalls != 1 {
		t.Errorf("ApplyAccept called %d times, want 1", cmp.acceptCalls)
	}
	if len(wus.expireCalls) != 1 || wus.expireCalls[0] != "SUPERSEDED" {
		t.Errorf("expected one SUPERSEDED ExpireLiveCopies call, got %v", wus.expireCalls)
	}
}

// TestTransitioner_DefaultNoSupersede: a plain redundancy-2 unit (target == quorum) still
// supersedes, but with zero live extras it is a no-op call — confirming the path is inert for
// existing leaves (ApplyAccept fires, the supersede closes 0 copies).
func TestTransitioner_RejectRequeues(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2})
	pend := pendingResults(2)
	wus := &fakeWUS{
		wu:    &workunit.WorkUnit{ID: types.NewID(), LeafID: lf.ID, State: workunit.WorkUnitStateCompleted},
		live:  0, // no stragglers -> reject (disagreement)
		total: 2,
	}
	cmp := &fakeComparator{majority: pend[:1]} // 1 of 2 agree -> ratio 0.5 < threshold 1.0
	out := runEval(t, wus, lf, pend, cmp)
	if out != OutcomeRejected {
		t.Fatalf("outcome = %v, want REJECTED", out)
	}
	if cmp.rejectCalls != 1 {
		t.Errorf("ApplyReject called %d times, want 1", cmp.rejectCalls)
	}
}

// TestTransitioner_DeadLetter: a unit with quorum unmet, no live copy, and an exhausted budget
// dead-letters (DeadLetterIfExhausted returns true).
func TestTransitioner_DeadLetter(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2})
	wus := &fakeWUS{
		wu:         &workunit.WorkUnit{ID: types.NewID(), LeafID: lf.ID, State: workunit.WorkUnitStateQueued},
		live:       0,
		total:      8,
		deadLetter: true,
	}
	out := runEval(t, wus, lf, nil, &fakeComparator{})
	if out != OutcomeDeadLettered {
		t.Fatalf("outcome = %v, want FAILED", out)
	}
	if wus.deadLetterCalls != 1 {
		t.Errorf("DeadLetterIfExhausted called %d times, want 1", wus.deadLetterCalls)
	}
}

// TestTransitioner_TerminalNoop: a VALIDATED unit is inert.
func TestTransitioner_TerminalNoop(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2})
	wus := &fakeWUS{wu: &workunit.WorkUnit{ID: types.NewID(), LeafID: lf.ID, State: workunit.WorkUnitStateValidated}}
	out := runEval(t, wus, lf, nil, &fakeComparator{})
	if out != OutcomeNoop {
		t.Fatalf("outcome = %v, want noop", out)
	}
}
