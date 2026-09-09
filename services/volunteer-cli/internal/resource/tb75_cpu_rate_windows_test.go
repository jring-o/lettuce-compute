//go:build windows

package resource

import (
	"log/slog"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-75 regression tests, Windows limiter half: the Job Object's CPU rate is
// the task's SHARE of the budget as a fraction of the machine, and it can be
// rewritten while the process runs.

// TestTB75_CPURateIsTheShareOfTheMachine: 1.5 cores of an 8-CPU machine is
// 18.75 % → 1875; the API's 1 %–100 % range clamps a tiny or oversized share.
func TestTB75_CPURateIsTheShareOfTheMachine(t *testing.T) {
	cases := []struct {
		share  float64
		numCPU int
		want   uint32
	}{
		{1.5, 8, 1875}, {2, 4, 5000}, {1, 8, 1250}, {0.5, 16, 312}, {0.01, 64, 100}, {16, 8, 10000},
	}
	for _, c := range cases {
		if got := cpuRateFor(c.share, c.numCPU); got != c.want {
			t.Errorf("cpuRateFor(%v, %d) = %d, want %d", c.share, c.numCPU, got, c.want)
		}
	}
}

// TestTB75_JobObjectRateCanBeRewrittenWhileRunning: the job handle is kept
// for the process's life so SetCPU can change its rate; after cleanup the
// pid is unknown to the limiter. Pre-fix the handle lived only in the cleanup
// closure and the rate was fixed at the whole budget.
func TestTB75_JobObjectRateCanBeRewrittenWhileRunning(t *testing.T) {
	l := NewWindowsLimiter(slog.Default())
	pid := startLimiterTestChild(t)

	if err := l.SetCPU(pid, runtime.CPUGrant{ShareCores: 1, BudgetCores: 2}); err == nil {
		t.Error("SetCPU before Enforce succeeded; the limiter has no job for that pid yet")
	}
	cleanup, err := l.Enforce(pid, &TaskLimits{CPU: runtime.CPUGrant{ShareCores: 2, BudgetCores: 2}})
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if err := l.SetCPU(pid, runtime.CPUGrant{ShareCores: 1, BudgetCores: 2}); err != nil {
		t.Errorf("SetCPU while the process runs: %v", err)
	}
	if err := l.SetCPU(pid, runtime.CPUGrant{ShareCores: 0}); err != nil {
		t.Errorf("SetCPU lifting the cap: %v", err)
	}
	cleanup()
	if err := l.SetCPU(pid, runtime.CPUGrant{ShareCores: 1, BudgetCores: 2}); err == nil {
		t.Error("SetCPU after cleanup succeeded; the job handle should be released")
	}
}
