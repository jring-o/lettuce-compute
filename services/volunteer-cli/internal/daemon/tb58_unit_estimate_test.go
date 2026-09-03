package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TB-58: a unit's expected duration was rsc_fpops_est ÷ a three-second
// single-threaded CPU benchmark, corrected by a per-leaf factor that decayed
// only 10 % per completion when units ran faster than that yardstick. Every
// GPU unit and every multi-core container does, so their estimates stayed
// hours too high for dozens of completions: the work buffer booked each unit
// at the inflated figure and latched full at two or three units ("2-hour
// buffer, ran out in 40 minutes"), and the app's remaining-time figure blended
// toward it ("80 % done, 3h14 left"). The filing's numbers: with the benchmark
// saying 3600 s and units taking 1500 s, the estimate read 3390 s after one
// completion and still 1755 s after twenty.

// newEstimateTestDaemon is a one-slot buffer-test daemon whose benchmark makes
// the raw FP-ops figure read as seconds (1 FP-op/s), with a fresh tracker.
func newEstimateTestDaemon(t *testing.T, hours float64) *Daemon {
	t.Helper()
	d := newBufferTestDaemon(t, hours, 1, 1.0)
	d.durations = LoadDurationTracker(t.TempDir())
	return d
}

func TestTB58_EstimateFollowsTheFirstCompletion(t *testing.T) {
	d := newEstimateTestDaemon(t, 2.0)
	const benchmarkSays, unitTakes = 3600.0, 1500.0

	if got := d.estSecondsForUnit("leaf", benchmarkSays); got != benchmarkSays {
		t.Fatalf("before any completion the benchmark figure should stand: got %g, want %g", got, benchmarkSays)
	}
	d.durations.Record("leaf", benchmarkSays, unitTakes)
	if got := d.estSecondsForUnit("leaf", benchmarkSays); got < unitTakes*0.99 || got > unitTakes*1.01 {
		t.Errorf("after one completion the estimate = %.0f s, want %.0f (pre-fix: 3390, a 10 %% step toward the truth)", got, unitTakes)
	}
	for i := 0; i < 19; i++ {
		d.durations.Record("leaf", benchmarkSays, unitTakes)
	}
	if got := d.estSecondsForUnit("leaf", benchmarkSays); got < unitTakes*0.99 || got > unitTakes*1.01 {
		t.Errorf("after twenty completions the estimate = %.0f s, want %.0f (pre-fix: 1755)", got, unitTakes)
	}
}

// The over-run direction still corrects in one completion, as before.
func TestTB58_EstimateFollowsAnOverrunToo(t *testing.T) {
	d := newEstimateTestDaemon(t, 2.0)
	d.durations.Record("leaf", 1500, 3600)
	if got := d.estSecondsForUnit("leaf", 1500); got != 3600 {
		t.Errorf("after one over-running completion the estimate = %g, want 3600", got)
	}
}

// The learned rate scales with each unit's own FP-ops figure, so a leaf whose
// units differ in size is still estimated per unit.
func TestTB58_LearnedRateScalesWithUnitSize(t *testing.T) {
	d := newEstimateTestDaemon(t, 2.0)
	d.durations.Record("leaf", 1000, 250) // 0.25 s per FP-op here
	if got := d.estSecondsForUnit("leaf", 4000); got != 1000 {
		t.Errorf("estimate for a 4000 FP-op unit = %g, want 1000 (4000 × 0.25)", got)
	}
}

// A learned leaf no longer needs the benchmark at all: the estimate is
// available on a host whose benchmark is missing.
func TestTB58_LearnedEstimateNeedsNoBenchmark(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 1, 0) // un-benchmarked
	d.durations = LoadDurationTracker(t.TempDir())
	if got := d.estSecondsForUnit("leaf", 3600); got != 0 {
		t.Fatalf("un-benchmarked, un-learned: estimate = %g, want 0 (unknown)", got)
	}
	d.durations.Record("leaf", 3600, 1500)
	if got := d.estSecondsForUnit("leaf", 3600); got != 1500 {
		t.Errorf("un-benchmarked but learned: estimate = %g, want 1500", got)
	}
}

