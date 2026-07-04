package transition

import (
	"math/rand"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// probRes builds a result stamped with a trust subject + score AND a PROBATION submit-time
// standing (BG-24b), so it is non-countable — invisible to the verdict and to redundancy
// coverage while still riding along in the pending set.
func probRes(subject string, score int) *result.Result {
	r := subjRes(subject, score)
	st := volunteer.StandingProbation
	r.StandingAtSubmit = &st
	return r
}

func TestStandingCountable(t *testing.T) {
	if !StandingCountable(&result.Result{}) {
		t.Error("nil StandingAtSubmit (legacy row) should be countable")
	}
	ok := volunteer.StandingOK
	if !StandingCountable(&result.Result{StandingAtSubmit: &ok}) {
		t.Error("OK stamp should be countable")
	}
	prob := volunteer.StandingProbation
	if StandingCountable(&result.Result{StandingAtSubmit: &prob}) {
		t.Error("PROBATION stamp should not be countable")
	}
}

func TestBuildComparisonVerdict_StandingExclusion(t *testing.T) {
	// (a) a probation-stamped agreeing result is invisible to Total, MajorityCount,
	// TrustedMajorityCount, and Ratio — as if never submitted. Excluding it from Total (not
	// just the majority) is what keeps it from dragging Ratio / the strict-majority gate down.
	t.Run("probation result invisible to all counts", func(t *testing.T) {
		a := subjRes("did:plc:a", 30)
		b := subjRes("did:plc:b", 30)
		prob := probRes("did:plc:c", 30) // a third agreeing subject, IF it counted
		pending := []*result.Result{a, b, prob}
		majority := []*result.Result{a, b, prob} // comparator lumped it in; the verdict still skips it
		v := BuildComparisonVerdict(pending, majority, 25)
		if v.Total != 2 {
			t.Fatalf("Total = %d, want 2 (probation subject invisible)", v.Total)
		}
		if v.MajorityCount != 2 || v.TrustedMajorityCount != 2 {
			t.Fatalf("Majority=%d Trusted=%d, want 2/2", v.MajorityCount, v.TrustedMajorityCount)
		}
		if v.Ratio != 1.0 {
			t.Errorf("Ratio = %v, want 1.0 (probation neither helps nor hurts)", v.Ratio)
		}
	})

	// (b) all-probation pending → zero verdict (nothing is countable).
	t.Run("all probation yields zero verdict", func(t *testing.T) {
		p1 := probRes("did:plc:a", 30)
		p2 := probRes("did:plc:b", 30)
		pending := []*result.Result{p1, p2}
		v := BuildComparisonVerdict(pending, pending, 25)
		if v.Total != 0 || v.MajorityCount != 0 || v.TrustedMajorityCount != 0 || v.Ratio != 0 {
			t.Fatalf("all-probation verdict = %+v, want all zero", v)
		}
	})

	// (c) a subject with one OK result (in the majority) and one probation result (outside it):
	// the probation result is skipped BEFORE subject aggregation, so it neither breaks the
	// subject's coherence nor contributes to its score. Here subject A's ONLY countable result
	// scores 0 (untrusted) while its skipped probation device scores 100 — A must count toward
	// the majority (coherent) but NOT toward the trusted majority.
	t.Run("mixed subject skips probation before aggregation", func(t *testing.T) {
		aOK := subjRes("did:plc:a", 0)     // countable, score 0
		aProb := probRes("did:plc:a", 100) // skipped: neither breaks coherence nor lifts score
		b := subjRes("did:plc:b", 30)      // countable, trusted
		pending := []*result.Result{aOK, aProb, b}
		majority := []*result.Result{aOK, b} // aProb is OUTSIDE the majority group
		v := BuildComparisonVerdict(pending, majority, 25)
		if v.Total != 2 {
			t.Fatalf("Total = %d, want 2 (A and B; A's probation device invisible)", v.Total)
		}
		if v.MajorityCount != 2 {
			t.Fatalf("MajorityCount = %d, want 2 (A stays coherent — its probation device is skipped, not counted out-of-majority)", v.MajorityCount)
		}
		if v.TrustedMajorityCount != 1 {
			t.Fatalf("TrustedMajorityCount = %d, want 1 (only B; A's skipped device does not lift A's score to 100)", v.TrustedMajorityCount)
		}
	})
}

// TestDecide_ProbationForcesReplication: a unit nominally at target but whose pending copies are
// all probation covers nothing, so it must keep dispatching full replication (ActionWait with
// headroom, stays QUEUED) and never reject — the forced-replication case.
func TestDecide_ProbationForcesReplication(t *testing.T) {
	p := pol(2, 2, 8)
	s := UnitSnapshot{State: workunit.WorkUnitStateQueued, Policy: p, LiveCopies: 0, TotalCopies: 2,
		PendingCount: 2, ProbationPendingCount: 2, Comparison: &ComparisonVerdict{}}
	d := Decide(s)
	if d.Action != ActionWait {
		t.Fatalf("Decide() = %v, want WAIT (probation copies keep headroom open)", d.Action)
	}
	if d.CompleteFirst {
		t.Errorf("CompleteFirst = true, want false (headroom remains → stay QUEUED for replication)")
	}
	if !Dispatchable(s) {
		t.Errorf("Dispatchable = false, want true (countable coverage 0 < target 2)")
	}
	if RedundancyMet(s) {
		t.Errorf("RedundancyMet = true, want false (probation copies cover nothing)")
	}
}

// TestDecide_ProbationCoverageArithmetic: probation LIVE copies are excluded from coverage the
// same way pending ones are, so Dispatchable/RedundancyMet see only the countable complement.
func TestDecide_ProbationCoverageArithmetic(t *testing.T) {
	p := pol(2, 2, 8)
	// Two live copies, both probation-held → countable coverage 0 → still needs copies.
	s := UnitSnapshot{State: workunit.WorkUnitStateQueued, Policy: p, LiveCopies: 2,
		ProbationLiveCopies: 2, TotalCopies: 2, PendingCount: 0}
	if !Dispatchable(s) {
		t.Errorf("Dispatchable = false, want true (2 live copies but both probation)")
	}
	if RedundancyMet(s) {
		t.Errorf("RedundancyMet = true, want false (probation live copies cover nothing)")
	}
	// One of the two live copies is OK-standing → countable coverage 1, still under target 2.
	s.ProbationLiveCopies = 1
	if !Dispatchable(s) || RedundancyMet(s) {
		t.Errorf("one countable of two: Dispatchable=%v RedundancyMet=%v, want true/false", Dispatchable(s), RedundancyMet(s))
	}
	// Neither probation → countable coverage 2 == target → covered.
	s.ProbationLiveCopies = 0
	if Dispatchable(s) || !RedundancyMet(s) {
		t.Errorf("both countable: Dispatchable=%v RedundancyMet=%v, want false/true", Dispatchable(s), RedundancyMet(s))
	}
}

// TestDecide_AllProbationRejectsOnlyWhenBudgetExhausted is the reject-cycle guard: a unit whose
// copies are all probation NEVER reaches ActionReject while its copy budget remains (it waits,
// dispatching replication); only once the total-copy ceiling is hit does the resource valve let
// it reject and requeue a fresh set.
func TestDecide_AllProbationRejectsOnlyWhenBudgetExhausted(t *testing.T) {
	p := pol(2, 2, 8) // MaxTotalCopies 8
	base := UnitSnapshot{State: workunit.WorkUnitStateCompleted, Policy: p, LiveCopies: 0,
		PendingCount: 2, ProbationPendingCount: 2, Comparison: &ComparisonVerdict{}}

	withBudget := base
	withBudget.TotalCopies = 2 // well under ceiling
	if got := Decide(withBudget).Action; got != ActionWait {
		t.Errorf("with budget: Decide() = %v, want WAIT (never reject while budget remains)", got)
	}

	exhausted := base
	exhausted.TotalCopies = 8 // ceiling reached → capsExhausted
	if got := Decide(exhausted).Action; got != ActionReject {
		t.Errorf("budget exhausted: Decide() = %v, want REJECT (resource valve)", got)
	}
}

// randProbation copies a snapshot and fills in random probation counts bounded by the live /
// pending totals — the shape the transitioner produces (0 <= probation <= its total).
func randProbation(r *rand.Rand, s UnitSnapshot) UnitSnapshot {
	if s.LiveCopies > 0 {
		s.ProbationLiveCopies = r.Intn(s.LiveCopies + 1)
	}
	if s.PendingCount > 0 {
		s.ProbationPendingCount = r.Intn(s.PendingCount + 1)
	}
	return s
}

// TestProperty_Standing_DispatchableImpliesNotRedundancyMet re-locks the #49 firewall WITH
// probation counts present: Dispatchable and RedundancyMet still derive from the SAME countable
// (live+pending-minus-probation) vs target comparison, so a dispatchable unit still needs copies.
func TestProperty_Standing_DispatchableImpliesNotRedundancyMet(t *testing.T) {
	r := rand.New(rand.NewSource(21))
	for i := 0; i < propIters; i++ {
		s := randProbation(r, randSnapshot(r, false))
		if Dispatchable(s) && RedundancyMet(s) {
			t.Fatalf("dispatchable AND redundancy-met with probation (drift!): %+v policy=%+v", s, s.Policy)
		}
	}
}

// TestProperty_Standing_ZeroProbationEquivalence is the standing inertness proof: with zero
// probation counts (what all-OK / nil-stamp results produce), Decide's terminal outcome is
// identical to the pre-standing golden model in the default target == quorum regime.
func TestProperty_Standing_ZeroProbationEquivalence(t *testing.T) {
	r := rand.New(rand.NewSource(22))
	for i := 0; i < propIters; i++ {
		s := randSnapshot(r, true) // target == quorum
		s.Policy.MaxErrorCopies = 0
		s.ProbationLiveCopies = 0
		s.ProbationPendingCount = 0
		if got, want := Decide(s).Action, goldenDecide(s); got != want {
			t.Fatalf("zero-probation equivalence mismatch: got %v want %v\nsnapshot=%+v\npolicy=%+v",
				got, want, s, s.Policy)
		}
	}
}

// TestTransitioner_ProbationPendingForcesReplication exercises the decideAndApply wiring: two
// agreeing PENDING results, both stamped PROBATION, are non-countable, so the transitioner counts
// them as probation-pending, sees countable coverage 0, and WAITS (keeps replicating) rather than
// rejecting or validating. With the wiring absent (probation-pending miscounted as 0) this same
// snapshot would instead REJECT, so the test pins the population of ProbationPendingCount.
func TestTransitioner_ProbationPendingForcesReplication(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2})
	prob := volunteer.StandingProbation
	pend := []*result.Result{
		{ID: types.NewID(), VolunteerID: types.NewID(), ValidationStatus: result.ValidationPending, StandingAtSubmit: &prob},
		{ID: types.NewID(), VolunteerID: types.NewID(), ValidationStatus: result.ValidationPending, StandingAtSubmit: &prob},
	}
	wus := &fakeWUS{
		wu:    &workunit.WorkUnit{ID: types.NewID(), LeafID: lf.ID, State: workunit.WorkUnitStateQueued},
		live:  0,
		total: 2,
	}
	cmp := &fakeComparator{majority: pend} // they agree, but the verdict skips both as non-countable
	out := runEval(t, wus, lf, pend, cmp)
	if out != OutcomeWaiting {
		t.Fatalf("outcome = %v, want WAITING (probation-pending forces replication)", out)
	}
	if cmp.acceptCalls != 0 || cmp.rejectCalls != 0 {
		t.Errorf("accept=%d reject=%d, want 0/0 (neither validate nor reject)", cmp.acceptCalls, cmp.rejectCalls)
	}
}

// TestProperty_Standing_AllProbationNeverRejectsWithBudget generalizes the reject-cycle guard: an
// all-probation unit (every live copy and pending result non-countable, so the verdict is empty)
// is never rejected or dead-lettered while its copy budget remains — the resource valve
// (capsExhausted) is the only thing that ever ends the replication loop.
func TestProperty_Standing_AllProbationNeverRejectsWithBudget(t *testing.T) {
	r := rand.New(rand.NewSource(23))
	for i := 0; i < propIters; i++ {
		s := randSnapshot(r, false)
		if s.State == workunit.WorkUnitStateValidated || s.State == workunit.WorkUnitStateFailed {
			continue
		}
		s.ProbationLiveCopies = s.LiveCopies
		s.ProbationPendingCount = s.PendingCount
		if s.Comparison != nil {
			s.Comparison = &ComparisonVerdict{} // a real all-probation verdict is empty
		}
		if capsExhausted(s) {
			continue // budget spent: the valve is allowed to reject / dead-letter
		}
		if a := Decide(s).Action; a == ActionReject || a == ActionDeadLetter {
			t.Fatalf("all-probation unit with budget remaining got %v: %+v policy=%+v", a, s, s.Policy)
		}
	}
}
