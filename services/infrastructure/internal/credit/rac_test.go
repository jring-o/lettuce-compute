package credit

import (
	"math"
	"testing"
)

func TestCalculateRAC_ZeroPreviousNewCredit(t *testing.T) {
	// Zero previous RAC with new credit after some elapsed time.
	elapsed := 3600.0 // 1 hour
	newCredit := 10.0
	rac := CalculateRAC(0, elapsed, newCredit)

	decayFactor := math.Exp(-elapsed * math.Ln2 / HalfLifeSeconds)
	expected := newCredit * (1 - decayFactor)
	if math.Abs(rac-expected) > 1e-9 {
		t.Errorf("CalculateRAC(0, %v, %v) = %v, want %v", elapsed, newCredit, rac, expected)
	}
}

func TestCalculateRAC_ExistingRAC_NoNewCredit_Decays(t *testing.T) {
	// Existing RAC, no new credit, should decay.
	previousRAC := 100.0
	elapsed := 3600.0 // 1 hour
	rac := CalculateRAC(previousRAC, elapsed, 0)

	decayFactor := math.Exp(-elapsed * math.Ln2 / HalfLifeSeconds)
	expected := previousRAC * decayFactor
	if math.Abs(rac-expected) > 1e-9 {
		t.Errorf("CalculateRAC(%v, %v, 0) = %v, want %v", previousRAC, elapsed, rac, expected)
	}
}

func TestCalculateRAC_SevenDayHalfLife(t *testing.T) {
	// After exactly 7 days (604800s) with no credit, RAC should be ~50% of previous.
	previousRAC := 100.0
	elapsed := float64(HalfLifeSeconds) // exactly 7 days
	rac := CalculateRAC(previousRAC, elapsed, 0)

	// Should be ~50.
	expected := previousRAC * 0.5
	if math.Abs(rac-expected) > 1e-6 {
		t.Errorf("After 7 days: CalculateRAC(%v, %v, 0) = %v, want ~%v", previousRAC, elapsed, rac, expected)
	}
}

func TestCalculateRAC_FourWeeksDecay(t *testing.T) {
	// After 4 weeks (4 × 604800s) with no credit, RAC should be ~6.25% of previous.
	previousRAC := 100.0
	elapsed := 4 * float64(HalfLifeSeconds)
	rac := CalculateRAC(previousRAC, elapsed, 0)

	// 4 half-lives: 100 * (1/2)^4 = 6.25.
	expected := previousRAC * math.Pow(0.5, 4)
	if math.Abs(rac-expected) > 1e-6 {
		t.Errorf("After 4 weeks: CalculateRAC(%v, %v, 0) = %v, want ~%v", previousRAC, elapsed, rac, expected)
	}
}

func TestCalculateRAC_SuccessiveGrants(t *testing.T) {
	// Simulate multiple successive credit grants at different intervals.
	rac := 0.0

	// Grant 1: 10 credits after 0 seconds (initial).
	rac = CalculateRAC(rac, 0, 10.0)
	if math.Abs(rac-10.0) > 1e-9 {
		t.Fatalf("After grant 1: rac = %v, want 10.0", rac)
	}

	// Grant 2: 5 credits after 1 day (86400s).
	rac = CalculateRAC(rac, 86400, 5.0)
	// decay_factor for 1 day = exp(-86400 * ln2 / 604800) ≈ 0.9057
	decayFactor := math.Exp(-86400 * math.Ln2 / HalfLifeSeconds)
	expected := 10.0*decayFactor + 5.0*(1-decayFactor)
	if math.Abs(rac-expected) > 1e-6 {
		t.Fatalf("After grant 2: rac = %v, want %v", rac, expected)
	}

	// Grant 3: 3 credits after another 3 days (259200s).
	prevRAC := rac
	rac = CalculateRAC(rac, 259200, 3.0)
	decayFactor2 := math.Exp(-259200 * math.Ln2 / HalfLifeSeconds)
	expected2 := prevRAC*decayFactor2 + 3.0*(1-decayFactor2)
	if math.Abs(rac-expected2) > 1e-6 {
		t.Errorf("After grant 3: rac = %v, want %v", rac, expected2)
	}
}

func TestCalculateRAC_ZeroElapsed(t *testing.T) {
	// Zero elapsed time: RAC = previous + new credit.
	rac := CalculateRAC(50.0, 0, 10.0)
	if math.Abs(rac-60.0) > 1e-9 {
		t.Errorf("CalculateRAC(50, 0, 10) = %v, want 60.0", rac)
	}
}

func TestCalculateRAC_SubSecondElapsed(t *testing.T) {
	// Sub-second elapsed time: treated as zero (add credit directly).
	rac := CalculateRAC(50.0, 0.5, 10.0)
	if math.Abs(rac-60.0) > 1e-9 {
		t.Errorf("CalculateRAC(50, 0.5, 10) = %v, want 60.0", rac)
	}
}

func TestCalculateRAC_NegativeElapsed(t *testing.T) {
	// Negative elapsed time (shouldn't happen but handled gracefully).
	rac := CalculateRAC(50.0, -100, 10.0)
	if math.Abs(rac-60.0) > 1e-9 {
		t.Errorf("CalculateRAC(50, -100, 10) = %v, want 60.0", rac)
	}
}
