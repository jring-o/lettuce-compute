package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDaemonCheckpointRestore(t *testing.T) {
	// Create checkpoint data: a tar blob containing a state.bin file.
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "state.bin"), []byte("checkpoint-data"), 0644); err != nil {
		t.Fatal(err)
	}
	checkpointBlob, err := tarDirectory(srcDir)
	if err != nil {
		t.Fatal(err)
	}

	workUnitServed := false
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if workUnitServed {
				return nil, status.Error(codes.NotFound, "no work")
			}
			workUnitServed = true
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:                "2c7d1dae-749f-49cc-8adf-2b0f5cc9c7c8", // was wu-ckp-1
						LeafId:                    "proj-1",
						Runtime:                   "native",
						HeartbeatIntervalSeconds:  300,
						HasCheckpoint:             true,
						CheckpointSequence:        3,
						CheckpointIntervalSeconds: 60,
					},
				},
			}, nil
		},
		getCheckpointFn: func(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
			if req.WorkUnitId != "2c7d1dae-749f-49cc-8adf-2b0f5cc9c7c8" {
				t.Errorf("unexpected work unit id: %s", req.WorkUnitId)
			}
			return &lettucev1.GetCheckpointResponse{
				HasCheckpoint:      true,
				CheckpointData:     checkpointBlob,
				CheckpointSequence: 3,
			}, nil
		},
	}

	var extractedData []byte
	mr := &mockRuntime{
		canHandle: true,
		name:      "native",
		prepareFn: func(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			workDir := t.TempDir()
			return &runtime.PrepareResult{WorkDir: workDir}, nil
		},
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			// Verify checkpoint was extracted before execution.
			data, err := os.ReadFile(filepath.Join(prep.WorkDir, "checkpoint", "state.bin"))
			if err != nil {
				t.Errorf("checkpoint not restored: %v", err)
			} else {
				extractedData = data
			}
			return &runtime.ExecutionResult{
				OutputData:     []byte("result"),
				OutputChecksum: "abc",
				ExitCode:       0,
				Metrics:        runtime.ExecutionMetrics{WallClockSeconds: 5},
			}, nil
		},
	}

	d := newTestDaemon(mc, mr)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// Let it run one work unit, then stop.
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	d.Run(ctx)

	if string(extractedData) != "checkpoint-data" {
		t.Errorf("expected restored checkpoint data 'checkpoint-data', got %q", string(extractedData))
	}
}

func TestDaemonCheckpointGoroutineStarts(t *testing.T) {
	var mu sync.Mutex
	saveCalls := 0
	workUnitServed := false

	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if workUnitServed {
				return nil, status.Error(codes.NotFound, "no work")
			}
			workUnitServed = true
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:                "1ddc58f0-695e-42e0-82cb-b9ab1342077f", // was wu-ckp-2
						LeafId:                    "proj-1",
						Runtime:                   "native",
						HeartbeatIntervalSeconds:  300,
						CheckpointIntervalSeconds: 1, // 1 second â€” fast for test
					},
				},
			}, nil
		},
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			mu.Lock()
			saveCalls++
			mu.Unlock()
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	mr := &mockRuntime{
		canHandle: true,
		name:      "native",
		prepareFn: func(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			workDir := t.TempDir()
			// Create checkpoint data so the checkpoint manager has something to upload.
			checkpointDir := filepath.Join(workDir, "checkpoint")
			os.MkdirAll(checkpointDir, 0755)
			os.WriteFile(filepath.Join(checkpointDir, "state.bin"), []byte("state"), 0644)
			return &runtime.PrepareResult{WorkDir: workDir}, nil
		},
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			// Simulate long-running execution so checkpoint goroutine can fire.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
			return &runtime.ExecutionResult{
				OutputData:     []byte("result"),
				OutputChecksum: "abc",
				ExitCode:       0,
				Metrics:        runtime.ExecutionMetrics{WallClockSeconds: 3},
			}, nil
		},
	}

	d := newTestDaemon(mc, mr)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// Let checkpoint goroutine fire at least once, then cancel.
		time.Sleep(2500 * time.Millisecond)
		cancel()
	}()

	d.Run(ctx)

	mu.Lock()
	calls := saveCalls
	mu.Unlock()

	// At least 1 periodic save should have happened (interval=1s, ran for 2.5s).
	if calls < 1 {
		t.Errorf("expected at least 1 SaveCheckpoint call, got %d", calls)
	}
}

