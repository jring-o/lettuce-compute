package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// thresholdLimiter fails CheckDiskSpace when requiredMB exceeds availMB, so a
// test can distinguish the full-allowance check from the workspace check.
type thresholdLimiter struct{ availMB int }

func (l *thresholdLimiter) Apply(_ *exec.Cmd, _ *config.ResourceLimits) error { return nil }
func (l *thresholdLimiter) Enforce(_ int, _ *config.ResourceLimits) (func(), error) {
	return func() {}, nil
}
func (l *thresholdLimiter) CheckDiskSpace(_ string, requiredMB int) error {
	if requiredMB > l.availMB {
		return fmt.Errorf("insufficient: need %d, have %d", requiredMB, l.availMB)
	}
	return nil
}

// fakeDocker implements runtime.DockerClient but only answers ImageExists; any
// other call panics (none should happen in these tests).
type fakeDocker struct {
	runtime.DockerClient
	exists bool
}

func (f *fakeDocker) ImageExists(_ context.Context, _ string) (bool, error) {
	return f.exists, nil
}

// --- Item 5: disk gate accounts for the cached image ---

func TestShouldFetch_FastPathWhenDiskAmple(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 1 << 30}, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 100

	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false, want true when disk is ample")
	}
}

func TestShouldFetch_CachedImageRequiresOnlyWorkspace(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	// Enough for the 10 GB workspace headroom, but not the 100 GB full allowance.
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 50 * 1024}, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 100

	// Register a container runtime whose image is already cached.
	d.runtimeRegistry.Register(runtime.NewContainerRuntimeWithClient(t.TempDir(), quietLogger(), &fakeDocker{exists: true}))

	// Seed the leaf cache with a leaf that uses that image.
	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{
			Leafs: []*lettucev1.LeafInfo{{
				Id:            "leaf-1",
				Slug:          "example-leaf",
				State:         "ACTIVE",
				ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ghcr.io/example/img:1"},
			}},
		}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}

	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false, want true (cached image needs only workspace headroom)")
	}
}

func TestShouldFetch_NoCachedImageRequiresFullAllowance(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	// Below the full allowance and no container runtime registered, so no cached
	// image can rescue the fetch.
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 50 * 1024}, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 100

	if d.shouldFetch() {
		t.Fatal("shouldFetch = true, want false (no cached image, below full allowance)")
	}
}

// --- Item 4: abandon un-run units back to the head ---

func TestAbandonItem_ReturnsUnitToHead(t *testing.T) {
	mc := &mockClient{}
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &testLimiter{}, scheduler)

	item := &PreFetchItem{
		WU:   &runtime.WorkUnit{ID: "dc5ff9da-f084-4dd7-86b8-e829669814f8", LeafID: "leaf-1"},
		Conn: d.multiClient.Servers()[0],
	}
	d.abandonItem(item, "volunteer shutdown")

	if mc.getAbandonCalls() != 1 {
		t.Fatalf("AbandonWorkUnit calls = %d, want 1", mc.getAbandonCalls())
	}
	if mc.lastAbandonReq == nil || mc.lastAbandonReq.WorkUnitId != "dc5ff9da-f084-4dd7-86b8-e829669814f8" {
		t.Errorf("abandon req = %+v, want WorkUnitId=wu-1", mc.lastAbandonReq)
	}
	if mc.lastAbandonReq.Reason != "volunteer shutdown" {
		t.Errorf("reason = %q, want 'volunteer shutdown'", mc.lastAbandonReq.Reason)
	}
}

// --- Item 6: persist + retry result submission ---

