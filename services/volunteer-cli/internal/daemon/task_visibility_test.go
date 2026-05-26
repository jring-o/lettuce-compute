package daemon

// Tests for v0.9.5 task visibility features (S106):
// - CPU time accumulation across multiple pause/resume cycles (scenario 1)
// - Per-task suspend isolation and status fields (scenario 2)

import (
	"context"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// makeBlockingItem creates a PreFetchItem whose execution blocks until blockCh
// is closed or the context is cancelled.
func makeBlockingItem(id string, blockCh chan struct{}) *PreFetchItem {
	return &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: id, LeafID: "leaf-1", Runtime: "native", DeadlineSeconds: 3600},
		WUResp: &lettucev1.RequestWorkUnitResponse{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/" + id},
		Runtime: &mockRuntime{
			canHandle: true,
			executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				select {
				case <-blockCh:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0}, nil
			},
		},
		Conn:      makeTestConn(),
		FetchedAt: time.Now(),
	}
}

// TestCPUTimeAccumulation verifies that TotalPausedDur accumulates correctly
// across multiple pause/resume cycles via per-task SuspendSlot/ResumeSlot.
func TestCPUTimeAccumulation(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := makeBlockingItem("wu-cputime", blockCh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)
	sm.SetProcessHandle(slotID, &mockProcessHandle{pid: 7001})

	// Run 3 suspend/resume cycles of ~150ms each.
	for cycle := 1; cycle <= 3; cycle++ {
		if err := sm.SuspendSlot("wu-cputime"); err != nil {
			t.Fatalf("cycle %d suspend: %v", cycle, err)
		}
		time.Sleep(150 * time.Millisecond)
		if err := sm.ResumeSlot("wu-cputime"); err != nil {
			t.Fatalf("cycle %d resume: %v", cycle, err)
		}
	}

	// Complete the task and verify cumulative pause duration.
	close(blockCh)
	result, err := sm.WaitForCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}

	// 3 cycles × ~150ms = ~450ms total. Allow tolerance for scheduling jitter.
	if result.TotalPausedDur < 300*time.Millisecond {
		t.Errorf("TotalPausedDur = %v, want >= 300ms (3 pause cycles of ~150ms)", result.TotalPausedDur)
	}
	if result.TotalPausedDur > 3*time.Second {
		t.Errorf("TotalPausedDur = %v, suspiciously high (expected ~450ms)", result.TotalPausedDur)
	}
}

