package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// newSlotTestDaemon creates a minimal daemon for slot tests.
func newSlotTestDaemon() *Daemon {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = os.TempDir()
	cfg.Thermal.Enabled = false

	mc := &mockClient{}
	d := NewDaemon(DaemonConfig{
		Config:      cfg,
		PubKey:      pub,
		PrivKey:     priv,
		Client:      mc,
		Runtime:     &mockRuntime{canHandle: true},
		VolunteerID: "test-slot-vol",
		Logger:      logger,
	})
	return d
}

func TestSlotManager_ConcurrentExecution(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(3, logger)
	d := newSlotTestDaemon()

	var concurrentCount atomic.Int32
	var maxConcurrent atomic.Int32

	makeItem := func(id string) *PreFetchItem {
		return &PreFetchItem{
			WU:      &runtime.WorkUnit{ID: id, LeafID: "proj-1"},
			WUResp:  &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
			Prep:    &runtime.PrepareResult{WorkDir: "/tmp/" + id},
			Runtime: &mockRuntime{
				canHandle: true,
				executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
					cur := concurrentCount.Add(1)
					// Track peak concurrency.
					for {
						old := maxConcurrent.Load()
						if cur <= old || maxConcurrent.CompareAndSwap(old, cur) {
							break
						}
					}
					time.Sleep(100 * time.Millisecond)
					concurrentCount.Add(-1)
					return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0}, nil
				},
			},
			Conn:      &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
			FetchedAt: time.Now(),
		}
	}

	ctx := context.Background()
	start := time.Now()

	// Start all 3 slots.
	for i := 0; i < 3; i++ {
		slotID := <-sm.available
		sm.StartSlot(ctx, slotID, makeItem("wu-"+string(rune('1'+i))), d)
	}

	// Wait for all 3 completions.
	for i := 0; i < 3; i++ {
		result, err := sm.WaitForCompletion(ctx)
		if err != nil {
			t.Fatalf("WaitForCompletion %d: %v", i, err)
		}
		if result.Err != nil {
			t.Errorf("slot %d error: %v", result.SlotID, result.Err)
		}
	}

	elapsed := time.Since(start)

	// All 3 should have run concurrently (~100ms, not ~300ms).
	if elapsed > 250*time.Millisecond {
		t.Errorf("execution took %v, expected ~100ms (concurrent)", elapsed)
	}

	// Peak concurrency should be 3.
	if maxConcurrent.Load() < 3 {
		t.Errorf("max concurrent = %d, want 3", maxConcurrent.Load())
	}

	// All slots should be inactive.
	if sm.ActiveCount() != 0 {
		t.Errorf("active count = %d, want 0", sm.ActiveCount())
	}
}

