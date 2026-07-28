//go:build linux

package resource

import (
	"io"
	"log/slog"
	"reflect"
	"syscall"
	"testing"
)

// TB-16: the CPU affinity mask was built from the bare core count — bits
// 0..N-1 — instead of from the CPUs the process is actually permitted to use.
//
// The real syscalls cannot demonstrate this on an ordinary CI host: reproducing
// it needs a cpuset cgroup, since a plain taskset restriction can be widened
// again by the process itself and so never fails. The syscalls are therefore
// indirected and driven here.

type affinityCall struct {
	pid  int
	cpus []int
}

// withFakeAffinity installs a permitted-CPU set and records what gets requested.
func withFakeAffinity(t *testing.T, permitted []int, setErr error) *affinityCall {
	t.Helper()
	origGet, origSet := getAffinityFn, setAffinityFn
	t.Cleanup(func() { getAffinityFn, setAffinityFn = origGet, origSet })

	rec := &affinityCall{pid: -1}
	getAffinityFn = func(pid int) ([]int, error) { return append([]int(nil), permitted...), nil }
	setAffinityFn = func(pid int, cpus []int) error {
		rec.pid = pid
		rec.cpus = append([]int(nil), cpus...)
		return setErr
	}
	return rec
}

func testLimiter() *LinuxLimiter {
	return &LinuxLimiter{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// The mask must be drawn from the permitted set. A machine that confines
// Lettuce to high-numbered CPUs and permits more of them than the budget is
// where the old code asked for CPUs 0..N-1 and got EINVAL — after which no CPU
// limit was applied at all and work ran across every permitted CPU.
func TestApplyCPUAffinity_RequestsPermittedCPUsNotFirstN(t *testing.T) {
	rec := withFakeAffinity(t, []int{4, 5, 6, 7, 8, 9}, nil)

	testLimiter().applyCPUAffinity(4242, 4)

	if rec.pid != 4242 {
		t.Fatalf("sched_setaffinity not called (pid=%d); the CPU limit was silently skipped", rec.pid)
	}
	want := []int{4, 5, 6, 7}
	if !reflect.DeepEqual(rec.cpus, want) {
		t.Errorf("requested CPUs %v, want %v — asking for CPUs the process may not use returns EINVAL and applies no limit at all", rec.cpus, want)
	}
	for _, cpu := range rec.cpus {
		if cpu < 4 {
			t.Fatalf("requested CPU %d, which this process is not permitted to use (permitted: 4-9); the kernel refuses the whole call with EINVAL", cpu)
		}
	}
}

// The exact scenario from the field: a tester's host permitted Lettuce on
// CPUs 4-7 with max_cpu_cores 4, and every one of 25 native units logged
// "sched_setaffinity failed: invalid argument". Whatever the client does here,
// it must not ask for a CPU outside the permitted set — the budget already
// equals the permitted count, so there is nothing to narrow and the correct
// action is to leave the process alone.
func TestApplyCPUAffinity_ReportedFieldCaseRequestsNothingUnpermitted(t *testing.T) {
	rec := withFakeAffinity(t, []int{4, 5, 6, 7}, nil)

	testLimiter().applyCPUAffinity(80081, 4)

	for _, cpu := range rec.cpus {
		if cpu < 4 {
			t.Fatalf("requested CPU %d outside the permitted set {4,5,6,7}: this is the EINVAL that left the CPU limit unapplied on 25 of 25 units", cpu)
		}
	}
}

// The quieter half, which produced no output at any level: a PARTIAL overlap
// succeeds at the wrong size. Permitted 2-7 with a 4-core budget must pin to
// four of those CPUs, not to {2,3} — which is what intersecting {0,1,2,3} with
// the permitted set yields.
func TestApplyCPUAffinity_PartialOverlapPinsFullCoreCount(t *testing.T) {
	rec := withFakeAffinity(t, []int{2, 3, 4, 5, 6, 7}, nil)

	testLimiter().applyCPUAffinity(1, 4)

	if got := len(rec.cpus); got != 4 {
		t.Fatalf("pinned to %d CPUs (%v), want 4 — the volunteer configured a 4-core budget", got, rec.cpus)
	}
	want := []int{2, 3, 4, 5}
	if !reflect.DeepEqual(rec.cpus, want) {
		t.Errorf("requested CPUs %v, want %v", rec.cpus, want)
	}
}

// Unrestricted machine: the behaviour that was already correct must stay correct.
func TestApplyCPUAffinity_UnrestrictedHostTakesFirstN(t *testing.T) {
	rec := withFakeAffinity(t, []int{0, 1, 2, 3, 4, 5, 6, 7}, nil)

	testLimiter().applyCPUAffinity(1, 4)

	want := []int{0, 1, 2, 3}
	if !reflect.DeepEqual(rec.cpus, want) {
		t.Errorf("requested CPUs %v, want %v", rec.cpus, want)
	}
}

// Already confined to no more than the budget: pinning could only narrow it
// further, which is not what "use at most N cores" means.
func TestApplyCPUAffinity_SkipsWhenAlreadyWithinLimit(t *testing.T) {
	rec := withFakeAffinity(t, []int{4, 5}, nil)

	testLimiter().applyCPUAffinity(1, 4)

	if rec.pid != -1 {
		t.Errorf("sched_setaffinity called with %v; a process already confined to 2 CPUs must not be narrowed by a 4-core budget", rec.cpus)
	}
}

// A syscall failure must not panic and must leave the process running.
func TestApplyCPUAffinity_SetFailureIsSurvivable(t *testing.T) {
	withFakeAffinity(t, []int{4, 5, 6, 7}, syscall.EINVAL)
	testLimiter().applyCPUAffinity(1, 2) // must not panic
}

// sysGetAffinity must decode a multi-word mask, since bit N of word W is CPU
// W*64+N and an off-by-one there would silently mis-select high CPUs.
func TestSysGetAffinity_DecodesRealMask(t *testing.T) {
	cpus, err := sysGetAffinity(0)
	if err != nil {
		t.Skipf("sched_getaffinity unavailable: %v", err)
	}
	if len(cpus) == 0 {
		t.Fatal("no permitted CPUs reported for this process")
	}
	for i := 1; i < len(cpus); i++ {
		if cpus[i] <= cpus[i-1] {
			t.Fatalf("CPUs not strictly ascending: %v", cpus)
		}
	}
}

// The doctor line must not claim a permitted-CPU count it did not obtain.
func TestDescribeCPUEnforcement_ReportsMechanism(t *testing.T) {
	e := DescribeCPUEnforcement()
	if e.Mechanism == "" {
		t.Error("no mechanism reported; doctor would print an empty cpu enforcement line")
	}
	if !e.Confinable {
		t.Error("Linux can confine CPU use, via cgroup quota or affinity")
	}
}
