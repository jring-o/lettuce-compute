package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// --- PersistedTask serialization ---

func TestPersistedTask_PIDAndRscFpopsEst_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	tasks := []PersistedTask{
		{
			WorkUnitID:        "dc5ff9da-f084-4dd7-86b8-e829669814f8",
			LeafID:            "leaf-1",
			ServerGRPCAddress: "localhost:50051",
			ServerName:        "test-server",
			VolunteerID:       "vol-1",
			RuntimeName:       "native",
			WorkDir:           "/tmp/work1",
			BinaryPath:        "/tmp/bin",
			RscFpopsEst:       1.5e12,
			PID:               12345,
			StartedAt:         time.Now().UTC().Truncate(time.Second),
		},
	}

	// Save.
	if err := SaveActiveState(dir, tasks); err != nil {
		t.Fatalf("SaveActiveState: %v", err)
	}

	// Load.
	state, err := LoadActiveState(dir)
	if err != nil {
		t.Fatalf("LoadActiveState: %v", err)
	}
	if state == nil {
		t.Fatal("LoadActiveState returned nil")
	}
	if len(state.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(state.Tasks))
	}

	got := state.Tasks[0]
	if got.PID != 12345 {
		t.Errorf("PID = %d, want 12345", got.PID)
	}
	if got.RscFpopsEst != 1.5e12 {
		t.Errorf("RscFpopsEst = %g, want 1.5e12", got.RscFpopsEst)
	}
	if got.WorkUnitID != "dc5ff9da-f084-4dd7-86b8-e829669814f8" {
		t.Errorf("WorkUnitID = %q, want %q", got.WorkUnitID, "dc5ff9da-f084-4dd7-86b8-e829669814f8")
	}
	if got.LeafID != "leaf-1" {
		t.Errorf("LeafID = %q, want %q", got.LeafID, "leaf-1")
	}
	if got.ServerGRPCAddress != "localhost:50051" {
		t.Errorf("ServerGRPCAddress = %q, want %q", got.ServerGRPCAddress, "localhost:50051")
	}
}

