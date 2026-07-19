//go:build windows

package daemon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// PB-12 regression coverage: `lettuce-volunteer stop` on Windows must deliver a
// stop the daemon can handle gracefully ("finish current work unit, then stop"),
// not an instant TerminateProcess that also kills the compute child. The test
// re-executes this test binary as a daemon-shaped child that wires
// ListenForStopRequests into a cancel path the way runStart does, then invokes
// RequestGracefulStop — the primitive the stop command uses — and asserts the
// child observed the request and exited cleanly (marker file written, exit code
// 0). Under the pre-fix hard-kill semantics the child dies instantly: no marker,
// non-zero exit.

const (
	stopHelperEnv       = "LETTUCE_TEST_STOP_HELPER"
	stopHelperMarkerEnv = "LETTUCE_TEST_STOP_MARKER"
)

// TestGracefulStopHelperProcess is the child half of
// TestRequestGracefulStop_DaemonHandlesStopGracefully; it skips unless
// re-executed with the helper environment set.
func TestGracefulStopHelperProcess(t *testing.T) {
	if os.Getenv(stopHelperEnv) != "1" {
		t.Skip("helper process for TestRequestGracefulStop_DaemonHandlesStopGracefully")
	}
	marker := os.Getenv(stopHelperMarkerEnv)
	if marker == "" {
		t.Fatal("helper: marker path not set")
	}

	stopCh, err := ListenForStopRequests()
	if err != nil {
		t.Fatalf("helper: ListenForStopRequests: %v", err)
	}

	// Signal readiness AFTER the listener exists, so the parent cannot race it.
	fmt.Println("HELPER_READY")

	select {
	case <-stopCh:
		// The graceful path: the daemon gets to run its shutdown work. Writing
		// the marker stands in for "finish current work unit".
		if err := os.WriteFile(marker, []byte("graceful"), 0o644); err != nil {
			t.Fatalf("helper: writing marker: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper: no stop request within 30s")
	}
}

func TestRequestGracefulStop_DaemonHandlesStopGracefully(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "graceful-marker")

	cmd := exec.Command(os.Args[0], "-test.run", "^TestGracefulStopHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		stopHelperEnv+"=1",
		stopHelperMarkerEnv+"="+marker,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	defer cmd.Process.Kill() // last-resort cleanup; a passing test has already Waited

	// Wait for the helper to have its stop listener installed.
	ready := make(chan error, 1)
	scanner := bufio.NewScanner(stdout)
	go func() {
		for scanner.Scan() {
			if scanner.Text() == "HELPER_READY" {
				ready <- nil
				// Keep draining so the helper never blocks on a full pipe.
				io.Copy(io.Discard, stdout)
				return
			}
		}
		ready <- fmt.Errorf("helper exited without HELPER_READY (scan err: %v)", scanner.Err())
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("helper not ready: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("helper not ready within 60s")
	}

	// The stop command's primitive: must reach the helper's graceful path.
	if err := RequestGracefulStop(cmd.Process.Pid); err != nil {
		t.Fatalf("RequestGracefulStop: %v", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("helper did not exit cleanly after the stop request (hard kill?): %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper still running 30s after the stop request")
	}

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("stop was not graceful: the helper never ran its shutdown path (marker missing: %v)", err)
	}
	if string(data) != "graceful" {
		t.Fatalf("unexpected marker content %q", data)
	}
}
