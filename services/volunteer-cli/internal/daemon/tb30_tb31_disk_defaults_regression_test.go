package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
)

// TB-31 regression: the unknown-need fallback must clear the DEFAULT
// max_disk_gb allowance with real usage present.
//
// At v0.10.5 both numbers were 10,240 MB — the fallback was
// runtime.DefaultPerTaskDiskMB and the default allowance max_disk_gb = 10 —
// so `usage + need <= allowance` held only at exactly zero measured usage,
// and a default-configured volunteer was silently disk-gated on any leaf that
// declares no min_disk_mb (both lbry Beyblade leaves) the moment the daemon
// had written config, identity and a log file. The tester's Mac needed
// max_disk_gb 11 to run; the repro volunteer gated at 454 MB of usage with
// 47 GB genuinely free.
//
// Uses only APIs that predate the fix (leafDiskNeedMB, DiskBudgetVerdict,
// config.Defaults), so the red half is demonstrated by running this file
// against the pre-fix daemon.
func TestTB31_DefaultAllowanceClearsUnknownNeedWithRealUsage(t *testing.T) {
	allowanceMB := int64(config.Defaults().ResourceLimits.MaxDiskGB) * 1024

	need := leafDiskNeedMB(CachedLeafInfo{}) // the head did not say
	if need <= 0 {
		t.Fatalf("unknown-need fallback = %d, want > 0 — unknown is not needs-nothing", need)
	}

	// 454 MB: the repro volunteer's real usage minutes after first run.
	ok, reason := DiskBudgetVerdict(need, false, false, 454, allowanceMB)
	if !ok {
		t.Fatalf("a default-configured volunteer (allowance %d MB) with 454 MB of usage is disk-gated on an undeclared-need leaf (fallback need %d MB): %s",
			allowanceMB, need, reason)
	}
}

// TB-31 regression, end to end: a fresh default-config volunteer must fetch
// work for a leaf whose stored resource_requirements predate the head's
// min_disk_mb default (the client receives 0 — the lbry Beyblade shape).
func TestTB31_DefaultVolunteerFetchesUndeclaredNeedLeaf(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	// 47,969 MB free — the repro machine's real reading. The allowance stays at
	// config.Defaults(): the default IS the bug's subject.
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 47969}, scheduler)

	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{
			Leafs: []*lettucev1.LeafInfo{{
				Id:    "leaf-bb",
				Slug:  "beyblade-cpu",
				State: "ACTIVE",
				// Requirements present but min_disk_mb unset — a legacy leaf.
				ResourceRequirements: &lettucev1.LeafResourceRequirements{MinCpuCores: 1},
			}},
		}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}

	// Pre-measured usage: 454 MB, the repro volunteer's own figure.
	d.diskUsageMu.Lock()
	d.diskUsageChecked = time.Now()
	d.diskUsageMB, d.diskUsageOK = 454, true
	d.diskUsageMu.Unlock()

	if !d.shouldFetch() {
		_, reason := d.leafDiskGate(d.allEnabledLeafs()[0])
		t.Fatalf("shouldFetch = false — a fresh default-config volunteer is disk-gated out of the box on an undeclared-need leaf (TB-31): %s", reason)
	}
}

