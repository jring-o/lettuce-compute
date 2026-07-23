package runtime

import (
	"os/exec"
	"strings"
	"testing"
)

// PB-27 regression coverage: the podman machine is a host-wide singleton shared
// with every other container on the box, so only a machine THIS process
// actually started may be stopped at shutdown. Setup() no-ops idempotently on
// an already-running machine — that must never be read as ownership.

const runningInspectJSON = `[{
	"Name": "default",
	"State": "running",
	"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
	"ConnectionInfo": {}
}]`

const stoppedInspectJSON = `[{
	"Name": "default",
	"State": "stopped",
	"Resources": {"CPUs": 4, "Memory": 8589934592, "DiskSize": 21474836480},
	"ConnectionInfo": {}
}]`

// TestSetup_AlreadyRunning_NotOwnedByThisProcess: setup that no-ops on a
// machine somebody else is running must NOT mark the machine as started by
// this process — pre-fix, the daemon treated "setup succeeded" as ownership
// and its shutdown hook tore the machine (and every co-tenant container) down.
func TestSetup_AlreadyRunning_NotOwnedByThisProcess(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(runningInspectJSON), nil
		}
		return nil, exec.ErrNotFound
	})

	if err := m.Setup(4, 8192, 20); err != nil {
		t.Fatalf("Setup on an already-running machine should succeed: %v", err)
	}
	if m.StartedByThisProcess() {
		t.Error("Setup no-op on an already-running machine must NOT claim ownership (PB-27)")
	}
}

// TestSetup_StoppedMachine_OwnedUntilStopped: a machine this process actually
// starts IS owned — and a successful Stop hands the ownership back.
func TestSetup_StoppedMachine_OwnedUntilStopped(t *testing.T) {
	if !needsMachine() {
		t.Skip("test only runs on Windows/macOS")
	}

	m := newTestManager(t)
	var cmds []string
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		cmds = append(cmds, strings.Join(args, " "))
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(stoppedInspectJSON), nil
		}
		if len(args) >= 2 && args[0] == "machine" && (args[1] == "start" || args[1] == "stop") {
			return []byte("OK\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	if err := m.Setup(4, 8192, 20); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	started := false
	for _, c := range cmds {
		if strings.Contains(c, "machine start") {
			started = true
		}
	}
	if !started {
		t.Fatal("test premise broken: Setup on a stopped machine should run machine start")
	}
	if !m.StartedByThisProcess() {
		t.Error("a machine this process started must be marked owned")
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.StartedByThisProcess() {
		t.Error("ownership must clear once this process stopped the machine")
	}
}
