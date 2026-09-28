//go:build darwin

package resource

import (
	"log/slog"
	"syscall"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TestEnforceRunsTheTaskInTheBackground: a task put under the limiter runs at
// nice 19 (which its children inherit) and in the background state. Before,
// it got nice 10 and kept the normal state.
func TestEnforceRunsTheTaskInTheBackground(t *testing.T) {
	l := NewDarwinLimiter(slog.Default())
	pid := startLimiterTestChild(t)

	cleanup, err := l.Enforce(pid, &TaskLimits{CPU: runtime.CPUGrant{Cores: 1, BudgetCores: 1}})
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	defer cleanup()

	nice, err := syscall.Getpriority(syscall.PRIO_PROCESS, pid)
	if err != nil {
		t.Fatalf("getpriority(PRIO_PROCESS): %v", err)
	}
	if nice != lowestNice {
		t.Errorf("task runs at nice %d, want %d", nice, lowestNice)
	}
	bg, err := syscall.Getpriority(prioDarwinProcess, pid)
	if err != nil {
		t.Fatalf("getpriority(PRIO_DARWIN_PROCESS): %v", err)
	}
	if bg == 0 {
		t.Error("task is not in the background state (getpriority(PRIO_DARWIN_PROCESS) = 0)")
	}
}
