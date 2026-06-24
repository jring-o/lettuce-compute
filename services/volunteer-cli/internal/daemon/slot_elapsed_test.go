package daemon

import (
	"context"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TestResumedTask_ElapsedExcludesDaemonDownGap verifies the persisted-run-time
// accounting: a task resumed from a previous session reports elapsed/CPU time based
// on run time accrued while actually executing, NOT wall-clock since its original
// start. The gap during which the daemon was stopped must not be counted.
func TestResumedTask_ElapsedExcludesDaemonDownGap(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	// A resumed task: it first started an hour ago, but only accrued 120s of run time
	// and 30s of paused time across prior sessions. The ~58 minutes in between is
	// daemon-down (or process-frozen) time that must be excluded from elapsed/CPU.
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-resumed", LeafID: "leaf-1"},
		WUResp: &lettucev1.WorkUnitAssignment{},
		Prep: &runtime.PrepareResult{
			WorkDir:           t.TempDir(),
			OriginalStartedAt: time.Now().Add(-time.Hour),
			ElapsedAccrued:    120 * time.Second,
			PausedAccrued:     30 * time.Second,
		},
		Runtime: &mockRuntime{
			canHandle: true,
			executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				select {
				case <-blockCh:
				case <-ctx.Done():
				}
				return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0}, nil
			},
		},
		Conn:      makeTestConn(),
		FetchedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slotID := <-sm.available
	if err := sm.StartSlot(ctx, slotID, item, d); err != nil {
		t.Fatalf("StartSlot: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	tasks := sm.GetCurrentTasks(0, func(string) float64 { return 0 })
	if len(tasks) != 1 {
		t.Fatalf("got %d active tasks, want 1", len(tasks))
	}
	task := tasks[0]

	// Elapsed ~= 120s accrued + the few ms since resume — emphatically NOT ~3600s.
	if task.ElapsedSeconds < 118 || task.ElapsedSeconds > 140 {
		t.Errorf("ElapsedSeconds = %d, want ~120 (accrued run time, excluding daemon-down gap)", task.ElapsedSeconds)
	}
	// Paused ~= 30s accrued (no new pauses this session).
	if task.TotalPausedSeconds < 28 || task.TotalPausedSeconds > 40 {
		t.Errorf("TotalPausedSeconds = %d, want ~30 (accrued)", task.TotalPausedSeconds)
	}
	// StartedAt stays the original first-start for reference.
	if d := time.Since(task.StartedAt); d < 55*time.Minute {
		t.Errorf("StartedAt = %v ago, want ~1h ago (original start preserved)", d)
	}

	// Persisting now must carry the accrued bases forward for the next resume.
	persisted := sm.GetActivePersistableTasks()
	if len(persisted) != 1 {
		t.Fatalf("got %d persisted tasks, want 1", len(persisted))
	}
	if persisted[0].ElapsedAccruedSeconds < 118 || persisted[0].ElapsedAccruedSeconds > 140 {
		t.Errorf("persisted ElapsedAccruedSeconds = %d, want ~120", persisted[0].ElapsedAccruedSeconds)
	}
	if persisted[0].PausedAccruedSeconds < 28 || persisted[0].PausedAccruedSeconds > 40 {
		t.Errorf("persisted PausedAccruedSeconds = %d, want ~30", persisted[0].PausedAccruedSeconds)
	}

	close(blockCh)
	sm.WaitForCompletion(ctx)
}

// TestFreshTask_ElapsedStartsAtZero confirms a brand-new task accrues no base.
func TestFreshTask_ElapsedStartsAtZero(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-fresh", LeafID: "leaf-1"},
		WUResp: &lettucev1.WorkUnitAssignment{},
		Prep:   &runtime.PrepareResult{WorkDir: t.TempDir()},
		Runtime: &mockRuntime{
			canHandle: true,
			executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				select {
				case <-blockCh:
				case <-ctx.Done():
				}
				return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0}, nil
			},
		},
		Conn:      makeTestConn(),
		FetchedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slotID := <-sm.available
	if err := sm.StartSlot(ctx, slotID, item, d); err != nil {
		t.Fatalf("StartSlot: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	tasks := sm.GetCurrentTasks(0, func(string) float64 { return 0 })
	if len(tasks) != 1 {
		t.Fatalf("got %d active tasks, want 1", len(tasks))
	}
	if tasks[0].ElapsedSeconds > 5 {
		t.Errorf("fresh ElapsedSeconds = %d, want ~0", tasks[0].ElapsedSeconds)
	}

	close(blockCh)
	sm.WaitForCompletion(ctx)
}