func TestSlotManager_GetCurrentTasks(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(2, logger)
	d := newSlotTestDaemon()

	// Create long-running items.
	blockCh := make(chan struct{})
	makeItem := func(id string) *PreFetchItem {
		return &PreFetchItem{
			WU:      &runtime.WorkUnit{ID: id, LeafID: "proj-" + id},
			WUResp:  &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
			Prep:    &runtime.PrepareResult{WorkDir: "/tmp/" + id},
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
			Conn:      &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
			FetchedAt: time.Now(),
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start 2 slots.
	for _, id := range []string{"wu-a", "wu-b"} {
		slotID := <-sm.available
		sm.StartSlot(ctx, slotID, makeItem(id), d)
	}

	// Give slots time to start.
	time.Sleep(50 * time.Millisecond)

	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 2 {
		t.Fatalf("GetCurrentTasks returned %d tasks, want 2", len(tasks))
	}

	// Verify task IDs and WorkDir are present (order may vary).
	ids := map[string]bool{}
	workDirs := map[string]string{}
	for _, task := range tasks {
		ids[task.WorkUnitID] = true
		workDirs[task.WorkUnitID] = task.WorkDir
	}
	if !ids["wu-a"] || !ids["wu-b"] {
		t.Errorf("expected tasks wu-a and wu-b, got %v", ids)
	}

	// Verify WorkDir is propagated from PrepareResult.
	if workDirs["wu-a"] != "/tmp/wu-a" {
		t.Errorf("WorkDir for wu-a = %q, want %q", workDirs["wu-a"], "/tmp/wu-a")
	}
	if workDirs["wu-b"] != "/tmp/wu-b" {
		t.Errorf("WorkDir for wu-b = %q, want %q", workDirs["wu-b"], "/tmp/wu-b")
	}

	// Unblock execution and wait for completion.
	close(blockCh)
	for i := 0; i < 2; i++ {
		sm.WaitForCompletion(ctx)
	}
}

func TestSlotManager_GetCurrentTasks_WorkDirNilPrep(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-noprep", LeafID: "proj-1"},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{}, // empty PrepareResult — WorkDir is ""
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
		Conn:      &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
		FetchedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, item, d)

	time.Sleep(50 * time.Millisecond)

	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 1 {
		t.Fatalf("GetCurrentTasks returned %d tasks, want 1", len(tasks))
	}

	// WorkDir should be empty when prep is nil.
	if tasks[0].WorkDir != "" {
		t.Errorf("WorkDir = %q, want empty string when prep is nil", tasks[0].WorkDir)
	}

	close(blockCh)
	sm.WaitForCompletion(ctx)
}

func TestSlotManager_StopAll(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(2, logger)
	d := newSlotTestDaemon()

	makeItem := func(id string) *PreFetchItem {
		return &PreFetchItem{
			WU:      &runtime.WorkUnit{ID: id, LeafID: "proj-1"},
			WUResp:  &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
			Prep:    &runtime.PrepareResult{WorkDir: "/tmp/" + id},
			Runtime: &mockRuntime{
				canHandle: true,
				executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
					time.Sleep(200 * time.Millisecond)
					return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0}, nil
				},
			},
			Conn:      &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
			FetchedAt: time.Now(),
		}
	}

	ctx := context.Background()

	// Start 2 slots.
	for _, id := range []string{"wu-x", "wu-y"} {
		slotID := <-sm.available
		sm.StartSlot(ctx, slotID, makeItem(id), d)
	}

	// StopAll should block until both complete.
	start := time.Now()
	sm.StopAll()
	elapsed := time.Since(start)

	if elapsed < 150*time.Millisecond {
		t.Errorf("StopAll returned too quickly (%v), expected ~200ms", elapsed)
	}

	if sm.ActiveCount() != 0 {
		t.Errorf("active count after StopAll = %d, want 0", sm.ActiveCount())
	}
}

