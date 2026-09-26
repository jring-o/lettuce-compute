package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-84 regression tests: the hours-based work buffer was inert for any unit
// that carried no FP-ops estimate — which every unit on both fleet heads is —
// because estSecondsForUnit returned 0 before consulting the learned medians
// (TB-58) or the head's leaf-level figure. Every host therefore held the
// unit-count fallback (two per slot) whatever work_buffer_hours said, while the
// ask, sized by leafEstSeconds (which did read the medians), was hours-sized:
// one round handed out five, kept two and returned three, and the batch-feedback
// cap then pinned every later round at "keep one, return one" (306 give-backs
// beside 291 completions in 48 h on one host). Four fixes are pinned here: the
// per-unit estimate's fallback chain, the count fallback binding only when
// nothing is known, honest give-back wording for the count case, and a
// no-estimate ask bounded by the count's headroom.

// tb84Tracker returns a tracker holding five completions of leaf-1 at 1400 s,
// none carrying an FP-ops figure — the fleet's durations.json on every host.
func tb84Tracker(t *testing.T) *DurationTracker {
	t.Helper()
	tr := LoadDurationTracker(t.TempDir())
	for i := 0; i < 5; i++ {
		tr.Record("leaf-1", 0, 1400)
	}
	return tr
}

// TestTB84_NoFpopsUnitsBookTheLearnedMedian is the filed reproduction: a 2 h ×
// 1-slot buffer whose units bring no FP-ops figure must fill by HOURS from the
// leaf's learned median (5 × 1400 s under a 7200 s target), not stop at two
// units.
func TestTB84_NoFpopsUnitsBookTheLearnedMedian(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 1, 1e9)
	d.durations = tb84Tracker(t)

	if got := d.estSecondsForUnit("leaf-1", 0); got != 1400 {
		t.Fatalf("estSecondsForUnit(no fpops) = %g, want 1400 (the learned median)", got)
	}
	for i := 1; i <= 2; i++ {
		d.prefetchQueue.Push(bufItem(fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i), 0))
	}
	if got := d.bufferedSeconds(); got != 2800 {
		t.Errorf("bufferedSeconds after two no-fpops units = %g, want 2800", got)
	}
	if d.workBufferHoursFull() {
		t.Fatal("two units (2800 s) reported full against a 7200 s target — the unit-count fallback bound (TB-84)")
	}
	if ok, why := d.bufferAccepts(&runtime.WorkUnit{ID: "00000000-0000-4000-8000-000000000003", LeafID: "leaf-1"}); !ok {
		t.Fatalf("third no-fpops unit refused: %q (TB-84)", why)
	}
	for i := 3; i <= 5; i++ {
		d.prefetchQueue.Push(bufItem(fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i), 0))
	}
	if d.workBufferHoursFull() {
		t.Error("five units (7000 s) reported full against a 7200 s target")
	}
	d.prefetchQueue.Push(bufItem("00000000-0000-4000-8000-000000000006", 0))
	if !d.workBufferHoursFull() {
		t.Error("six units (8400 s) not reported full against a 7200 s target")
	}
}

// TestTB84_HeadEstimateCarriesAnUncompletedLeaf: before a leaf's first
// completion here, a no-fpops unit is booked at the head's leaf-level estimate,
// so a fresh host on a head that publishes one sizes its buffer by hours from
// the start; and the rest of the fallback order holds.
func TestTB84_HeadEstimateCarriesAnUncompletedLeaf(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 1, 1e9)
	d.durations = LoadDurationTracker(t.TempDir())
	d.leafCache = NewLeafCache(0, d.logger)
	d.leafCache.PopulateForTest("head-a", &CachedHeadInfo{Name: "head-a", Leafs: []CachedLeafInfo{
		{ID: "leaf-1", Slug: "leaf-1", State: "ACTIVE", EstimatedDurationSeconds: 900},
	}})
	if got := d.estSecondsForUnit("leaf-1", 0); got != 900 {
		t.Errorf("estSecondsForUnit(no fpops, no completions) = %g, want 900 (the head's leaf-level estimate)", got)
	}
	// A per-unit FP-ops figure against the benchmark outranks the head's
	// leaf-level figure (TB-34 found the latter can sit 60× below the truth).
	if got := d.estSecondsForUnit("leaf-1", 2e12); got != 2000 {
		t.Errorf("estSecondsForUnit(fpops, benchmark) = %g, want 2000 (fpops ÷ benchmark before the head's figure)", got)
	}
	// Once the leaf has completed here, the median outranks both.
	d.durations.Record("leaf-1", 0, 1400)
	if got := d.estSecondsForUnit("leaf-1", 0); got != 1400 {
		t.Errorf("estSecondsForUnit after a completion = %g, want 1400 (the median)", got)
	}
	// A leaf nobody knows anything about: only then is the estimate 0.
	if got := d.estSecondsForUnit("leaf-unknown", 0); got != 0 {
		t.Errorf("estSecondsForUnit with nothing known = %g, want 0", got)
	}
}

