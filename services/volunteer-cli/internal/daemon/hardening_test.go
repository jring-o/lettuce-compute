package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
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

// fakeDocker implements runtime.DockerClient but only answers ImageExists and
// Info; any other call panics (none should happen in these tests).
type fakeDocker struct {
	runtime.DockerClient
	exists      bool
	storePath   string   // primary image-store path reported by Info (TODO #31)
	storePaths  []string // every filesystem the gate should check (containerd snapshotter)
	snapshotter bool
}

func (f *fakeDocker) ImageExists(_ context.Context, _ string) (bool, error) {
	return f.exists, nil
}

func (f *fakeDocker) ContainerList(_ context.Context, _ string) ([]runtime.ContainerSummary, error) {
	return nil, nil
}

func (f *fakeDocker) Info(_ context.Context) (*runtime.EngineInfo, error) {
	return &runtime.EngineInfo{
		StoragePath:     f.storePath,
		ImageStorePaths: f.storePaths,
		Snapshotter:     f.snapshotter,
	}, nil
}

// pathLimiter reports per-path free space (MB) so a test can model a roomy data
// dir on one filesystem and a small image store on another. A path not in the
// map is treated as effectively unlimited.
type pathLimiter struct{ availMB map[string]int }

func (l *pathLimiter) Apply(_ *exec.Cmd, _ *config.ResourceLimits) error { return nil }
func (l *pathLimiter) Enforce(_ int, _ *config.ResourceLimits) (func(), error) {
	return func() {}, nil
}
func (l *pathLimiter) CheckDiskSpace(path string, requiredMB int) error {
	avail, ok := l.availMB[path]
	if !ok {
		return nil // unmodeled path → ample
	}
	if requiredMB > avail {
		return fmt.Errorf("insufficient on %s: need %d, have %d", path, requiredMB, avail)
	}
	return nil
}

// --- The per-leaf disk gate (TB-24; formerly item 5's allowance-as-floor) ---

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
	// Enough for the bounded workspace requirement, but not the 100 GB allowance
	// (which the gate must not demand — TB-24).
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 50 * 1024}, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 100

	// Register a container runtime whose image is already cached, and seed a
	// leaf using that image.
	seedContainerLeaf(t, d, mc, &fakeDocker{exists: true}, "ghcr.io/example/img:1")

	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false, want true (cached image needs only workspace headroom)")
	}
}

// TestShouldFetch_AllowanceIsNotAFreeSpaceFloor pins TB-24's core repair: with
// free space far below the max_disk_gb allowance but comfortably above what any
// leaf actually needs, fetching proceeds. Under the old gate this exact setup
// (50 GB free, 100 GB allowance, no cached image) blocked all fetching — the
// allowance doubled as a free-space floor, so OFFERING more capacity to the
// head made the volunteer's own fetches harder.
func TestShouldFetch_AllowanceIsNotAFreeSpaceFloor(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 50 * 1024}, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 100

	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false, want true — the allowance is a capacity offer, not a free-space floor (TB-24)")
	}
}

// --- TODO #31: disk gate must check the container image-store filesystem ---

// seedContainerLeaf registers a container runtime backed by dc and seeds the leaf
// cache with one leaf that uses image, so the daemon knows it would pull that
// image. Returns nothing; mutates d.
func seedContainerLeaf(t *testing.T, d *Daemon, mc *mockClient, dc runtime.DockerClient, image string) {
	t.Helper()
	seedContainerLeafNeed(t, d, mc, dc, image, 0)
}