func TestDaemonCheckpointNotStartedWhenDisabled(t *testing.T) {
	workUnitServed := false
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if workUnitServed {
				return nil, status.Error(codes.NotFound, "no work")
			}
			workUnitServed = true
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:                "b924d1be-435a-4d13-8641-f85ba261c54c", // was wu-no-ckp
						LeafId:                    "proj-1",
						Runtime:                   "native",
						HeartbeatIntervalSeconds:  300,
						CheckpointIntervalSeconds: 0, // disabled
					},
				},
			}, nil
		},
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			t.Error("SaveCheckpoint should not be called when checkpointing is disabled")
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
		getCheckpointFn: func(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
			t.Error("GetCheckpoint should not be called when checkpointing is disabled")
			return &lettucev1.GetCheckpointResponse{HasCheckpoint: false}, nil
		},
	}

	mr := &mockRuntime{
		canHandle: true,
		name:      "native",
	}

	d := newTestDaemon(mc, mr)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	d.Run(ctx)
}

func TestDaemonHeartbeatCheckpointStatus(t *testing.T) {
	workUnitServed := false
	var mu sync.Mutex
	var heartbeatStatuses []string

	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if workUnitServed {
				return nil, status.Error(codes.NotFound, "no work")
			}
			workUnitServed = true
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:                "821673a2-01aa-47ce-810b-d313b9492389", // was wu-hb-1
						LeafId:                    "proj-1",
						Runtime:                   "native",
						HeartbeatIntervalSeconds:  1,
						CheckpointIntervalSeconds: 1,
					},
				},
			}, nil
		},
		heartbeatFn: func(ctx context.Context, req *lettucev1.HeartbeatRequest) (*lettucev1.HeartbeatResponse, error) {
			mu.Lock()
			heartbeatStatuses = append(heartbeatStatuses, req.Status)
			mu.Unlock()
			return &lettucev1.HeartbeatResponse{ContinueExecution: true}, nil
		},
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	mr := &mockRuntime{
		canHandle: true,
		name:      "native",
		prepareFn: func(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			workDir := t.TempDir()
			checkpointDir := filepath.Join(workDir, "checkpoint")
			os.MkdirAll(checkpointDir, 0755)
			os.WriteFile(filepath.Join(checkpointDir, "state.bin"), []byte("data"), 0644)
			return &runtime.PrepareResult{WorkDir: workDir}, nil
		},
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(4 * time.Second):
			}
			return &runtime.ExecutionResult{
				OutputData:     []byte("result"),
				OutputChecksum: "abc",
				ExitCode:       0,
				Metrics:        runtime.ExecutionMetrics{WallClockSeconds: 4},
			}, nil
		},
	}

	d := newTestDaemon(mc, mr)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(3500 * time.Millisecond)
		cancel()
	}()

	d.Run(ctx)

	mu.Lock()
	statuses := heartbeatStatuses
	mu.Unlock()

	// After checkpoint saves, at least some heartbeats should report CHECKPOINT_SAVED.
	hasCheckpointStatus := false
	for _, s := range statuses {
		if s == "CHECKPOINT_SAVED" {
			hasCheckpointStatus = true
			break
		}
	}

	if !hasCheckpointStatus && len(statuses) > 1 {
		t.Errorf("expected at least one CHECKPOINT_SAVED heartbeat status, got: %v", statuses)
	}
}

