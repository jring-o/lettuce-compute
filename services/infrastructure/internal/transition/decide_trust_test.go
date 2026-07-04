package transition

import (
	"math/rand"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// polK is pol() plus a resolved trust gate (K distinct trusted corroborators required).
func polK(target, quorum, maxTotal, k int) RedundancyPolicy {
	p := pol(target, quorum, maxTotal)
	p.MinTrustedCorroborators = k
	return p
}

// verdictT is verdict() plus a trusted-agreeing-subject count.
func verdictT(majority, total, trusted int) *ComparisonVerdict {
	v := verdict(majority, total)
	v.TrustedMajorityCount = trusted
	return v
}

func TestDecide_TrustGate(t *testing.T) {
	completed := workunit.WorkUnitStateCompleted
	q := workunit.WorkUnitStateQueued

	// Trusted corroborator present -> validates (all four gates hold).
	t.Run("trusted met validates", func(t *testing.T) {
		s := UnitSnapshot{State: q, Policy: polK(2, 2, 8, 1), LiveCopies: 0, TotalCopies: 2,
			PendingCount: 2, Comparison: verdictT(2, 2, 1)}
		if got := Decide(s).Action; got != ActionValidate {
			t.Errorf("Decide() = %v, want VALIDATE", got)
		}
	})

	// Results agree but no trusted corroborator, and MORE COPIES CAN STILL ARRIVE (a live
	// straggler) -> wait, exactly like "threshold unmet". A blocked-by-trust unit must never
	// reject while a trusted corroborator could still show up.
	t.Run("trusted unmet with live copy waits", func(t *testing.T) {
		s := UnitSnapshot{State: q, Policy: polK(2, 2, 8, 1), LiveCopies: 1, TotalCopies: 3,
			PendingCount: 2, Comparison: verdictT(2, 2, 0)}
		if got := Decide(s).Action; got != ActionWait {
			t.Errorf("Decide() = %v, want WAIT (trusted corroborator may still arrive)", got)
		}
	})

	// Results agree but no trusted corroborator, and dispatch headroom remains (target >
	// pending, budget not exhausted) -> wait so more copies go out.
	t.Run("trusted unmet with dispatch headroom waits", func(t *testing.T) {
		s := UnitSnapshot{State: q, Policy: polK(3, 2, 9, 1), LiveCopies: 0, TotalCopies: 2,
			PendingCount: 2, Comparison: verdictT(2, 2, 0)}
		if got := Decide(s).Action; got != ActionWait {
			t.Errorf("Decide() = %v, want WAIT (dispatch headroom for a trusted corroborator)", got)
		}
	})

	// Results agree but no trusted corroborator and NO more copies can come (no live copy, no
	// headroom) -> reject the round and requeue for a fresh set. Only now does blocked-by-trust
	// resolve to a reject, matching the "threshold unmet; no more copies" path.
	t.Run("trusted unmet with no more copies rejects", func(t *testing.T) {
		s := UnitSnapshot{State: completed, Policy: polK(2, 2, 8, 1), LiveCopies: 0, TotalCopies: 2,
			PendingCount: 2, Comparison: verdictT(2, 2, 0)}
		if got := Decide(s).Action; got != ActionReject {
			t.Errorf("Decide() = %v, want REJECT (agree but untrusted, no more copies)", got)
		}
	})
}

// TestProperty_GateOffIgnoresTrustedCount is the deploy-safety proof for the trust gate: with
// the gate off (MinTrustedCorroborators == 0, the resolution for GateEnabled false), Decide's
// action is IDENTICAL for EVERY possible TrustedMajorityCount. So enabling the feature code
// without enabling the gate cannot change any decision.
func TestProperty_GateOffIgnoresTrustedCount(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	for i := 0; i < propIters; i++ {
		s := randSnapshot(r, false) // Policy.MinTrustedCorroborators == 0 (gate off)
		if s.Comparison == nil {
			continue
		}
		baseline := Decide(s).Action
		for trusted := 0; trusted <= s.Comparison.Total; trusted++ {
			s.Comparison.TrustedMajorityCount = trusted
			if got := Decide(s).Action; got != baseline {
				t.Fatalf("gate-off decision changed with TrustedMajorityCount=%d: got %v want %v\nsnapshot=%+v",
					trusted, got, baseline, s)
			}
		}
	}
}
