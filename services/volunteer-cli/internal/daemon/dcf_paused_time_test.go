package daemon

import (
	"testing"
	"time"
)

// TB-18: the duration correction factor was updated with the raw wall clock,
// which includes any period a unit spent suspended — frozen by a thermal
// throttle, the resource monitor, or a schedule window closing.
//
// The factor scales every future estimate for that leaf, ramps up at 80/20
// while decaying at only 10% per unit, and is persisted to dcf.json, so one
// suspended unit throttled the volunteer's own work intake for days across
// restarts. The observed unit reported 10212 s of wall clock for roughly 1400 s
// of computation after a 2 h 28 min freeze, against a 146–2821 s normal range.

func TestActiveSeconds_ExcludesSuspendedTime(t *testing.T) {
	// The field case: a container unit that computed for ~22 min across a
	// 2 h 27 min 50 s thermal freeze, reported as 10212 s of wall clock.
	const wallClock = 10212
	const frozen = 8870 * time.Second

	got := activeSeconds(wallClock, frozen)
	if want := int64(1342); got != want {
		t.Fatalf("active seconds = %d, want %d", got, want)
	}
	if got >= wallClock {
		t.Errorf("active seconds %d not reduced below the %d s wall clock; suspended time is being counted as computation", got, wallClock)
	}
}

func TestActiveSeconds_NeverNegative(t *testing.T) {
	// Pause accounting is sampled independently of the runtime's wall clock, so
	// a rounding disagreement must not produce a negative duration that would be
	// fed to the tracker as a nonsensical ratio.
	if got := activeSeconds(10, 30*time.Second); got != 0 {
		t.Errorf("active seconds = %d, want 0", got)
	}
}

// The consequence the volunteer actually feels: a suspended unit must not move
// the leaf's duration factor far from where an unsuspended one leaves it.
func TestDCF_SuspendedUnitDoesNotPoisonTheFactor(t *testing.T) {
	const estimated = 1400.0
	const wallClock = 10212
	const frozen = 8870 * time.Second

	poisoned := LoadDCFTracker(t.TempDir())
	poisoned.Update("leaf", estimated, float64(wallClock)) // pre-fix: raw wall clock

	corrected := LoadDCFTracker(t.TempDir())
	corrected.Update("leaf", estimated, float64(activeSeconds(wallClock, frozen)))

	gotPoisoned := poisoned.Get("leaf")
	gotCorrected := corrected.Get("leaf")

	// The raw wall clock is a ~7.3x ratio, which the 80/20 ramp turns into a
	// factor near 6 in a single step.
	if gotPoisoned < 5 {
		t.Fatalf("sanity: raw wall clock gave factor %.2f, expected it to blow past 5", gotPoisoned)
	}
	// The corrected factor must stay close to 1 — the unit did roughly what the
	// estimate said, once the freeze is excluded.
	if gotCorrected > 1.2 {
		t.Errorf("factor after the fix = %.2f, want <= 1.2; the client would ask for ~%.0f%% of its normal batch",
			gotCorrected, 100/gotCorrected)
	}
	if gotCorrected >= gotPoisoned {
		t.Errorf("factor %.2f is no better than the pre-fix %.2f", gotCorrected, gotPoisoned)
	}
}

// The ramp is asymmetric and persisted, which is why a single bad sample was
// expensive: it takes many good units to work back out. Pinned so the cost of
// getting this wrong stays visible.
func TestDCF_RampIsSlowToRecover(t *testing.T) {
	tr := LoadDCFTracker(t.TempDir())
	tr.Update("leaf", 1400, 10212) // one suspended unit, pre-fix accounting

	units := 0
	for tr.Get("leaf") > 1.2 && units < 100 {
		tr.Update("leaf", 1400, 1400) // a perfectly-estimated unit
		units++
	}
	if units < 10 {
		t.Fatalf("recovered in %d units; the 10%%-per-unit decay should need far more", units)
	}
	t.Logf("one suspended unit costs %d subsequent normal units to decay out", units)
}