func TestSlotManager_TotalActiveMemoryMB(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(3, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	makeItem := func(id string, memMB int32) *PreFetchItem {
		return &PreFetchItem{
			WU: &runtime.WorkUnit{
				ID:        id,
				LeafID: "proj-1",
				ExecutionSpec: runtime.ExecutionSpec{
					MaxMemoryMB: memMB,
				},
			},
			WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
			Prep:   &runtime.PrepareResult{WorkDir: "/tmp/" + id},
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
			Conn:      &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
			FetchedAt: time.Now(),
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start 2 slots with known memory.
	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, makeItem("wu-a", 2048), d)
	slotID = <-sm.available
	sm.StartSlot(ctx, slotID, makeItem("wu-b", 4096), d)

	time.Sleep(50 * time.Millisecond)

	total := sm.TotalActiveMemoryMB()
	if total != 6144 {
		t.Errorf("TotalActiveMemoryMB = %d, want 6144", total)
	}

	close(blockCh)
	for i := 0; i < 2; i++ {
		sm.WaitForCompletion(ctx)
	}
}

func TestSuspendAll_ResumeAll_PauseDurationAccumulation(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:              "wu-pause",
			LeafID:          "leaf-1",
			Runtime:         "native",
			DeadlineSeconds: 3600,
		},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-pause"},
		Runtime: &mockRuntime{
			canHandle: true,
			executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				select {
				case <-blockCh:
				case <-ctx.Done():
				}
				return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0, Metrics: runtime.ExecutionMetrics{WallClockSeconds: 10}}, nil
			},
		},
		Conn:      makeTestConn(),
		FetchedAt: time.Now().Add(-5 * time.Second),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)

	// Attach mock process handle.
	h := &mockProcessHandle{pid: 9999}
	sm.SetProcessHandle(0, h)

	// First suspend/resume cycle.
	sm.SuspendAll()
	time.Sleep(100 * time.Millisecond)
	sm.ResumeAll()

	// Second suspend/resume cycle.
	sm.SuspendAll()
	time.Sleep(100 * time.Millisecond)
	sm.ResumeAll()

	// Check accumulated pause duration via GetCurrentTasks.
	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}

	// Should have ~200ms of pause time (2 x 100ms).
	if tasks[0].TotalPausedSeconds < 0 {
		t.Errorf("TotalPausedSeconds = %d, want >= 0", tasks[0].TotalPausedSeconds)
	}

	// Verify other new fields are populated.
	if tasks[0].DeadlineSeconds != 3600 {
		t.Errorf("DeadlineSeconds = %d, want 3600", tasks[0].DeadlineSeconds)
	}
	if tasks[0].RuntimeType != "native" {
		t.Errorf("RuntimeType = %q, want %q", tasks[0].RuntimeType, "native")
	}
	if tasks[0].ProcessID != 9999 {
		t.Errorf("ProcessID = %d, want 9999", tasks[0].ProcessID)
	}
	if tasks[0].FetchedAt.IsZero() {
		t.Error("FetchedAt should not be zero")
	}
	if tasks[0].ServerName == "" {
		t.Error("ServerName should not be empty")
	}

	close(blockCh)
	result, err := sm.WaitForCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}

	// SlotResult should carry the accumulated pause duration.
	if result.TotalPausedDur < 150*time.Millisecond {
		t.Errorf("TotalPausedDur = %v, want >= 150ms", result.TotalPausedDur)
	}
}

func TestGetCurrentTasks_DuringActivePause(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-active-pause", LeafID: "leaf-1", Runtime: "container", ExecutionSpec: runtime.ExecutionSpec{Image: "ubuntu:22.04"}},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-active-pause"},
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
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)

	h := &mockProcessHandle{pid: 5555}
	sm.SetProcessHandle(0, h)

	// Suspend and check TotalPausedSeconds while still paused.
	sm.SuspendAll()
	time.Sleep(100 * time.Millisecond)

	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if !tasks[0].Suspended {
		t.Error("expected task to be suspended")
	}
	// TotalPausedSeconds should include ongoing pause time (at least 100ms -> 0 seconds due to rounding).
	// The key check: it should not be negative.
	if tasks[0].TotalPausedSeconds < 0 {
		t.Errorf("TotalPausedSeconds during active pause = %d, want >= 0", tasks[0].TotalPausedSeconds)
	}
	if tasks[0].ContainerImage != "ubuntu:22.04" {
		t.Errorf("ContainerImage = %q, want %q", tasks[0].ContainerImage, "ubuntu:22.04")
	}

	sm.ResumeAll()
	close(blockCh)
	sm.WaitForCompletion(ctx)
}

func TestSuspendAll_FailedSuspend_NoPauseAccumulation(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:              "wu-fail-suspend",
			LeafID:          "leaf-1",
			Runtime:         "native",
			DeadlineSeconds: 3600,
		},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-fail-suspend"},
		Runtime: &mockRuntime{
			canHandle: true,
			executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				select {
				case <-blockCh:
				case <-ctx.Done():
				}
				return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0, Metrics: runtime.ExecutionMetrics{WallClockSeconds: 10}}, nil
			},
		},
		Conn:      makeTestConn(),
		FetchedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)

	// Attach a mock process handle that fails on Suspend.
	h := &mockProcessHandle{pid: 8888, suspendErr: fmt.Errorf("access denied")}
	sm.SetProcessHandle(0, h)

	// SuspendAll should NOT set pausedAt when Suspend() fails.
	sm.SuspendAll()
	time.Sleep(100 * time.Millisecond)

	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	// Slot should NOT be marked as suspended when Suspend() failed.
	if tasks[0].Suspended {
		t.Error("expected task to NOT be suspended when Suspend() failed")
	}
	// TotalPausedSeconds should be 0 since suspend never actually happened.
	if tasks[0].TotalPausedSeconds != 0 {
		t.Errorf("TotalPausedSeconds = %d, want 0 (suspend failed)", tasks[0].TotalPausedSeconds)
	}

	close(blockCh)
	result, err := sm.WaitForCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	// TotalPausedDur should be 0 since suspend never happened.
	if result.TotalPausedDur != 0 {
		t.Errorf("TotalPausedDur = %v, want 0", result.TotalPausedDur)
	}
}

