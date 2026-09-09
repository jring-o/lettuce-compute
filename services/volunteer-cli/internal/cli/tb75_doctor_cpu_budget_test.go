package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TB-75 regression tests, diagnostics half.
//
// `doctor`'s "cpu limit" line read as a per-task figure ("a head only sends
// leafs whose required cores fit under this") and never mentioned that the
// number is the whole machine's budget, shared by every running task, or that
// the container engine's virtual machine bounds it on macOS/Windows.

// tb75VMCaps is a Mac with max_cpu_cores 6 and a 4-vCPU Podman machine, as
// doctor now sees it: a 4-core budget bounded by the VM.
func tb75VMCaps() volunteerCaps {
	return volunteerCaps{
		maxMemoryMB: 8192, configMemoryMB: 8192, containerUsable: true,
		maxCPUCores: 4, configCPUCores: 6, containerVMCPUs: 4, cpuLimitedByVM: true,
	}
}

// TestTB75_DoctorCPULineSaysSharedTotalAndNamesTheVM: the line says the
// figure is shared by all running tasks; when the VM bounds it, it is a WARN
// naming both figures and the machine to enlarge; a VM that honors the limit
// says so; no VM prints the plain line.
func TestTB75_DoctorCPULineSaysSharedTotalAndNamesTheVM(t *testing.T) {
	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	checkCPUBudget(rep, tb75VMCaps())
	out := buf.String()
	if rep.warns != 1 {
		t.Errorf("VM-bounded budget: warns=%d, want 1\n%s", rep.warns, out)
	}
	for _, want := range []string{"4 cores", "your limit is 6", "4 CPUs", "share it equally", "podman machine set --cpus", "raising max_cpu_cores alone changes nothing"} {
		if !strings.Contains(out, want) {
			t.Errorf("cpu limit line lacks %q:\n%s", want, out)
		}
	}

	buf.Reset()
	rep = &doctorReport{w: &buf}
	checkCPUBudget(rep, volunteerCaps{maxCPUCores: 2, configCPUCores: 2, containerVMCPUs: 4})
	if rep.warns != 0 || !strings.Contains(buf.String(), "shared equally by all running tasks") || !strings.Contains(buf.String(), "4 CPUs, enough to honor it") {
		t.Errorf("VM that honors the limit: warns=%d\n%s", rep.warns, buf.String())
	}

	buf.Reset()
	rep = &doctorReport{w: &buf}
	checkCPUBudget(rep, volunteerCaps{maxCPUCores: 2, configCPUCores: 2})
	out = buf.String()
	if rep.warns != 0 || !strings.Contains(out, "2 cores (resource_limits.max_cpu_cores)") || !strings.Contains(out, "shared equally by all running tasks") || strings.Contains(out, "virtual machine") {
		t.Errorf("no-VM line:\n%s", out)
	}
}

// TestTB75_ClassifyLeafNamesTheVMForCores: a leaf needing more cores than the
// VM has is blocked on cores with a reason that names the VM and its resize
// command rather than a limit to raise; within the budget it is eligible.
func TestTB75_ClassifyLeafNamesTheVMForCores(t *testing.T) {
	caps := tb75VMCaps()
	six := leafRequirementsFromSpec("grep", "ghcr.io/example/grep:1", nil, 1024, false, leafMachineNeeds{cpuCores: 6})
	le, blocked := classifyLeaf(six, caps, trustingHead)
	if le.eligible || blocked != "cores" {
		t.Fatalf("6-core leaf: eligible=%v blocked=%q, want blocked on cores", le.eligible, blocked)
	}
	for _, want := range []string{"6 CPU cores", "4 this machine can give", "4 CPUs", "podman machine set --cpus"} {
		if !strings.Contains(le.reason, want) {
			t.Errorf("reason lacks %q: %s", want, le.reason)
		}
	}
	if strings.Contains(le.reason, "your limit") {
		t.Errorf("reason blames the limit, which is not the bound: %s", le.reason)
	}

	four := leafRequirementsFromSpec("small", "ghcr.io/example/small:1", nil, 1024, false, leafMachineNeeds{cpuCores: 4})
	if le, _ := classifyLeaf(four, caps, trustingHead); !le.eligible {
		t.Errorf("4-core leaf blocked against a 4-core budget: %s", le.reason)
	}

	// Without a VM the reason still points at the setting.
	plain := volunteerCaps{maxMemoryMB: 8192, maxCPUCores: 2, configCPUCores: 2, containerUsable: true}
	if le, blocked := classifyLeaf(six, plain, trustingHead); le.eligible || blocked != "cores" || !strings.Contains(le.reason, "config set resource_limits.max_cpu_cores 6") {
		t.Errorf("no-VM 6-core leaf: eligible=%v blocked=%q reason=%s", le.eligible, blocked, le.reason)
	}
}
