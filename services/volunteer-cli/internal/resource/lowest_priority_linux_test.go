//go:build linux

package resource

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Every task runs at the lowest priority: nice 19 on each of its threads on
// both limiter paths, and cpu.weight 1 on its cgroup where it has one. Neither
// changes what the task may use; they decide who yields when the machine's
// CPUs are contended.

// threadNice reads the nice value of every thread of pid from /proc, keyed by
// thread id — the kernel's own record, field 19 of each thread's stat line.
func threadNice(t *testing.T, pid int) map[int]int {
	t.Helper()
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatalf("list threads of %d: %v", pid, err)
	}
	out := make(map[int]int)
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/stat", pid, tid))
		if err != nil {
			continue // the thread exited
		}
		// The command name may hold spaces; fields resume after its ')'.
		s := string(b)
		fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(fields) < 17 {
			t.Fatalf("short stat line for thread %d: %q", tid, s)
		}
		nice, err := strconv.Atoi(fields[16]) // field 19 of the full line
		if err != nil {
			t.Fatalf("parse nice of thread %d from %q: %v", tid, s, err)
		}
		out[tid] = nice
	}
	return out
}

// TestEnforceRunsEveryThreadAtNice19: on the fallback path (the one most
// volunteers get) every thread of a running task ends at nice 19 — including
// threads it started before Enforce ran, which a nice value set on the process
// id alone would miss (a nice value belongs to one thread on Linux). Before,
// nothing changed the task's priority on Linux.
func TestEnforceRunsEveryThreadAtNice19(t *testing.T) {
	l := &LinuxLimiter{logger: slog.Default(), useCgroups: false}
	pid := startLimiterTestChild(t)

	// The child is a Go program: wait until its runtime has started threads
	// of its own, so the test covers threads that exist before Enforce.
	deadline := time.Now().Add(3 * time.Second)
	for len(threadNice(t, pid)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the test child never started a second thread; the test would prove nothing")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cleanup, err := l.Enforce(pid, &TaskLimits{CPU: runtime.CPUGrant{Cores: 1, BudgetCores: 1}})
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	defer cleanup()

	nice := threadNice(t, pid)
	if len(nice) == 0 {
		t.Fatal("no threads read; the test proves nothing")
	}
	for tid, n := range nice {
		if n != lowestNice {
			t.Errorf("thread %d of task %d runs at nice %d, want %d", tid, pid, n, lowestNice)
		}
	}
	t.Logf("%d threads checked", len(nice))
}

// TestCgroupCPUWeightIsTheLowest: the task's cgroup gets cpu.weight 1. Written
// to a scratch directory standing in for the cgroup scope, since delegation is
// not available on CI.
func TestCgroupCPUWeightIsTheLowest(t *testing.T) {
	dir := t.TempDir()
	if err := writeCPUWeight(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "cpu.weight"))
	if err != nil {
		t.Fatalf("read cpu.weight: %v", err)
	}
	if got := string(b); got != "1" {
		t.Errorf("cpu.weight = %q, want \"1\" (the default is 100)", got)
	}
}
