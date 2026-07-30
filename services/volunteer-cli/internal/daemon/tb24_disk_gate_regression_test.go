package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-24 regression: raising max_disk_gb to qualify for a big leaf must not gate
// the volunteer's own image downloads.
//
// The tester's exact fixed point, 2026-07-28: 30 GB free before pulling a
// 5.58 GB image, allowance raised to 30 (to clear the GREP leaves' 15 GB
// min_disk_mb), 24,721 MB free after the pull. The old gate demanded the WHOLE
// allowance (30,720 MB) free for any fresh pull — no credit for the space the
// allowance itself had just spent — so every future pull was gated,
// permanently, on a machine with ~24 GB free and no image over 6 GB anywhere.
// The gate must instead require the LEAF's declared need (15,000 MB + the
// floor), which this machine comfortably has.
//
// This test intentionally uses only APIs that predate the fix (shouldFetch,
// thresholdLimiter, fakeDocker, the leaf cache), so the red half can be
// demonstrated by running this exact file against the pre-fix daemon.
func TestTB24_RaisedAllowanceDoesNotGateFreshPulls(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 24721}, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 30

	// The GREP CPU leaf: container image not yet cached (a NEW leaf's image —
	// the fresh pull the tester could never make again), min_disk_mb 15000.
	d.runtimeRegistry.Register(runtime.NewContainerRuntimeWithClient(t.TempDir(), quietLogger(), &fakeDocker{exists: false}))
	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{
			Leafs: []*lettucev1.LeafInfo{{
				Id:                   "leaf-grep",
				Slug:                 "grep-cpu",
				State:                "ACTIVE",
				ExecutionSpec:        &lettucev1.ExecutionSpec{Image: "ghcr.io/example/grep:next"},
				ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 15000},
			}},
		}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}

	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false with 24,721 MB free, allowance 30 GB, leaf need 15,000 MB — " +
			"the volunteer raised the allowance to qualify for this leaf and the gate then blocked its download (TB-24)")
	}
}

// TB-24 secondary defect 2: the cached-image relaxation must be per leaf. Under
// the old gate ANY cached image relaxed the gate for EVERY leaf, so a
// fresh-pull leaf slipped past the image-store check because some OTHER leaf's
// image was cached, then died mid-pull with ENOSPC — the exact failure the gate
// exists to prevent. Now the cached leaf keeps fetching while the fresh-pull
// leaf is individually refused.
func TestTB24_CachedImageRelaxationIsPerLeaf(t *testing.T) {
	const dataDir = "/data"
	const storePath = "/var/lib/containers/storage"
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	// Data dir roomy; image store too small for the fresh leaf's 30 GB need.
	lim := &pathLimiter{availMB: map[string]int{dataDir: 200 * 1024, storePath: 20 * 1024}}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, lim, scheduler)
	d.cfg.DataDir = dataDir
	d.cfg.ResourceLimits.MaxDiskGB = 100

	// Two leaves: one cached, one needing a fresh 30 GB pull.
	dc := &fakeDockerPerImage{cached: map[string]bool{"ghcr.io/example/cached:1": true}, storePath: storePath}
	d.runtimeRegistry.Register(runtime.NewContainerRuntimeWithClient(t.TempDir(), quietLogger(), dc))
	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{
			Leafs: []*lettucev1.LeafInfo{
				{
					Id: "leaf-cached", Slug: "cached-leaf", State: "ACTIVE",
					ExecutionSpec:        &lettucev1.ExecutionSpec{Image: "ghcr.io/example/cached:1"},
					ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 15000},
				},
				{
					Id: "leaf-fresh", Slug: "fresh-leaf", State: "ACTIVE",
					ExecutionSpec:        &lettucev1.ExecutionSpec{Image: "ghcr.io/example/fresh:1"},
					ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 30 * 1024},
				},
			},
		}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}

	leafs := d.allEnabledLeafs()
	if len(leafs) != 2 {
		t.Fatalf("enabled leafs = %d, want 2", len(leafs))
	}
	byID := map[string]CachedLeafInfo{}
	for _, lf := range leafs {
		byID[lf.ID] = lf
	}

	if ok, reason := d.leafDiskGate(byID["leaf-cached"]); !ok {
		t.Errorf("cached leaf refused (%s); its image needs no pull and the workspace fits", reason)
	}
	if ok, _ := d.leafDiskGate(byID["leaf-fresh"]); ok {
		t.Error("fresh-pull leaf admitted although the image store cannot hold its 30 GB pull — " +
			"the OTHER leaf's cached image must not relax this leaf's gate (TB-24 secondary defect 2)")
	}
	// And the daemon still fetches overall: one leaf passing is enough.
	if !d.shouldFetch() {
		t.Error("shouldFetch = false, want true — the cached leaf is fetchable even while the fresh one is gated")
	}
}

// fakeDockerPerImage is fakeDocker with per-image cache answers.
type fakeDockerPerImage struct {
	runtime.DockerClient
	cached    map[string]bool
	storePath string
}

func (f *fakeDockerPerImage) ImageExists(_ context.Context, ref string) (bool, error) {
	return f.cached[ref], nil
}

func (f *fakeDockerPerImage) ContainerList(_ context.Context, _ string) ([]runtime.ContainerSummary, error) {
	return nil, nil
}

func (f *fakeDockerPerImage) Info(_ context.Context) (*runtime.EngineInfo, error) {
	return &runtime.EngineInfo{StoragePath: f.storePath, ImageStorePaths: []string{f.storePath}}, nil
}

// TB-24 chosen direction 2: the allowance is also a budget on Lettuce's OWN
// usage. When measured usage plus a leaf's incremental need exceeds
// max_disk_gb, fetching that leaf is refused with a reason naming the setting —
// even when raw free space would allow it.
func TestTB24_UsageBudgetRefusesOverAllowance(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 1 << 30}, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 10 // 10,240 MB allowance

	seedContainerLeafNeed(t, d, mc, &fakeDocker{exists: false}, "ghcr.io/example/big:1", 8000)

	// Pre-measured usage: 5,000 MB already used. 5,000 + 8,000 > 10,240.
	d.diskUsageMu.Lock()
	d.diskUsageChecked = time.Now()
	d.diskUsageMB, d.diskUsageOK = 5000, true
	d.diskUsageMu.Unlock()

	leafs := d.allEnabledLeafs()
	if len(leafs) != 1 {
		t.Fatalf("enabled leafs = %d, want 1", len(leafs))
	}
	ok, reason := d.leafDiskGate(leafs[0])
	if ok {
		t.Fatal("leafDiskGate = ok, want refused: 5,000 MB used + 8,000 MB need exceeds the 10,240 MB allowance")
	}
	if !strings.Contains(reason, "max_disk_gb") {
		t.Errorf("budget refusal must name the setting; got: %s", reason)
	}
}
