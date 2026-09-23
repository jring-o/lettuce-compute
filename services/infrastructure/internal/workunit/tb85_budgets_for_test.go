package workunit

import "testing"

// TestTB85_BudgetsForPicksTheLeafRuntimesBudgets: a NATIVE or WASM leaf (and
// one with no runtime, NATIVE by default) is matched against the requester's
// host budgets; a CONTAINER leaf against the single, VM-clipped figures; a
// requester that reported no host budget is matched on the single figures
// for every runtime, one dimension at a time.
func TestTB85_BudgetsForPicksTheLeafRuntimesBudgets(t *testing.T) {
	opts := AssignmentOptions{MaxCPUCores: 2, MaxMemoryMB: 768, HostMaxCPUCores: 4, HostMaxMemoryMB: 1024}
	for _, tc := range []struct {
		runtime   string
		cpu, memo int
	}{
		{"NATIVE", 4, 1024}, {"WASM", 4, 1024}, {"", 4, 1024}, {"CONTAINER", 2, 768},
	} {
		if cpu, mem := opts.BudgetsFor(tc.runtime); cpu != tc.cpu || mem != tc.memo {
			t.Errorf("BudgetsFor(%q) = %d cores / %d MB, want %d / %d", tc.runtime, cpu, mem, tc.cpu, tc.memo)
		}
	}
	old := AssignmentOptions{MaxCPUCores: 2, MaxMemoryMB: 768}
	if cpu, mem := old.BudgetsFor("NATIVE"); cpu != 2 || mem != 768 {
		t.Errorf("no host budgets reported: BudgetsFor(NATIVE) = %d / %d, want the single figures 2 / 768", cpu, mem)
	}
	memOnly := AssignmentOptions{MaxCPUCores: 2, MaxMemoryMB: 768, HostMaxMemoryMB: 1024}
	if cpu, mem := memOnly.BudgetsFor("NATIVE"); cpu != 2 || mem != 1024 {
		t.Errorf("only the memory host budget reported: BudgetsFor(NATIVE) = %d / %d, want 2 / 1024", cpu, mem)
	}
}
