package runtime

import (
	"reflect"
	"testing"
)

// The CPU grant's arithmetic: a grant converts to a CFS quota exactly, the
// budget is clipped to the engine VM's CPUs (TB-75), a task is told its grant,
// and the grant a unit starts with is the one the daemon gave it.

// TestTB75_CFSQuotaIsTheGrant: 3 cores is 300000 µs of a 100000 µs period; no
// limit is 0/0 (the engine's "unlimited").
func TestTB75_CFSQuotaIsTheGrant(t *testing.T) {
	cases := []struct {
		cores         int
		quota, period int64
	}{
		{3, 300000, 100000}, {1, 100000, 100000}, {0, 0, 0},
	}
	for _, c := range cases {
		q, p := CFSQuota(c.cores)
		if q != c.quota || p != c.period {
			t.Errorf("CFSQuota(%d) = %d/%d, want %d/%d", c.cores, q, p, c.quota, c.period)
		}
	}
}

// TestTB75_BudgetIsClippedToTheEngineVM: a 4-vCPU Podman machine bounds a
// 6-core limit to 4; a limit that fits stands; no VM (Linux) never clips.
func TestTB75_BudgetIsClippedToTheEngineVM(t *testing.T) {
	cases := []struct{ config, vm, want int }{
		{6, 4, 4}, {2, 4, 2}, {4, 4, 4}, {6, 0, 6}, {0, 4, 0},
	}
	for _, c := range cases {
		if got := ContainerCPUBudget(c.config, c.vm); got != c.want {
			t.Errorf("ContainerCPUBudget(%d, %d) = %d, want %d", c.config, c.vm, got, c.want)
		}
	}
}

// TestGrantEnvTellsTheTaskItsCores: LETTUCE_CPU_LIMIT carries the
// grant and every thread-pool knob carries it as a thread count, including
// DOCLING_NUM_THREADS for document-conversion leafs; no limit means no
// entries at all.
func TestGrantEnvTellsTheTaskItsCores(t *testing.T) {
	got := CPUGrant{Cores: 3, BudgetCores: 4}.Env()
	want := []string{"LETTUCE_CPU_LIMIT=3", "OMP_NUM_THREADS=3", "OPENBLAS_NUM_THREADS=3", "MKL_NUM_THREADS=3", "NUMEXPR_MAX_THREADS=3", "DOCLING_NUM_THREADS=3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Env() for 3 cores = %v, want %v", got, want)
	}
	if got := (CPUGrant{}).Threads(); got != 1 {
		t.Errorf("Threads() with no grant = %d, want 1 (never zero threads)", got)
	}
	if got := (CPUGrant{}).Env(); got != nil {
		t.Errorf("Env() with no limit = %v, want nothing", got)
	}
	if got := (CPUGrant{Cores: 3, BudgetCores: 4}).String(); got != "3 of 4 cores" {
		t.Errorf("String() = %q", got)
	}
}

// TestTheUnitsGrantIsTheOneUsed: a unit the daemon granted cores runs
// on that grant whatever the runtime's own budget; a unit given none (the
// audit runner's case) gets the runtime's whole fixed budget; with neither
// there is no limit.
func TestTheUnitsGrantIsTheOneUsed(t *testing.T) {
	budget := staticCPUGrant(8)
	granted := &WorkUnit{ID: "u", CPUGrant: CPUGrant{Cores: 3, BudgetCores: 4}}
	if got := grantFor(granted, budget); got != (CPUGrant{Cores: 3, BudgetCores: 4}) {
		t.Errorf("grantFor(granted unit) = %+v, want its own 3 of 4", got)
	}
	if got := grantFor(&WorkUnit{ID: "a"}, budget); got != (CPUGrant{Cores: 8, BudgetCores: 8}) {
		t.Errorf("grantFor(ungranted unit) = %+v, want the runtime's whole 8-core budget", got)
	}
	if got := grantFor(&WorkUnit{ID: "b"}, nil); got != (CPUGrant{}) {
		t.Errorf("grantFor with no grant and no budget = %+v, want no limit", got)
	}
	if got := staticCPUGrant(0)(); got != (CPUGrant{}) {
		t.Errorf("staticCPUGrant(0) = %+v, want no limit", got)
	}
}
