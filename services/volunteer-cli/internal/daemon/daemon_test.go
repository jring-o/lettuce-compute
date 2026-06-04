package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- Mock client (satisfies WorkClient interface) ---

type mockClient struct {
	requestWorkUnitFn func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error)
	submitResultFn    func(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error)
	startWorkFn       func(ctx context.Context, req *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error)
	saveCheckpointFn  func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error)
	getCheckpointFn   func(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error)
	getHeadInfoFn     func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error)

	abandonFn func(ctx context.Context, req *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error)

	mu             sync.Mutex
	requestCalls   int
	submitCalls    int
	startWorkCalls int
	abandonCalls   int
	lastSubmitReq  *lettucev1.SubmitResultRequest
	lastAbandonReq *lettucev1.AbandonWorkUnitRequest
}

func (m *mockClient) Close() error { return nil }

func (m *mockClient) RequestWorkUnit(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
	m.mu.Lock()
	m.requestCalls++
	m.mu.Unlock()
	if m.requestWorkUnitFn != nil {
		return m.requestWorkUnitFn(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "no work available")
}

func (m *mockClient) SubmitResult(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
	m.mu.Lock()
	m.submitCalls++
	m.lastSubmitReq = req
	m.mu.Unlock()
	if m.submitResultFn != nil {
		return m.submitResultFn(ctx, req)
	}
	return &lettucev1.SubmitResultResponse{ResultId: "result-1", Accepted: true}, nil
}

func (m *mockClient) StartWork(ctx context.Context, req *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error) {
	m.mu.Lock()
	m.startWorkCalls++
	m.mu.Unlock()
	if m.startWorkFn != nil {
		return m.startWorkFn(ctx, req)
	}
	return &lettucev1.StartWorkResponse{Ok: true}, nil
}

func (m *mockClient) SaveCheckpoint(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
	if m.saveCheckpointFn != nil {
		return m.saveCheckpointFn(ctx, req)
	}
	return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
}

func (m *mockClient) GetCheckpoint(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
	if m.getCheckpointFn != nil {
		return m.getCheckpointFn(ctx, req)
	}
	return &lettucev1.GetCheckpointResponse{HasCheckpoint: false}, nil
}

func (m *mockClient) GetHeadInfo(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
	if m.getHeadInfoFn != nil {
		return m.getHeadInfoFn(ctx, req)
	}
	return &lettucev1.GetHeadInfoResponse{}, nil
}

func (m *mockClient) AbandonWorkUnit(ctx context.Context, req *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error) {
	m.mu.Lock()
	m.abandonCalls++
	m.lastAbandonReq = req
	m.mu.Unlock()
	if m.abandonFn != nil {
		return m.abandonFn(ctx, req)
	}
	return &lettucev1.AbandonWorkUnitResponse{Requeued: true, Message: "mock requeued"}, nil
}

func (m *mockClient) getAbandonCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.abandonCalls
}

func (m *mockClient) getRequestCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requestCalls
}

func (m *mockClient) getSubmitCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.submitCalls
}

func (m *mockClient) getStartWorkCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startWorkCalls
}

// --- Mock runtime ---

type mockRuntime struct {
	prepareFn func(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error)
	executeFn func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error)
	cleanupFn func(prep *runtime.PrepareResult) error
	canHandle bool
	name      string

	mu           sync.Mutex
	prepareCalls int
	executeCalls int
	cleanupCalls int
}

func (m *mockRuntime) Prepare(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
	m.mu.Lock()
	m.prepareCalls++
	m.mu.Unlock()
	if m.prepareFn != nil {
		return m.prepareFn(ctx, wu)
	}
	return &runtime.PrepareResult{WorkDir: "/tmp/work"}, nil
}

func (m *mockRuntime) Execute(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
	m.mu.Lock()
	m.executeCalls++
	m.mu.Unlock()
	if m.executeFn != nil {
		return m.executeFn(ctx, wu, prep)
	}
	return &runtime.ExecutionResult{
		OutputData:     []byte("output"),
		OutputChecksum: "abc123",
		ExitCode:       0,
		Metrics: runtime.ExecutionMetrics{
			WallClockSeconds: 10,
			CPUSecondsUser:   8.0,
		},
	}, nil
}

func (m *mockRuntime) Cleanup(prep *runtime.PrepareResult) error {
	m.mu.Lock()
	m.cleanupCalls++
	m.mu.Unlock()
	if m.cleanupFn != nil {
		return m.cleanupFn(prep)
	}
	return nil
}

func (m *mockRuntime) CanHandle(spec *runtime.ExecutionSpec) bool {
	return m.canHandle
}

