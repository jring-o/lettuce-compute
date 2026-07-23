package cli

import (
	"bytes"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	rt "github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// PB-27 regression coverage for the shutdown decision itself: the daemon's
// shutdown hook (stopMachineIfDaemonStarted) may issue `podman machine stop`
// ONLY when this process started the machine. Pre-fix the hook fired whenever
// machine SETUP had succeeded — which is also true when Setup() no-ops on a
// machine somebody else was already running — so a graceful volunteer stop
// tore down the host's shared machine and every co-tenant container in it
// (observed live twice during the Batch-A closeout: it killed the closeout
// head's Postgres).

// withRealisticMachineExecutor swaps the cli TestMain's blocked executor for a
// scripted podman for one test.
func withRealisticMachineExecutor(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	old := rt.CommandExecutor
	rt.CommandExecutor = fn
	t.Cleanup(func() { rt.CommandExecutor = old })
}

func TestStopMachine_LeavesForeignMachineRunning(t *testing.T) {
	if !rt.NeedsMachineForTest() {
		t.Skip("test only runs on Windows/macOS")
	}

	stopIssued := false
	withRealisticMachineExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{"Name":"default","State":"running","Resources":{"CPUs":4,"Memory":8589934592,"DiskSize":21474836480},"ConnectionInfo":{}}]`), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "stop" {
			stopIssued = true
			return []byte("OK\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	mm := rt.NewPodmanMachineManager("/usr/bin/podman", logger)

	// The daemon's startup flow: Setup no-ops on the already-running machine.
	if err := mm.Setup(4, 8192, 20); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// The daemon's shutdown hook.
	stopMachineIfDaemonStarted(mm, logger)

	if stopIssued {
		t.Error("shutdown stopped a podman machine this daemon did not start (PB-27)")
	}
	if !strings.Contains(logBuf.String(), "leaving podman machine running") {
		t.Errorf("shutdown should say it is leaving the foreign machine running; log:\n%s", logBuf.String())
	}
}

func TestStopMachine_StopsMachineThisDaemonStarted(t *testing.T) {
	if !rt.NeedsMachineForTest() {
		t.Skip("test only runs on Windows/macOS")
	}

	stopIssued := false
	withRealisticMachineExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "machine" && args[1] == "inspect" {
			return []byte(`[{"Name":"default","State":"stopped","Resources":{"CPUs":4,"Memory":8589934592,"DiskSize":21474836480},"ConnectionInfo":{}}]`), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "start" {
			return []byte("OK\n"), nil
		}
		if len(args) >= 2 && args[0] == "machine" && args[1] == "stop" {
			stopIssued = true
			return []byte("OK\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	mm := rt.NewPodmanMachineManager("/usr/bin/podman", logger)

	// The daemon's startup flow: the machine is stopped, so Setup starts it.
	if err := mm.Setup(4, 8192, 20); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	stopMachineIfDaemonStarted(mm, logger)

	if !stopIssued {
		t.Error("shutdown must stop the machine this daemon itself started")
	}
}
