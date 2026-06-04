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


// The per-task and PREPARING heartbeat loop tests (TestRunSlotHeartbeat_* and
// TestRunPrepareHeartbeat_SendsPreparing) were removed: runSlotHeartbeat and
// runPrepareHeartbeat no longer exist. Run-start is now StartWork and liveness is
// deadline-based; the assignment-lost / transient-error semantics those tests
// covered are surfaced at StartWork/SubmitResult and are WP-VOL's to re-cover once
// the slot StartWork call is wired.