func TestPersistedTask_PIDZero_Omitted(t *testing.T) {
	dir := t.TempDir()

	tasks := []PersistedTask{
		{
			WorkUnitID:  "wu-no-pid",
			LeafID:      "leaf-1",
			RuntimeName: "native",
			WorkDir:     "/tmp/work",
			// PID is 0 â€” should be omitted in JSON.
			// RscFpopsEst is 0 â€” should be omitted in JSON.
		},
	}

	if err := SaveActiveState(dir, tasks); err != nil {
		t.Fatalf("SaveActiveState: %v", err)
	}

	// Read raw JSON and verify omitempty works.
	data, err := os.ReadFile(filepath.Join(dir, "active-tasks.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	jsonStr := string(data)
	// "pid" and "rsc_fpops_est" should NOT appear when zero (omitempty).
	if strings.Contains(jsonStr, `"pid"`) {
		t.Error("JSON contains 'pid' field when PID=0 (omitempty should omit it)")
	}
	if strings.Contains(jsonStr, `"rsc_fpops_est"`) {
		t.Error("JSON contains 'rsc_fpops_est' field when value=0 (omitempty should omit it)")
	}

	// Round-trip should still produce zero values.
	state, err := LoadActiveState(dir)
	if err != nil {
		t.Fatalf("LoadActiveState: %v", err)
	}
	if state.Tasks[0].PID != 0 {
		t.Errorf("PID = %d, want 0", state.Tasks[0].PID)
	}
	if state.Tasks[0].RscFpopsEst != 0 {
		t.Errorf("RscFpopsEst = %g, want 0", state.Tasks[0].RscFpopsEst)
	}
}

func TestLoadActiveState_NoFile(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadActiveState(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected nil state, got %+v", state)
	}
}

func TestClearActiveState(t *testing.T) {
	dir := t.TempDir()

	tasks := []PersistedTask{{WorkUnitID: "wu-clear", LeafID: "leaf-1"}}
	if err := SaveActiveState(dir, tasks); err != nil {
		t.Fatalf("SaveActiveState: %v", err)
	}

	ClearActiveState(dir)

	state, err := LoadActiveState(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected nil after clear, got %+v", state)
	}
}

func TestPersistedTask_VizBundlePath_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	tasks := []PersistedTask{
		{
			WorkUnitID:    "wu-viz",
			LeafID:        "leaf-viz",
			RuntimeName:   "native",
			WorkDir:       "/home/user/.lettuce/work/wu-viz",
			VizBundlePath: "/home/user/.lettuce/work/wu-viz/.lettuce-viz",
			StartedAt:     time.Now().UTC().Truncate(time.Second),
		},
		{
			WorkUnitID:  "wu-no-viz",
			LeafID:      "leaf-plain",
			RuntimeName: "wasm",
			WorkDir:     "/home/user/.lettuce/wasm-work/wu-no-viz",
			// No VizBundlePath â€” should remain empty after round-trip.
			StartedAt: time.Now().UTC().Truncate(time.Second),
		},
	}

	if err := SaveActiveState(dir, tasks); err != nil {
		t.Fatalf("SaveActiveState: %v", err)
	}

	state, err := LoadActiveState(dir)
	if err != nil {
		t.Fatalf("LoadActiveState: %v", err)
	}
	if state == nil || len(state.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %v", state)
	}

	if state.Tasks[0].VizBundlePath != "/home/user/.lettuce/work/wu-viz/.lettuce-viz" {
		t.Errorf("VizBundlePath not preserved: got %q", state.Tasks[0].VizBundlePath)
	}
	if state.Tasks[1].VizBundlePath != "" {
		t.Errorf("expected empty VizBundlePath, got %q", state.Tasks[1].VizBundlePath)
	}

	// Verify omitempty: empty VizBundlePath should not appear in JSON.
	data, err := os.ReadFile(filepath.Join(dir, "active-tasks.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	jsonStr := string(data)
	// First task should contain viz_bundle_path.
	if !strings.Contains(jsonStr, `"viz_bundle_path"`) {
		t.Error("JSON should contain viz_bundle_path for task with viz")
	}
}

func TestSaveActiveState_MultipleTasks(t *testing.T) {
	dir := t.TempDir()

	tasks := []PersistedTask{
		{WorkUnitID: "wu-a", LeafID: "leaf-a", PID: 100, RscFpopsEst: 5e9},
		{WorkUnitID: "wu-b", LeafID: "leaf-b", PID: 200, RscFpopsEst: 3e10},
		{WorkUnitID: "wu-c", LeafID: "leaf-c", PID: 0, RscFpopsEst: 0},
	}

	if err := SaveActiveState(dir, tasks); err != nil {
		t.Fatalf("SaveActiveState: %v", err)
	}

	state, err := LoadActiveState(dir)
	if err != nil {
		t.Fatalf("LoadActiveState: %v", err)
	}
	if len(state.Tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(state.Tasks))
	}

	// Verify PIDs preserved correctly.
	for i, expected := range []int{100, 200, 0} {
		if state.Tasks[i].PID != expected {
			t.Errorf("task[%d].PID = %d, want %d", i, state.Tasks[i].PID, expected)
		}
	}
}

// --- ProcessHandle PID() ---

func TestContainerProcessHandle_PIDReturnsZero(t *testing.T) {
	handle := NewContainerProcessHandle(nil, "container-123")
	if handle.PID() != 0 {
		t.Errorf("containerProcessHandle.PID() = %d, want 0", handle.PID())
	}
}

func TestNativeProcessHandle_PIDReturnsStoredPID(t *testing.T) {
	handle := NewNativeProcessHandle(42)
	if handle.PID() != 42 {
		t.Errorf("nativeProcessHandle.PID() = %d, want 42", handle.PID())
	}
}

// --- GetActivePersistableTasks with PID ---

func makeTestConn() *ServerConnection {
	return &ServerConnection{
		Name:        "test",
		VolunteerID: "vol-1",
		Client:      &mockClient{},
		Config:      config.ServerConfig{GRPCAddress: "localhost:50051"},
	}
}

func TestGetActivePersistableTasks_IncludesPID(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:          "wu-pid-test",
			LeafID:      "leaf-1",
			Runtime:     "native",
			RscFpopsEst: 2.5e11,
		},
		WUResp:  &lettucev1.WorkUnitAssignment{},
		Prep:    &runtime.PrepareResult{WorkDir: "/tmp/wu-pid-test"},
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

	// Set a mock process handle on the slot.
	sm.SetProcessHandle(slotID, &mockProcessHandle{pid: 9999})

	tasks := sm.GetActivePersistableTasks()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}

	if tasks[0].PID != 9999 {
		t.Errorf("PID = %d, want 9999", tasks[0].PID)
	}
	if tasks[0].RscFpopsEst != 2.5e11 {
		t.Errorf("RscFpopsEst = %g, want 2.5e11", tasks[0].RscFpopsEst)
	}
	if tasks[0].WorkUnitID != "wu-pid-test" {
		t.Errorf("WorkUnitID = %q, want %q", tasks[0].WorkUnitID, "wu-pid-test")
	}

	close(blockCh)
	sm.WaitForCompletion(ctx)
}