// TestTB84_CountFallbackSaysSo: when the unit-count fallback IS the bound — no
// completion, no head figure, no FP-ops — the give-back reason names it. The
// old wording, "over the hours target", landed in the head's outcome_reason of
// 304 give-backs whose hours target had never been consulted.
func TestTB84_CountFallbackSaysSo(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 1, 0) // nothing known: no tracker, no cache, no benchmark
	d.prefetchQueue.Push(bufItem("00000000-0000-4000-8000-000000000001", 0))
	if full, why := d.workBufferFullVerdict(); full || why != "" {
		t.Fatalf("one unknown unit on one slot: full=%v reason=%q, want not full with no reason", full, why)
	}
	d.prefetchQueue.Push(bufItem("00000000-0000-4000-8000-000000000002", 0))
	full, why := d.workBufferFullVerdict()
	if !full {
		t.Fatal("two unknown units on one slot must still hit the unit-count fallback")
	}
	if !strings.Contains(why, "unit-count fallback") || !strings.Contains(why, "2 of 2") || strings.Contains(why, "hours target") {
		t.Errorf("count-fallback reason = %q, want it to name the unit-count fallback (2 of 2), not the hours target", why)
	}
	// The give-back reason bufferAccepts hands the head is that verdict.
	ok, reason := d.bufferAccepts(&runtime.WorkUnit{ID: "00000000-0000-4000-8000-000000000003", LeafID: "leaf-1"})
	if ok {
		t.Fatal("third unknown unit accepted past the unit-count fallback")
	}
	if reason != why {
		t.Errorf("bufferAccepts reason = %q, want the fullness verdict %q", reason, why)
	}
	// The hours wording survives for the bound it describes.
	d1 := newBufferTestDaemon(t, 1.0, 1, 1.0) // 3600 s target; benchmark 1 ⇒ seconds == fpops
	d1.prefetchQueue.Push(bufItem("00000000-0000-4000-8000-000000000011", 3600))
	if full, why := d1.workBufferFullVerdict(); !full || why != "work buffer full (over the hours target)" {
		t.Errorf("hours-bound verdict: full=%v reason=%q", full, why)
	}
}

// TestTB84_NoEstimateAskIsBoundedByTheCountHeadroom: with no estimate for the
// leaf or anything held, the count fallback will bound acceptance, so the ask
// must be its headroom — not a full 64-unit batch of which two are kept.
func TestTB84_NoEstimateAskIsBoundedByTheCountHeadroom(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 2, 0) // 2 slots ⇒ fallback 4 units
	if got := d.requestBatchSize(CachedLeafInfo{ID: "leaf-1"}, 0); got != 4 {
		t.Errorf("no-estimate ask on an empty 2-slot buffer = %d, want 4 (the count headroom)", got)
	}
	d.prefetchQueue.Push(bufItem("00000000-0000-4000-8000-000000000001", 0))
	if got := d.requestBatchSize(CachedLeafInfo{ID: "leaf-1"}, 0); got != 3 {
		t.Errorf("no-estimate ask with one unknown unit held = %d, want 3", got)
	}
	// Once something is known the hours arithmetic sizes the ask as before.
	d.durations = tb84Tracker(t)
	if got := d.requestBatchSize(CachedLeafInfo{ID: "leaf-1"}, d.leafEstSeconds(CachedLeafInfo{ID: "leaf-1"})); got != 9 {
		t.Errorf("ask with a 1400 s median against a 14400 s target holding one unit = %d, want 9 ((14400 - 1400) / 1400)", got)
	}
}

// TestTB84_FleetRoundKeepsWhatItAsksFor drives one fetch round through the real
// daemon hooks against a head handing out exactly what is asked — the fleet's
// shape: units with no FP-ops figure, a leaf that has completed here five
// times at 1400 s, a 2 h × 1-slot buffer. The round must ask for the hours
// deficit (5 units) and KEEP all five with no give-back. Before the fix the
// same round asked for five, kept two, and returned three.
func TestTB84_FleetRoundKeepsWhatItAsksFor(t *testing.T) {
	var askedMax []int32
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			askedMax = append(askedMax, req.MaxAssignments)
			asgs := make([]*lettucev1.WorkUnitAssignment, req.MaxAssignments)
			for i := range asgs {
				asgs[i] = &lettucev1.WorkUnitAssignment{
					WorkUnitId:    fmt.Sprintf("00000000-0000-4000-8000-0000000%02d%03d", len(askedMax), i),
					LeafId:        "leaf-1",
					Runtime:       "native",
					InputData:     []byte("input"),
					ExecutionSpec: &lettucev1.ExecutionSpec{},
				}
			}
			return &lettucev1.RequestWorkUnitResponse{Assignments: asgs}, nil
		},
	}
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	d := newFetcherTestDaemon(servers)
	d.cfg.WorkBufferHours = 2
	d.cfg.MaxConcurrentTasks = 1
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)
	d.slotManager = NewSlotManager(1, d.logger)
	d.durations = tb84Tracker(t)
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)

	leaf := CachedLeafInfo{ID: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"}
	pushed, stop := f.requestAndBuffer(context.Background(), servers[0], leaf, []string{leaf.ID}, nil, 1)
	if stop {
		t.Fatal("round stopped")
	}
	if len(askedMax) != 1 || askedMax[0] != 5 {
		t.Fatalf("asked %v, want one ask of 5 (7200 s / 1400 s median)", askedMax)
	}
	if pushed != 5 {
		t.Errorf("kept %d of 5, want all 5 (TB-84: the count fallback kept two and returned three)", pushed)
	}
	if n := mc.getAbandonCalls(); n != 0 {
		t.Errorf("%d units given back, want 0 (TB-84: one round, one give-back, forever)", n)
	}
	if got := d.bufferedSeconds(); got != 7000 {
		t.Errorf("bufferedSeconds after the round = %g, want 7000", got)
	}
	if _, capped := f.batchCap[batchCapKey("server-a", "leaf-1")]; capped {
		t.Error("batch-feedback cap set after a fully-kept round")
	}
}
