package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-63 regression tests, diagnostics half.
//
// `doctor` compared a leaf's memory requirement with resource_limits.max_memory_mb
// and never mentioned the container engine's virtual machine, so on the
// tester's Mac it called a 7000 MB leaf eligible while the 2 GiB Podman machine
// killed every unit of it. The memory budget both diagnostics reason from is now
// the configuration clipped to the VM (runtime.ContainerMemoryBudgetMB), and a
// leaf blocked by the VM names the machine to enlarge, not a limit to raise.

// tb63VMCaps is the tester's Mac as doctor now sees it: 8192 configured, a
// 2048 MB machine, so 1536 MB for container work.
func tb63VMCaps() volunteerCaps {
	return volunteerCaps{
		maxMemoryMB:         runtime.ContainerMemoryBudgetMB(8192, 2048),
		configMemoryMB:      8192,
		containerVMMemoryMB: 2048,
		memoryLimitedByVM:   true,
		containerUsable:     true,
	}
}

// TestTB63_ClassifyLeafNamesTheContainerVM: a 7000 MB container leaf is
// blocked on memory, and the reason names the budget, the VM's size and the
// resize command rather than "your limit 8192 MB" — which would send the
// volunteer to raise a setting that changes nothing. A leaf within the budget
// is eligible. Pre-fix: the 7000 MB leaf was eligible against 8192.
func TestTB63_ClassifyLeafNamesTheContainerVM(t *testing.T) {
	caps := tb63VMCaps()
	grep := leafRequirementsFromSpec("grep-f14", "ghcr.io/example/grep:1.2", nil, 7000, false, leafMachineNeeds{})

	le, blocked := classifyLeaf(grep, caps, trustingHead)
	if le.eligible || blocked != "memory" {
		t.Fatalf("7000 MB leaf: eligible=%v blocked=%q, want blocked on memory", le.eligible, blocked)
	}
	for _, want := range []string{"7000 MB", "1536 MB", "2048 MB", "podman machine set --memory"} {
		if !strings.Contains(le.reason, want) {
			t.Errorf("reason lacks %q: %s", want, le.reason)
		}
	}
	if strings.Contains(le.reason, "your limit") {
		t.Errorf("reason blames the limit, which is not the bound: %s", le.reason)
	}

	small := leafRequirementsFromSpec("small", "ghcr.io/example/small:1", nil, 1024, false, leafMachineNeeds{})
	if le, _ := classifyLeaf(small, caps, trustingHead); !le.eligible {
		t.Errorf("1024 MB leaf blocked against a 1536 MB budget: %s", le.reason)
	}

	// Without a VM the wording is the configuration's, as before.
	plain := volunteerCaps{maxMemoryMB: 8192, configMemoryMB: 8192, containerUsable: true}
	big := leafRequirementsFromSpec("big", "ghcr.io/example/big:1", nil, 16000, false, leafMachineNeeds{})
	if le, _ := classifyLeaf(big, plain, trustingHead); le.eligible || !strings.Contains(le.reason, "your limit 8192 MB") {
		t.Errorf("no-VM wording changed: eligible=%v %s", le.eligible, le.reason)
	}
}

// TestTB63_EvaluateLeafEligibilityCountsTheVMBlock: the per-head summary
// counts a VM-blocked leaf as memory-blocked, so the head line's remedy
// branch fires.
func TestTB63_EvaluateLeafEligibilityCountsTheVMBlock(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{
		{Id: "grep", Slug: "grep-f14", ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ghcr.io/example/grep:1.2", MaxMemoryMb: 7000}},
		{Id: "small", Slug: "small", ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ghcr.io/example/small:1", MaxMemoryMb: 1024}},
	}
	res := evaluateLeafEligibility(leafs, tb63VMCaps(), trustingHead, nil)
	if res.total != 2 || res.eligible != 1 || res.memoryBlocked != 1 {
		t.Errorf("total=%d eligible=%d memoryBlocked=%d, want 2/1/1", res.total, res.eligible, res.memoryBlocked)
	}
}

// TestTB63_CheckMemoryBudgetWarnsWhenTheVMClips: the "memory limit" line is a
// warning naming the budget, the limit, the VM and the headroom, with the
// resize remedy; with a VM that honors the limit it is informational and says
// so; with no VM it is the old line.
func TestTB63_CheckMemoryBudgetWarnsWhenTheVMClips(t *testing.T) {
	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	checkMemoryBudget(rep, tb63VMCaps())
	out := buf.String()
	if rep.warns != 1 {
		t.Errorf("warns = %d, want 1:\n%s", rep.warns, out)
	}
	for _, want := range []string{"1536 MB", "8192 MB", "2048 MB", "512 MB", "podman machine set --memory", "raising max_memory_mb alone changes nothing"} {
		if !strings.Contains(out, want) {
			t.Errorf("memory limit line lacks %q:\n%s", want, out)
		}
	}

	buf.Reset()
	rep = &doctorReport{w: &buf}
	checkMemoryBudget(rep, volunteerCaps{maxMemoryMB: 4096, configMemoryMB: 4096, containerVMMemoryMB: 8192})
	if rep.warns != 0 || !strings.Contains(buf.String(), "8192 MB, enough to honor it") {
		t.Errorf("VM that honors the limit: warns=%d\n%s", rep.warns, buf.String())
	}

	buf.Reset()
	rep = &doctorReport{w: &buf}
	checkMemoryBudget(rep, volunteerCaps{maxMemoryMB: 8192, configMemoryMB: 8192})
	if rep.warns != 0 || !strings.Contains(buf.String(), "8192 MB (resource_limits.max_memory_mb)") || strings.Contains(buf.String(), "virtual machine") {
		t.Errorf("no-VM line changed:\n%s", buf.String())
	}
}

// TestTB63_LeafsTableReadsTheVMFiguresFromTheDaemon: `leafs list` takes the
// budget and the VM figures from the running daemon's machine record and
// prints the same VM-naming reason under the table, with WILL FETCH "no".
func TestTB63_LeafsTableReadsTheVMFiguresFromTheDaemon(t *testing.T) {
	resp := &leafsAPIResponse{
		Machine: leafsAPIMachine{Runtimes: []string{"container", "wasm"}, MaxMemoryMB: 1536,
			ContainerVMMemoryMB: 2048, MemoryLimitedByVM: true, MaxDiskMB: 100 * 1024, MaxCPUCores: 4},
		Heads: []leafsAPIHead{{
			Name: "test-head", GRPCAddress: "head:9090", LeafsRefreshedAt: time.Now(),
			Leafs: []leafsAPILeaf{{
				Slug: "grep-f14", Name: "GREP f14", State: "ACTIVE", Enabled: true,
				ExecutionSpec: &leafsAPIExecutionSpec{Image: "ghcr.io/example/grep:1.2", MaxMemoryMB: 7000},
			}},
		}},
	}
	servers := []config.ServerConfig{{Name: "test-head", GRPCAddress: "head:9090", TrustedRuntimes: []string{"CONTAINER"}}}

	var buf bytes.Buffer
	printLeafsTable(&buf, resp, servers)
	out := buf.String()
	for _, want := range []string{"7000 MB", "1536 MB", "2048 MB", "podman machine set --memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("leafs list lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "grep-f14") && strings.Contains(line, "ACTIVE") && !strings.Contains(line, "no") {
			t.Errorf("the leaf's row does not say WILL FETCH no: %q", line)
		}
	}
}
