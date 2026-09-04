package transition

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// TB-61 regression tests: every unit state the transitioner writes must reach the
// dispatch cache. The cache stages a QUEUED unit as a ready candidate and never re-reads
// it, so a dead-letter (or validate / reject / reopen) that does not evict leaves a stale
// candidate the cache keeps handing out — refused at the SQL landing every time. Before
// this hook only the abandon RPC evicted after its own dead-letter; the fault monitor's
// and the recovery sweeper's dead-letters went through this same Evaluate and evicted
// nothing.

// recordingInvalidator stands in for the dispatch cache handle and records the ids the
// transitioner asked it to evict.
type recordingInvalidator struct{ ids []types.ID }

func (r *recordingInvalidator) InvalidateWorkUnit(id types.ID) { r.ids = append(r.ids, id) }

// runEvalWithDispatch is runEval with a recording dispatch invalidator wired in.
func runEvalWithDispatch(t *testing.T, wus *fakeWUS, lf *leaf.Leaf, results []*result.Result, cmp Comparator) (Outcome, *recordingInvalidator) {
	t.Helper()
	inv := &recordingInvalidator{}
	tr := NewTransitioner(NoopLocker{}, wus, fakeLeaf{lf}, fakeResults{results}, cmp, TrustPolicy{}, nil)
	tr.SetDispatchInvalidator(inv)
	out, err := tr.Evaluate(context.Background(), wus.wu.ID)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return out, inv
}

// TestTransitioner_DeadLetterEvictsDispatchCandidate pins the TB-61 shape: a unit the
// transitioner dead-letters (redundancy unmet, copy budget exhausted — the fault monitor's
// and the recovery sweeper's path, not the abandon RPC's) must be evicted from dispatch
// exactly once, by id. Red before the hook: Evaluate returned FAILED and told the cache
// nothing, so the staged candidate outlived the unit until a head restart.
func TestTransitioner_DeadLetterEvictsDispatchCandidate(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2})
	wus := &fakeWUS{
		wu:         &workunit.WorkUnit{ID: types.NewID(), LeafID: lf.ID, State: workunit.WorkUnitStateQueued},
		live:       0,
		total:      8,
		deadLetter: true,
	}
	out, inv := runEvalWithDispatch(t, wus, lf, nil, &fakeComparator{})
	if out != OutcomeDeadLettered {
		t.Fatalf("outcome = %v, want FAILED", out)
	}
	if len(inv.ids) != 1 || inv.ids[0] != wus.wu.ID {
		t.Fatalf("dispatch invalidations = %v, want exactly [%s]: a dead-lettered unit left staged is re-offered to the one volunteer whose stale snapshot admits it every 60 s, forever (TB-61)", inv.ids, wus.wu.ID)
	}
}

// TestTransitioner_DispatchEvictionFollowsEveryStateWrite: the hook fires on each outcome
// that wrote a new unit state (VALIDATED, REJECTED, FAILED, REOPENED) and stays silent on
// a WAIT and on a terminal no-op — the dispatch snapshot is only stale when the state moved.
func TestTransitioner_DispatchEvictionFollowsEveryStateWrite(t *testing.T) {
	cases := []struct {
		name    string
		lf      *leaf.Leaf
		wus     *fakeWUS
		pending int
		agree   int // how many of the pending results agree (majority size)
		want    Outcome
		evicts  bool
	}{
		{
			name:    "validate at quorum",
			lf:      leafWith(leaf.ValidationConfig{RedundancyFactor: 2}),
			wus:     &fakeWUS{live: 0, total: 2},
			pending: 2, agree: 2,
			want: OutcomeValidated, evicts: true,
		},
		{
			name:    "reject and requeue",
			lf:      leafWith(leaf.ValidationConfig{RedundancyFactor: 2}),
			wus:     &fakeWUS{live: 0, total: 2},
			pending: 2, agree: 1,
			want: OutcomeRejected, evicts: true,
		},
		{
			name:   "dead-letter",
			lf:     leafWith(leaf.ValidationConfig{RedundancyFactor: 2}),
			wus:    &fakeWUS{live: 0, total: 8, deadLetter: true},
			want:   OutcomeDeadLettered,
			evicts: true,
		},
		{
			name: "reopen a parked COMPLETED unit",
			lf:   leafWith(leaf.ValidationConfig{RedundancyFactor: 2, TargetCopies: 3, MinQuorum: 2}),
			// Quorum reached but the two disagree, no live stragglers, budget headroom:
			// Decide reopens (COMPLETED -> QUEUED) so dispatch can supply the tiebreaker.
			wus:     &fakeWUS{live: 0, total: 2},
			pending: 2, agree: 1,
			want: OutcomeReopened, evicts: true,
		},
		{
			name:    "wait: quorum not yet reached",
			lf:      leafWith(leaf.ValidationConfig{RedundancyFactor: 2}),
			wus:     &fakeWUS{live: 1, total: 2},
			pending: 1, agree: 1,
			want: OutcomeWaiting, evicts: false,
		},
		{
			name:   "terminal no-op",
			lf:     leafWith(leaf.ValidationConfig{RedundancyFactor: 2}),
			wus:    &fakeWUS{},
			want:   OutcomeNoop,
			evicts: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := workunit.WorkUnitStateQueued
			switch tc.want {
			case OutcomeReopened:
				state = workunit.WorkUnitStateCompleted
			case OutcomeNoop:
				state = workunit.WorkUnitStateValidated
			}
			tc.wus.wu = &workunit.WorkUnit{ID: types.NewID(), LeafID: tc.lf.ID, State: state}
			pend := pendingResults(tc.pending)
			cmp := &fakeComparator{majority: pend[:tc.agree]}
			out, inv := runEvalWithDispatch(t, tc.wus, tc.lf, pend, cmp)
			if out != tc.want {
				t.Fatalf("outcome = %v, want %v", out, tc.want)
			}
			if tc.evicts && (len(inv.ids) != 1 || inv.ids[0] != tc.wus.wu.ID) {
				t.Fatalf("dispatch invalidations = %v, want exactly [%s] after a %s write", inv.ids, tc.wus.wu.ID, tc.want)
			}
			if !tc.evicts && len(inv.ids) != 0 {
				t.Fatalf("dispatch invalidations = %v, want none: %q wrote no unit state", inv.ids, tc.name)
			}
		})
	}
}

// TestTransitioner_NoDispatchInvalidatorIsInert: an unwired transitioner (tests, the
// e2e browser harness) dead-letters exactly as before — the hook is optional.
func TestTransitioner_NoDispatchInvalidatorIsInert(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2})
	wus := &fakeWUS{
		wu:         &workunit.WorkUnit{ID: types.NewID(), LeafID: lf.ID, State: workunit.WorkUnitStateQueued},
		total:      8,
		deadLetter: true,
	}
	if out := runEval(t, wus, lf, nil, &fakeComparator{}); out != OutcomeDeadLettered {
		t.Fatalf("outcome = %v, want FAILED", out)
	}
}