func (m *mockRuntime) Name() string {
	if m.name != "" {
		return m.name
	}
	return "native"
}

func (m *mockRuntime) getPrepareCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prepareCalls
}

func (m *mockRuntime) getExecuteCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.executeCalls
}

func (m *mockRuntime) getCleanupCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cleanupCalls
}

// newTestDaemon creates a Daemon with mock client and runtime, using fast backoffs for tests.
func newTestDaemon(mc *mockClient, mr *mockRuntime) *Daemon {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = os.TempDir()
	cfg.Thermal.Enabled = false

	d := NewDaemon(DaemonConfig{
		Config:      cfg,
		PubKey:      pub,
		PrivKey:     priv,
		Client:      mc,
		Runtime:     mr,
		VolunteerID: "test-volunteer-id",
		Logger:      logger,
	})
	// Use fast backoffs for tests.
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond
	d.multiClient.SetBackoff(1*time.Millisecond, 16*time.Millisecond)
	// Disable the fetcher's inter-request throttle so short-window tests aren't
	// paced by the 2s production floor. Negative = gate off (see resolveMinInterval).
	return d
}

// --- Tests ---

func TestDaemonExecuteCycle(t *testing.T) {
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
						WorkUnitId:               "dc5ff9da-f084-4dd7-86b8-e829669814f8", // was wu-1
						LeafId:                   "proj-1",
						Runtime:                  "native",
						InputData:                []byte("input"),
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
	}
	mr := &mockRuntime{canHandle: true}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	if mr.getPrepareCalls() != 1 {
		t.Errorf("prepare calls = %d, want 1", mr.getPrepareCalls())
	}
	if mr.getExecuteCalls() != 1 {
		t.Errorf("execute calls = %d, want 1", mr.getExecuteCalls())
	}
	if mc.getSubmitCalls() != 1 {
		t.Errorf("submit calls = %d, want 1", mc.getSubmitCalls())
	}
	if mr.getCleanupCalls() != 1 {
		t.Errorf("cleanup calls = %d, want 1", mr.getCleanupCalls())
	}
}

func TestDaemonBackoffOnNotFound(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work available")
		},
	}
	mr := &mockRuntime{canHandle: true}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	calls := mc.getRequestCalls()
	if calls < 2 {
		t.Errorf("expected at least 2 request calls with backoff, got %d", calls)
	}
	if mr.getExecuteCalls() != 0 {
		t.Errorf("execute calls = %d, want 0", mr.getExecuteCalls())
	}
}

func TestDaemonGracefulShutdown(t *testing.T) {
	executionStarted := make(chan struct{})
	executionDone := make(chan struct{})
	var startOnce, doneOnce sync.Once

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
						WorkUnitId:               "dc5ff9da-f084-4dd7-86b8-e829669814f8", // was wu-1
						LeafId:                   "proj-1",
						Runtime:                  "native",
						InputData:                []byte("input"),
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
	}
	mr := &mockRuntime{
		canHandle: true,
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			startOnce.Do(func() { close(executionStarted) })
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
			}
			doneOnce.Do(func() { close(executionDone) })
			return &runtime.ExecutionResult{
				OutputData:     []byte("result"),
				OutputChecksum: "checksum",
				ExitCode:       0,
				Metrics:        runtime.ExecutionMetrics{WallClockSeconds: 1},
			}, nil
		},
	}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx)
	}()

	<-executionStarted
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop within timeout")
	}

	<-executionDone
	if mc.getSubmitCalls() != 1 {
		t.Errorf("submit calls = %d, want 1 (should finish work unit on shutdown)", mc.getSubmitCalls())
	}
}

func disabledTestDaemonHeartbeatAbort(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId: "dc5ff9da-f084-4dd7-86b8-e829669814f8", // was wu-1
						LeafId:     "proj-1",
						Runtime:    "native",
						InputData:  []byte("input"),
						ExecutionSpec: &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
		startWorkFn: func(ctx context.Context, req *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error) {
			return &lettucev1.StartWorkResponse{Ok: false, Message: "leaf paused"}, nil
		},
	}
	_ = mc
	// Body retired: the per-task heartbeat abort path no longer exists. WP-VOL will
	// add a StartWork-drop test once the slot wires the StartWork call.
}

