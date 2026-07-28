package cli

import (
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// gpuLeafInfo is a GPU leaf as the head describes it over gRPC, modelled on the
// live ones that exposed TB-21: NVIDIA, 4 GiB of VRAM, container runtime.
func gpuLeafInfo(vramMB int32, gpuType, computeCap string) *lettucev1.LeafInfo {
	return &lettucev1.LeafInfo{
		Id: "gpu-leaf", Slug: "gpu-leaf",
		ExecutionSpec: &lettucev1.ExecutionSpec{
			Image: "example.test/model:1.0", GpuRequired: true, GpuType: gpuType, MaxMemoryMb: 6000,
		},
		ResourceRequirements: &lettucev1.LeafResourceRequirements{
			MinDiskMb: 1024, MinCpuCores: 1,
			MinGpuVramMb: vramMB, GpuType: gpuType, GpuComputeCapability: computeCap,
			GpuRequired: true,
		},
	}
}

// TestEvaluateLeafEligibility_GPURequiredFromEitherFlag pins the presence gate to
// the dispatch predicate, which requires a GPU when EITHER gpu_required flag is
// set (they were historically unsynced — issue #30). The client read only the
// execution spec's, so a leaf that set just resource_requirements.gpu_required
// slipped past every GPU gate and a GPU-less machine reported itself eligible.
func TestEvaluateLeafEligibility_GPURequiredFromEitherFlag(t *testing.T) {
	noGPU := volunteerCaps{maxMemoryMB: 16384, containerUsable: true, hasGPU: false,
		maxDiskMB: 100 * 1024, maxCPUCores: 8}

	for _, tc := range []struct {
		name         string
		specFlag     bool
		rrFlag       bool
		wantEligible int
	}{
		{"neither flag", false, false, 1},
		{"execution spec only", true, false, 0},
		{"resource requirements only", false, true, 0},
		{"both", true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leafs := []*lettucev1.LeafInfo{{
				Id: "l", Slug: "l",
				ExecutionSpec: &lettucev1.ExecutionSpec{Image: "x:1", GpuRequired: tc.specFlag, MaxMemoryMb: 1024},
				ResourceRequirements: &lettucev1.LeafResourceRequirements{
					MinDiskMb: 1024, MinCpuCores: 1, GpuRequired: tc.rrFlag,
				},
			}}
			if res := evaluateLeafEligibility(leafs, noGPU, trustingHead); res.eligible != tc.wantEligible {
				t.Errorf("eligible=%d, want %d — the head requires a GPU when either flag is set",
					res.eligible, tc.wantEligible)
			}
		})
	}
}

// gpuCaps is a machine with one NVIDIA card, allowing pct% of it.
func gpuCaps(cardMB, pct int) volunteerCaps {
	return volunteerCaps{
		maxMemoryMB: 16384, containerUsable: true, maxDiskMB: 100 * 1024, maxCPUCores: 8,
		hasGPU:        true,
		maxGPUVRAMMB:  cardMB * pct / 100,
		gpuCardVRAMMB: cardMB,
		gpuVRAMPct:    pct,
		gpuVendors:    []string{"NVIDIA"},
	}
}

