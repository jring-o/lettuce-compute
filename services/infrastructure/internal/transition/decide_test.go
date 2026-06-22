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
		MaxSuccessCopies:   target,
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
				AgreementThreshold: 1.0, MaxTotalCopies: 100, MaxErrorCopies: 2, MaxSuccessCopies: 2},
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

func TestDecide_ThresholdBelowOne(t *testing.T) {
	// With threshold 0.6 and a 2-of-3 majority (ratio 0.67), the unit validates exactly as the
	// historical applyThreshold did — min_quorum is the attempt gate, NOT an agreeing-group floor.
	p := RedundancyPolicy{TargetCopies: 3, MinQuorum: 3, AgreementThreshold: 0.6, MaxTotalCopies: 9, MaxSuccessCopies: 3}
	s := UnitSnapshot{State: workunit.WorkUnitStateCompleted, Policy: p, LiveCopies: 0, TotalCopies: 3,
		PendingCount: 3, Comparison: verdict(2, 3)}
	if got := Decide(s).Action; got != ActionValidate {
		t.Errorf("Decide() = %v, want VALIDATE (ratio 0.67 >= threshold 0.6)", got)
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
