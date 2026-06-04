package daemon

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// newBufferTestDaemon builds a minimal Daemon with a real prefetch queue and slot
// manager so the hours-based buffer helpers can be exercised directly.
func newBufferTestDaemon(t *testing.T, hours float64, maxSlots int, benchFPOPS float64) *Daemon {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.WorkBufferHours = hours
	cfg.MaxConcurrentTasks = maxSlots
	return &Daemon{
		cfg:            cfg,
		logger:         logger,
		benchmarkFPOPS: benchFPOPS,
		prefetchQueue:  NewPreFetchQueue(workBufferQueueDepth, logger),
		slotManager:    NewSlotManager(maxSlots, logger),
	}
}

func bufItem(id string, fpops float64) *PreFetchItem {
	return &PreFetchItem{WU: &runtime.WorkUnit{ID: id, LeafID: "leaf-1", RscFpopsEst: fpops}}
}

func TestBufferTargetSeconds(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 3, 1e9)
	// 2 hours * 3600 * 3 slots = 21600s.
	if got := d.bufferTargetSeconds(); got != 21600 {
		t.Errorf("bufferTargetSeconds = %g, want 21600", got)
	}

	// hours == 0 disables the hours target.
	d0 := newBufferTestDaemon(t, 0, 1, 1e9)
	if got := d0.bufferTargetSeconds(); got != 0 {
		t.Errorf("bufferTargetSeconds with hours=0 = %g, want 0", got)
	}
}

func TestWorkBufferFull_HoursBased(t *testing.T) {
	// 1 hour * 3600 * 1 slot = 3600s target. Benchmark 1 fpops => est seconds ==
	// RscFpopsEst, so two 1800-fpops units (3600s) exactly fill the buffer.
	d := newBufferTestDaemon(t, 1.0, 1, 1.0)
	if d.workBufferFull() {
		t.Fatal("empty buffer should not be full")
	}
	d.prefetchQueue.Push(bufItem("00000000-0000-4000-8000-000000000001", 1800))
	if d.workBufferFull() {
		t.Error("1800s buffered against a 3600s target should not be full")
	}
	d.prefetchQueue.Push(bufItem("00000000-0000-4000-8000-000000000002", 1800))
	if !d.workBufferFull() {
		t.Error("3600s buffered against a 3600s target should be full")
	}
}

func TestWorkBufferFull_UnitCountFallback(t *testing.T) {
	// No benchmark => no per-unit time estimate => unit-count fallback
	// (fallbackBufferUnitsPerSlot * maxSlots). maxSlots=1 => fallback 2.
	d := newBufferTestDaemon(t, 2.0, 1, 0)
	if d.workBufferFull() {
		t.Fatal("empty buffer should not be full")
	}
	d.prefetchQueue.Push(bufItem("00000000-0000-4000-8000-000000000001", 0))
	if d.workBufferFull() {
		t.Error("1 unit < fallback (2) should not be full")
	}
	d.prefetchQueue.Push(bufItem("00000000-0000-4000-8000-000000000002", 0))
	if !d.workBufferFull() {
		t.Error("2 units >= fallback (2) should be full under the unit-count fallback")
	}
}

func TestRequestBatchSize(t *testing.T) {
	d := newBufferTestDaemon(t, 1.0, 1, 1.0) // target 3600s
	// With a 600s/unit estimate and an empty buffer: 3600/600 = 6, capped at 8.
	if got := d.requestBatchSize(600); got != 6 {
		t.Errorf("requestBatchSize(600) = %d, want 6", got)
	}
	// A tiny per-unit estimate is capped at maxBatchPerRequest.
	if got := d.requestBatchSize(1); got != maxBatchPerRequest {
		t.Errorf("requestBatchSize(1) = %d, want %d (cap)", got, maxBatchPerRequest)
	}
	// No estimate and an empty buffer => full batch to refill quickly.
	if got := d.requestBatchSize(0); got != maxBatchPerRequest {
		t.Errorf("requestBatchSize(0) on empty buffer = %d, want %d", got, maxBatchPerRequest)
	}
	// A per-unit estimate larger than the whole deficit yields a single unit.
	if got := d.requestBatchSize(100000); got != 1 {
		t.Errorf("requestBatchSize(huge) = %d, want 1", got)
	}
}

func TestRequestBatchSize_BufferingDisabled(t *testing.T) {
	d := newBufferTestDaemon(t, 0, 1, 1.0) // hours == 0 disables hours target
	if got := d.requestBatchSize(600); got != 1 {
		t.Errorf("requestBatchSize with buffering disabled = %d, want 1", got)
	}
}

