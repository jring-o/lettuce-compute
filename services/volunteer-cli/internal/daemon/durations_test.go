package daemon

import (
	"math"
	"testing"
	"time"
)

// TB-18: the duration estimate was updated with the raw wall clock, which
// includes any period a unit spent suspended — frozen by a thermal throttle,
// the resource monitor, or a schedule window closing.
//
// The figure scales every future estimate for that leaf and is persisted, so
// one suspended unit throttled the volunteer's own work intake for days across
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
	// fed to the tracker as a nonsensical figure.
	if got := activeSeconds(10, 30*time.Second); got != 0 {
		t.Errorf("active seconds = %d, want 0", got)
	}
}

// The consequence the volunteer actually feels: a suspended unit must not move
// the leaf's estimate far from where an unsuspended one leaves it.
func TestDurations_SuspendedUnitDoesNotPoisonTheEstimate(t *testing.T) {
	const fpops = 1400.0 // at a benchmark of 1 FP-op/s the raw figure reads as seconds
	const wallClock = 10212
	const frozen = 8870 * time.Second

	poisoned := LoadDurationTracker(t.TempDir())
	poisoned.Record("leaf", fpops, float64(wallClock)) // pre-fix: raw wall clock

	corrected := LoadDurationTracker(t.TempDir())
	corrected.Record("leaf", fpops, float64(activeSeconds(wallClock, frozen)))

	gotPoisoned, _ := poisoned.UnitSeconds("leaf")
	gotCorrected, _ := corrected.UnitSeconds("leaf")

	if gotPoisoned < 5*fpops {
		t.Fatalf("sanity: raw wall clock gave %.0f s, expected it to blow past %.0f", gotPoisoned, 5*fpops)
	}
	// The corrected figure must stay close to the ~1400 s the unit really computed.
	if gotCorrected > 1.2*fpops {
		t.Errorf("estimate after the fix = %.0f s, want <= %.0f; the client would ask for ~%.0f%% of its normal batch",
			gotCorrected, 1.2*fpops, 100*fpops/gotCorrected)
	}
	if gotCorrected >= gotPoisoned {
		t.Errorf("estimate %.0f is no better than the pre-fix %.0f", gotCorrected, gotPoisoned)
	}
}

// One bad sample was expensive under the old correction factor (an 80/20 ramp
// up, a 10 %-per-unit decay back: dozens of units to work out). The median
// bounds the cost: a bad first sample is outvoted by the next two completions,
// and a bad sample among good ones never moves the figure (TB-58).
func TestDurations_OneBadSampleIsOutvotedWithinTwoUnits(t *testing.T) {
	tr := LoadDurationTracker(t.TempDir())
	tr.Record("leaf", 1400, 10212) // one suspended unit, pre-fix accounting

	units := 0
	for {
		got, _ := tr.UnitSeconds("leaf")
		if got <= 1.2*1400 || units >= 100 {
			break
		}
		tr.Record("leaf", 1400, 1400) // a perfectly-estimated unit
		units++
	}
	if units > 2 {
		t.Fatalf("recovered only after %d units; the median should need at most two", units)
	}
	t.Logf("one suspended unit costs %d subsequent normal units to outvote", units)
}

func TestDurations_PersistAcrossLoad(t *testing.T) {
	dir := t.TempDir()
	tr := LoadDurationTracker(dir)
	for i := 0; i < 7; i++ {
		tr.Record("leaf", 100, float64(10+i))
	}
	tr.Record("other", 0, 42)    // no FP-ops figure: counts for unit seconds only
	tr.Record("ignored", 100, 0) // a non-positive duration is not a sample

	re := LoadDurationTracker(dir)
	if got := re.Completions("leaf"); got != durationSampleWindow {
		t.Errorf("held %d completions after reload, want the window of %d", got, durationSampleWindow)
	}
	// The last five samples are 12..16 s, median 14.
	if got, ok := re.UnitSeconds("leaf"); !ok || got != 14 {
		t.Errorf("UnitSeconds after reload = %g (ok=%v), want 14", got, ok)
	}
	if got, ok := re.SecondsPerFpop("leaf"); !ok || math.Abs(got-0.14) > 1e-9 {
		t.Errorf("SecondsPerFpop after reload = %g (ok=%v), want 0.14", got, ok)
	}
	if _, ok := re.SecondsPerFpop("other"); ok {
		t.Error("a sample with no FP-ops figure must not yield a seconds-per-FP-op rate")
	}
	if got, ok := re.UnitSeconds("other"); !ok || got != 42 {
		t.Errorf("UnitSeconds for the FP-ops-less leaf = %g (ok=%v), want 42", got, ok)
	}
	if got := re.Completions("ignored"); got != 0 {
		t.Errorf("a zero-duration record was kept (%d samples), want none", got)
	}
	if _, ok := re.UnitSeconds("never-seen"); ok {
		t.Error("an unknown leaf must report no estimate")
	}
}

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
		ok   bool
	}{
		{nil, 0, false},
		{[]float64{7}, 7, true},
		{[]float64{3, 1}, 2, true},
		{[]float64{9, 1, 5}, 5, true},
		{[]float64{4, 1, 3, 2}, 2.5, true},
	}
	for _, c := range cases {
		got, ok := median(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("median(%v) = %g, %v; want %g, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
