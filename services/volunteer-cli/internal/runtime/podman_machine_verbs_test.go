package runtime

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// Podman machine verbs against a machine that does not behave like the
// command says: a start that takes longer than the 30 seconds every other
// external command is allowed, a start or stop podman reports as failed
// although the machine got where it was going, and a machine that is still
// booting. Written against the manager's public surface, so each test here
// fails on a tree without the fix for the reason given in its comment.

// verbPodman is a mocked podman whose start and stop change the machine's
// state independently of what the command answers, as a real one can.
type verbPodman struct {
	mu    sync.Mutex
	state string // running | stopped | starting
	// startTo / stopTo: the state after `machine start` / `machine stop`,
	// whatever the command reports; "" leaves it unchanged.
	startTo, stopTo   string
	startErr, stopErr error
}

func (p *verbPodman) set(state string) {
	p.mu.Lock()
	p.state = state
	p.mu.Unlock()
}

func (p *verbPodman) exec(name string, args ...string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case len(args) >= 2 && args[0] == "machine" && args[1] == "inspect":
		return []byte(fmt.Sprintf(`[{"Name":"podman-machine-default","State":%q,"Resources":{"CPUs":2,"DiskSize":100,"Memory":2048},"ConnectionInfo":{}}]`, p.state)), nil
	case len(args) >= 2 && args[0] == "machine" && args[1] == "start":
		if p.startTo != "" {
			p.state = p.startTo
		}
		if p.startErr != nil {
			return []byte("Starting machine \"podman-machine-default\"\n"), p.startErr
		}
		return []byte("Machine started\n"), nil
	case len(args) >= 2 && args[0] == "machine" && args[1] == "stop":
		if p.stopTo != "" {
			p.state = p.stopTo
		}
		if p.stopErr != nil {
			return []byte("Stopping machine\n"), p.stopErr
		}
		return []byte("Machine stopped\n"), nil
	}
	return nil, exec.ErrNotFound
}

func verbManager(t *testing.T, p *verbPodman) *PodmanMachineManager {
	t.Helper()
	t.Cleanup(SetNeedsMachineForTest(true))
	withMockExecutor(t, p.exec)
	return newTestManager(t)
}

// TestMachineStart_SlowStartRunsToCompletion: `podman machine start` takes
// 30-120 s on an ordinary Intel Mac. Run under the 30-second bound every
// external command shares, such a start was killed part-way: the machine came
// up anyway, but the start was recorded as failed ("signal: killed", shown in
// red on the runtime card beside a running engine), this process never became
// the machine's owner, and the kill skipped podman's own clean-up, leaving its
// "starting" flag set so `podman machine list` read "Currently starting". A
// start that takes 31 s must run to completion. A real process (the fake
// podman), because the bound is the executor's.
func TestMachineStart_SlowStartRunsToCompletion(t *testing.T) {
	m, dir := fakePodmanManager(t, time.Second, 31*time.Second)

	began := time.Now()
	err := m.Start()
	elapsed := time.Since(began).Round(time.Second)
	if err != nil {
		t.Fatalf("Start of a machine that takes 31 s to start failed after %s: %v", elapsed, err)
	}
	if !m.StartedByThisProcess() {
		t.Error("the start this process ran to completion must make it the machine's owner")
	}
	if info := m.Status(); info.Status != MachineRunning || info.Error != "" {
		t.Errorf("Status after the start = %s / %q, want running with no error", info.Status, info.Error)
	}
	if fakePodmanFile(dir, "starting") {
		t.Error("podman's starting flag is still set: the start was killed before podman could clear it")
	}
}

// TestMachineStart_FailureReportedAfterTheMachineCameUpIsAStart: podman can
// report a start as failed after the machine has in fact come up. The
// machine is running, which is what the start was for, so it is not a
// failure: no error for the runtime card to show in red beside a running
// engine. Ownership is not claimed from such a start — podman may have been
// refusing a machine somebody else already had running, and a machine left
// running at shutdown is the safe side of that doubt.
func TestMachineStart_FailureReportedAfterTheMachineCameUpIsAStart(t *testing.T) {
	p := &verbPodman{state: "stopped", startTo: "running", startErr: errors.New("exit status 125")}
	m := verbManager(t, p)

	if err := m.Start(); err != nil {
		t.Fatalf("Start = %v, want success: the machine is running", err)
	}
	if info := m.Status(); info.Status != MachineRunning || info.Error != "" {
		t.Errorf("Status after the start = %s / %q, want running with no error", info.Status, info.Error)
	}
	if m.StartedByThisProcess() {
		t.Error("a start podman itself reported as failed must not claim the machine")
	}
}