// TestLeafEstSeconds_BenchmarkIndependent is the #29 core regression: the leaf-
// level seconds estimate sizes the FIRST request to a leaf and must stay non-zero
// even when the host has NO CPU benchmark (benchmarkFPOPS == 0), the exact case
// the old FP-ops-only seam tripped to 0.
func TestLeafEstSeconds_BenchmarkIndependent(t *testing.T) {
	// benchmarkFPOPS = 0 (un-benchmarked host).
	d := newBufferTestDaemon(t, 2.0, 1, 0)
	leaf := CachedLeafInfo{ID: "leaf-1", EstimatedDurationSeconds: 30}
	if got := d.leafEstSeconds(leaf); got != 30 {
		t.Errorf("leafEstSeconds on un-benchmarked host = %g, want 30 (leaf-level estimate)", got)
	}
	// A leaf with no estimate yields 0 (caller falls back).
	if got := d.leafEstSeconds(CachedLeafInfo{ID: "leaf-2"}); got != 0 {
		t.Errorf("leafEstSeconds with no leaf estimate = %g, want 0", got)
	}
}

// TestLeafEstSeconds_AppliesDCF verifies the learned duration-correction factor
// refines the leaf-level estimate when one has been learned for that leaf.
func TestLeafEstSeconds_AppliesDCF(t *testing.T) {
	d := newBufferTestDaemon(t, 2.0, 1, 0)
	d.dcfTracker = LoadDCFTracker(t.TempDir())
	// Learn a DCF > 1: actual was twice the estimate.
	d.dcfTracker.Update("leaf-1", 10, 20)
	leaf := CachedLeafInfo{ID: "leaf-1", EstimatedDurationSeconds: 30}
	got := d.leafEstSeconds(leaf)
	if got <= 30 {
		t.Errorf("leafEstSeconds with DCF>1 = %g, want > 30 (refined upward)", got)
	}
}

// TestRequestBatchSize_ShortUnitLeafFillsPastEight is the #29 DoD: a short-unit
// leaf must fill work_buffer_hours in ONE request with a batch well above the old
// flat cap of 8, now that maxBatchPerRequest is a safety ceiling (64), not the
// primary limiter.
func TestRequestBatchSize_ShortUnitLeafFillsPastEight(t *testing.T) {
	// 1 hour * 3600 * 1 slot = 3600s target, empty buffer.
	d := newBufferTestDaemon(t, 1.0, 1, 1.0)
	// 30s/unit short units: 3600/30 = 120 desired, clamped to the 64 ceiling.
	got := d.requestBatchSize(30)
	if got <= 8 {
		t.Errorf("short-unit batch = %d, want > 8 (old flat cap should no longer bind)", got)
	}
	if got != maxBatchPerRequest {
		t.Errorf("short-unit batch = %d, want %d (deficit exceeds ceiling)", got, maxBatchPerRequest)
	}
	// A 60s/unit leaf: 3600/60 = 60, under the 64 ceiling, so the deficit math binds.
	if got := d.requestBatchSize(60); got != 60 {
		t.Errorf("60s-unit batch = %d, want 60 (deficit math, not ceiling)", got)
	}
}

// leaseItem builds a buffered item with a reservation lease expiry (unix seconds).
func leaseItem(id string, reservedUntilUnix int64) *PreFetchItem {
	return &PreFetchItem{WU: &runtime.WorkUnit{ID: id, LeafID: "leaf-1", ReservedUntilUnix: reservedUntilUnix}}
}

func TestDropLapsedReservations(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	q := NewPreFetchQueue(8, logger)
	now := time.Unix(1_000_000, 0)

	// Lease already lapsed (before now) → dropped.
	q.Push(leaseItem("00000000-0000-4000-8000-000000000001", now.Add(-1*time.Minute).Unix()))
	// Lease within the safety margin (lapses before now+margin) → dropped.
	q.Push(leaseItem("00000000-0000-4000-8000-000000000002", now.Add(30*time.Second).Unix()))
	// Lease comfortably in the future → kept.
	q.Push(leaseItem("00000000-0000-4000-8000-000000000003", now.Add(10*time.Minute).Unix()))
	// No lease (0) → kept untouched (e.g. a unit with no reservation field).
	q.Push(leaseItem("00000000-0000-4000-8000-000000000004", 0))

	q.DropLapsedReservations(60*time.Second, now)

	got := q.Items()
	if len(got) != 2 {
		t.Fatalf("expected 2 items kept, got %d", len(got))
	}
	keptIDs := map[string]bool{}
	for _, it := range got {
		keptIDs[it.WU.ID] = true
	}
	if !keptIDs["00000000-0000-4000-8000-000000000003"] || !keptIDs["00000000-0000-4000-8000-000000000004"] {
		t.Fatalf("expected the future-lease and no-lease items kept, got %v", keptIDs)
	}
}