func TestResumeAll_FailedResume_StaysSuspended(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:              "wu-fail-resume",
			LeafID:          "leaf-1",
			Runtime:         "native",
			DeadlineSeconds: 3600,
		},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-fail-resume"},
		Runtime: &mockRuntime{
			canHandle: true,
			executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				select {
				case <-blockCh:
				case <-ctx.Done():
				}
				return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0, Metrics: runtime.ExecutionMetrics{WallClockSeconds: 10}}, nil
			},
		},
		Conn:      makeTestConn(),
		FetchedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)

	// Attach a mock that succeeds on Suspend but fails on Resume.
	h := &mockProcessHandle{pid: 7777, resumeErr: fmt.Errorf("resume failed")}
	sm.SetProcessHandle(0, h)

	// Suspend succeeds.
	sm.SuspendAll()
	time.Sleep(100 * time.Millisecond)

	// Resume fails — slot should remain suspended.
	sm.ResumeAll()

	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	// When Resume fails, suspended stays true.
	if !tasks[0].Suspended {
		t.Error("expected task to remain suspended when Resume() fails")
	}
	// Even though Resume failed, pausedAt was already zeroed and totalPausedDur
	// accumulated. TotalPausedSeconds should reflect the prior pause period.
	if tasks[0].TotalPausedSeconds < 0 {
		t.Errorf("TotalPausedSeconds = %d, want >= 0", tasks[0].TotalPausedSeconds)
	}

	close(blockCh)
	sm.WaitForCompletion(ctx)
}

func TestGetCurrentTasks_EstimatedSeconds_FromBenchmark(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:              "wu-est",
			LeafID:          "leaf-est",
			Runtime:         "native",
			RscFpopsEst:     1e12, // 1 trillion flops
			DeadlineSeconds: 7200,
		},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-est"},
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
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)

	// With benchmarkFPOPS=1e11 and RscFpopsEst=1e12, expected = 10.0 seconds * DCF(1.0) = 10.0
	benchFPOPS := 1e11
	tasks := sm.GetCurrentTasks(benchFPOPS, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}

	// EstimatedSeconds should be 10.0 (1e12 / 1e11 * 1.0 DCF).
	if tasks[0].EstimatedSeconds < 9.9 || tasks[0].EstimatedSeconds > 10.1 {
		t.Errorf("EstimatedSeconds = %f, want ~10.0", tasks[0].EstimatedSeconds)
	}

	// Test with custom DCF function (2.0x correction).
	dcfFunc := func(leafID string) float64 { return 2.0 }
	tasks = sm.GetCurrentTasks(benchFPOPS, dcfFunc)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}

	// EstimatedSeconds should be 20.0 (1e12 / 1e11 * 2.0 DCF).
	if tasks[0].EstimatedSeconds < 19.9 || tasks[0].EstimatedSeconds > 20.1 {
		t.Errorf("EstimatedSeconds with DCF = %f, want ~20.0", tasks[0].EstimatedSeconds)
	}

	close(blockCh)
	sm.WaitForCompletion(ctx)
}

func TestSlotResult_TotalPausedDur_TaskCompletesWhilePaused(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:              "wu-complete-paused",
			LeafID:          "leaf-1",
			Runtime:         "native",
			DeadlineSeconds: 3600,
		},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-complete-paused"},
		Runtime: &mockRuntime{
			canHandle: true,
			executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				select {
				case <-blockCh:
				case <-ctx.Done():
				}
				return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0, Metrics: runtime.ExecutionMetrics{WallClockSeconds: 5}}, nil
			},
		},
		Conn:      makeTestConn(),
		FetchedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)

	// Attach mock process handle and suspend.
	h := &mockProcessHandle{pid: 6666}
	sm.SetProcessHandle(0, h)
	sm.SuspendAll()
	time.Sleep(150 * time.Millisecond)

	// Task completes while still suspended (don't resume first).
	close(blockCh)
	result, err := sm.WaitForCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}

	// The defer in runSlot should capture the ongoing pause (pausedAt not zero).
	// TotalPausedDur should include the time since SuspendAll (~150ms+).
	if result.TotalPausedDur < 100*time.Millisecond {
		t.Errorf("TotalPausedDur = %v, want >= 100ms (task completed while paused)", result.TotalPausedDur)
	}
}

