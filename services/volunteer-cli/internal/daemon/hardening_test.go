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

// TestDiskGateThresholds checks the shared threshold helper the live gate and
// the doctor preflight both consume (TODO #24).
func TestDiskGateThresholds(t *testing.T) {
	cases := []struct {
		maxDiskGB int
		wantFull  int
		wantCache int
	}{
		{maxDiskGB: 20, wantFull: 20 * 1024, wantCache: cachedImageWorkspaceHeadroomMB},
		{maxDiskGB: 10, wantFull: 10 * 1024, wantCache: cachedImageWorkspaceHeadroomMB},
		{maxDiskGB: 0, wantFull: 1024, wantCache: cachedImageWorkspaceHeadroomMB},  // unset → 1 GB floor
		{maxDiskGB: -5, wantFull: 1024, wantCache: cachedImageWorkspaceHeadroomMB}, // negative → 1 GB floor
	}
	for _, tc := range cases {
		full, cache := DiskGateThresholds(tc.maxDiskGB)
		if full != tc.wantFull || cache != tc.wantCache {
			t.Errorf("DiskGateThresholds(%d) = (%d, %d), want (%d, %d)",
				tc.maxDiskGB, full, cache, tc.wantFull, tc.wantCache)
		}
	}
}

// TestClassifyDiskGate verifies the shared classifier reproduces shouldFetch's
// three-region decision: ample (always fetch), cached-only (fetch iff an image
// is cached), and blocked. The cached-only band exists only when max_disk_gb
// exceeds the 10 GB cached-image headroom.
func TestClassifyDiskGate(t *testing.T) {
	cases := []struct {
		name        string
		availableMB int64
		maxDiskGB   int
		want        DiskGateVerdict
	}{
		// max_disk_gb=20 (full 20 GB, headroom 10 GB) → all three bands exist.
		{"ample_above_full", 25 * 1024, 20, DiskAmple},
		{"ample_at_full", 20 * 1024, 20, DiskAmple},
		{"cached_only_mid", 15 * 1024, 20, DiskCachedOnly},
		{"cached_only_at_headroom", 10 * 1024, 20, DiskCachedOnly},
		{"blocked_below_headroom", 10*1024 - 1, 20, DiskBlocked},
		// max_disk_gb=10 (full == headroom) → no cached-only band.
		{"small_ample", 10 * 1024, 10, DiskAmple},
		{"small_blocked", 10*1024 - 1, 10, DiskBlocked},
		// max_disk_gb=5 (full 5 GB < headroom) → cached path can't help; floor is full.
		{"tiny_ample", 5 * 1024, 5, DiskAmple},
		{"tiny_blocked", 5*1024 - 1, 5, DiskBlocked},
		// Unset allowance falls back to the 1 GB floor.
		{"unset_ample", 2048, 0, DiskAmple},
		{"unset_blocked", 512, 0, DiskBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyDiskGate(tc.availableMB, tc.maxDiskGB); got != tc.want {
				t.Errorf("ClassifyDiskGate(%d, %d) = %d, want %d", tc.availableMB, tc.maxDiskGB, got, tc.want)
			}
		})
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
