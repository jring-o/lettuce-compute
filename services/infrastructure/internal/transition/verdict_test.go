package transition

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// subjRes builds a result stamped with an explicit trust subject + snapshot score.
func subjRes(subject string, score int) *result.Result {
	subj := subject
	sc := score
	return &result.Result{ID: types.NewID(), VolunteerID: types.NewID(), TrustSubject: &subj, TrustScoreAtSubmit: &sc}
}

// legacyRes builds a legacy (pre-trust) result: nil subject + nil score, so the builder
// must fall back to the per-volunteer sentinel subject with score 0.
func legacyRes(vol types.ID) *result.Result {
	return &result.Result{ID: types.NewID(), VolunteerID: vol}
}

func TestBuildComparisonVerdict_Table(t *testing.T) {
	// (i) two results, one subject, both agreeing -> MajorityCount 1 (a lone principal cannot
	// validate a 2-of-2 quorum by itself).
	t.Run("one subject two devices counts once", func(t *testing.T) {
		a1 := subjRes("did:plc:a", 30)
		a2 := subjRes("did:plc:a", 30)
		pending := []*result.Result{a1, a2}
		v := BuildComparisonVerdict(pending, pending, 25)
		if v.Total != 1 || v.MajorityCount != 1 {
			t.Fatalf("Total=%d MajorityCount=%d, want 1/1", v.Total, v.MajorityCount)
		}
		if v.TrustedMajorityCount != 1 {
			t.Errorf("TrustedMajorityCount = %d, want 1", v.TrustedMajorityCount)
		}
	})

	// (ii) subjects A, A, B all agreeing -> MajorityCount 2, Total 2.
	t.Run("distinct subjects A A B", func(t *testing.T) {
		a1 := subjRes("did:plc:a", 30)
		a2 := subjRes("did:plc:a", 30)
		b := subjRes("did:plc:b", 30)
		pending := []*result.Result{a1, a2, b}
		v := BuildComparisonVerdict(pending, pending, 25)
		if v.Total != 2 || v.MajorityCount != 2 {
			t.Fatalf("Total=%d MajorityCount=%d, want 2/2", v.Total, v.MajorityCount)
		}
		if v.Ratio != 1.0 {
			t.Errorf("Ratio = %v, want 1.0", v.Ratio)
		}
	})

	// (iii) A agrees on one device, disagrees on another -> A in Total, not in MajorityCount.
	t.Run("incoherent subject counts in total but never corroborates", func(t *testing.T) {
		aAgree := subjRes("did:plc:a", 30)
		aDisagree := subjRes("did:plc:a", 30)
		b := subjRes("did:plc:b", 30)
		pending := []*result.Result{aAgree, aDisagree, b}
		majority := []*result.Result{aAgree, b} // A's other device is outside the majority
		v := BuildComparisonVerdict(pending, majority, 25)
		if v.Total != 2 {
			t.Fatalf("Total = %d, want 2 (A and B)", v.Total)
		}
		if v.MajorityCount != 1 {
			t.Fatalf("MajorityCount = %d, want 1 (only B; A is incoherent)", v.MajorityCount)
		}
		if v.TrustedMajorityCount != 1 {
			t.Errorf("TrustedMajorityCount = %d, want 1", v.TrustedMajorityCount)
		}
	})

	// (iv) floor 25, snapshots 30 and 0 -> TrustedMajorityCount 1.
	t.Run("floor separates trusted from untrusted agreeing subjects", func(t *testing.T) {
		trusted := subjRes("did:plc:a", 30)
		untrusted := subjRes("did:plc:b", 0)
		pending := []*result.Result{trusted, untrusted}
		v := BuildComparisonVerdict(pending, pending, 25)
		if v.MajorityCount != 2 {
			t.Fatalf("MajorityCount = %d, want 2", v.MajorityCount)
		}
		if v.TrustedMajorityCount != 1 {
			t.Fatalf("TrustedMajorityCount = %d, want 1 (only the score-30 subject)", v.TrustedMajorityCount)
		}
	})

	// (v) legacy rows (nil stamps) behave as distinct per-volunteer subjects with score 0.
	t.Run("legacy nil-stamp rows are distinct score-0 subjects", func(t *testing.T) {
		vol1 := types.NewID()
		vol2 := types.NewID()
		l1 := legacyRes(vol1)
		l2 := legacyRes(vol2)
		pending := []*result.Result{l1, l2}

		// Subjects fall back to the per-volunteer sentinel.
		if got := SubjectForResult(l1); got != trust.SubjectForVolunteerID(vol1) {
			t.Errorf("legacy subject = %q, want %q", got, trust.SubjectForVolunteerID(vol1))
		}

		// Floor 0: two distinct subjects, both agreeing, both "trusted" (score 0 >= floor 0).
		v0 := BuildComparisonVerdict(pending, pending, 0)
		if v0.Total != 2 || v0.MajorityCount != 2 || v0.TrustedMajorityCount != 2 {
			t.Fatalf("floor 0: Total=%d Majority=%d Trusted=%d, want 2/2/2", v0.Total, v0.MajorityCount, v0.TrustedMajorityCount)
		}
		// Any positive floor: score-0 legacy subjects are untrusted.
		v1 := BuildComparisonVerdict(pending, pending, 1)
		if v1.MajorityCount != 2 || v1.TrustedMajorityCount != 0 {
			t.Fatalf("floor 1: Majority=%d Trusted=%d, want 2/0", v1.MajorityCount, v1.TrustedMajorityCount)
		}
	})

	// Empty / nil majority -> the tie-decides-nothing rule.
	t.Run("empty majority yields zero majority and trusted counts", func(t *testing.T) {
		a := subjRes("did:plc:a", 30)
		b := subjRes("did:plc:b", 30)
		pending := []*result.Result{a, b}
		v := BuildComparisonVerdict(pending, nil, 25)
		if v.Total != 2 {
			t.Fatalf("Total = %d, want 2", v.Total)
		}
		if v.MajorityCount != 0 || v.TrustedMajorityCount != 0 || v.Ratio != 0 {
			t.Fatalf("empty majority: Majority=%d Trusted=%d Ratio=%v, want 0/0/0", v.MajorityCount, v.TrustedMajorityCount, v.Ratio)
		}
	})
}