// seedContainerLeafNeed is seedContainerLeaf with the leaf's declared
// min_disk_mb — the number the per-leaf disk gate compares free space against
// (TB-24). 0 omits resource_requirements (the gate then uses the per-task
// default need).
func seedContainerLeafNeed(t *testing.T, d *Daemon, mc *mockClient, dc runtime.DockerClient, image string, minDiskMB int64) {
	t.Helper()
	d.runtimeRegistry.Register(runtime.NewContainerRuntimeWithClient(t.TempDir(), quietLogger(), dc))
	leaf := &lettucev1.LeafInfo{
		Id:            "leaf-1",
		Slug:          "big-image-leaf",
		State:         "ACTIVE",
		ExecutionSpec: &lettucev1.ExecutionSpec{Image: image},
	}
	if minDiskMB > 0 {
		leaf.ResourceRequirements = &lettucev1.LeafResourceRequirements{MinDiskMb: minDiskMB}
	}
	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{Leafs: []*lettucev1.LeafInfo{leaf}}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}
}

// TestShouldFetch_GatesImageStoreFilesystem reproduces TODO #31: the big image a
// container leaf pulls lands in the engine's image store (Docker DockerRootDir /
// Podman graphroot), NOT under the lettuce data dir. On a host with a roomy
// data-dir volume but a small image-store volume, the old gate checked only the
// data dir, passed, and the pull then died with ENOSPC on a filesystem it never
// looked at. The gate must also reject when the image-store volume can't hold
// the pull — sized by the LEAF's declared need, not the allowance (TB-24). (No
// cached image here, so a fresh pull is required.)
func TestShouldFetch_GatesImageStoreFilesystem(t *testing.T) {
	const dataDir = "/data"
	const storePath = "/var/lib/containers/storage"
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	// Data dir is roomy (200 GB) but the image store is on a small volume
	// (20 GB), below the leaf's declared 30 GB need.
	lim := &pathLimiter{availMB: map[string]int{dataDir: 200 * 1024, storePath: 20 * 1024}}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, lim, scheduler)
	d.cfg.DataDir = dataDir
	d.cfg.ResourceLimits.MaxDiskGB = 100

	seedContainerLeafNeed(t, d, mc, &fakeDocker{exists: false, storePath: storePath}, "ghcr.io/example/big:1", 30*1024)

	if d.shouldFetch() {
		t.Fatal("shouldFetch = true, want false — the image-store volume is too small to pull the image (TODO #31)")
	}
}

// TestShouldFetch_ImageStoreAmpleStillFetches guards against over-blocking: when
// the image-store volume has room, a roomy data dir + ample store must still
// fetch.
func TestShouldFetch_ImageStoreAmpleStillFetches(t *testing.T) {
	const dataDir = "/data"
	const storePath = "/var/lib/containers/storage"
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	lim := &pathLimiter{availMB: map[string]int{dataDir: 200 * 1024, storePath: 200 * 1024}}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, lim, scheduler)
	d.cfg.DataDir = dataDir
	d.cfg.ResourceLimits.MaxDiskGB = 100

	seedContainerLeaf(t, d, mc, &fakeDocker{exists: false, storePath: storePath}, "ghcr.io/example/big:1")

	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false, want true — both the data dir and image store have ample space")
	}
}

// TestShouldFetch_GatesContainerdSnapshotterRoot covers the Docker containerd
// snapshotter case: DockerRootDir (/var/lib/docker) is roomy, but the image
// content actually lands under the containerd root (/var/lib/containerd), which
// here is a small volume below the allowance. The gate must check the containerd
// root too and refuse to fetch — a DockerRootDir-only gate would pass and the
// pull would then ENOSPC on /var/lib/containerd.
func TestShouldFetch_GatesContainerdSnapshotterRoot(t *testing.T) {
	const dataDir = "/data"
	const dockerRoot = "/var/lib/docker"         // roomy
	const containerdRoot = "/var/lib/containerd" // small — where the blobs land
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	lim := &pathLimiter{availMB: map[string]int{
		dataDir:        200 * 1024,
		dockerRoot:     200 * 1024,
		containerdRoot: 20 * 1024,
	}}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, lim, scheduler)
	d.cfg.DataDir = dataDir
	d.cfg.ResourceLimits.MaxDiskGB = 100

	dc := &fakeDocker{
		exists:      false,
		storePath:   dockerRoot,
		storePaths:  []string{dockerRoot, containerdRoot},
		snapshotter: true,
	}
	seedContainerLeafNeed(t, d, mc, dc, "ghcr.io/example/big:1", 30*1024)

	if d.shouldFetch() {
		t.Fatal("shouldFetch = true, want false — the containerd image-store root is too small (Docker containerd snapshotter)")
	}
}