// TestEvaluateLeafEligibility_VRAMGate is the TB-21 regression test. The head
// compares a leaf's min_gpu_vram_mb against VRAM * max_gpu_vram_pct / 100 — the
// share the volunteer allows, not the card's size — and refuses the machine
// silently. The client checked GPU *presence* only, so `doctor` reported such a
// machine eligible and it idled with every local check passing.
func TestEvaluateLeafEligibility_VRAMGate(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{gpuLeafInfo(4096, "NVIDIA", "")}

	// A 6 GB card at the default 50% offers 3072 MB — below the 4096 gate. The
	// machine has a GPU, so presence alone would call this eligible.
	res := evaluateLeafEligibility(leafs, gpuCaps(6144, 50), trustingHead)
	if res.total != 1 || res.eligible != 0 || res.vramBlocked != 1 {
		t.Fatalf("total=%d eligible=%d vramBlocked=%d, want 1/0/1", res.total, res.eligible, res.vramBlocked)
	}

	// The reason must name all four numbers: what the leaf needs, what this machine
	// offers, the card it came from, and the percentage — because the setting is
	// what has to change, and naming only the shortfall sends people shopping for
	// hardware they already own.
	reason := res.leaves[0].reason
	for _, want := range []string{"4096", "3072", "6144", "50%", "max_gpu_vram_pct 67"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q missing %q", reason, want)
		}
	}

	// 67% of 6144 is 4116 — clears 4096. Rounding the suggestion down to 66 would
	// give 4055 and leave the volunteer still refused after restarting.
	if res := evaluateLeafEligibility(leafs, gpuCaps(6144, 67), trustingHead); res.eligible != 1 {
		t.Errorf("at the suggested 67%%, eligible=%d want 1 — the remedy does not clear the gate", res.eligible)
	}

	// An 8 GB card at the default clears it exactly (4096 >= 4096).
	if res := evaluateLeafEligibility(leafs, gpuCaps(8192, 50), trustingHead); res.eligible != 1 {
		t.Errorf("8 GB card at 50%%: eligible=%d, want 1", res.eligible)
	}
}

// TestEvaluateLeafEligibility_VRAMCardTooSmall covers the case where no setting
// helps. Suggesting a percentage that still falls short is the documented way this
// class of message wastes people's time.
func TestEvaluateLeafEligibility_VRAMCardTooSmall(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{gpuLeafInfo(12288, "", "")}
	res := evaluateLeafEligibility(leafs, gpuCaps(6144, 50), trustingHead)
	if res.vramBlocked != 1 {
		t.Fatalf("vramBlocked=%d, want 1", res.vramBlocked)
	}
	reason := res.leaves[0].reason
	if !strings.Contains(reason, "too small") {
		t.Errorf("reason %q should say the card cannot cover it whatever the percentage", reason)
	}
	if strings.Contains(reason, "max_gpu_vram_pct") {
		t.Errorf("reason %q suggests a percentage that cannot help", reason)
	}
}

// TestEvaluateLeafEligibility_GPUVendorGate covers the second invisible GPU
// dimension: dispatch matches execution_config.gpu_type against the vendors the
// volunteer advertises, so an AMD machine is refused an NVIDIA-pinned leaf while
// reporting itself eligible on GPU presence.
func TestEvaluateLeafEligibility_GPUVendorGate(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{gpuLeafInfo(0, "NVIDIA", "")}

	amd := gpuCaps(8192, 50)
	amd.gpuVendors = []string{"AMD"}
	res := evaluateLeafEligibility(leafs, amd, trustingHead)
	if res.eligible != 0 || res.gpuBlocked != 1 {
		t.Fatalf("eligible=%d gpuBlocked=%d, want 0/1", res.eligible, res.gpuBlocked)
	}
	if reason := res.leaves[0].reason; !strings.Contains(reason, "NVIDIA") || !strings.Contains(reason, "AMD") {
		t.Errorf("reason %q should name both the required and the present vendor", reason)
	}

	// "ANY" and empty are not constraints — the dispatch predicate admits both.
	for _, gpuType := range []string{"", "ANY"} {
		if res := evaluateLeafEligibility([]*lettucev1.LeafInfo{gpuLeafInfo(0, gpuType, "")}, amd, trustingHead); res.eligible != 1 {
			t.Errorf("gpu_type=%q: eligible=%d, want 1 (not a constraint)", gpuType, res.eligible)
		}
	}
}

// TestEvaluateLeafEligibility_GPUComputeCapabilityGate covers the third.
func TestEvaluateLeafEligibility_GPUComputeCapabilityGate(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{gpuLeafInfo(0, "", "8.6")}

	caps := gpuCaps(8192, 50)
	caps.gpuComputeCapabilities = []string{"7.5"}
	res := evaluateLeafEligibility(leafs, caps, trustingHead)
	if res.eligible != 0 || res.gpuBlocked != 1 {
		t.Fatalf("eligible=%d gpuBlocked=%d, want 0/1", res.eligible, res.gpuBlocked)
	}

	caps.gpuComputeCapabilities = []string{"8.6"}
	if res := evaluateLeafEligibility(leafs, caps, trustingHead); res.eligible != 1 {
		t.Errorf("matching compute capability: eligible=%d, want 1", res.eligible)
	}
}

