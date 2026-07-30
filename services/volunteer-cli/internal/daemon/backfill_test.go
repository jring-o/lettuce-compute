package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// mkBufferedItem builds a work unit of the given declared memory whose
// execution blocks on blockCh, mirroring the TB-22 fleet shape: long-running
// units of mixed sizes sharing one memory budget.
func mkBufferedItem(id string, memMB int32, blockCh chan struct{}) *PreFetchItem {
	return &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID: id, LeafID: "leaf-" + id,
			ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: memMB},
		},
		WUResp: &lettucev1.WorkUnitAssignment{},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/" + id},
		Runtime: &mockRuntime{canHandle: true, executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			<-blockCh
			return &runtime.ExecutionResult{ExitCode: 0, OutputData: []byte("ok")}, nil
		}},
		Conn:      &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
		FetchedAt: time.Now(),
	}
}

// newBackfillTestDaemon is the shared TB-22/TB-23 fixture: 2 slots, an
// 8192 MB budget, the free-RAM gate neutralized, and a 6000 MB unit already
// running in slot 0 — the tester's exact machine shape. Cleanup restores the
// free-RAM probe and unblocks the running unit.
func newBackfillTestDaemon(t *testing.T, blockCh chan struct{}) *Daemon {
	t.Helper()
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	d.slotManager = NewSlotManager(2, d.logger)
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)
	d.cfg.ResourceLimits.MaxMemoryMB = 8192

	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	t.Cleanup(func() { freeSystemMemoryMB = orig })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	slotID := <-d.slotManager.available
	if err := d.slotManager.StartSlot(ctx, slotID, mkBufferedItem("grep-running", 6000, blockCh), d); err != nil {
		t.Fatalf("StartSlot: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := d.slotManager.ActiveCount(); got != 1 {
		t.Fatalf("setup: ActiveCount = %d, want 1", got)
	}
	return d
}

// TestFillSlots_BackfillsPastBlockedHead is the TB-22 regression test: with a
// 6000 MB unit running against the 8192 MB budget and a free second slot, a
// fitting 768 MB buffered unit must start even though another 6000 MB unit
// sits at the head of the buffer (6000+768 <= 8192; 6000+6000 > 8192). Before
// the fix the picker only ever offered the FIFO head, so the slot idled until
// the running unit finished — observed live as ~33 idle slot-minutes.
func TestFillSlots_BackfillsPastBlockedHead(t *testing.T) {
	blockCh := make(chan struct{})
	defer close(blockCh)
	d := newBackfillTestDaemon(t, blockCh)

	d.prefetchQueue.Push(mkBufferedItem("grep-buffered", 6000, blockCh))
	d.prefetchQueue.Push(mkBufferedItem("beyblade-buffered", 768, blockCh))

	d.fillSlots(context.Background())
	time.Sleep(50 * time.Millisecond)

	if got := d.slotManager.ActiveCount(); got != 2 {
		t.Errorf("ActiveCount = %d, want 2 (the free slot must run the fitting 768 MB unit)", got)
	}
	if got := d.prefetchQueue.Len(); got != 1 {
		t.Errorf("queue len = %d, want 1 (only the non-fitting 6000 MB head should remain)", got)
	}
	if items := d.prefetchQueue.Items(); len(items) == 1 && items[0].WU.ID != "grep-buffered" {
		t.Errorf("queue head = %q, want grep-buffered (FIFO order preserved for the waiting unit)", items[0].WU.ID)
	}
}

// TestFillSlots_CapacityWaitLogsOnceAtInfo is the TB-23 regression test: a
// buffered unit waiting for memory capacity must produce one Info line, on the
// transition into the wait — not one per admission check. Before the fix the
// refusal logged at Info on every 1-second coordinator tick (~30k identical
// lines/day observed across two hosts).
func TestFillSlots_CapacityWaitLogsOnceAtInfo(t *testing.T) {
	blockCh := make(chan struct{})
	defer close(blockCh)
	d := newBackfillTestDaemon(t, blockCh)

	// Capture the daemon's log at Info level from here on.
	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	d.prefetchQueue.Push(mkBufferedItem("grep-buffered", 6000, blockCh))

	// Three coordinator passes over the same blocked unit.
	ctx := context.Background()
	d.fillSlots(ctx)
	d.fillSlots(ctx)
	d.fillSlots(ctx)

	if got := strings.Count(buf.String(), "waiting for capacity"); got != 1 {
		t.Errorf("Info-level capacity-wait lines = %d, want exactly 1 (transition only); log:\n%s", got, buf.String())
	}
}