func TestDaemonCanHandleSkip(t *testing.T) {
	callCount := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			callCount++
			if callCount > 3 {
				return nil, status.Error(codes.NotFound, "no work")
			}
			return &lettucev1.RequestWorkUnitResponse{
				Assignments: []*lettucev1.WorkUnitAssignment{
					{
						WorkUnitId:    fmt.Sprintf("00000000-0000-4000-8000-%012d", callCount),
						LeafId:        "proj-1",
						Runtime:       "container",
						InputData:     []byte("input"),
						ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ubuntu:latest"},
					},
				},
			}, nil
		},
	}
	mr := &mockRuntime{canHandle: false}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	if mc.getRequestCalls() < 3 {
		t.Errorf("request calls = %d, want >= 3", mc.getRequestCalls())
	}
	if mr.getExecuteCalls() != 0 {
		t.Errorf("execute calls = %d, want 0 (CanHandle returned false)", mr.getExecuteCalls())
	}
}

func TestDaemonStop(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	mr := &mockRuntime{canHandle: true}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	d.Stop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("daemon did not stop within timeout")
	}
}

func TestPIDFileLifecycle(t *testing.T) {
	dir := t.TempDir()

	err := WritePID(dir)
	if err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	pid, err := ReadPID(dir)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("PID = %d, want %d", pid, os.Getpid())
	}

	if !IsProcessRunning(pid) {
		t.Error("IsProcessRunning returned false for current process")
	}

	if IsProcessRunning(99999999) {
		t.Error("IsProcessRunning returned true for non-existent PID")
	}

	RemovePID(dir)

	_, err = ReadPID(dir)
	if err == nil {
		t.Error("ReadPID succeeded after RemovePID")
	}
}

func TestNewDaemon(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()

	mc := &mockClient{}
	d := NewDaemon(DaemonConfig{
		Config:  cfg,
		PubKey:  pub,
		PrivKey: priv,
		Servers: []*ServerConnection{{
			Client:      mc,
			VolunteerID: "vol-123",
			Name:        "test-server",
			Available:   true,
		}},
		Runtime: &mockRuntime{canHandle: true},
		Logger:  logger,
	})

	if d == nil {
		t.Fatal("NewDaemon returned nil")
	}
	servers := d.multiClient.Servers()
	if len(servers) != 1 {
		t.Fatalf("server count = %d, want 1", len(servers))
	}
	if servers[0].VolunteerID != "vol-123" {
		t.Errorf("volunteerID = %q, want vol-123", servers[0].VolunteerID)
	}
	if d.IsRunning() {
		t.Error("new daemon should not be running")
	}
}

func TestIsRunning(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	mr := &mockRuntime{canHandle: true}
	d := newTestDaemon(mc, mr)

	if d.IsRunning() {
		t.Error("expected not running before start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})

	go func() {
		close(started)
		d.Run(ctx)
		close(done)
	}()

	<-started
	time.Sleep(10 * time.Millisecond)

	if !d.IsRunning() {
		t.Error("expected running after start")
	}

	cancel()
	<-done

	if d.IsRunning() {
		t.Error("expected not running after stop")
	}
}

func TestDaemonNonZeroExitCode(t *testing.T) {
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
						WorkUnitId:               "b3d35f5a-68fa-4003-85b0-f8aef5448fca", // was wu-fail
						LeafId:                   "proj-1",
						Runtime:                  "native",
						InputData:                []byte("input"),
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
	}
	mr := &mockRuntime{
		canHandle: true,
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			return &runtime.ExecutionResult{
				OutputData:     []byte("partial"),
				OutputChecksum: "abc",
				ExitCode:       1,
				Metrics:        runtime.ExecutionMetrics{WallClockSeconds: 5},
			}, nil
		},
	}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	if mr.getExecuteCalls() != 1 {
		t.Errorf("execute calls = %d, want 1", mr.getExecuteCalls())
	}
	if mc.getSubmitCalls() != 0 {
		t.Errorf("submit calls = %d, want 0 (non-zero exit code should not submit)", mc.getSubmitCalls())
	}
	if mr.getCleanupCalls() != 1 {
		t.Errorf("cleanup calls = %d, want 1", mr.getCleanupCalls())
	}
}

func TestDaemonPrepareFails(t *testing.T) {
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
						WorkUnitId:               "37d3687a-7192-41a8-87da-8721acd0bc76", // was wu-prep-fail
						LeafId:                   "proj-1",
						Runtime:                  "native",
						InputData:                []byte("input"),
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
	}
	mr := &mockRuntime{
		canHandle: true,
		prepareFn: func(ctx context.Context, wu *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			return nil, fmt.Errorf("download failed: connection refused")
		},
	}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	if mr.getPrepareCalls() != 1 {
		t.Errorf("prepare calls = %d, want 1", mr.getPrepareCalls())
	}
	if mr.getExecuteCalls() != 0 {
		t.Errorf("execute calls = %d, want 0 (prepare failed)", mr.getExecuteCalls())
	}
	if mc.getSubmitCalls() != 0 {
		t.Errorf("submit calls = %d, want 0 (prepare failed)", mc.getSubmitCalls())
	}
}

