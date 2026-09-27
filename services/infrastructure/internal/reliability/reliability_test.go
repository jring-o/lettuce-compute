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

func TestCeiling(t *testing.T) {
	tests := []struct {
		name                         string
		flatCap, perUnit, cores, gpu int
		want                         int
	}{
		{"small machine keeps the flat cap", 10, 2, 2, 0, 10},
		{"five cores is exactly the flat cap", 10, 2, 5, 0, 10},
		{"six cores passes it", 10, 2, 6, 0, 12},
		{"cores and GPUs both count", 10, 2, 32, 1, 66},
		{"GPUs alone count", 10, 2, 0, 8, 16},
		{"scaling off keeps the flat cap", 10, 0, 256, 1, 10},
		{"negative scaling is off", 10, -1, 256, 1, 10},
		{"unbounded flat cap stays unbounded", 0, 2, 256, 1, 0},
		{"negative figures count as none", 10, 2, -4, -1, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Ceiling(tt.flatCap, tt.perUnit, tt.cores, tt.gpu); got != tt.want {
				t.Errorf("Ceiling(%d, %d, %d, %d) = %d, want %d", tt.flatCap, tt.perUnit, tt.cores, tt.gpu, got, tt.want)
			}
		})
	}
}

func TestScaledBudget(t *testing.T) {
	const floor, cap, ramp = 2, 10, 5.0
	tests := []struct {
		name    string
		score   float64
		ceiling int
		want    int
	}{
		{"cold host -> floor, whatever its ceiling", 0, 512, floor},
		{"inside the ramp -> Budget", 2.5, 512, 6},
		{"at the ramp -> the flat cap", 5, 512, cap},
		{"one unit past the ramp -> one more", 6, 512, 11},
		{"part of a unit earns nothing yet", 5.9, 512, cap},
		{"seven past the ramp -> seven more", 12, 512, 17},
		{"far past the ramp -> the ceiling", 10000, 64, 64},
		{"ceiling at the flat cap -> the flat cap", 10000, cap, cap},
		{"ceiling below the flat cap -> the flat cap", 10000, 4, cap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScaledBudget(tt.score, floor, cap, tt.ceiling, ramp); got != tt.want {
				t.Errorf("ScaledBudget(%v, %d, %d, %d, %v) = %d, want %d", tt.score, floor, cap, tt.ceiling, ramp, got, tt.want)
			}
		})
	}
}

// TestScaledBudgetMatchesBudgetUpToTheFlatCap: with no headroom above the flat cap the
// scaled budget is Budget exactly, so small machines see no change.
func TestScaledBudgetMatchesBudgetUpToTheFlatCap(t *testing.T) {
	const floor, cap = 2, 10
	for score := -2.0; score <= 50; score += 0.25 {
		if got, want := ScaledBudget(score, floor, cap, cap, DefaultRampUnits), Budget(score, floor, cap, DefaultRampUnits); got != want {
			t.Fatalf("score %v: ScaledBudget = %d, Budget = %d", score, got, want)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
