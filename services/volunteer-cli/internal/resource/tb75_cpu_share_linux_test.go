//go:build linux

package resource

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-75 regression tests, Linux limiter half: the cgroup path holds a
// task to its grant, and the affinity fallback — which cannot hold one
// process to a quota — confines every task to the same budget-sized CPU set,
// so the total is still bounded by the budget.

// TestTB75_CgroupCPUMaxIsTheGrant: cpu.max carries the grant's CFS quota
// (3 cores = "300000 100000"); a grant of 0 lifts the cap. Written to a
// scratch directory standing in for the cgroup scope, since delegation is not
// available on CI.
func TestTB75_CgroupCPUMaxIsTheGrant(t *testing.T) {
	dir := t.TempDir()
	read := func() string {
		b, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
		if err != nil {
			t.Fatalf("read cpu.max: %v", err)
		}
		return string(b)
	}
	if err := writeCPUMax(dir, 3); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "300000 100000" {
		t.Errorf("cpu.max for 3 cores = %q, want \"300000 100000\"", got)
	}
	if err := writeCPUMax(dir, 2); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "200000 100000" {
		t.Errorf("cpu.max for 2 cores = %q, want \"200000 100000\"", got)
	}
	if err := writeCPUMax(dir, 0); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "max 100000" {
		t.Errorf("cpu.max with no limit = %q, want \"max 100000\"", got)
	}
}

// TestFallbackPinsEachTaskToItsGrant: on the affinity fallback each
// task is pinned to as many of the budget's CPUs as it was granted, the ones
// the fewest running tasks are on, so a 4-core budget runs a 2-core GREP on
// two CPUs and two 1-core tasks on one each, never beyond the budget's four;
// a task that ends frees its CPUs for the next. Pre-fix every task was pinned
// to the whole budget's set, so no task was held to its own grant.
func TestFallbackPinsEachTaskToItsGrant(t *testing.T) {
	rec := withFakeAffinity(t, []int{0, 1, 2, 3, 4, 5, 6, 7}, nil)
	l := &LinuxLimiter{logger: testLimiter().logger, useCgroups: false}

	start := func(pid, cores int) (func(), []int) {
		t.Helper()
		cleanup, err := l.enforceFallback(pid, &TaskLimits{CPU: runtime.CPUGrant{Cores: cores, BudgetCores: 4}})
		if err != nil {
			t.Fatalf("enforceFallback: %v", err)
		}
		return cleanup, rec.cpus
	}
	endGrep, grep := start(100, 2)
	_, bb1 := start(101, 1)
	_, bb2 := start(102, 1)
	for _, c := range []struct {
		name string
		got  []int
		want []int
	}{{"GREP (2 cores)", grep, []int{0, 1}}, {"first Beyblade", bb1, []int{2}}, {"second Beyblade", bb2, []int{3}}} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s pinned to %v, want %v", c.name, c.got, c.want)
		}
	}

	endGrep()
	if _, next := start(103, 2); !reflect.DeepEqual(next, []int{0, 1}) {
		t.Errorf("after the GREP task ended the next 2-core task was pinned to %v, want the freed %v", next, []int{0, 1})
	}
}