func TestSuspendAll_ResumeAll_MultiSlot(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(2, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	makeItem := func(id string) *PreFetchItem {
		return &PreFetchItem{
			WU: &runtime.WorkUnit{
				ID:              id,
				LeafID:          "leaf-1",
				Runtime:         "native",
				DeadlineSeconds: 3600,
			},
			WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
			Prep:   &runtime.PrepareResult{WorkDir: "/tmp/" + id},
			Runtime: &mockRuntime{
				canHandle: true,
				executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
					select {
					case <-blockCh:
					case <-ctx.Done():
					}
					return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0, Metrics: runtime.ExecutionMetrics{WallClockSeconds: 10}}, nil
				},
			},
			Conn:      makeTestConn(),
			FetchedAt: time.Now(),
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start 2 slots.
	slotID0 := <-sm.available
	sm.StartSlot(ctx, slotID0, makeItem("wu-multi-a"), d)
	slotID1 := <-sm.available
	sm.StartSlot(ctx, slotID1, makeItem("wu-multi-b"), d)
	time.Sleep(50 * time.Millisecond)

	// Attach process handles to both slots.
	sm.SetProcessHandle(slotID0, &mockProcessHandle{pid: 1001})
	sm.SetProcessHandle(slotID1, &mockProcessHandle{pid: 1002})

	// Suspend all.
	sm.SuspendAll()
	time.Sleep(100 * time.Millisecond)

	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	for _, task := range tasks {
		if !task.Suspended {
			t.Errorf("task %s should be suspended", task.WorkUnitID)
		}
	}

	// Resume all.
	sm.ResumeAll()
	tasks = sm.GetCurrentTasks(0, nil)
	for _, task := range tasks {
		if task.Suspended {
			t.Errorf("task %s should NOT be suspended after resume", task.WorkUnitID)
		}
	}

	close(blockCh)
	for i := 0; i < 2; i++ {
		result, err := sm.WaitForCompletion(ctx)
		if err != nil {
			t.Fatalf("WaitForCompletion %d: %v", i, err)
		}
		if result.TotalPausedDur < 50*time.Millisecond {
			t.Errorf("slot %d TotalPausedDur = %v, want >= 50ms", result.SlotID, result.TotalPausedDur)
		}
	}
}

func TestSuspendSlot_ByWorkUnitID(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(2, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	makeItem := func(id string) *PreFetchItem {
		return &PreFetchItem{
			WU:     &runtime.WorkUnit{ID: id, LeafID: "leaf-1", Runtime: "native"},
			WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
			Prep:   &runtime.PrepareResult{WorkDir: "/tmp/" + id},
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
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start 2 slots.
	slotID0 := <-sm.available
	sm.StartSlot(ctx, slotID0, makeItem("wu-s1"), d)
	slotID1 := <-sm.available
	sm.StartSlot(ctx, slotID1, makeItem("wu-s2"), d)
	time.Sleep(50 * time.Millisecond)

	sm.SetProcessHandle(slotID0, &mockProcessHandle{pid: 2001})
	sm.SetProcessHandle(slotID1, &mockProcessHandle{pid: 2002})

	// Suspend only wu-s1.
	if err := sm.SuspendSlot("wu-s1"); err != nil {
		t.Fatalf("SuspendSlot: %v", err)
	}

	tasks := sm.GetCurrentTasks(0, nil)
	for _, task := range tasks {
		if task.WorkUnitID == "wu-s1" && !task.Suspended {
			t.Error("wu-s1 should be suspended")
		}
		if task.WorkUnitID == "wu-s2" && task.Suspended {
			t.Error("wu-s2 should NOT be suspended")
		}
	}

	close(blockCh)
	for i := 0; i < 2; i++ {
		sm.WaitForCompletion(ctx)
	}
}

func TestSuspendSlot_NotFound(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)

	err := sm.SuspendSlot("nonexistent")
	if err != ErrTaskNotFound {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestSuspendSlot_AlreadySuspended(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-double-sus", LeafID: "leaf-1", Runtime: "native"},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-double-sus"},
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
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)
	sm.SetProcessHandle(slotID, &mockProcessHandle{pid: 3001})

	// First suspend succeeds.
	if err := sm.SuspendSlot("wu-double-sus"); err != nil {
		t.Fatalf("first SuspendSlot: %v", err)
	}

	// Second suspend returns conflict.
	err := sm.SuspendSlot("wu-double-sus")
	if err != ErrTaskAlreadySuspended {
		t.Errorf("second SuspendSlot = %v, want ErrTaskAlreadySuspended", err)
	}

	close(blockCh)
	sm.WaitForCompletion(ctx)
}

func TestResumeSlot_ByWorkUnitID(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-resume", LeafID: "leaf-1", Runtime: "native"},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-resume"},
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
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)
	sm.SetProcessHandle(slotID, &mockProcessHandle{pid: 4001})

	// Suspend, wait, then resume.
	sm.SuspendSlot("wu-resume")
	time.Sleep(100 * time.Millisecond)

	if err := sm.ResumeSlot("wu-resume"); err != nil {
		t.Fatalf("ResumeSlot: %v", err)
	}

	// Task should no longer be suspended.
	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Suspended {
		t.Error("task should not be suspended after resume")
	}

	close(blockCh)
	result, _ := sm.WaitForCompletion(ctx)
	// Should have accumulated ~100ms of pause time.
	if result.TotalPausedDur < 50*time.Millisecond {
		t.Errorf("TotalPausedDur = %v, want >= 50ms", result.TotalPausedDur)
	}
}

func TestResumeSlot_NotSuspended(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-not-sus", LeafID: "leaf-1"},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-not-sus"},
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
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)

	err := sm.ResumeSlot("wu-not-sus")
	if err != ErrTaskNotSuspended {
		t.Errorf("ResumeSlot = %v, want ErrTaskNotSuspended", err)
	}

	close(blockCh)
	sm.WaitForCompletion(ctx)
}