func TestDaemonGenericError(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.Internal, "server is down")
		},
	}
	mr := &mockRuntime{canHandle: true}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	if mc.getRequestCalls() < 2 {
		t.Errorf("expected at least 2 request calls on generic error, got %d", mc.getRequestCalls())
	}
	if mr.getExecuteCalls() != 0 {
		t.Errorf("execute calls = %d, want 0", mr.getExecuteCalls())
	}
}

func TestDaemonMultipleWorkUnits(t *testing.T) {
	callCount := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			callCount++
			if callCount <= 3 {
				return &lettucev1.RequestWorkUnitResponse{
					Assignments: []*lettucev1.WorkUnitAssignment{
						{
							WorkUnitId:               fmt.Sprintf("00000000-0000-4000-8000-%012d", callCount),
							LeafId:                   "proj-1",
							Runtime:                  "native",
							InputData:                []byte(fmt.Sprintf("input-%d", callCount)),
							ExecutionSpec:            &lettucev1.ExecutionSpec{},
						},
					},
				}, nil
			}
			return nil, status.Error(codes.NotFound, "no more work")
		},
	}
	mr := &mockRuntime{canHandle: true}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	d.Run(ctx)

	if mr.getPrepareCalls() != 3 {
		t.Errorf("prepare calls = %d, want 3", mr.getPrepareCalls())
	}
	if mr.getExecuteCalls() != 3 {
		t.Errorf("execute calls = %d, want 3", mr.getExecuteCalls())
	}
	if mc.getSubmitCalls() != 3 {
		t.Errorf("submit calls = %d, want 3", mc.getSubmitCalls())
	}
	if mr.getCleanupCalls() != 3 {
		t.Errorf("cleanup calls = %d, want 3", mr.getCleanupCalls())
	}
}

func TestPIDFileInvalidContent(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(dir+"/daemon.pid", []byte("not-a-number"), 0644); err != nil {
		t.Fatalf("writing invalid PID: %v", err)
	}

	_, err := ReadPID(dir)
	if err == nil {
		t.Error("ReadPID should fail on invalid content")
	}
}

func TestPIDFileNonExistentDir(t *testing.T) {
	dir := t.TempDir()
	nestedDir := dir + "/a/b/c"

	if err := WritePID(nestedDir); err != nil {
		t.Fatalf("WritePID with nested dir: %v", err)
	}

	pid, err := ReadPID(nestedDir)
	if err != nil {
		t.Fatalf("ReadPID after WritePID in nested dir: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("PID = %d, want %d", pid, os.Getpid())
	}
}

func TestDaemonCleanupAlwaysCalledOnExecFailure(t *testing.T) {
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
						WorkUnitId:               "eb494575-e87d-4acc-8346-5e0fcb10ce87", // was wu-exec-fail
						LeafId:                   "proj-1",
						Runtime:                  "native",
						InputData:                []byte("input"),
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
	}
	mr := &mockRuntime{
		canHandle: true,
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			return nil, fmt.Errorf("segmentation fault")
		},
	}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	if mr.getExecuteCalls() != 1 {
		t.Errorf("execute calls = %d, want 1", mr.getExecuteCalls())
	}
	if mc.getSubmitCalls() != 0 {
		t.Errorf("submit calls = %d, want 0 (execution failed)", mc.getSubmitCalls())
	}
	if mr.getCleanupCalls() != 1 {
		t.Errorf("cleanup calls = %d, want 1 (cleanup should always run)", mr.getCleanupCalls())
	}
}

func TestRemovePIDIdempotent(t *testing.T) {
	dir := t.TempDir()

	RemovePID(dir)

	if err := WritePID(dir); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	RemovePID(dir)
	RemovePID(dir)
}

func TestDaemonSubmitFails(t *testing.T) {
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
						WorkUnitId:               "2fab6bd1-67d8-40ab-80b3-1e6cd67f46cc", // was wu-submit-fail
						LeafId:                   "proj-1",
						Runtime:                  "native",
						InputData:                []byte("input"),
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
		submitResultFn: func(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
			return nil, status.Error(codes.Unavailable, "server temporarily unavailable")
		},
	}
	mr := &mockRuntime{canHandle: true}

	d := newTestDaemon(mc, mr)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	if mr.getExecuteCalls() != 1 {
		t.Errorf("execute calls = %d, want 1", mr.getExecuteCalls())
	}
	if mc.getSubmitCalls() != 1 {
		t.Errorf("submit calls = %d, want 1", mc.getSubmitCalls())
	}
	if mr.getCleanupCalls() != 1 {
		t.Errorf("cleanup calls = %d, want 1", mr.getCleanupCalls())
	}
}