func TestGetActivePersistableTasks_NilProcessHandle(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-no-handle", LeafID: "leaf-1"},
		WUResp: &lettucev1.WorkUnitAssignment{},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-no-handle"},
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

	// No process handle set â€” PID should be 0.
	tasks := sm.GetActivePersistableTasks()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].PID != 0 {
		t.Errorf("PID = %d, want 0 when no process handle", tasks[0].PID)
	}

	close(blockCh)
	sm.WaitForCompletion(ctx)
}

// --- waitForOrphan ---

func TestWaitForOrphan_ProcessExitsImmediately(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "output.dat")
	if err := os.WriteFile(outputPath, []byte("orphan-result"), 0600); err != nil {
		t.Fatalf("writing output.dat: %v", err)
	}

	// Override isProcessAliveFunc to return false immediately (process already exited).
	origIsAlive := isProcessAliveFunc
	isProcessAliveFunc = func(pid int) bool { return false }
	defer func() { isProcessAliveFunc = origIsAlive }()

	prep := &runtime.PrepareResult{
		WorkDir:   dir,
		OrphanPID: 12345,
	}
	wu := &runtime.WorkUnit{ID: "wu-orphan-immediate", DeadlineSeconds: 0}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := waitForOrphan(ctx, wu, prep)
	if err != nil {
		t.Fatalf("waitForOrphan error: %v", err)
	}
	if result == nil {
		t.Fatal("waitForOrphan returned nil result")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if string(result.OutputData) != "orphan-result" {
		t.Errorf("OutputData = %q, want %q", string(result.OutputData), "orphan-result")
	}
}

func TestWaitForOrphan_ContextCancelled(t *testing.T) {
	// Override isProcessAliveFunc to always return true (process never exits).
	origIsAlive := isProcessAliveFunc
	isProcessAliveFunc = func(pid int) bool { return true }
	defer func() { isProcessAliveFunc = origIsAlive }()

	// A plain cancel (daemon shutdown/stop) must NOT kill the orphan — it stays
	// running-frozen for the preserve/resume path. Record any StopProcess call so we
	// can assert it did not happen.
	var stopCalls int
	origStop := stopProcessFunc
	stopProcessFunc = func(pid int) error { stopCalls++; return nil }
	defer func() { stopProcessFunc = origStop }()

	prep := &runtime.PrepareResult{
		WorkDir:   t.TempDir(),
		OrphanPID: 12345,
	}
	wu := &runtime.WorkUnit{ID: "wu-orphan-cancel", DeadlineSeconds: 0}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := waitForOrphan(ctx, wu, prep)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if stopCalls != 0 {
		t.Errorf("stopProcessFunc called %d times on plain cancel, want 0 (must preserve for resume)", stopCalls)
	}
}

