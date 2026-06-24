package management

import "testing"

func TestETA_NoProgress_UsesStaticEstimate(t *testing.T) {
	e := newETATracker()
	// No live progress, but a 1000s benchmark estimate with 200s of run time.
	got, ok := e.estimate("wu", 0, 200, 1000)
	if !ok {
		t.Fatal("expected an estimate from the static benchmark")
	}
	if got != 800 {
		t.Errorf("remaining = %d, want 800 (1000 - 200)", got)
	}
}

func TestETA_NoProgress_NoStatic_NoEstimate(t *testing.T) {
	e := newETATracker()
	if _, ok := e.estimate("wu", 0, 200, 0); ok {
		t.Error("expected no estimate with neither progress nor benchmark")
	}
}

func TestETA_StaticOverElapsed_ClampsToZero(t *testing.T) {
	e := newETATracker()
	got, ok := e.estimate("wu", 0, 1500, 1000) // overran the estimate
	if !ok || got != 0 {
		t.Errorf("remaining = %d (ok=%v), want 0", got, ok)
	}
}

// TestETA_BlendTamesSlowStart is the core of the fix: a task that has done only 1%
// after 100s would extrapolate to ~9900s remaining under naive elapsed/progress, but
// blending with the benchmark estimate (weighted by fraction done) keeps the first
// number close to the benchmark instead of spiking.
func TestETA_BlendTamesSlowStart(t *testing.T) {
	e := newETATracker()
	const static = 1000.0

	naive := 100.0 / 1.0 * 99.0 // 9900s
	first, ok := e.estimate("wu", 1, 100, static)
	if !ok {
		t.Fatal("expected an estimate")
	}
	if float64(first) >= naive/5 {
		t.Errorf("first estimate = %d, want far below the naive %.0f (blend should tame it)", first, naive)
	}
	// As progress accrues at a steady rate, the estimate should decrease, not lurch.
	second, _ := e.estimate("wu", 11, 110, static)
	third, _ := e.estimate("wu", 21, 120, static)
	if !(first > second && second > third) {
		t.Errorf("estimate did not converge downward: %d, %d, %d", first, second, third)
	}
	if third <= 0 {
		t.Errorf("third estimate = %d, want > 0 with 79%% remaining", third)
	}
}

// TestETA_SmoothedRateIgnoresOneSlowSample checks that a single stalled observation
// (no progress between two reads) does not blow up the estimate the way an
// instantaneous rate of ~0 would.
func TestETA_SmoothedRateIgnoresStall(t *testing.T) {
	e := newETATracker()
	const static = 1000.0
	// Establish a steady 1%/s rate.
	e.estimate("wu", 10, 10, static)
	e.estimate("wu", 20, 20, static)
	steady, _ := e.estimate("wu", 30, 30, static)
	// A stall: progress unchanged while elapsed advances. Rate sample is skipped, so
	// the smoothed rate (and thus the estimate) should not explode.
	stalled, _ := e.estimate("wu", 30, 40, static)
	if stalled > steady*3 {
		t.Errorf("estimate exploded on a single stall: steady=%d stalled=%d", steady, stalled)
	}
}

func TestETA_Retain(t *testing.T) {
	e := newETATracker()
	e.estimate("keep", 5, 10, 1000)
	e.estimate("drop", 5, 10, 1000)
	e.retain(map[string]bool{"keep": true})
	if _, ok := e.samples["drop"]; ok {
		t.Error("retain should have dropped inactive 'drop'")
	}
	if _, ok := e.samples["keep"]; !ok {
		t.Error("retain should have kept active 'keep'")
	}
}
