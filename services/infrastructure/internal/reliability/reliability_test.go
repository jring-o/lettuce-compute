package reliability

import (
	"math"
	"testing"
)

func TestDecayedScore(t *testing.T) {
	// Over exactly one half-life the score halves; over zero/negative elapsed it is
	// unchanged; a long elapsed decays toward ~0.
	if got := DecayedScore(8, 0); got != 8 {
		t.Errorf("DecayedScore(8, 0) = %v, want 8 (no decay at zero elapsed)", got)
	}
	if got := DecayedScore(8, -5); got != 8 {
		t.Errorf("DecayedScore(8, -5) = %v, want 8 (no decay at negative elapsed)", got)
	}
	if got := DecayedScore(8, HalfLifeSeconds); math.Abs(got-4) > 1e-9 {
		t.Errorf("DecayedScore(8, halflife) = %v, want 4 (one half-life halves it)", got)
	}
	if got := DecayedScore(8, 2*HalfLifeSeconds); math.Abs(got-2) > 1e-9 {
		t.Errorf("DecayedScore(8, 2*halflife) = %v, want 2", got)
	}
}

func TestBudget(t *testing.T) {
	const floor, cap, ramp = 2, 10, 5.0

	tests := []struct {
		name  string
		score float64
		floor int
		cap   int
		ramp  float64
		want  int
	}{
		{"zero score -> floor (cold start)", 0, floor, cap, ramp, floor},
		{"negative score -> floor", -3, floor, cap, ramp, floor},
		{"score at ramp -> full cap", 5, floor, cap, ramp, cap},
		{"score above ramp -> clamped to cap", 50, floor, cap, ramp, cap},
		// midpoint: score/ramp = 0.5 -> floor + 0.5*(cap-floor) = 2 + 4 = 6.
		{"half ramp -> midpoint", 2.5, floor, cap, ramp, 6},
		// one validated unit out of ramp=5 -> floor + 0.2*8 = 2 + 1.6 -> round 4 (3.6->4).
		{"one unit -> just above floor", 1, floor, cap, ramp, 4},
		{"degenerate cap<=floor -> floor", 100, 5, 5, ramp, 5},
		{"degenerate ramp<=0 -> floor", 100, floor, cap, 0, floor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Budget(tt.score, tt.floor, tt.cap, tt.ramp)
			if got != tt.want {
				t.Errorf("Budget(%v, %d, %d, %v) = %d, want %d", tt.score, tt.floor, tt.cap, tt.ramp, got, tt.want)
			}
			if got < tt.floor || got > maxInt(tt.floor, tt.cap) {
				t.Errorf("Budget(%v) = %d out of [%d, %d]", tt.score, got, tt.floor, tt.cap)
			}
		})
	}
}

// TestBudgetRampReachesFullInAFewUnits asserts the honest-host ramp: starting from a fresh
// score, ~rampUnits validated units (good steps) take the budget from floor to the full cap
// — the "earn your buffer over a few validated units" property (decision #5).
func TestBudgetRampReachesFullInAFewUnits(t *testing.T) {
	const floor, cap = 2, 16
	score := 0.0
	// Simulate consecutive validated units (negligible elapsed between them -> no decay).
	reachedCapAfter := -1
	for i := 1; i <= 10; i++ {
		score += DefaultGoodStep
		if Budget(score, floor, cap, DefaultRampUnits) >= cap && reachedCapAfter == -1 {
			reachedCapAfter = i
		}
	}
	if reachedCapAfter == -1 {
		t.Fatalf("budget never reached cap after 10 validated units")
	}
	if reachedCapAfter > int(DefaultRampUnits)+1 {
		t.Errorf("budget reached cap after %d units, want <= %d (a few units)", reachedCapAfter, int(DefaultRampUnits)+1)
	}
}

// TestBudgetSingleBadAmongManyGoodsBarelyMoves asserts the false-positive guard: one bad
// event when a host has a long good record drops the budget by at most a small amount, NOT
// to the floor (one slow unit is not a liar).
func TestBudgetSingleBadAmongManyGoodsBarelyMoves(t *testing.T) {
	const floor, cap = 2, 16
	// A well-established host: many goods -> score well past the ramp -> full cap.
	score := DefaultRampUnits * 4
	before := Budget(score, floor, cap, DefaultRampUnits)
	if before != cap {
		t.Fatalf("established host budget = %d, want cap %d", before, cap)
	}
	// One bad event (no decay): score drops by BadStep but stays well above the ramp.
	score = math.Max(0, score-DefaultBadStep)
	after := Budget(score, floor, cap, DefaultRampUnits)
	if after != cap {
		t.Errorf("after one bad event budget = %d, want still %d (single bad must not tank an established host)", after, cap)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