func TestWaitForOrphan_ProcessExitsAfterPolls(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "output.dat")
	if err := os.WriteFile(outputPath, []byte("delayed-result"), 0600); err != nil {
		t.Fatalf("writing output.dat: %v", err)
	}

	// Process stays alive for 2 polls, then exits.
	pollCount := 0
	origIsAlive := isProcessAliveFunc
	isProcessAliveFunc = func(pid int) bool {
		pollCount++
		return pollCount <= 2
	}
	defer func() { isProcessAliveFunc = origIsAlive }()

	prep := &runtime.PrepareResult{
		WorkDir:   dir,
		OrphanPID: 999,
	}
	wu := &runtime.WorkUnit{ID: "wu-orphan-polls", DeadlineSeconds: 0}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := waitForOrphan(ctx, wu, prep)
	if err != nil {
		t.Fatalf("waitForOrphan error: %v", err)
	}
	if string(result.OutputData) != "delayed-result" {
		t.Errorf("OutputData = %q, want %q", string(result.OutputData), "delayed-result")
	}
	if pollCount != 3 {
		t.Errorf("pollCount = %d, want 3 (alive, alive, dead)", pollCount)
	}
}

func TestWaitForOrphan_NoOutputFile(t *testing.T) {
	// No output.dat in work dir. A gone orphan with no readable output is a FAILURE,
	// not a fabricated empty success (BG-26): it must return a non-nil error and nil
	// result rather than submitting stale-or-nil output as a valid result.
	dir := t.TempDir()

	origIsAlive := isProcessAliveFunc
	isProcessAliveFunc = func(pid int) bool { return false }
	defer func() { isProcessAliveFunc = origIsAlive }()

	prep := &runtime.PrepareResult{
		WorkDir:   dir,
		OrphanPID: 777,
	}
	wu := &runtime.WorkUnit{ID: "wu-orphan-no-output", DeadlineSeconds: 0}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := waitForOrphan(ctx, wu, prep)
	if err == nil {
		t.Fatal("expected non-nil error when output.dat is missing after process exit")
	}
	if result != nil {
		t.Errorf("expected nil result on missing output.dat, got %+v", result)
	}
}

func TestWaitForOrphan_DeadlineKillsAndFails(t *testing.T) {
	// A resumed orphan that outlives its deadline must be killed and fail (BG-26 defect
	// A), not loop forever occupying the slot until the next daemon restart.
	origIsAlive := isProcessAliveFunc
	isProcessAliveFunc = func(pid int) bool { return true } // never exits on its own
	defer func() { isProcessAliveFunc = origIsAlive }()

	var stopCalls int
	var stoppedPID int
	origStop := stopProcessFunc
	stopProcessFunc = func(pid int) error { stopCalls++; stoppedPID = pid; return nil }
	defer func() { stopProcessFunc = origStop }()

	prep := &runtime.PrepareResult{
		WorkDir:   t.TempDir(),
		OrphanPID: 4242,
	}
	wu := &runtime.WorkUnit{ID: "wu-orphan-deadline", DeadlineSeconds: 1}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	result, err := waitForOrphan(ctx, wu, prep)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("waitForOrphan took %v, want it to fail on the 1s deadline (within ~3s)", elapsed)
	}
	if err == nil {
		t.Fatal("expected non-nil error when deadline exceeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected errors.Is(err, context.DeadlineExceeded), got %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result on deadline, got %+v", result)
	}
	if stopCalls != 1 {
		t.Errorf("stopProcessFunc called %d times, want exactly 1", stopCalls)
	}
	if stoppedPID != prep.OrphanPID {
		t.Errorf("stopProcessFunc called with PID %d, want %d", stoppedPID, prep.OrphanPID)
	}
}