// The tester's buffer: "2 hr buffer, only 2 units including the active one, at
// 20–30 min per unit". Booked at the inflated figure, three 25-minute units
// filled a two-hour target; booked at what they actually take, four do not.
func TestTB58_BufferBooksUnitsAtTheirLearnedDuration(t *testing.T) {
	d := newEstimateTestDaemon(t, 2.0) // 7200 s target, one slot
	const benchmarkSays, unitTakes = 3600.0, 1500.0
	d.durations.Record("leaf-1", benchmarkSays, unitTakes)

	for i := 1; i <= 4; i++ {
		d.prefetchQueue.Push(bufItem(fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i), benchmarkSays))
	}
	if d.workBufferFull() {
		t.Errorf("four 1500 s units (6000 s) against a 7200 s target read as full: booked %.0f s (pre-fix booked 3390 s each, full at three)", d.bufferedSeconds())
	}
	d.prefetchQueue.Push(bufItem("00000000-0000-4000-8000-000000000005", benchmarkSays))
	if !d.workBufferFull() {
		t.Errorf("five 1500 s units (7500 s) against a 7200 s target should be full: booked %.0f s", d.bufferedSeconds())
	}
}

// Sizing the first request: the leaf-level figure prefers this machine's own
// completions over the head's guess, the arrival booking is re-derived under
// the learned rate rather than frozen at what the benchmark implied when the
// unit arrived, and a restarted daemon sizes its first ask from the same record.
func TestTB58_FirstRequestSizedFromThisMachinesCompletions(t *testing.T) {
	d := newEstimateTestDaemon(t, 2.0)
	leaf := CachedLeafInfo{ID: "leaf", EstimatedDurationSeconds: 600}
	const benchmarkSays, unitTakes = 3600.0, 1500.0

	d.noteArrivalEstimate("leaf", benchmarkSays) // a benchmark-era arrival
	if got := d.leafEstSeconds(leaf); got != benchmarkSays {
		t.Fatalf("before any completion: leafEstSeconds = %g, want %g (the arrival booking under the benchmark)", got, benchmarkSays)
	}
	d.durations.Record("leaf", benchmarkSays, unitTakes)
	if got := d.leafEstSeconds(leaf); got != unitTakes {
		t.Errorf("after one completion: leafEstSeconds = %g, want %g (this machine's median; the arrival booking must not stay at the benchmark's %g)", got, unitTakes, benchmarkSays)
	}
	d2 := newBufferTestDaemon(t, 2.0, 1, 1.0)
	d2.durations = LoadDurationTracker(d.durations.dataDir)
	if got := d2.leafEstSeconds(leaf); got != unitTakes {
		t.Errorf("after a restart: leafEstSeconds = %g, want %g", got, unitTakes)
	}
}

// The learned figure is a median over the last five completions: one slow
// unit (competing load) does not move it, and a change in the machine's real
// speed shows within three.
func TestTB58_MedianIgnoresOneOutlierAndFollowsARealChange(t *testing.T) {
	d := newEstimateTestDaemon(t, 2.0)
	for i := 0; i < 3; i++ {
		d.durations.Record("leaf", 3600, 1500)
	}
	d.durations.Record("leaf", 3600, 10212) // one unit under heavy competing load
	if got := d.estSecondsForUnit("leaf", 3600); got != 1500 {
		t.Errorf("one outlier moved the estimate to %g, want 1500", got)
	}
	for i := 0; i < 3; i++ {
		d.durations.Record("leaf", 3600, 6000) // the machine really is slower now
	}
	if got := d.estSecondsForUnit("leaf", 3600); got != 6000 {
		t.Errorf("three slower completions should carry the median: got %g, want 6000", got)
	}
}

func TestTB58_LegacyFactorFileIsRemovedOnLoad(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "dcf.json")
	if err := os.WriteFile(legacy, []byte(`{"leaf": 0.5}`), 0600); err != nil {
		t.Fatal(err)
	}
	LoadDurationTracker(dir)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("dcf.json still present after load (stat err=%v); nothing reads it any more", err)
	}
}