// --- S28 Resource Limits & Scheduling coverage tests ---

// testLimiter is a mock limiter for daemon tests.
type testLimiter struct {
	mu           sync.Mutex
	diskErr      error
	applyCalls   int
	enforceCalls int
}

func (tl *testLimiter) Apply(_ *exec.Cmd, _ *config.ResourceLimits) error {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.applyCalls++
	return nil
}

func (tl *testLimiter) Enforce(_ int, _ *config.ResourceLimits) (func(), error) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.enforceCalls++
	return func() {}, nil
}

func (tl *testLimiter) CheckDiskSpace(_ string, _ int) error {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.diskErr
}

func (tl *testLimiter) setDiskErr(err error) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.diskErr = err
}

// newTestDaemonWithResources creates a daemon with explicit limiter and scheduler.
func newTestDaemonWithResources(mc *mockClient, mr *mockRuntime, limiter resource.Limiter, scheduler *resource.Scheduler) *Daemon {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = os.TempDir()
	cfg.Thermal.Enabled = false

	d := NewDaemon(DaemonConfig{
		Config:      cfg,
		PubKey:      pub,
		PrivKey:     priv,
		Client:      mc,
		Runtime:     mr,
		VolunteerID: "test-volunteer-id",
		Logger:      logger,
		Limiter:     limiter,
		Scheduler:   scheduler,
	})
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond
	d.multiClient.SetBackoff(1*time.Millisecond, 16*time.Millisecond)
	// Disable the fetcher inter-request throttle for fast tests (see resolveMinInterval).
	return d
}

func TestNewDaemon_WithExplicitLimiterAndScheduler(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()

	limiter := &testLimiter{}
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, logger)

	d := NewDaemon(DaemonConfig{
		Config:      cfg,
		PubKey:      pub,
		PrivKey:     priv,
		Client:      nil,
		Runtime:     &mockRuntime{canHandle: true},
		VolunteerID: "vol-explicit",
		Logger:      logger,
		Limiter:     limiter,
		Scheduler:   scheduler,
	})

	if d == nil {
		t.Fatal("NewDaemon returned nil")
	}
	if d.limiter != limiter {
		t.Error("NewDaemon should use provided limiter")
	}
	if d.scheduler != scheduler {
		t.Error("NewDaemon should use provided scheduler")
	}
}

func TestDaemonDiskSpaceCheckPausesExecution(t *testing.T) {
	// When disk space is low, the daemon should back off instead of requesting work.
	limiter := &testLimiter{diskErr: fmt.Errorf("insufficient disk space: 500 MB available, 10240 MB required")}
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, slog.Default())

	mc := &mockClient{}
	mr := &mockRuntime{canHandle: true}

	d := newTestDaemonWithResources(mc, mr, limiter, scheduler)

	// Use a very short timeout so we don't wait the full 60s disk backoff.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	// No work should be requested because disk space check fails first.
	if mc.getRequestCalls() != 0 {
		t.Errorf("request calls = %d, want 0 (disk space check should prevent work requests)", mc.getRequestCalls())
	}
}

func TestDaemonSchedulerPreventsWork(t *testing.T) {
	// When the scheduler says not to run, daemon should wait.
	limiter := &testLimiter{}
	scheduler := resource.NewScheduler(&config.Scheduling{
		Mode:              "WHEN_IDLE",
		IdleThresholdMins: 5,
	}, slog.Default())
	// Inject: never idle.
	scheduler.SetIdleFunc(func() (int, error) { return 0, nil })

	mc := &mockClient{}
	mr := &mockRuntime{canHandle: true}

	d := newTestDaemonWithResources(mc, mr, limiter, scheduler)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	// The scheduler says not to run, so the daemon should not request work.
	if mc.getRequestCalls() != 0 {
		t.Errorf("request calls = %d, want 0 (scheduler should block work requests)", mc.getRequestCalls())
	}
}

func TestDaemonLeafPreferences_Specific(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			// Verify leaf IDs are passed through.
			if len(req.LeafIds) != 2 || req.LeafIds[0] != "proj-a" || req.LeafIds[1] != "proj-b" {
				t.Errorf("LeafIds = %v, want [proj-a, proj-b]", req.LeafIds)
			}
			if len(req.BlockedLeafIds) != 0 {
				t.Errorf("BlockedLeafIds = %v, want empty", req.BlockedLeafIds)
			}
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	mr := &mockRuntime{canHandle: true}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = os.TempDir()
	cfg.Leafs.Mode = "SPECIFIC"
	cfg.Leafs.LeafIDs = []string{"proj-a", "proj-b"}

	d := NewDaemon(DaemonConfig{
		Config:      cfg,
		PubKey:      pub,
		PrivKey:     priv,
		Client:      mc,
		Runtime:     mr,
		VolunteerID: "test-vol",
		Logger:      logger,
	})
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	if mc.getRequestCalls() < 1 {
		t.Errorf("expected at least 1 request call, got %d", mc.getRequestCalls())
	}
}