func TestRunSlot_OrphanDroppedAtRunStartIsKilled(t *testing.T) {
	// A resumed orphan dropped at run-start (StartWork Ok=false) has already been
	// unfrozen; runSlot must kill it before the deferred cleanup deletes its work dir
	// (BG-26b), otherwise it burns CPU unmonitored with its work dir gone.
	logger := newTestLogger()
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	// Defensive: keep the (fake) process reported alive and record any kill so the drop
	// path can never touch a real process.
	origIsAlive := isProcessAliveFunc
	isProcessAliveFunc = func(pid int) bool { return true }
	defer func() { isProcessAliveFunc = origIsAlive }()

	var stopCalls int
	var stoppedPID int
	origStop := stopProcessFunc
	stopProcessFunc = func(pid int) error { stopCalls++; stoppedPID = pid; return nil }
	defer func() { stopProcessFunc = origStop }()

	conn := &ServerConnection{
		Name:        "test",
		VolunteerID: "vol-1",
		Config:      config.ServerConfig{GRPCAddress: "localhost:50051"},
		Client: &mockClient{
			startWorkFn: func(ctx context.Context, req *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error) {
				return &lettucev1.StartWorkResponse{Ok: false, Message: "reassigned"}, nil
			},
		},
	}

	item := &PreFetchItem{
		WU:        &runtime.WorkUnit{ID: "wu-orphan-drop", LeafID: "leaf-1", Runtime: "native"},
		WUResp:    &lettucev1.WorkUnitAssignment{},
		Prep:      &runtime.PrepareResult{WorkDir: t.TempDir(), OrphanPID: 4242},
		Runtime:   &mockRuntime{canHandle: true},
		Conn:      conn,
		FetchedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, item, d)

	result, err := sm.WaitForCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if !errors.Is(result.Err, errStartWorkDropped) {
		t.Errorf("result.Err = %v, want errStartWorkDropped", result.Err)
	}
	if stopCalls != 1 {
		t.Errorf("stopProcessFunc called %d times, want exactly 1 (orphan killed on drop)", stopCalls)
	}
	if stoppedPID != 4242 {
		t.Errorf("stopProcessFunc called with PID %d, want 4242", stoppedPID)
	}
}

// --- Mock ProcessHandle ---

type mockProcessHandle struct {
	pid          int
	suspendErr   error
	resumeErr    error
	suspendCalls int
	resumeCalls  int
}

func (m *mockProcessHandle) Suspend() error {
	m.suspendCalls++
	return m.suspendErr
}

func (m *mockProcessHandle) Resume() error {
	m.resumeCalls++
	return m.resumeErr
}

func (m *mockProcessHandle) PID() int {
	return m.pid
}

// --- SuspendAll and ResumeAll with PID ---

