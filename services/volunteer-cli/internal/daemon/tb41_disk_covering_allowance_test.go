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

// TB-41: the budget refusal must name the allowance that actually clears it —
// max(the leaf's declared need, usage + incremental need) — because a refusal
// without the covering value sent a tester stepping max_disk_gb
// 20 → 22 → 24 → 26 → 27, each step "as advised" by a message naming only the
// shortfall. His exact numbers: 16,527 MB used, 15,000 MB declared need with
// the image cached (10,240 incremental), 20 GB allowance; the number that
// ended the chase was 27.
func TestTB41_BudgetRefusalNamesTheCoveringAllowance(t *testing.T) {
	ok, reason := DiskBudgetVerdict(15000, true, true, 16527, 20480)
	if ok {
		t.Fatal("16,527 + 10,240 exceeds 20,480 — expected a refusal")
	}
	if !strings.Contains(reason, "raise resource_limits.max_disk_gb to 27") {
		t.Errorf("the refusal must name the covering allowance (27), not just the setting; got: %s", reason)
	}
}

// TB-41: the covering allowance honors BOTH gates. A cached image charges only
// 10,240 MB incrementally, but the head's dispatch gate still compares the
// full declared need against the advertised allowance — so a low-usage machine
// must not be told a number the head would then refuse dispatch under.
func TestTB41_CoveringAllowanceHonorsBothGates(t *testing.T) {
	// Usage-bound: max(15000, 16527+10240) = 26767 → 27 GB.
	if got := DiskAllowanceGBToCover(15000, true, true, 16527); got != 27 {
		t.Errorf("usage-bound covering allowance = %d, want 27", got)
	}
	// Need-bound: max(15000, 1000+10240) = 15000 → 15 GB; anything less and
	// the head stops dispatching the leaf at all.
	if got := DiskAllowanceGBToCover(15000, true, true, 1000); got != 15 {
		t.Errorf("need-bound covering allowance = %d, want 15", got)
	}
	// Fresh pull charges the full need: max(15000, 16527+15000) = 31527 → 31.
	if got := DiskAllowanceGBToCover(15000, true, false, 16527); got != 31 {
		t.Errorf("fresh-pull covering allowance = %d, want 31", got)
	}
}

// TB-41/TB-42: LeafDiskGateStatus is the verdict the management API serves so
// `leafs list` and doctor can quote the live gate instead of recomputing it.
// It must agree with leafDiskGate and carry the covering allowance computed
// with the REAL cachedness — the input no client-side recomputation has.
func TestTB41_LeafDiskGateStatusMatchesTheLiveGate(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 1 << 30}, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 20 // the tester's 20,480 MB state

	// The GREP leaf with its image already cached, as on the tester's host.
	dc := &fakeDockerPerImage{cached: map[string]bool{"ghcr.io/example/grep:1": true}}
	d.runtimeRegistry.Register(runtime.NewContainerRuntimeWithClient(t.TempDir(), quietLogger(), dc))
	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{
			Leafs: []*lettucev1.LeafInfo{{
				Id: "leaf-grep", Slug: "grep-cpu", State: "ACTIVE",
				ExecutionSpec:        &lettucev1.ExecutionSpec{Image: "ghcr.io/example/grep:1"},
				ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 15000},
			}},
		}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}
	d.diskUsageMu.Lock()
	d.diskUsageChecked = time.Now()
	d.diskUsageMB, d.diskUsageOK = 16527, true
	d.diskUsageMu.Unlock()

	leafs := d.allEnabledLeafs()
	if len(leafs) != 1 {
		t.Fatalf("enabled leafs = %d, want 1", len(leafs))
	}

	st := d.LeafDiskGateStatus(leafs[0])
	if !st.Blocked {
		t.Fatal("Blocked = false at the 20 GB allowance the live gate refuses (16,527 + 10,240 > 20,480)")
	}
	if liveOK, liveReason := d.leafDiskGate(leafs[0]); liveOK || st.Reason != liveReason {
		t.Errorf("status must quote the live gate verbatim: status=%q live=(%v, %q)", st.Reason, liveOK, liveReason)
	}
	if st.RaiseToGB != 27 {
		t.Errorf("RaiseToGB = %d, want 27 — the cached-image arithmetic only the daemon can do", st.RaiseToGB)
	}

	// At the tester's fixed point, 27 GB, the gate opens and the status says so.
	d.cfg.ResourceLimits.MaxDiskGB = 27
	if st := d.LeafDiskGateStatus(leafs[0]); st.Blocked {
		t.Errorf("Blocked = true at 27 GB (26,767 ≤ 27,648), reason: %s", st.Reason)
	}
}
