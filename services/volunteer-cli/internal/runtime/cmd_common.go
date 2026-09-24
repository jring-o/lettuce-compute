package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// SkipHardwareDetectionEnv, when truthy ("1"/"true"/"yes"/"on"), bypasses
// every platform detection call (CPU model, memory, disk, GPUs, thermal).
// Tests set this in TestMain so `go test ./...` cannot trigger DiskPart UAC
// prompts or vendor-CLI hangs on Windows. The client package's
// DetectHardware and this package's DetectGPUs both honor it.
const SkipHardwareDetectionEnv = "LETTUCE_SKIP_HARDWARE_DETECTION"

// SkipHardwareDetection reports whether the env var is set to a truthy value.
func SkipHardwareDetection() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(SkipHardwareDetectionEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// DefaultCommandTimeout is the maximum time any single external command can run
// when invoked via CommandExecutor — except the Podman machine lifecycle
// verbs, which have their own bounds (machineVerbTimeout).
const DefaultCommandTimeout = 30 * time.Second

// Bounds for the Podman machine lifecycle verbs. `podman machine start` and
// `stop` take 30-120 s on an ordinary Intel Mac, and `init` downloads the
// machine's disk image before it creates anything. Under DefaultCommandTimeout
// a slow start was killed part-way: the machine came up anyway, but the start
// was recorded as failed, and the kill skipped podman's own clean-up, leaving
// its "starting" flag set (`podman machine list` then reads "Currently
// starting" until a later start completes). Each verb's bound is generous
// enough that reaching it means the verb is hung. Variables so tests can
// shorten them.
var (
	machineStartTimeout = 5 * time.Minute
	machineStopTimeout  = 5 * time.Minute
	machineInitTimeout  = 30 * time.Minute
	// machineVerbGrace is how long a verb that reached its bound is given to
	// exit after being asked to (interruptCommand) before it is killed.
	machineVerbGrace = 30 * time.Second
)

// errMachineVerbTimedOut marks a machine verb this process stopped at its
// bound, as opposed to one that podman ended with an error of its own.
var errMachineVerbTimedOut = errors.New("timed out")

// machineVerbTimeout returns the bound for a `podman machine start`, `stop`
// or `init`, and false for every other command.
func machineVerbTimeout(args []string) (time.Duration, bool) {
	if len(args) < 2 || args[0] != "machine" {
		return 0, false
	}
	switch args[1] {
	case "start":
		return machineStartTimeout, true
	case "stop":
		return machineStopTimeout, true
	case "init":
		return machineInitTimeout, true
	}
	return 0, false
}

// defaultCommandExecutor is CommandExecutor's implementation. The machine
// verbs are recognised here rather than given a seam of their own because
// CommandExecutor is the seam every package's tests replace — and block — so
// no test can reach a real `podman machine start` through a second one.
func defaultCommandExecutor(name string, args ...string) ([]byte, error) {
	if timeout, ok := machineVerbTimeout(args); ok {
		return runMachineVerb(timeout, name, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultCommandTimeout)
	defer cancel()
	return defaultCommandExecutorCtx(ctx, name, args...)
}

func defaultCommandExecutorCtx(ctx context.Context, name string, args ...string) ([]byte, error) {
	return newCommand(ctx, name, args...).Output()
}

// runMachineVerb runs a machine verb under its own bound. At the bound the
// verb is asked to stop and given machineVerbGrace to do so, so podman can
// reset its "starting" flag; only then is it killed. An error from a verb
// stopped at its bound wraps errMachineVerbTimedOut.
func runMachineVerb(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := newCommand(ctx, name, args...)
	cmd.Cancel = func() error { return interruptCommand(cmd.Process) }
	cmd.WaitDelay = machineVerbGrace
	out, err := cmd.Output()
	if err != nil && ctx.Err() != nil {
		return out, fmt.Errorf("%w after %s (%v)", errMachineVerbTimedOut, timeout, err)
	}
	return out, err
}

// DetectionCommandTimeout is the per-command upper bound used by hardware/GPU
// detection paths. Detection CLIs (nvidia-smi, rocm-smi, amd-smi, wmic,
// system_profiler, sysctl, ...) should respond in well under a second on a
// healthy host. Anything beyond a few seconds is almost certainly a hung tool
// (e.g. amd-smi probing a missing driver), and we'd rather degrade to "no GPU"
// than block volunteer registration.
const DetectionCommandTimeout = 5 * time.Second

// DetectHardwareTimeout caps the total wall time spent in DetectHardware,
// regardless of how many sub-detections are in flight. Sub-detections run in
// parallel and each has its own per-command timeout, but this is the hard
// ceiling so a pathological host can never block Register past it.
const DetectHardwareTimeout = 10 * time.Second
