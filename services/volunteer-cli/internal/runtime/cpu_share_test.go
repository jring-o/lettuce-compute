package runtime

import (
	"reflect"
	"testing"
)

// TB-75 regression tests, arithmetic half: the CPU budget is a total that
// running tasks share equally, a share converts to a CFS quota exactly, the
// budget is clipped to the engine VM's CPUs, and a task is told its share.

// TestTB75_SharesSplitTheBudgetAndNeverExceedIt is the decided rule: a task
// alone gets the whole budget, two get half each, and the shares of any
// number of tasks sum to at most the budget. Pre-fix every task was given
// the whole figure, so two tasks under a 2-core limit used four cores.
func TestTB75_SharesSplitTheBudgetAndNeverExceedIt(t *testing.T) {
	cases := []struct {
		budget, running int
		want            float64
	}{
		{2, 1, 2}, {2, 2, 1}, {2, 3, 0.66}, {3, 2, 1.5}, {1, 4, 0.25}, {8, 3, 2.66},
		{2, 0, 2},  // fewer than one running task is the lone task's share
		{0, 2, 0},  // no limit configured: no share, nothing enforced
		{-1, 1, 0}, // a nonsense budget is no budget
	}
	for _, c := range cases {
		if got := CPUShareCores(c.budget, c.running); got != c.want {
			t.Errorf("CPUShareCores(%d, %d) = %v, want %v", c.budget, c.running, got, c.want)
		}
	}
	for budget := 1; budget <= 16; budget++ {
		for running := 1; running <= 8; running++ {
			if sum := CPUShareCores(budget, running) * float64(running); sum > float64(budget)+1e-9 {
				t.Errorf("budget %d shared by %d: shares sum to %v, above the budget", budget, running, sum)
			}
		}
	}
}

// TestTB75_CFSQuotaIsExactForFractions: 1.5 cores is 150000 µs of a 100000 µs
// period; no limit is 0/0 (the engine's "unlimited"); a tiny share is floored
// at the kernel's 1 ms minimum rather than rejected.
func TestTB75_CFSQuotaIsExactForFractions(t *testing.T) {
	cases := []struct {
		share         float64
		quota, period int64
	}{
		{2, 200000, 100000}, {1.5, 150000, 100000}, {0.66, 66000, 100000}, {0.005, 1000, 100000}, {0, 0, 0},
	}
	for _, c := range cases {
		q, p := CFSQuota(c.share)
		if q != c.quota || p != c.period {
			t.Errorf("CFSQuota(%v) = %d/%d, want %d/%d", c.share, q, p, c.quota, c.period)
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

// TestTB75_GrantEnvTellsTheTaskItsShare: LETTUCE_CPU_LIMIT carries the exact
// share and the thread knobs carry it as a whole thread count (at least 1);
// no limit means no entries at all.
func TestTB75_GrantEnvTellsTheTaskItsShare(t *testing.T) {
	got := CPUGrant{ShareCores: 1.5, BudgetCores: 3}.Env()
	want := []string{"LETTUCE_CPU_LIMIT=1.5", "OMP_NUM_THREADS=2", "OPENBLAS_NUM_THREADS=2", "MKL_NUM_THREADS=2", "NUMEXPR_MAX_THREADS=2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Env() for 1.5 cores = %v, want %v", got, want)
	}
	if got := (CPUGrant{ShareCores: 2, BudgetCores: 2}).Env()[0]; got != "LETTUCE_CPU_LIMIT=2" {
		t.Errorf("a whole share prints with a decimal point: %s", got)
	}
	if got := (CPUGrant{ShareCores: 0.25, BudgetCores: 1}).Threads(); got != 1 {
		t.Errorf("Threads() for a quarter core = %d, want 1 (never zero threads)", got)
	}
	if got := (CPUGrant{}).Env(); got != nil {
		t.Errorf("Env() with no limit = %v, want nothing", got)
	}
	if got := (CPUGrant{ShareCores: 0.66, BudgetCores: 2}).String(); got != "0.66 of 2 cores" {
		t.Errorf("String() = %q", got)
	}
}
