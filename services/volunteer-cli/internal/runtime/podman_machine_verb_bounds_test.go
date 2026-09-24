package runtime

import (
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// The bound each Podman machine verb runs under, and what happens when a verb
// reaches it: asked to stop so podman can clear its "starting" flag, killed
// only after a grace, and — for a start — counted as a start when the machine
// came up regardless. Kept apart from podman_machine_verbs_test.go because
// these tests shorten the bounds, which a tree without them does not have.

// shortenMachineStart sets the start bound and the grace for one test.
func shortenMachineStart(t *testing.T, timeout, grace time.Duration) {
	t.Helper()
	origTimeout, origGrace := machineStartTimeout, machineVerbGrace
	machineStartTimeout, machineVerbGrace = timeout, grace
	t.Cleanup(func() { machineStartTimeout, machineVerbGrace = origTimeout, origGrace })
}

// TestMachineVerbTimeout_EachLifecycleVerbHasItsOwnBound: start, stop and
// init get bounds measured in minutes (init longest: it downloads the
// machine's image first); every other command keeps the 30-second default.
func TestMachineVerbTimeout_EachLifecycleVerbHasItsOwnBound(t *testing.T) {
	for _, tc := range []struct {
		verb string
		min  time.Duration
	}{
		{"start", 3 * time.Minute},
		{"stop", 3 * time.Minute},
		{"init", 15 * time.Minute},
	} {
		got, ok := machineVerbTimeout([]string{"machine", tc.verb, "--now"})
		if !ok || got < tc.min {
			t.Errorf("podman machine %s: bound %s (own bound: %v), want at least %s", tc.verb, got, ok, tc.min)
		}
	}
	for _, args := range [][]string{
		{"machine", "inspect"},
		{"machine", "list"},
		{"info", "--format", "{{.Host.RemoteSocket.Exists}}"},
		{"--version"},
		{"machine"},
	} {
		if got, ok := machineVerbTimeout(args); ok {
			t.Errorf("%v was given the machine-verb bound %s; it keeps the default", args, got)
		}
	}
}

// TestMachineStart_TimedOutStartIsInterruptedAndCountsWhenTheMachineCameUp:
// a start still running at its bound whose machine is up is a start, and
// this process — which cut it off part-way — owns the machine. Where the
// platform can interrupt a process (not Windows), podman is interrupted
// rather than killed, and its own clean-up clears the "starting" flag.
func TestMachineStart_TimedOutStartIsInterruptedAndCountsWhenTheMachineCameUp(t *testing.T) {
	m, dir := fakePodmanManager(t, 200*time.Millisecond, time.Minute)
	shortenMachineStart(t, 3*time.Second, 10*time.Second)

	began := time.Now()
	err := m.Start()
	elapsed := time.Since(began)
	if err != nil {
		t.Fatalf("Start = %v, want success: the machine came up before the bound", err)
	}
	if elapsed > 8*time.Second {
		t.Errorf("Start returned after %s: the interrupted start should end within moments of the 3 s bound, not at the grace", elapsed)
	}
	if !m.StartedByThisProcess() {
		t.Error("a start this process cut off part-way, whose machine is up, must make it the owner")
	}
	if info := m.Status(); info.Status != MachineRunning || info.Error != "" {
		t.Errorf("Status = %s / %q, want running with no error", info.Status, info.Error)
	}
	if goruntime.GOOS == "windows" {
		return // no interrupt for a console-less process: it is killed
	}
	if !fakePodmanFile(dir, "interrupted") {
		t.Error("podman was not interrupted at the bound (killed instead)")
	}
	if fakePodmanFile(dir, "starting") {
		t.Error("podman's starting flag is still set after the interrupted start")
	}
}

// TestMachineStart_TimedOutStartOfAMachineThatNeverCameUpFails: a start that
// reaches its bound with the machine still down is a failure, says so, and
// claims nothing.
func TestMachineStart_TimedOutStartOfAMachineThatNeverCameUpFails(t *testing.T) {
	m, _ := fakePodmanManager(t, -1, time.Minute)
	shortenMachineStart(t, time.Second, 10*time.Second)

	err := m.Start()
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("Start = %v, want a failure naming the 1 s bound", err)
	}
	if m.StartedByThisProcess() {
		t.Error("a failed start must not claim the machine")
	}
	if info := m.Status(); info.Status != MachineStopped || !strings.Contains(info.Error, "timed out") {
		t.Errorf("Status = %s / %q, want stopped with the timed-out start", info.Status, info.Error)
	}
}