// TestMachineStop_FailureReportedAfterTheMachineWentDownIsAStop is the same
// for a stop: podman reported a failure, the machine is stopped. Reported as
// a failure, the app's stop hands the machine back to the re-detection loop
// ("the machine is still up"), which then starts again the machine the
// volunteer just stopped.
func TestMachineStop_FailureReportedAfterTheMachineWentDownIsAStop(t *testing.T) {
	p := &verbPodman{state: "stopped", startTo: "running", stopTo: "stopped", stopErr: errors.New("exit status 125")}
	m := verbManager(t, p)
	if err := m.Start(); err != nil {
		t.Fatalf("premise: Start = %v", err)
	}

	done := make(chan error, 1)
	if err := m.StopAsync(func(err error) { done <- err }); err != nil {
		t.Fatalf("StopAsync: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the stop reported %v, want success: the machine is stopped", err)
	}
	if info := waitStatus(t, m, MachineStopped); info.Error != "" {
		t.Errorf("Status.Error after the stop = %q, want none", info.Error)
	}
	if m.StartedByThisProcess() {
		t.Error("ownership must clear once the machine is down")
	}
}

// TestMachineStatus_OperationErrorClearsOnceTheMachineGetsThere: the error
// of a failed start used to be carried on every status read until a later
// start or stop through this manager succeeded, so a machine that came up
// another way (a start by hand, or a start that finished after Lettuce gave
// up on it) showed "start failed" in red beside a running engine, even after
// the app was reloaded. Once the machine is in the state the failed operation
// was for, the error is dropped; an error beside a machine still in the other
// state stands.
func TestMachineStatus_OperationErrorClearsOnceTheMachineGetsThere(t *testing.T) {
	p := &verbPodman{state: "stopped", startErr: errors.New("exit status 125"), stopErr: errors.New("exit status 125")}
	m := verbManager(t, p)

	if err := m.Start(); err == nil {
		t.Fatal("premise: the start must fail while the machine stays stopped")
	}
	if info := m.Status(); info.Status != MachineStopped || !strings.Contains(info.Error, "podman machine start failed") {
		t.Fatalf("premise: Status = %s / %q, want stopped with the start failure", info.Status, info.Error)
	}

	// Started by hand.
	p.set("running")
	m.InvalidateStatus()
	if info := m.Status(); info.Status != MachineRunning || info.Error != "" {
		t.Errorf("Status once the machine runs = %s / %q, want running with no error", info.Status, info.Error)
	}

	// A stop that fails while the machine keeps running: the error stands.
	if err := m.Stop(); err == nil {
		t.Fatal("premise: the stop must fail while the machine stays running")
	}
	m.InvalidateStatus()
	if info := m.Status(); info.Status != MachineRunning || !strings.Contains(info.Error, "podman machine stop failed") {
		t.Errorf("Status after the failed stop = %s / %q, want running with the stop failure", info.Status, info.Error)
	}

	// Stopped by hand: the stop's error is stale now.
	p.set("stopped")
	m.InvalidateStatus()
	if info := m.Status(); info.Status != MachineStopped || info.Error != "" {
		t.Errorf("Status once the machine is stopped = %s / %q, want stopped with no error", info.Status, info.Error)
	}
}

// TestMachineStatus_BootingMachineReadsStarting: `podman machine inspect`
// reports a machine that is still booting as "starting". Read as stopped, a
// booting machine could be taken for one the volunteer stopped, and the
// daemon would hold it rather than connect to it.
func TestMachineStatus_BootingMachineReadsStarting(t *testing.T) {
	p := &verbPodman{state: "starting"}
	m := verbManager(t, p)

	if info := m.Status(); info.Status != MachineStarting {
		t.Errorf("Status of a booting machine = %s, want starting", info.Status)
	}
}
