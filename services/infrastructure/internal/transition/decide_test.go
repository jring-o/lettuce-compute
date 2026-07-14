package transition

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// pol is a small helper to build a RedundancyPolicy for tests.
func pol(target, quorum, maxTotal int) RedundancyPolicy {
	return RedundancyPolicy{
		TargetCopies:       target,
		MinQuorum:          quorum,
		AgreementThreshold: 1.0,
		MaxTotalCopies:     maxTotal,
	}
}

// verdict builds a comparator verdict with the given majority/total (ratio derived).
func verdict(majority, total int) *ComparisonVerdict {
	return &ComparisonVerdict{
		MajorityCount: majority,
		Total:         total,
		Ratio:         float64(majority) / float64(total),
	}
}

func TestDecide_Table(t *testing.T) {
	q := workunit.WorkUnitStateQueued
	completed := workunit.WorkUnitStateCompleted
	tests := []struct {
		name string
		s    UnitSnapshot
		want Action
	}{
		{
			name: "redundancy=1 single agreeing result validates",
			s: UnitSnapshot{State: q, Policy: pol(1, 1, 7), LiveCopies: 0, TotalCopies: 1,
				PendingCount: 1, Comparison: verdict(1, 1)},
			want: ActionValidate,
		},
		{
			name: "redundancy=2 both agree validates",
			s: UnitSnapshot{State: q, Policy: pol(2, 2, 8), LiveCopies: 0, TotalCopies: 2,
				PendingCount: 2, Comparison: verdict(2, 2)},
			want: ActionValidate,
		},
		{
			name: "redundancy=2 one result, one live copy → wait",
			s: UnitSnapshot{State: q, Policy: pol(2, 2, 8), LiveCopies: 1, TotalCopies: 2,
				PendingCount: 1, Comparison: nil},
			want: ActionWait,
		},
		{
			name: "redundancy=2 disagree, straggler live → wait",
			s: UnitSnapshot{State: completed, Policy: pol(2, 2, 8), LiveCopies: 1, TotalCopies: 3,
				PendingCount: 2, Comparison: verdict(1, 2)},
			want: ActionWait,
		},
		{
			name: "redundancy=2 disagree, no live copies → reject",
			s: UnitSnapshot{State: completed, Policy: pol(2, 2, 8), LiveCopies: 0, TotalCopies: 2,
				PendingCount: 2, Comparison: verdict(1, 2)},
			want: ActionReject,
		},
		{
			name: "quorum unmet, no live copy, under ceiling → wait (redispatch)",
			s: UnitSnapshot{State: q, Policy: pol(2, 2, 8), LiveCopies: 0, TotalCopies: 3,
				PendingCount: 0, Comparison: nil},
			want: ActionWait,
		},
		{
			name: "quorum unmet, no live copy, total >= ceiling → dead-letter",
			s: UnitSnapshot{State: q, Policy: pol(2, 2, 8), LiveCopies: 0, TotalCopies: 8,
				PendingCount: 0, Comparison: nil},
			want: ActionDeadLetter,
		},
		{
			name: "target>quorum: quorum agrees, validate without waiting for target",
			s: UnitSnapshot{State: q, Policy: pol(3, 2, 9), LiveCopies: 1, TotalCopies: 3,
				PendingCount: 2, Comparison: verdict(2, 2)},
			want: ActionValidate,
		},
		{
			name: "target>quorum: quorum-many disagree but dispatch headroom → wait (over-dispatch)",
			s: UnitSnapshot{State: q, Policy: pol(3, 2, 9), LiveCopies: 0, TotalCopies: 2,
				PendingCount: 2, Comparison: verdict(1, 2)},
			want: ActionWait,
		},
		{
			name: "max_error_copies hit, no live copy, quorum unmet → dead-letter",
			s: UnitSnapshot{State: q, Policy: RedundancyPolicy{TargetCopies: 2, MinQuorum: 2,
				AgreementThreshold: 1.0, MaxTotalCopies: 100, MaxErrorCopies: 2},
				LiveCopies: 0, TotalCopies: 2, ErrorCopies: 2, PendingCount: 0},
			want: ActionDeadLetter,
		},
		{
			name: "terminal VALIDATED → wait",
			s:    UnitSnapshot{State: workunit.WorkUnitStateValidated, Policy: pol(2, 2, 8)},
			want: ActionWait,
		},
		{
			name: "terminal FAILED → wait",
			s:    UnitSnapshot{State: workunit.WorkUnitStateFailed, Policy: pol(2, 2, 8)},
			want: ActionWait,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.s).Action
			if got != tt.want {
				t.Errorf("Decide() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecide_AgreeingGroupFloor(t *testing.T) {
	// A 2-of-3 majority (ratio 0.67) meets threshold 0.6 but the agreeing group (2) is below
	// min_quorum (3), so the floor blocks validation. With the target fully collected and no
	// more copies possible, the round is rejected (a fresh set will be requeued) rather than
	// validated — the Phase 0 hardening of the previously-vacuous min_quorum.
	p := RedundancyPolicy{TargetCopies: 3, MinQuorum: 3, AgreementThreshold: 0.6, MaxTotalCopies: 9}
	s := UnitSnapshot{State: workunit.WorkUnitStateCompleted, Policy: p, LiveCopies: 0, TotalCopies: 3,
		PendingCount: 3, Comparison: verdict(2, 3)}
	if got := Decide(s).Action; got != ActionReject {
		t.Errorf("Decide() = %v, want REJECT (agreeing group 2 < min_quorum 3)", got)
	}

	// Same 2-of-3 split while a copy is still live → wait for the straggler, not reject.
	s.LiveCopies = 1
	if got := Decide(s).Action; got != ActionWait {
		t.Errorf("Decide() = %v, want WAIT (floor unmet but a copy is still live)", got)
	}
}

func TestDecide_TargetOverQuorum_MajorityValidates(t *testing.T) {
	// The supported way to accept a majority: an explicit min_quorum below target. Here
	// target 3 / min_quorum 2 with threshold 0.6 validates a 2-of-3 group (floor 2>=2, strict
	// majority 2*2>3, ratio 0.67>=0.6).
	p := RedundancyPolicy{TargetCopies: 3, MinQuorum: 2, AgreementThreshold: 0.6, MaxTotalCopies: 9}
	s := UnitSnapshot{State: workunit.WorkUnitStateCompleted, Policy: p, LiveCopies: 0, TotalCopies: 3,
		PendingCount: 3, Comparison: verdict(2, 3)}
	if got := Decide(s).Action; got != ActionValidate {
		t.Errorf("Decide() = %v, want VALIDATE (2-of-3 with min_quorum 2)", got)
	}
}

func TestDecide_StrictMajority(t *testing.T) {
	// Threshold 0.5 with a 2-of-4 split: the ratio (0.5) and the floor (2>=2) are met, but the
	// agreeing group is not a STRICT majority (2*2 is not > 4), so the unit does not validate.
	// This is the runtime guard that protects legacy rows whose threshold slipped to 0.5.
	p := RedundancyPolicy{TargetCopies: 4, MinQuorum: 2, AgreementThreshold: 0.5, MaxTotalCopies: 10}
	s := UnitSnapshot{State: workunit.WorkUnitStateCompleted, Policy: p, LiveCopies: 0, TotalCopies: 4,
		PendingCount: 4, Comparison: verdict(2, 4)}
	if got := Decide(s).Action; got != ActionReject {
		t.Errorf("Decide() = %v, want REJECT (2-of-4 is not a strict majority)", got)
	}
}

func TestDecide_TieNeverValidates(t *testing.T) {
	// A tie is reported by the comparator as a zero-size agreeing group (MajorityCount 0). It
	// must never validate; with more copies possible it waits, otherwise it rejects.
	p := RedundancyPolicy{TargetCopies: 4, MinQuorum: 2, AgreementThreshold: 0.5, MaxTotalCopies: 10}
	s := UnitSnapshot{State: workunit.WorkUnitStateCompleted, Policy: p, LiveCopies: 0, TotalCopies: 4,
		PendingCount: 4, Comparison: &ComparisonVerdict{MajorityCount: 0, Total: 4, Ratio: 0}}
	if got := Decide(s).Action; got == ActionValidate {
		t.Errorf("Decide() = VALIDATE, want anything but VALIDATE for a tie (zero majority)")
	}
}

func TestCompleteFirst(t *testing.T) {
	// A fully-collected (no dispatch headroom) waiting unit is parked COMPLETED, matching the
	// historical COMPLETED-while-corroborating state.
	s := UnitSnapshot{State: workunit.WorkUnitStateQueued, Policy: pol(2, 2, 8), LiveCopies: 1,
		TotalCopies: 3, PendingCount: 2, Comparison: verdict(1, 2)}
	d := Decide(s)
	if d.Action != ActionWait || !d.CompleteFirst {
		t.Errorf("want WAIT+CompleteFirst, got %v CompleteFirst=%v", d.Action, d.CompleteFirst)
	}
	// A unit still under target stays QUEUED while waiting (so more copies dispatch).
	s2 := UnitSnapshot{State: workunit.WorkUnitStateQueued, Policy: pol(3, 2, 9), LiveCopies: 0,
		TotalCopies: 2, PendingCount: 2, Comparison: verdict(1, 2)}
	if d2 := Decide(s2); d2.Action != ActionWait || d2.CompleteFirst {
		t.Errorf("want WAIT without CompleteFirst (over-dispatch), got %v CompleteFirst=%v", d2.Action, d2.CompleteFirst)
	}
}
