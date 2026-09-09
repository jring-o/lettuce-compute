//go:build linux

package resource

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-75 regression tests, Linux limiter half: the cgroup path enforces the
// task's SHARE of the budget (fractional, rewritable), and the affinity
// fallback — which cannot express a fraction — confines every task to the
// same budget-sized CPU set, so the total is still bounded by the budget.

// TestTB75_CgroupCPUMaxIsTheShareAndCanBeRewritten: cpu.max carries the
// share's exact CFS quota (1.5 cores = "150000 100000"); a rewrite replaces
// it; a share of 0 lifts the cap. Written to a scratch directory standing in
// for the cgroup scope, since delegation is not available on CI.
func TestTB75_CgroupCPUMaxIsTheShareAndCanBeRewritten(t *testing.T) {
	dir := t.TempDir()
	read := func() string {
		b, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
		if err != nil {
			t.Fatalf("read cpu.max: %v", err)
		}
		return string(b)
	}
	if err := writeCPUMax(dir, 1.5); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "150000 100000" {
		t.Errorf("cpu.max for 1.5 cores = %q, want \"150000 100000\"", got)
	}
	if err := writeCPUMax(dir, 2); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "200000 100000" {
		t.Errorf("cpu.max after the rewrite to 2 cores = %q, want \"200000 100000\"", got)
	}
	if err := writeCPUMax(dir, 0); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "max 100000" {
		t.Errorf("cpu.max with no limit = %q, want \"max 100000\"", got)
	}
}

// TestTB75_FallbackPinsEveryTaskToTheBudgetSet: on the affinity fallback a
// task granted half a core of a 2-core budget is pinned to the budget's two
// CPUs, not to one CPU (which would halve the machine's throughput) and not
// to the whole machine (the pre-fix behaviour once N tasks were each pinned
// to N CPUs of their own count). SetCPU re-pins to a changed budget.
func TestTB75_FallbackPinsEveryTaskToTheBudgetSet(t *testing.T) {
	rec := withFakeAffinity(t, []int{0, 1, 2, 3, 4, 5, 6, 7}, nil)
	l := &LinuxLimiter{logger: testLimiter().logger, useCgroups: false}

	cleanup, err := l.enforceFallback(4242, &TaskLimits{CPU: runtime.CPUGrant{ShareCores: 0.5, BudgetCores: 2}})
	if err != nil {
		t.Fatalf("enforceFallback: %v", err)
	}
	defer cleanup()
	if want := []int{0, 1}; !reflect.DeepEqual(rec.cpus, want) {
		t.Errorf("pinned to %v, want the 2-core budget's set %v", rec.cpus, want)
	}

	if err := l.SetCPU(4242, runtime.CPUGrant{ShareCores: 1, BudgetCores: 3}); err != nil {
		t.Fatalf("SetCPU: %v", err)
	}
	if want := []int{0, 1, 2}; !reflect.DeepEqual(rec.cpus, want) {
		t.Errorf("after a budget change pinned to %v, want %v", rec.cpus, want)
	}
}
