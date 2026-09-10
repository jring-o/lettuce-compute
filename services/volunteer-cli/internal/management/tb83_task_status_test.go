package management

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// TB-83: a task frozen while the daemon yields to other programs is
// "suspended_busy" with a reason — not "suspended (reason not reported)".
func TestTB83_ComputeTaskStatus_SuspendedBusy(t *testing.T) {
	task := daemon.CurrentTask{Suspended: true}
	status, reason := computeTaskStatus(task, "busy", true)
	if status != "suspended_busy" {
		t.Errorf("status = %q, want %q", status, "suspended_busy")
	}
	if reason == nil || *reason != "Other programs are using the CPU" {
		t.Errorf("reason = %v, want %q", reason, "Other programs are using the CPU")
	}
}