func TestDaemonCheckpointRestoreFailure_StartsFresh(t *testing.T) {
	// When GetCheckpoint fails, the daemon should log a warning and start fresh
	// (not abort the work unit).
	workUnitServed := false
	getCheckpointCalled := false

	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if workUnitServed {
				return nil, status.Error(codes.NotFound, "no work")
			}
			workUnitServed = true
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:                "5e84956d-05df-4c7c-8ade-cd9355c03da8", // was wu-ckp-fail
						LeafId:                    "proj-1",
						Runtime:                   "native",
						HeartbeatIntervalSeconds:  300,
						HasCheckpoint:             true,
						CheckpointSequence:        2,
						CheckpointIntervalSeconds: 60,
					},
				},
			}, nil
		},
		getCheckpointFn: func(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
			getCheckpointCalled = true
			return nil, fmt.Errorf("server unavailable")
		},
	}

	executeCalled := false
	mr := &mockRuntime{
		canHandle: true,
		name:      "native",
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			executeCalled = true
			// Verify no checkpoint dir was created (since restore failed).
			_, err := os.Stat(filepath.Join(prep.WorkDir, "checkpoint"))
			if err == nil {
				t.Error("checkpoint dir should not exist when GetCheckpoint fails")
			}
			return &runtime.ExecutionResult{
				OutputData:     []byte("result"),
				OutputChecksum: "abc",
				ExitCode:       0,
				Metrics:        runtime.ExecutionMetrics{WallClockSeconds: 1},
			}, nil
		},
		prepareFn: func(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			return &runtime.PrepareResult{WorkDir: t.TempDir()}, nil
		},
	}

	d := newTestDaemon(mc, mr)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	d.Run(ctx)

	if !getCheckpointCalled {
		t.Error("GetCheckpoint should have been called")
	}
	if !executeCalled {
		t.Error("Execute should have been called despite GetCheckpoint failure")
	}
}

func TestDaemonGetCurrentTasks_WithCheckpoint(t *testing.T) {
	// Test that GetCurrentTasks returns checkpoint metadata while a work unit
	// with checkpointing is executing.
	executionStarted := make(chan struct{})
	executionBlock := make(chan struct{})

	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:                "6d237510-ab4e-4073-86fa-30da90429d51", // was wu-current-1
						LeafId:                    "proj-current",
						Runtime:                   "native",
						HeartbeatIntervalSeconds:  300,
						HasCheckpoint:             true,
						CheckpointSequence:        3,
						CheckpointIntervalSeconds: 1,
					},
				},
			}, nil
		},
		getCheckpointFn: func(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
			return &lettucev1.GetCheckpointResponse{HasCheckpoint: false}, nil
		},
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	mr := &mockRuntime{
		canHandle: true,
		name:      "native",
		prepareFn: func(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			workDir := t.TempDir()
			// Create checkpoint data for the checkpoint manager.
			checkpointDir := filepath.Join(workDir, "checkpoint")
			os.MkdirAll(checkpointDir, 0755)
			os.WriteFile(filepath.Join(checkpointDir, "state.bin"), []byte("data"), 0644)
			return &runtime.PrepareResult{WorkDir: workDir}, nil
		},
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			close(executionStarted)
			<-executionBlock
			return &runtime.ExecutionResult{
				OutputData:     []byte("result"),
				OutputChecksum: "abc",
				ExitCode:       0,
				Metrics:        runtime.ExecutionMetrics{WallClockSeconds: 5},
			}, nil
		},
	}

	d := newTestDaemon(mc, mr)
	ctx, cancel := context.WithCancel(context.Background())

	go d.Run(ctx)

	// Wait for execution to start.
	<-executionStarted

	// Wait a moment for checkpoint goroutine to run.
	time.Sleep(1500 * time.Millisecond)

	tasks := d.GetCurrentTasks()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 current task, got %d", len(tasks))
	}

	task := tasks[0]
	if task.WorkUnitID != "6d237510-ab4e-4073-86fa-30da90429d51" {
		t.Errorf("WorkUnitID = %q, want %q", task.WorkUnitID, "6d237510-ab4e-4073-86fa-30da90429d51")
	}
	if task.LeafID != "proj-current" {
		t.Errorf("LeafID = %q, want %q", task.LeafID, "proj-current")
	}
	if task.CheckpointSequence < 1 {
		t.Errorf("CheckpointSequence = %d, expected >= 1 after checkpoint save", task.CheckpointSequence)
	}
	if task.LastCheckpointAt.IsZero() {
		t.Error("LastCheckpointAt should be non-zero after a successful checkpoint save")
	}

	// Cleanup: unblock execution and cancel.
	close(executionBlock)
	cancel()
	// Wait a bit for cleanup.
	time.Sleep(100 * time.Millisecond)
}

func TestDaemonGetCurrentTasks_NoTask(t *testing.T) {
	mc := &mockClient{}
	mr := &mockRuntime{canHandle: true, name: "native"}

	d := newTestDaemon(mc, mr)

	tasks := d.GetCurrentTasks()
	if tasks != nil {
		t.Errorf("expected nil tasks when no work unit running, got %v", tasks)
	}
}