func TestSuspendAll_ResumeAll(t *testing.T) {
	logger := newTestLogger()
	sm := NewSlotManager(2, logger)
	d := newSlotTestDaemon()

	blockCh := make(chan struct{})
	makeItem := func(id string) *PreFetchItem {
		return &PreFetchItem{
			WU:     &runtime.WorkUnit{ID: id, LeafID: "leaf-1"},
			WUResp: &lettucev1.WorkUnitAssignment{},
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
	for _, id := range []string{"wu-s1", "wu-s2"} {
		slotID := <-sm.available
		sm.StartSlot(ctx, slotID, makeItem(id), d)
	}

	time.Sleep(50 * time.Millisecond)

	// Attach mock process handles.
	h1 := &mockProcessHandle{pid: 1001}
	h2 := &mockProcessHandle{pid: 1002}
	sm.SetProcessHandle(0, h1)
	sm.SetProcessHandle(1, h2)

	// SuspendAll.
	sm.SuspendAll()

	if h1.suspendCalls != 1 {
		t.Errorf("h1.suspendCalls = %d, want 1", h1.suspendCalls)
	}
	if h2.suspendCalls != 1 {
		t.Errorf("h2.suspendCalls = %d, want 1", h2.suspendCalls)
	}

	// SuspendAll again should not double-suspend (already suspended).
	sm.SuspendAll()
	if h1.suspendCalls != 1 {
		t.Errorf("h1.suspendCalls after double suspend = %d, want 1", h1.suspendCalls)
	}

	// ResumeAll.
	sm.ResumeAll()

	if h1.resumeCalls != 1 {
		t.Errorf("h1.resumeCalls = %d, want 1", h1.resumeCalls)
	}
	if h2.resumeCalls != 1 {
		t.Errorf("h2.resumeCalls = %d, want 1", h2.resumeCalls)
	}

	// ResumeAll again should not double-resume.
	sm.ResumeAll()
	if h1.resumeCalls != 1 {
		t.Errorf("h1.resumeCalls after double resume = %d, want 1", h1.resumeCalls)
	}

	// Verify PID is captured in persisted tasks after suspend.
	sm.SuspendAll()
	tasks := sm.GetActivePersistableTasks()
	pids := map[int]bool{}
	for _, task := range tasks {
		pids[task.PID] = true
	}
	if !pids[1001] || !pids[1002] {
		t.Errorf("expected PIDs 1001, 1002 in persisted tasks, got %v", pids)
	}

	close(blockCh)
	for i := 0; i < 2; i++ {
		sm.WaitForCompletion(ctx)
	}
}

// --- SuspendAndQuit ---

func TestSuspendAndQuit_SuspendsAndPersists(t *testing.T) {
	// Override os.Exit to prevent actual process termination.
	exitCalled := false
	exitCode := -1
	restore := SetOsExitFunc(func(code int) {
		exitCalled = true
		exitCode = code
	})
	defer restore()

	mc := &mockClient{}
	mr := &mockRuntime{canHandle: true}
	d := newTestDaemon(mc, mr)

	// Give daemon a slot manager with active tasks.
	d.slotManager = NewSlotManager(1, d.logger)

	// Manually set up an active slot with a mock process handle.
	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:          "wu-suspend-test",
			LeafID:      "leaf-1",
			Runtime:     "native",
			RscFpopsEst: 1e10,
		},
		WUResp:  &lettucev1.WorkUnitAssignment{},
		Prep:    &runtime.PrepareResult{WorkDir: t.TempDir()},
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

	slotID := <-d.slotManager.available
	d.slotManager.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)

	// Attach a mock process handle.
	handle := &mockProcessHandle{pid: 5555}
	d.slotManager.SetProcessHandle(slotID, handle)

	// Call SuspendAndQuit.
	d.SuspendAndQuit()

	// Verify: process was suspended.
	if handle.suspendCalls != 1 {
		t.Errorf("suspendCalls = %d, want 1", handle.suspendCalls)
	}

	// Verify: osExitFunc was called with 0.
	if !exitCalled {
		t.Error("expected osExitFunc to be called")
	}
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}

	// Verify: active tasks were persisted with PID.
	state, err := LoadActiveState(d.cfg.DataDir)
	if err != nil {
		t.Fatalf("LoadActiveState: %v", err)
	}
	if state == nil {
		t.Fatal("expected persisted state, got nil")
	}
	if len(state.Tasks) != 1 {
		t.Fatalf("expected 1 persisted task, got %d", len(state.Tasks))
	}
	if state.Tasks[0].PID != 5555 {
		t.Errorf("persisted PID = %d, want 5555", state.Tasks[0].PID)
	}
	if state.Tasks[0].RscFpopsEst != 1e10 {
		t.Errorf("persisted RscFpopsEst = %g, want 1e10", state.Tasks[0].RscFpopsEst)
	}

	// Cleanup: unblock the slot and drain it.
	close(blockCh)
	d.slotManager.WaitForCompletion(ctx)
}

func TestSuspendAndQuit_AlreadyStopping(t *testing.T) {
	// Override os.Exit to detect if it's called.
	exitCalled := false
	restore := SetOsExitFunc(func(code int) {
		exitCalled = true
	})
	defer restore()

	mc := &mockClient{}
	mr := &mockRuntime{canHandle: true}
	d := newTestDaemon(mc, mr)

	// Set stopping=true before calling SuspendAndQuit.
	d.mu.Lock()
	d.stopping = true
	d.mu.Unlock()

	d.SuspendAndQuit()

	if exitCalled {
		t.Error("SuspendAndQuit should return early when daemon is already stopping")
	}
}

func TestSuspendAndQuit_NilSlotManager(t *testing.T) {
	// Override os.Exit.
	exitCalled := false
	restore := SetOsExitFunc(func(code int) {
		exitCalled = true
	})
	defer restore()

	mc := &mockClient{}
	mr := &mockRuntime{canHandle: true}
	d := newTestDaemon(mc, mr)
	// slotManager is nil (daemon not Run yet).
	d.slotManager = nil

	d.SuspendAndQuit()

	// Should still call exit even with nil slotManager.
	if !exitCalled {
		t.Error("expected osExitFunc to be called even with nil slotManager")
	}
}