func newPendingRequest(t *testing.T, wuID string) []byte {
	t.Helper()
	blob, err := proto.Marshal(&lettucev1.SubmitResultRequest{WorkUnitId: wuID, VolunteerId: "vol-1"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return blob
}

func TestRetryPendingResults_DeletesOnSuccess(t *testing.T) {
	mc := &mockClient{}
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &testLimiter{}, scheduler)
	d.cfg.DataDir = t.TempDir()

	if err := SavePendingResult(d.cfg.DataDir, PendingResult{
		WorkUnitID:   "dc5ff9da-f084-4dd7-86b8-e829669814f8",
		LeafID:       "leaf-1",
		ServerName:   "default",
		RequestProto: newPendingRequest(t, "dc5ff9da-f084-4dd7-86b8-e829669814f8"),
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	mc.submitResultFn = func(_ context.Context, _ *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
		return &lettucev1.SubmitResultResponse{ResultId: "r1", Accepted: true}, nil
	}

	d.retryPendingResults(context.Background())

	if mc.getSubmitCalls() != 1 {
		t.Errorf("SubmitResult calls = %d, want 1", mc.getSubmitCalls())
	}
	remaining, _ := ListPendingResults(d.cfg.DataDir)
	if len(remaining) != 0 {
		t.Errorf("pending after success = %d, want 0 (should be deleted)", len(remaining))
	}
}

func TestRetryPendingResults_KeepsOnTransportError(t *testing.T) {
	mc := &mockClient{}
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &testLimiter{}, scheduler)
	d.cfg.DataDir = t.TempDir()

	if err := SavePendingResult(d.cfg.DataDir, PendingResult{
		WorkUnitID:   "dc5ff9da-f084-4dd7-86b8-e829669814f8",
		ServerName:   "default",
		RequestProto: newPendingRequest(t, "dc5ff9da-f084-4dd7-86b8-e829669814f8"),
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	mc.submitResultFn = func(_ context.Context, _ *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
		return nil, fmt.Errorf("connection refused")
	}

	d.retryPendingResults(context.Background())

	remaining, _ := ListPendingResults(d.cfg.DataDir)
	if len(remaining) != 1 {
		t.Errorf("pending after transport error = %d, want 1 (kept for retry)", len(remaining))
	}
}

func TestRetryPendingResults_DropsUnknownServerLater(t *testing.T) {
	mc := &mockClient{}
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &testLimiter{}, scheduler)
	d.cfg.DataDir = t.TempDir()

	// ServerName that doesn't match any connection: kept (no connection to retry on),
	// never submitted.
	if err := SavePendingResult(d.cfg.DataDir, PendingResult{
		WorkUnitID:   "dc5ff9da-f084-4dd7-86b8-e829669814f8",
		ServerName:   "nonexistent",
		RequestProto: newPendingRequest(t, "dc5ff9da-f084-4dd7-86b8-e829669814f8"),
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	d.retryPendingResults(context.Background())

	if mc.getSubmitCalls() != 0 {
		t.Errorf("SubmitResult calls = %d, want 0 (no matching server)", mc.getSubmitCalls())
	}
	remaining, _ := ListPendingResults(d.cfg.DataDir)
	if len(remaining) != 1 {
		t.Errorf("pending = %d, want 1 (kept until server reconnects)", len(remaining))
	}
}

// --- TODO #20: terminal vs. transient classification of resubmit errors ---

// A definitive gRPC rejection (here FailedPrecondition "no active assignment")
// means the head adjudicated the result and rejected it; resending the identical
// bytes can never succeed, so the persisted file must be dropped rather than
// retried forever.
func TestRetryPendingResults_DeletesOnTerminalRejection(t *testing.T) {
	mc := &mockClient{}
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &testLimiter{}, scheduler)
	d.cfg.DataDir = t.TempDir()

	if err := SavePendingResult(d.cfg.DataDir, PendingResult{
		WorkUnitID:   "dc5ff9da-f084-4dd7-86b8-e829669814f8",
		ServerName:   "default",
		RequestProto: newPendingRequest(t, "dc5ff9da-f084-4dd7-86b8-e829669814f8"),
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	mc.submitResultFn = func(_ context.Context, _ *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
		return nil, status.Error(codes.FailedPrecondition, "no active assignment for this volunteer and work unit")
	}

	d.retryPendingResults(context.Background())

	if mc.getSubmitCalls() != 1 {
		t.Errorf("SubmitResult calls = %d, want 1", mc.getSubmitCalls())
	}
	remaining, _ := ListPendingResults(d.cfg.DataDir)
	if len(remaining) != 0 {
		t.Errorf("pending after terminal rejection = %d, want 0 (should be dropped)", len(remaining))
	}
}

// A transient gRPC status (Unavailable) is a transport/availability failure: the
// result may still land on a later sweep, so the persisted file must be kept.
func TestRetryPendingResults_KeepsOnTransientStatus(t *testing.T) {
	mc := &mockClient{}
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &testLimiter{}, scheduler)
	d.cfg.DataDir = t.TempDir()

	if err := SavePendingResult(d.cfg.DataDir, PendingResult{
		WorkUnitID:   "dc5ff9da-f084-4dd7-86b8-e829669814f8",
		ServerName:   "default",
		RequestProto: newPendingRequest(t, "dc5ff9da-f084-4dd7-86b8-e829669814f8"),
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	mc.submitResultFn = func(_ context.Context, _ *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
		return nil, status.Error(codes.Unavailable, "head is restarting")
	}

	d.retryPendingResults(context.Background())

	if mc.getSubmitCalls() != 1 {
		t.Errorf("SubmitResult calls = %d, want 1", mc.getSubmitCalls())
	}
	remaining, _ := ListPendingResults(d.cfg.DataDir)
	if len(remaining) != 1 {
		t.Errorf("pending after transient status = %d, want 1 (kept for retry)", len(remaining))
	}
}

// When a slot's heartbeat returns PermissionDenied "not assigned", the volunteer
// has lost its assignment for this work unit. The slot must stop computing
// (cancelExec) and the heartbeat must exit (send returns false) so we don't run a
// doomed task to completion and submit a result that can never be credited.
func TestRunSlotHeartbeat_DropsOnAssignmentLost(t *testing.T) {
	mc := &mockClient{}
	mc.heartbeatFn = func(_ context.Context, _ *lettucev1.HeartbeatRequest) (*lettucev1.HeartbeatResponse, error) {
		return nil, status.Error(codes.PermissionDenied, "volunteer is not assigned to this work unit")
	}
	d := newSlotTestDaemon()

	execCtx, cancelExec := context.WithCancel(context.Background())
	defer cancelExec()

	conn := &ServerConnection{Name: "t", VolunteerID: "v", Client: mc}
	wu := &runtime.WorkUnit{ID: "dc5ff9da-f084-4dd7-86b8-e829669814f8", LeafID: "leaf-1"}

	done := make(chan struct{})
	go func() {
		// 1s interval; the terminal error fires on the very first (immediate) send,
		// so this returns well before the ticker would ever tick.
		d.runSlotHeartbeat(context.Background(), wu, t.TempDir(), 1, cancelExec, conn, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runSlotHeartbeat did not exit after assignment-lost rejection")
	}

	if execCtx.Err() != context.Canceled {
		t.Errorf("execCtx.Err() = %v, want context.Canceled (cancelExec should have run)", execCtx.Err())
	}
	if c := mc.getHeartbeatCalls(); c != 1 {
		t.Errorf("heartbeat calls = %d, want 1 (should stop after terminal rejection)", c)
	}
}

// Regression guard: a transient heartbeat error (Unavailable) must NOT drop the
// task. Execution stays alive (execCtx not cancelled) and the heartbeat keeps
// firing on the ticker.
func TestRunSlotHeartbeat_KeepsRunningOnTransientError(t *testing.T) {
	mc := &mockClient{}
	mc.heartbeatFn = func(_ context.Context, _ *lettucev1.HeartbeatRequest) (*lettucev1.HeartbeatResponse, error) {
		return nil, status.Error(codes.Unavailable, "connection refused")
	}
	d := newSlotTestDaemon()

	execCtx, cancelExec := context.WithCancel(context.Background())
	defer cancelExec()

	conn := &ServerConnection{Name: "t", VolunteerID: "v", Client: mc}
	wu := &runtime.WorkUnit{ID: "dc5ff9da-f084-4dd7-86b8-e829669814f8", LeafID: "leaf-1"}

	// heartbeatCtx drives the loop; we cancel it to stop the goroutine once we've
	// observed it kept running across at least two sends.
	heartbeatCtx, heartbeatCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.runSlotHeartbeat(heartbeatCtx, wu, t.TempDir(), 1, cancelExec, conn, nil)
		close(done)
	}()

	// Wait for the ticker to fire at least once more after the immediate send.
	deadline := time.After(4 * time.Second)
	for mc.getHeartbeatCalls() < 2 {
		select {
		case <-deadline:
			heartbeatCancel()
			t.Fatalf("heartbeat calls = %d, want >= 2 (transient error should keep heartbeating)", mc.getHeartbeatCalls())
		case <-time.After(10 * time.Millisecond):
		}
	}

	if execCtx.Err() != nil {
		heartbeatCancel()
		t.Fatalf("execCtx.Err() = %v, want nil (transient error must not cancel execution)", execCtx.Err())
	}

	// Stop the heartbeat goroutine and confirm it exits cleanly.
	heartbeatCancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runSlotHeartbeat did not exit after ctx cancel")
	}
}

// --- Item 2: PREPARING heartbeats during the pull ---

func TestRunPrepareHeartbeat_SendsPreparing(t *testing.T) {
	mc := &mockClient{}
	got := make(chan string, 4)
	mc.heartbeatFn = func(_ context.Context, req *lettucev1.HeartbeatRequest) (*lettucev1.HeartbeatResponse, error) {
		select {
		case got <- req.Status:
		default:
		}
		return &lettucev1.HeartbeatResponse{ContinueExecution: true}, nil
	}

	f := &Fetcher{logger: quietLogger()}
	conn := &ServerConnection{Client: mc, VolunteerID: "vol-1", Name: "head"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.runPrepareHeartbeat(ctx, conn, &runtime.WorkUnit{ID: "dc5ff9da-f084-4dd7-86b8-e829669814f8"}, 0)

	select {
	case status := <-got:
		if status != "PREPARING" {
			t.Errorf("heartbeat status = %q, want PREPARING", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no heartbeat sent within 2s")
	}
}