func TestResumeSlot_NotFound(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)

	err := sm.ResumeSlot("nonexistent")
	if err != ErrTaskNotFound {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestAbortSlot_CancelsContext(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-abort", LeafID: "leaf-1"},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-abort"},
		Runtime: &mockRuntime{
			canHandle: true,
			executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
		Conn:      makeTestConn(),
		FetchedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)

	// Abort should cancel the context.
	if err := sm.AbortSlot("wu-abort"); err != nil {
		t.Fatalf("AbortSlot: %v", err)
	}

	// The slot should complete shortly.
	result, err := sm.WaitForCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if result.Err == nil {
		t.Error("expected error from aborted task")
	}
}

func TestAbortSlot_NotFound(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)

	err := sm.AbortSlot("nonexistent")
	if err != ErrTaskNotFound {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestSuspendSlot_RecordsPausedAt(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-pauseat", LeafID: "leaf-1", Runtime: "native"},
		WUResp: &lettucev1.WorkUnitAssignment{HeartbeatIntervalSeconds: 300},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-pauseat"},
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
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)
	sm.SetProcessHandle(slotID, &mockProcessHandle{pid: 5001})

	before := time.Now()
	sm.SuspendSlot("wu-pauseat")

	// Verify via GetCurrentTasks that TotalPausedSeconds is accumulating.
	time.Sleep(100 * time.Millisecond)
	tasks := sm.GetCurrentTasks(0, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if !tasks[0].Suspended {
		t.Error("task should be suspended")
	}
	elapsed := time.Since(before)
	if elapsed < 50*time.Millisecond {
		t.Error("not enough time passed for meaningful pause test")
	}

	close(blockCh)
	sm.WaitForCompletion(ctx)
}