// TestShouldFetch_CachedImageSkipsImageStoreGate confirms the image-store gate is
// skipped when an enabled leaf's image is already cached: no pull happens, so a
// tight image-store volume must not block a cached-image rerun.
func TestShouldFetch_CachedImageSkipsImageStoreGate(t *testing.T) {
	const dataDir = "/data"
	const storePath = "/var/lib/containers/storage"
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	// Data dir has the 10 GB workspace headroom but not the 100 GB full allowance;
	// the image store is tight (1 GB). With the image cached, fetch still proceeds.
	lim := &pathLimiter{availMB: map[string]int{dataDir: 50 * 1024, storePath: 1 * 1024}}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, lim, scheduler)
	d.cfg.DataDir = dataDir
	d.cfg.ResourceLimits.MaxDiskGB = 100

	seedContainerLeaf(t, d, mc, &fakeDocker{exists: true, storePath: storePath}, "ghcr.io/example/big:1")

	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false, want true — image is cached, so the image-store volume should not gate a rerun")
	}
}

// TestLeafDiskThresholds checks the shared per-leaf threshold helper the live
// gate and the doctor preflight both consume (TODO #24 / TB-24). The
// requirements derive from the LEAF's declared need — never from max_disk_gb.
func TestLeafDiskThresholds(t *testing.T) {
	cases := []struct {
		name                  string
		needMB                int64
		isContainer, cached   bool
		wantDataDir, wantStore int64
	}{
		// Fresh container pull: full declared need on both volumes.
		{"container_fresh", 15000, true, false, 15000 + DiskFloorMB, 15000 + DiskFloorMB},
		// Cached image: workspace only (bounded by the headroom cap), no store gate.
		{"container_cached_small", 1024, true, true, 1024 + DiskFloorMB, 0},
		{"container_cached_big", 15000, true, true, cachedImageWorkspaceHeadroomMB + DiskFloorMB, 0},
		// Native/wasm: declared need on the data dir only.
		{"native", 5000, false, false, 5000 + DiskFloorMB, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir, store := LeafDiskThresholds(tc.needMB, tc.isContainer, tc.cached)
			if dataDir != tc.wantDataDir || store != tc.wantStore {
				t.Errorf("LeafDiskThresholds(%d, %v, %v) = (%d, %d), want (%d, %d)",
					tc.needMB, tc.isContainer, tc.cached, dataDir, store, tc.wantDataDir, tc.wantStore)
			}
		})
	}
}

// TestDiskBudgetVerdict checks the allowance budget: measured usage + the
// leaf's incremental need must fit under max_disk_gb, and a refusal names the
// setting (TB-24).
func TestDiskBudgetVerdict(t *testing.T) {
	// Within budget.
	if ok, _ := DiskBudgetVerdict(15000, true, false, 5000, 30*1024); !ok {
		t.Error("DiskBudgetVerdict = blocked, want ok (5000 used + 15000 need <= 30720 allowance)")
	}
	// Over budget: fresh need counts in full.
	ok, reason := DiskBudgetVerdict(15000, true, false, 20000, 30*1024)
	if ok {
		t.Error("DiskBudgetVerdict = ok, want blocked (20000 used + 15000 need > 30720 allowance)")
	}
	if !strings.Contains(reason, "max_disk_gb") {
		t.Errorf("budget refusal must name the setting; got: %s", reason)
	}
	// Cached image: only the (bounded) workspace is incremental — the image is
	// already inside the measured usage.
	if ok, _ := DiskBudgetVerdict(15000, true, true, 20000, 31*1024); !ok {
		t.Error("DiskBudgetVerdict = blocked, want ok (cached: 20000 used + min(15000, headroom)=10240 <= 31744)")
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