// TestCPUTimeAccumulation_DuringActivePause verifies that GetCurrentTasks
// includes ongoing pause time in TotalPausedSeconds (not just completed cycles).
func TestCPUTimeAccumulation_DuringActivePause(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := makeBlockingItem("wu-active-pause", blockCh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)
	sm.SetProcessHandle(slotID, &mockProcessHandle{pid: 7002})

	// Cycle 1: quick pause/resume.
	if err := sm.SuspendSlot("wu-active-pause"); err != nil {
		t.Fatalf("cycle 1 suspend: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := sm.ResumeSlot("wu-active-pause"); err != nil {
		t.Fatalf("cycle 1 resume: %v", err)
	}

	// Cycle 2: suspend and hold — verify GetCurrentTasks includes ongoing pause.
	if err := sm.SuspendSlot("wu-active-pause"); err != nil {
		t.Fatalf("cycle 2 suspend: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)

	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}

	// Total paused: ~100ms (cycle 1) + ~1100ms (ongoing) ≈ 1200ms → at least 1 integer second.
	if tasks[0].TotalPausedSeconds < 1 {
		t.Errorf("TotalPausedSeconds = %d during active pause, want >= 1", tasks[0].TotalPausedSeconds)
	}
	if !tasks[0].Suspended {
		t.Error("task should be suspended")
	}

	// Resume and complete.
	if err := sm.ResumeSlot("wu-active-pause"); err != nil {
		t.Fatalf("cycle 2 resume: %v", err)
	}
	close(blockCh)
	sm.WaitForCompletion(ctx)
}

// TestCPUTimeAccumulation_SuspendAll verifies that SuspendAll/ResumeAll correctly
// tracks pause duration across multiple slots.
func TestCPUTimeAccumulation_SuspendAll(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(2, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID1 := <-sm.available
	sm.StartSlot(ctx, slotID1, makeBlockingItem("wu-all-1", blockCh), d)
	slotID2 := <-sm.available
	sm.StartSlot(ctx, slotID2, makeBlockingItem("wu-all-2", blockCh), d)
	time.Sleep(50 * time.Millisecond)
	sm.SetProcessHandle(slotID1, &mockProcessHandle{pid: 7101})
	sm.SetProcessHandle(slotID2, &mockProcessHandle{pid: 7102})

	// SuspendAll, wait, ResumeAll.
	sm.SuspendAll()
	time.Sleep(200 * time.Millisecond)
	sm.ResumeAll()

	// Both tasks should have non-zero pause time.
	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	for _, task := range tasks {
		if task.Suspended {
			t.Errorf("task %s should not be suspended after ResumeAll", task.WorkUnitID)
		}
	}

	// Complete and verify both results have accumulated pause time.
	close(blockCh)
	for i := 0; i < 2; i++ {
		result, err := sm.WaitForCompletion(ctx)
		if err != nil {
			t.Fatalf("WaitForCompletion %d: %v", i, err)
		}
		if result.TotalPausedDur < 100*time.Millisecond {
			t.Errorf("slot %d TotalPausedDur = %v, want >= 100ms", result.SlotID, result.TotalPausedDur)
		}
	}
}

// TestPerTaskSuspendIsolation verifies that suspending one task does not affect
// the suspended/paused state of other tasks.
func TestPerTaskSuspendIsolation(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(2, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID1 := <-sm.available
	sm.StartSlot(ctx, slotID1, makeBlockingItem("wu-iso-1", blockCh), d)
	slotID2 := <-sm.available
	sm.StartSlot(ctx, slotID2, makeBlockingItem("wu-iso-2", blockCh), d)
	time.Sleep(50 * time.Millisecond)
	sm.SetProcessHandle(slotID1, &mockProcessHandle{pid: 8001})
	sm.SetProcessHandle(slotID2, &mockProcessHandle{pid: 8002})

	// Suspend only task 1.
	if err := sm.SuspendSlot("wu-iso-1"); err != nil {
		t.Fatalf("SuspendSlot: %v", err)
	}

	tasks := sm.GetCurrentTasks(0, nil)
	for _, task := range tasks {
		switch task.WorkUnitID {
		case "wu-iso-1":
			if !task.Suspended {
				t.Error("wu-iso-1 should be suspended")
			}
			if task.TotalPausedSeconds < 0 {
				t.Error("wu-iso-1 should have non-negative TotalPausedSeconds")
			}
		case "wu-iso-2":
			if task.Suspended {
				t.Error("wu-iso-2 should NOT be suspended")
			}
			if task.TotalPausedSeconds != 0 {
				t.Errorf("wu-iso-2 TotalPausedSeconds = %d, want 0", task.TotalPausedSeconds)
			}
		}
	}

	// Resume task 1 and verify pause time accumulated only for task 1.
	time.Sleep(100 * time.Millisecond)
	if err := sm.ResumeSlot("wu-iso-1"); err != nil {
		t.Fatalf("ResumeSlot: %v", err)
	}

	tasks = sm.GetCurrentTasks(0, nil)
	for _, task := range tasks {
		if task.Suspended {
			t.Errorf("task %s should not be suspended after resume", task.WorkUnitID)
		}
	}

	close(blockCh)
	for i := 0; i < 2; i++ {
		result, err := sm.WaitForCompletion(ctx)
		if err != nil {
			t.Fatalf("WaitForCompletion %d: %v", i, err)
		}
		// Only the slot that was suspended should have pause time.
		if result.WU.ID == "wu-iso-1" && result.TotalPausedDur < 50*time.Millisecond {
			t.Errorf("wu-iso-1 TotalPausedDur = %v, want >= 50ms", result.TotalPausedDur)
		}
		if result.WU.ID == "wu-iso-2" && result.TotalPausedDur != 0 {
			t.Errorf("wu-iso-2 TotalPausedDur = %v, want 0", result.TotalPausedDur)
		}
	}
}