// TB-30 (message polish) regression: when every fetchable leaf is disk-gated,
// the aggregate WARN must name its example leaf, and the example must be a
// leaf a disk remedy could actually unblock — not one the head refuses on GPU
// regardless of any allowance.
//
// The tester's log had both defects at once: "this leaf needs 10240 MB more"
// with no leaf named, and, for the -gpu leaves on a GPU-less host, disk
// remedies ("raise max_disk_gb") that could never change the outcome.
func TestTB30_AllGatedWarnNamesAFetchableLeaf(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 1 << 30}, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 10 // 10,240 MB allowance

	// First leaf requires a GPU this machine does not offer (cachedHW is nil)
	// and carries the bigger disk need — the misleading "first" reason the
	// pre-fix WARN quoted. Second is an ordinary native leaf gated by the
	// budget: 5,000 used + 8,000 need > 10,240.
	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{
			Leafs: []*lettucev1.LeafInfo{
				{
					Id: "leaf-gpu", Slug: "beyblade-gpu", State: "ACTIVE",
					ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 20000, GpuRequired: true},
				},
				{
					Id: "leaf-cpu", Slug: "beyblade-cpu", State: "ACTIVE",
					ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 8000},
				},
			},
		}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}

	d.diskUsageMu.Lock()
	d.diskUsageChecked = time.Now()
	d.diskUsageMB, d.diskUsageOK = 5000, true
	d.diskUsageMu.Unlock()

	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if d.shouldFetch() {
		t.Fatal("shouldFetch = true, want false — the fetchable leaf is over the budget")
	}
	s := buf.String()
	if !strings.Contains(s, "beyblade-cpu") {
		t.Errorf("the all-gated WARN must name its example leaf (beyblade-cpu); got: %s", s)
	}
	if strings.Contains(s, "20000") {
		t.Errorf("the quoted disk numbers must come from a leaf a disk remedy could unblock, not the GPU-impossible one (20,000 MB); got: %s", s)
	}
}

// TB-30 (message polish) regression: when every enabled leaf requires a GPU
// this machine does not offer, the daemon must not raise the disk-gated WARN
// at all — its remedies (free space, raise max_disk_gb) cannot change the
// outcome on this host.
func TestTB30_GPUOnlyCatalogDoesNotWarnDiskGated(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 1 << 30}, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 10

	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{
			Leafs: []*lettucev1.LeafInfo{{
				Id: "leaf-gpu", Slug: "beyblade-gpu", State: "ACTIVE",
				ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 20000, GpuRequired: true},
			}},
		}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}

	d.diskUsageMu.Lock()
	d.diskUsageChecked = time.Now()
	d.diskUsageMB, d.diskUsageOK = 5000, true
	d.diskUsageMu.Unlock()

	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if d.shouldFetch() {
		t.Fatal("shouldFetch = true, want false — the only leaf needs a GPU this machine does not offer")
	}
	if strings.Contains(buf.String(), "disk-gated") {
		t.Errorf("disk-gate WARN raised for a catalog no disk remedy could unblock; got: %s", buf.String())
	}
}

// TB-30 (message polish) regression, fetcher half: a leaf that requires a GPU
// this machine does not offer is skipped BEFORE RequestWorkUnit — the head
// refuses it at dispatch anyway — while ordinary leafs keep being requested.
func TestTB30_FetcherSkipsGPUImpossibleLeafBeforeRequesting(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 1 << 30}, scheduler)

	var requested []string
	mc.requestWorkUnitFn = func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		requested = append(requested, req.GetLeafIds()...)
		return &lettucev1.RequestWorkUnitResponse{}, nil
	}
	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{
			Leafs: []*lettucev1.LeafInfo{
				{
					Id: "leaf-gpu", Slug: "beyblade-gpu", State: "ACTIVE",
					ResourceRequirements: &lettucev1.LeafResourceRequirements{GpuRequired: true},
				},
				{
					Id: "leaf-cpu", Slug: "beyblade-cpu", State: "ACTIVE",
					ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 1024},
				},
			},
		}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}

	fetcher := NewFetcher(d, NewPreFetchQueue(4, quietLogger()), d.weightedSelector, d.leafCache)
	if _, err := fetcher.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne: %v", err)
	}

	for _, id := range requested {
		if id == "leaf-gpu" {
			t.Errorf("RequestWorkUnit issued for the GPU-impossible leaf — the head refuses it at dispatch, so the RPC only burns the request budget; requested: %v", requested)
		}
	}
	found := false
	for _, id := range requested {
		if id == "leaf-cpu" {
			found = true
		}
	}
	if !found {
		t.Errorf("the ordinary leaf was never requested; requested: %v", requested)
	}
}
