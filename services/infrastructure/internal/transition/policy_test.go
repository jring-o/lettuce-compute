package transition

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// leafWith builds a leaf with the given validation config (defaults applied so threshold is
// the documented 1.0 unless overridden).
func leafWith(vc leaf.ValidationConfig) *leaf.Leaf {
	if vc.AgreementThreshold == 0 {
		vc.AgreementThreshold = 1.0
	}
	return &leaf.Leaf{ValidationConfig: vc}
}

func TestResolvePolicy_DefaultsReproduceRedundancyFactor(t *testing.T) {
	for _, r := range []int{1, 2, 3, 5} {
		lf := leafWith(leaf.ValidationConfig{RedundancyFactor: r})
		wu := &workunit.WorkUnit{}
		p := ResolvePolicy(lf, wu)
		if p.TargetCopies != r {
			t.Errorf("r=%d: TargetCopies = %d, want %d", r, p.TargetCopies, r)
		}
		if p.MinQuorum != r {
			t.Errorf("r=%d: MinQuorum = %d, want %d", r, p.MinQuorum, r)
		}
		if p.MaxTotalCopies != r+defaultCopyRetryMargin {
			t.Errorf("r=%d: MaxTotalCopies = %d, want %d", r, p.MaxTotalCopies, r+defaultCopyRetryMargin)
		}
		if p.MaxSuccessCopies != r {
			t.Errorf("r=%d: MaxSuccessCopies = %d, want target %d", r, p.MaxSuccessCopies, r)
		}
		if p.MaxErrorCopies != 0 {
			t.Errorf("r=%d: MaxErrorCopies = %d, want 0 (unlimited)", r, p.MaxErrorCopies)
		}
	}
}

func TestResolvePolicy_ExplicitTargetQuorum(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2, TargetCopies: 4, MinQuorum: 2})
	p := ResolvePolicy(lf, &workunit.WorkUnit{})
	if p.TargetCopies != 4 || p.MinQuorum != 2 {
		t.Fatalf("got target=%d quorum=%d, want 4/2", p.TargetCopies, p.MinQuorum)
	}
	// Default ceiling derives from target, not redundancy_factor.
	if p.MaxTotalCopies != 4+defaultCopyRetryMargin {
		t.Errorf("MaxTotalCopies = %d, want %d", p.MaxTotalCopies, 4+defaultCopyRetryMargin)
	}
}

func TestResolvePolicy_PerUnitOverrideWins(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2, TargetCopies: 3, MinQuorum: 2})
	wu := &workunit.WorkUnit{TargetCopies: 5, MinQuorum: 4, MaxTotalCopies: 9}
	p := ResolvePolicy(lf, wu)
	if p.TargetCopies != 5 || p.MinQuorum != 4 || p.MaxTotalCopies != 9 {
		t.Fatalf("per-unit override not applied: %+v", p)
	}
}

func TestResolvePolicy_SpotCheckForcesTwoOfTwo(t *testing.T) {
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 1, SpotCheckEnabled: true, SpotCheckPercentage: 5})
	wu := &workunit.WorkUnit{SpotCheck: true}
	p := ResolvePolicy(lf, wu)
	if p.TargetCopies != 2 || p.MinQuorum != 2 {
		t.Fatalf("spot-check should force 2-of-2, got target=%d quorum=%d", p.TargetCopies, p.MinQuorum)
	}
	if !p.SpotCheck {
		t.Error("SpotCheck flag not carried")
	}
}

func TestResolvePolicy_QuorumNeverExceedsTarget(t *testing.T) {
	// Even a malformed config (min_quorum > target) is clamped defensively (the validator
	// rejects it up front, but the resolver must never produce quorum > target).
	lf := leafWith(leaf.ValidationConfig{RedundancyFactor: 2, TargetCopies: 2, MinQuorum: 5})
	p := ResolvePolicy(lf, &workunit.WorkUnit{})
	if p.MinQuorum > p.TargetCopies {
		t.Fatalf("MinQuorum %d exceeds TargetCopies %d", p.MinQuorum, p.TargetCopies)
	}
}
