package transition

import (
	"math/rand"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// randSnapshot generates a wide variety of unit snapshots for the property tests. When
// forceEqualTargetQuorum is set, target == quorum (the default, behavior-preserving regime the
// golden model covers). It always supplies a Comparison verdict when PendingCount >= MinQuorum
// (the caller's contract), and only ever uses the states the transitioner actually decides on.
func randSnapshot(r *rand.Rand, forceEqualTargetQuorum bool) UnitSnapshot {
	target := 1 + r.Intn(5) // 1..5
	quorum := target
	if !forceEqualTargetQuorum {
		quorum = 1 + r.Intn(target) // 1..target
	}
	threshold := 1.0
	if r.Intn(3) == 0 {
		threshold = 0.5 + 0.5*r.Float64() // 0.5..1.0
	}
	maxTotal := target + r.Intn(8) // target..target+7
	maxError := 0
	if r.Intn(4) == 0 {
		maxError = 1 + r.Intn(5)
	}
	p := RedundancyPolicy{
		TargetCopies:       target,
		MinQuorum:          quorum,
		AgreementThreshold: threshold,
		MaxTotalCopies:     maxTotal,
		MaxErrorCopies:     maxError,
		MaxSuccessCopies:   target,
	}

	states := []workunit.WorkUnitState{
		workunit.WorkUnitStateQueued,
		workunit.WorkUnitStateCompleted,
		workunit.WorkUnitStateValidated,
		workunit.WorkUnitStateFailed,
	}
	st := states[r.Intn(len(states))]

	live := r.Intn(target + 2)
	pending := r.Intn(target + 3)
	total := live + pending + r.Intn(6) // total >= live (closed + live + a margin)
	errCopies := r.Intn(total + 1)

	s := UnitSnapshot{
		State:        st,
		Policy:       p,
		LiveCopies:   live,
		TotalCopies:  total,
		ErrorCopies:  errCopies,
		PendingCount: pending,
	}
	// Contract: a verdict is present iff there are at least quorum pending results.
	if pending >= quorum {
		majority := 1 + r.Intn(pending)
		s.Comparison = &ComparisonVerdict{
			MajorityCount: majority,
			Total:         pending,
			Ratio:         float64(majority) / float64(pending),
		}
	}
	return s
}

const propIters = 20000

// TestProperty_DispatchableImpliesNotRedundancyMet is the structural #49 firewall: a unit the
// dispatcher considers dispatchable still needs copies (its redundancy is NOT met). Both
// predicates derive from the SAME (live+pending) vs target comparison, so this holds by
// construction — the test LOCKS it so a future change that makes Dispatchable use a different
// bound than RedundancyMet fails loudly.
func TestProperty_DispatchableImpliesNotRedundancyMet(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < propIters; i++ {
		s := randSnapshot(r, false)
		if Dispatchable(s) && RedundancyMet(s) {
			t.Fatalf("dispatchable AND redundancy-met (drift!): %+v policy=%+v", s, s.Policy)
		}
	}
}

// TestProperty_DispatchableNonExhaustedIsNeverTerminalNegative: a dispatchable unit whose copy
// budget is NOT yet exhausted is never rejected or dead-lettered — it can only wait for more
// results or validate at quorum. (The dead-letter ceiling is deliberately enforced at copy-
// close, not in the dispatch predicate, matching pre-#50 behavior — so a unit PAST its ceiling
// may still be briefly dispatchable until the transitioner's next tick parks it FAILED. That
// transient is pre-existing and benign; the firewall that matters is the redundancy-headroom
// agreement above.)
func TestProperty_DispatchableNonExhaustedIsNeverTerminalNegative(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for i := 0; i < propIters; i++ {
		s := randSnapshot(r, false)
		if !Dispatchable(s) || capsExhausted(s) {
			continue
		}
		if a := Decide(s).Action; a == ActionReject || a == ActionDeadLetter {
			t.Fatalf("dispatchable non-exhausted unit got %v: %+v policy=%+v", a, s, s.Policy)
		}
	}
}

// TestProperty_DeadLetterOnlyWithNoLiveCopy: a running copy is always allowed to finish — the
// transitioner never dead-letters a unit that still has a live copy (matches the
// DeadLetterIfExhausted "no live copy" guard).
func TestProperty_DeadLetterOnlyWithNoLiveCopy(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for i := 0; i < propIters; i++ {
		s := randSnapshot(r, false)
		if Decide(s).Action == ActionDeadLetter && s.LiveCopies != 0 {
			t.Fatalf("dead-letter with %d live copies: %+v", s.LiveCopies, s)
		}
	}
}

// TestProperty_ValidateOrRejectRequiresQuorum: the transitioner never validates or rejects a
// unit that hasn't collected a quorum's worth of results.
func TestProperty_ValidateOrRejectRequiresQuorum(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	for i := 0; i < propIters; i++ {
		s := randSnapshot(r, false)
		a := Decide(s).Action
		if (a == ActionValidate || a == ActionReject) && s.PendingCount < s.Policy.MinQuorum {
			t.Fatalf("%v with pending %d < quorum %d: %+v", a, s.PendingCount, s.Policy.MinQuorum, s)
		}
	}
}

// TestProperty_CapsHitImpliesDeadLetter: when the copy budget is exhausted with no live copy
// and quorum unmet, the only decision is to dead-letter.
func TestProperty_CapsHitImpliesDeadLetter(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	for i := 0; i < propIters; i++ {
		s := randSnapshot(r, false)
		if CapsHit(s) && Decide(s).Action != ActionDeadLetter {
			t.Fatalf("CapsHit but action %v: %+v policy=%+v", Decide(s).Action, s, s.Policy)
		}
	}
}

// TestProperty_TerminalIsInert: a VALIDATED/FAILED unit is never dispatchable and always waits.
func TestProperty_TerminalIsInert(t *testing.T) {
	r := rand.New(rand.NewSource(6))
	for i := 0; i < propIters; i++ {
		s := randSnapshot(r, false)
		if s.State != workunit.WorkUnitStateValidated && s.State != workunit.WorkUnitStateFailed {
			continue
		}
		if Dispatchable(s) {
			t.Fatalf("terminal unit dispatchable: %+v", s)
		}
		if Decide(s).Action != ActionWait {
			t.Fatalf("terminal unit action %v: %+v", Decide(s).Action, s)
		}
	}
}

// goldenDecide models the decision for the DEFAULT regime (target == quorum ==
// redundancy_factor, default caps): SubmitResult marked COMPLETED at pending >= R, the decider
// accepted on the agreement gates, else held PENDING while active copies remained, else
// rejected, and DeadLetterIfExhausted parked FAILED when QUEUED with no live copy, quorum
// unmet, and total >= ceiling. The acceptance gates fold in the Phase 0 hardening (agreeing
// group >= min_quorum AND a strict majority AND ratio >= threshold), so this stays an
// independent reference for Decide's arithmetic rather than the pre-hardening ratio-only rule.
func goldenDecide(s UnitSnapshot) Action {
	if s.State == workunit.WorkUnitStateValidated || s.State == workunit.WorkUnitStateFailed {
		return ActionWait
	}
	R := s.Policy.MinQuorum // == target in the default regime
	if s.PendingCount >= R {
		v := s.Comparison
		if v != nil && v.Ratio >= s.Policy.AgreementThreshold &&
			v.MajorityCount >= s.Policy.MinQuorum && 2*v.MajorityCount > v.Total {
			return ActionValidate
		}
		if s.LiveCopies > 0 {
			return ActionWait
		}
		return ActionReject
	}
	if s.LiveCopies == 0 && s.TotalCopies >= s.Policy.MaxTotalCopies {
		return ActionDeadLetter
	}
	return ActionWait
}

// TestProperty_DefaultEquivalence is the executable proof of "existing leaves behave
// identically by default": for every snapshot in the target == quorum regime with default
// caps, Decide's terminal outcome matches the golden model of the pre-#50 logic.
func TestProperty_DefaultEquivalence(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < propIters; i++ {
		s := randSnapshot(r, true) // target == quorum
		// The error cap is a #50 addition with no pre-#50 analogue; the golden model does not
		// model it, so exercise equivalence only with the error cap unset (its default).
		s.Policy.MaxErrorCopies = 0
		got := Decide(s).Action
		want := goldenDecide(s)
		if got != want {
			t.Fatalf("default-equivalence mismatch: got %v want %v\nsnapshot=%+v\npolicy=%+v",
				got, want, s, s.Policy)
		}
	}
}