func TestDaemonLeafPreferences_Blocklist(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if len(req.BlockedLeafIds) != 1 || req.BlockedLeafIds[0] != "proj-blocked" {
				t.Errorf("BlockedLeafIds = %v, want [proj-blocked]", req.BlockedLeafIds)
			}
			if len(req.LeafIds) != 0 {
				t.Errorf("LeafIds = %v, want empty", req.LeafIds)
			}
			return nil, status.Error(codes.NotFound, "no work")
		},
	}
	mr := &mockRuntime{canHandle: true}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = os.TempDir()
	cfg.Leafs.Mode = "BLOCKLIST"
	cfg.Leafs.BlockedIDs = []string{"proj-blocked"}

	d := NewDaemon(DaemonConfig{
		Config:      cfg,
		PubKey:      pub,
		PrivKey:     priv,
		Client:      mc,
		Runtime:     mr,
		VolunteerID: "test-vol",
		Logger:      logger,
	})
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	if mc.getRequestCalls() < 1 {
		t.Errorf("expected at least 1 request call, got %d", mc.getRequestCalls())
	}
}

func TestStopProcess_NonExistentPID(t *testing.T) {
	err := StopProcess(99999999)
	if err == nil {
		t.Error("StopProcess should fail for non-existent PID")
	}
}

func TestSetBackoff(t *testing.T) {
	mc := &mockClient{}
	mr := &mockRuntime{canHandle: true}
	d := newTestDaemon(mc, mr)

	d.SetBackoff(5*time.Second, 60*time.Second)
	if d.initialBackoff != 5*time.Second {
		t.Errorf("initialBackoff = %v, want 5s", d.initialBackoff)
	}
	if d.maxBackoff != 60*time.Second {
		t.Errorf("maxBackoff = %v, want 60s", d.maxBackoff)
	}
}

// --- History tracking tests ---

func TestDaemonWritesHistory(t *testing.T) {
	dir := t.TempDir()
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
						WorkUnitId:               "30ee01ce-b932-4e58-8d58-5bcce572681b", // was wu-hist-1
						LeafId:                   "proj-1",
						Runtime:                  "native",
						InputData:                []byte("input"),
						ExecutionSpec:            &lettucev1.ExecutionSpec{},
					},
				},
			}, nil
		},
	}
	mr := &mockRuntime{canHandle: true}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := NewDaemon(DaemonConfig{
		Config:      cfg,
		PubKey:      pub,
		PrivKey:     priv,
		Client:      mc,
		Runtime:     mr,
		VolunteerID: "vol-hist",
		Logger:      logger,
	})
	d.initialBackoff = 1 * time.Millisecond
	d.maxBackoff = 16 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d.Run(ctx)

	// Verify history was written.
	entries, err := ReadHistory(dir, 10)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("history entries = %d, want 1", len(entries))
	}
	if entries[0].WorkUnitID != "30ee01ce-b932-4e58-8d58-5bcce572681b" {
		t.Errorf("work_unit_id = %q, want wu-hist-1", entries[0].WorkUnitID)
	}
	if entries[0].LeafID != "proj-1" {
		t.Errorf("leaf_id = %q, want proj-1", entries[0].LeafID)
	}
	if !entries[0].ResultAccepted {
		t.Error("result_accepted = false, want true")
	}
}

