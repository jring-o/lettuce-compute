//go:build windows

package resource

import (
	"log/slog"
	"testing"
	"unsafe"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

var (
	procGetPriorityClass          = kernel32W.NewProc("GetPriorityClass")
	procQueryInformationJobObject = kernel32W.NewProc("QueryInformationJobObject")
)

const processQueryLimitedInformation = 0x1000

// processPriorityClass reads a live process's priority class as Windows holds it.
func processPriorityClass(t *testing.T, pid int) uint32 {
	t.Helper()
	h, _, err := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		t.Fatalf("OpenProcess(%d): %v", pid, err)
	}
	defer procCloseHandle.Call(h)
	class, _, err := procGetPriorityClass.Call(h)
	if class == 0 {
		t.Fatalf("GetPriorityClass(%d): %v", pid, err)
	}
	return uint32(class)
}

// jobLimits reads back the extended limits of the Job Object the limiter holds
// for pid.
func jobLimits(t *testing.T, l *WindowsLimiter, pid int) jobobjectExtendedLimitInfo {
	t.Helper()
	l.mu.Lock()
	job, ok := l.jobs[pid]
	l.mu.Unlock()
	if !ok {
		t.Fatalf("the limiter holds no job for pid %d", pid)
	}
	var info jobobjectExtendedLimitInfo
	ret, _, err := procQueryInformationJobObject.Call(job, uintptr(infoClassExtendedLimit),
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
	if ret == 0 {
		t.Fatalf("QueryInformationJobObject: %v", err)
	}
	return info
}

// TestEnforceRunsTheTaskAtIdlePriority: a task put under the limiter runs at
// IDLE_PRIORITY_CLASS, with and without a memory limit, and the memory limit
// still holds beside the priority (the two share one set of job limit flags,
// which each call replaces as a whole). Before, the task kept the normal
// priority class it was started with.
func TestEnforceRunsTheTaskAtIdlePriority(t *testing.T) {
	for _, tc := range []struct {
		name  string
		memMB int
	}{
		{"with a memory limit", 256},
		{"with no memory limit", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewWindowsLimiter(slog.Default())
			pid := startLimiterTestChild(t)
			if got := processPriorityClass(t, pid); got == idlePriorityClass {
				t.Fatalf("the test child already runs at idle priority (0x%x); the test would prove nothing", got)
			}

			cleanup, err := l.Enforce(pid, &TaskLimits{MaxMemoryMB: tc.memMB, CPU: runtime.CPUGrant{Cores: 1, BudgetCores: 2}})
			if err != nil {
				t.Fatalf("Enforce: %v", err)
			}
			defer cleanup()

			if got := processPriorityClass(t, pid); got != idlePriorityClass {
				t.Errorf("priority class after Enforce = 0x%x, want IDLE_PRIORITY_CLASS (0x%x)", got, idlePriorityClass)
			}
			info := jobLimits(t, l, pid)
			if info.BasicLimitInformation.LimitFlags&jobObjectLimitPriorityClass == 0 || info.BasicLimitInformation.PriorityClass != idlePriorityClass {
				t.Errorf("job limit flags 0x%x, priority class 0x%x: want JOB_OBJECT_LIMIT_PRIORITY_CLASS at IDLE_PRIORITY_CLASS",
					info.BasicLimitInformation.LimitFlags, info.BasicLimitInformation.PriorityClass)
			}
			hasMem := info.BasicLimitInformation.LimitFlags&jobObjectLimitProcessMemory != 0
			if tc.memMB > 0 && (!hasMem || info.ProcessMemoryLimit != uintptr(tc.memMB)*1024*1024) {
				t.Errorf("memory limit lost beside the priority: flags 0x%x, limit %d bytes, want %d MiB",
					info.BasicLimitInformation.LimitFlags, info.ProcessMemoryLimit, tc.memMB)
			}
			if tc.memMB == 0 && hasMem {
				t.Errorf("a task declaring no memory limit got one (%d bytes)", info.ProcessMemoryLimit)
			}
		})
	}
}