// TestEvaluateLeafEligibility_GPUUnknownBudgetsDoNotBlock pins the
// unknown-is-not-zero rule TB-15 established, on the GPU dimensions. A head too
// old to send them, or a daemon started before this change, reports zeroes — and
// blocking every GPU leaf on that would be a false alarm worse than the silence it
// replaces.
func TestEvaluateLeafEligibility_GPUUnknownBudgetsDoNotBlock(t *testing.T) {
	// Head says nothing about GPU requirements (pre-TB-21 head).
	old := &lettucev1.LeafInfo{
		Id: "old", Slug: "old",
		ExecutionSpec:        &lettucev1.ExecutionSpec{Image: "x:1", GpuRequired: true, MaxMemoryMb: 1024},
		ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 1024, MinCpuCores: 1},
	}
	if res := evaluateLeafEligibility([]*lettucev1.LeafInfo{old}, gpuCaps(4096, 25), trustingHead); res.eligible != 1 {
		t.Errorf("leaf with no stated GPU requirements: eligible=%d, want 1", res.eligible)
	}

	// Daemon reports no GPU budget (pre-TB-21 daemon), leaf does state one.
	unknown := volunteerCaps{maxMemoryMB: 16384, containerUsable: true, hasGPU: true,
		maxDiskMB: 100 * 1024, maxCPUCores: 8}
	if res := evaluateLeafEligibility([]*lettucev1.LeafInfo{gpuLeafInfo(4096, "NVIDIA", "8.6")}, unknown, trustingHead); res.eligible != 1 {
		t.Errorf("machine with unknown GPU budgets: eligible=%d, want 1", res.eligible)
	}
}

// TestPrintLeafsTable_VRAMBlockedLeafSaysNo pins the other surface: `leafs list`
// routes through the same classifier, so its WILL FETCH column must answer
// identically. TB-4 added that column precisely to answer "will this machine run
// this leaf".
func TestPrintLeafsTable_VRAMBlockedLeafSaysNo(t *testing.T) {
	resp := &leafsAPIResponse{
		Machine: leafsAPIMachine{
			Runtimes: []string{"container"}, HasGPU: true, MaxMemoryMB: 16384,
			MaxDiskMB: 100 * 1024, MaxCPUCores: 8,
			MaxGPUVRAMMB: 3072, GPUCardVRAMMB: 6144, GPUVRAMPct: 50,
			GPUVendors: []string{"NVIDIA"},
		},
		Heads: []leafsAPIHead{{
			Name: "test-head", GRPCAddress: "test-head:9090",
			Leafs: []leafsAPILeaf{{
				Slug: "gpu-leaf", Name: "GPU Leaf", State: "ACTIVE", Enabled: true,
				ExecutionSpec:        &leafsAPIExecutionSpec{Image: "x:1", GPURequired: true, MaxMemoryMB: 6000},
				ResourceRequirements: &leafsAPIResourceRequirements{MinDiskMB: 1024, MinCPUCores: 1, MinGPUVRAMMB: 4096},
			}},
		}},
	}

	var out strings.Builder
	printLeafsTable(&out, resp, []config.ServerConfig{trustingHead})
	got := out.String()
	if !strings.Contains(got, "gpu-leaf") {
		t.Fatalf("leaf missing from table:\n%s", got)
	}
	// The row must not claim the machine will fetch a leaf the head refuses.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "gpu-leaf") && strings.HasSuffix(strings.TrimSpace(line), "yes") {
			t.Errorf("WILL FETCH says yes for a leaf the head refuses on VRAM:\n%s", line)
		}
	}
	if !strings.Contains(got, "max_gpu_vram_pct") {
		t.Errorf("blocking note should name the setting to change:\n%s", got)
	}
}