func TestHistoryFileOperations(t *testing.T) {
	dir := t.TempDir()

	// Read from nonexistent file should return nil.
	entries, err := ReadHistory(dir, 10)
	if err != nil {
		t.Fatalf("ReadHistory empty: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries, got %d", len(entries))
	}

	// Write entries.
	for i := 0; i < 5; i++ {
		if err := AppendHistory(dir, HistoryEntry{
			WorkUnitID:       fmt.Sprintf("wu-%d", i),
			LeafID:           "proj-1",
			CompletedAt:      time.Now().UTC(),
			WallClockSeconds: int64(i + 1),
			ResultAccepted:   i%2 == 0,
		}); err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
	}

	// Read with limit.
	entries, err = ReadHistory(dir, 3)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	// Newest first.
	if entries[0].WorkUnitID != "wu-4" {
		t.Errorf("first = %q, want wu-4", entries[0].WorkUnitID)
	}

	// Read all.
	entries, err = ReadHistory(dir, 50)
	if err != nil {
		t.Fatalf("ReadHistory all: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("entries = %d, want 5", len(entries))
	}
}

func TestReadHistoryDefaultLimit(t *testing.T) {
	dir := t.TempDir()

	for i := 0; i < 3; i++ {
		if err := AppendHistory(dir, HistoryEntry{
			WorkUnitID:       fmt.Sprintf("wu-%d", i),
			LeafID:           "proj-1",
			CompletedAt:      time.Now().UTC(),
			WallClockSeconds: int64(i + 1),
			ResultAccepted:   true,
		}); err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
	}

	// Pass 0 as limit Ã¢â‚¬â€ should use default (50) and return all entries.
	entries, err := ReadHistory(dir, 0)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
}

func TestReadHistoryMalformedLines(t *testing.T) {
	dir := t.TempDir()

	// Write a mix of valid and malformed lines.
	path := HistoryFilePath(dir)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"work_unit_id":"dc5ff9da-f084-4dd7-86b8-e829669814f8","leaf_id":"proj-1","completed_at":"2026-01-01T00:00:00Z","wall_clock_seconds":10,"result_accepted":true}` + "\n")
	f.WriteString("this is not json\n")
	f.WriteString("\n") // empty line
	f.WriteString(`{"work_unit_id":"be55d0b1-40f5-41f6-8037-448e86bcda6d","leaf_id":"proj-1","completed_at":"2026-01-02T00:00:00Z","wall_clock_seconds":20,"result_accepted":false}` + "\n")
	f.Close()

	entries, err := ReadHistory(dir, 50)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	// Only 2 valid entries should be returned (malformed and empty lines skipped).
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// Newest first.
	if entries[0].WorkUnitID != "be55d0b1-40f5-41f6-8037-448e86bcda6d" {
		t.Errorf("first = %q, want wu-2", entries[0].WorkUnitID)
	}
	if entries[1].WorkUnitID != "dc5ff9da-f084-4dd7-86b8-e829669814f8" {
		t.Errorf("second = %q, want wu-1", entries[1].WorkUnitID)
	}
}

func TestHistoryFilePath(t *testing.T) {
	got := HistoryFilePath("/tmp/data")
	if !strings.Contains(got, "history.jsonl") {
		t.Errorf("HistoryFilePath = %q, want to contain history.jsonl", got)
	}
}

func TestDaemon_CanAccommodateWU(t *testing.T) {
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	d.slotManager = NewSlotManager(4, d.logger)

	// No memory limit configured Ã¢â‚¬â€ always allow.
	d.cfg.ResourceLimits.MaxMemoryMB = 0
	wu := &runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 4096}}
	if !d.canAccommodateWU(wu) {
		t.Error("should accommodate when no memory limit configured")
	}

	// Set memory limit to 8GB.
	d.cfg.ResourceLimits.MaxMemoryMB = 8192

	// WU with 4GB Ã¢â‚¬â€ should fit (0 active + 4096 <= 8192).
	if !d.canAccommodateWU(wu) {
		t.Error("should accommodate 4GB WU with 8GB limit and 0 active")
	}

	// Simulate an active slot using 6GB by starting a blocking WU.
	blockCh := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slotID := <-d.slotManager.available
	d.slotManager.StartSlot(ctx, slotID, &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID: "wu-active", LeafID: "proj-1",
			ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 6144},
		},
		WUResp: &lettucev1.WorkUnitAssignment{},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/active"},
		Runtime: &mockRuntime{canHandle: true, executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			<-blockCh
			return &runtime.ExecutionResult{ExitCode: 0, OutputData: []byte("ok")}, nil
		}},
		Conn:      &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
		FetchedAt: time.Now(),
	}, d)

	time.Sleep(50 * time.Millisecond)

	// 6GB active + 4GB WU = 10GB > 8GB limit Ã¢â‚¬â€ should NOT accommodate.
	if d.canAccommodateWU(wu) {
		t.Error("should NOT accommodate 4GB WU when 6GB active and 8GB limit")
	}

	// Smaller WU (1GB) should fit: 6GB + 1GB = 7GB <= 8GB.
	smallWU := &runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 1024}}
	if !d.canAccommodateWU(smallWU) {
		t.Error("should accommodate 1GB WU when 6GB active and 8GB limit")
	}

	// WU with 0 MaxMemoryMB should use 512 default: 6GB + 512MB = 6.5GB <= 8GB.
	defaultWU := &runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 0}}
	if !d.canAccommodateWU(defaultWU) {
		t.Error("should accommodate WU with default 512MB when 6GB active and 8GB limit")
	}

	close(blockCh)
	cancel()
}

func TestResolveLeafInfo_FoundInCache(t *testing.T) {
	lc := NewLeafCache(5*time.Minute, testLogger())
	lc.PopulateForTest("test-server", &CachedHeadInfo{
		Name: "Test Head",
		Leafs: []CachedLeafInfo{
			{ID: "leaf-abc", Name: "Prime Gap Study", Slug: "prime-gap-study"},
			{ID: "leaf-xyz", Name: "Protein Folding", Slug: "protein-folding"},
		},
	})

	d := &Daemon{leafCache: lc}

	name, slug := d.resolveLeafInfo("leaf-abc")
	if name != "Prime Gap Study" {
		t.Errorf("resolveLeafInfo name = %q, want %q", name, "Prime Gap Study")
	}
	if slug != "prime-gap-study" {
		t.Errorf("resolveLeafInfo slug = %q, want %q", slug, "prime-gap-study")
	}
}

func TestResolveLeafInfo_NotFoundInCache(t *testing.T) {
	lc := NewLeafCache(5*time.Minute, testLogger())
	lc.PopulateForTest("test-server", &CachedHeadInfo{
		Name: "Test Head",
		Leafs: []CachedLeafInfo{
			{ID: "leaf-abc", Name: "Prime Gap Study", Slug: "prime-gap-study"},
		},
	})

	d := &Daemon{leafCache: lc}

	name, slug := d.resolveLeafInfo("leaf-not-exists")
	if name != "leaf-not-exists" {
		t.Errorf("resolveLeafInfo name = %q, want %q (should return leafID when not found)", name, "leaf-not-exists")
	}
	if slug != "leaf-not-exists" {
		t.Errorf("resolveLeafInfo slug = %q, want %q (should return leafID when not found)", slug, "leaf-not-exists")
	}
}

func TestResolveLeafInfo_NilLeafCache(t *testing.T) {
	d := &Daemon{leafCache: nil}

	name, slug := d.resolveLeafInfo("leaf-123")
	if name != "leaf-123" {
		t.Errorf("resolveLeafInfo name = %q, want %q (should return leafID when cache is nil)", name, "leaf-123")
	}
	if slug != "leaf-123" {
		t.Errorf("resolveLeafInfo slug = %q, want %q (should return leafID when cache is nil)", slug, "leaf-123")
	}
}

func TestResolveLeafInfo_MultipleServers(t *testing.T) {
	lc := NewLeafCache(5*time.Minute, testLogger())
	lc.PopulateForTest("server-a", &CachedHeadInfo{
		Name: "Server A",
		Leafs: []CachedLeafInfo{
			{ID: "leaf-1", Name: "Leaf One", Slug: "leaf-one"},
		},
	})
	lc.PopulateForTest("server-b", &CachedHeadInfo{
		Name: "Server B",
		Leafs: []CachedLeafInfo{
			{ID: "leaf-2", Name: "Leaf Two", Slug: "leaf-two"},
		},
	})

	d := &Daemon{leafCache: lc}

	// Should find leaf from server-b.
	name, slug := d.resolveLeafInfo("leaf-2")
	if name != "Leaf Two" {
		t.Errorf("resolveLeafInfo name = %q, want %q", name, "Leaf Two")
	}
	if slug != "leaf-two" {
		t.Errorf("resolveLeafInfo slug = %q, want %q", slug, "leaf-two")
	}
}

func TestResolveLeafInfo_EmptyCache(t *testing.T) {
	lc := NewLeafCache(5*time.Minute, testLogger())
	// Cache exists but has no servers populated.

	d := &Daemon{leafCache: lc}

	name, slug := d.resolveLeafInfo("leaf-123")
	if name != "leaf-123" {
		t.Errorf("resolveLeafInfo name = %q, want %q (should return leafID for empty cache)", name, "leaf-123")
	}
	if slug != "leaf-123" {
		t.Errorf("resolveLeafInfo slug = %q, want %q (should return leafID for empty cache)", slug, "leaf-123")
	}
}

func TestNewSlotManager_ClampsToOne(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	sm := NewSlotManager(0, logger)
	if len(sm.slots) != 1 {
		t.Errorf("NewSlotManager(0) slots = %d, want 1", len(sm.slots))
	}

	sm2 := NewSlotManager(-5, logger)
	if len(sm2.slots) != 1 {
		t.Errorf("NewSlotManager(-5) slots = %d, want 1", len(sm2.slots))
	}
}
