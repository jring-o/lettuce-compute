package resource

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// startLimiterTestChild spawns a short-lived child process for the Enforce tests
// to target, and returns its PID.
//
// Enforcing limits against the test process ITSELF is unsafe and was the real
// (mis-diagnosed) cause of the flaky #36 CI OOM. On Linux the fallback path sets
// RLIMIT_AS — a virtual-address-space cap — via prlimit64. Lowering your own
// limit needs no privilege, so `Enforce(os.Getpid(), {MaxMemoryMB: 256})`
// actually SUCCEEDS in capping the test binary at 256 MB, and enforceFallback's
// cleanup is a no-op that never restores it. The Go runtime then can't mmap past
// that cap and the package dies with "fatal error: runtime: cannot allocate
// memory" — nondeterministically, depending on the process's virtual footprint
// at that moment (hence the flakiness; it crashed even under `go test -p 1`,
// with no parallel package binaries to blame).
//
// Enforce is only ever meant to constrain a CHILD compute process (see
// daemon.SetProcessNotifier), so the tests do the same. The child is the test
// binary re-exec'd to run TestLimiterHelperProcess, which just blocks; it is
// killed on cleanup. If the limiter caps the child's address space and the child
// crashes, that's fine — its stdio is discarded and we never inspect its exit.
func startLimiterTestChild(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestLimiterHelperProcess")
	cmd.Env = append(os.Environ(), "GO_LIMITER_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start limiter helper child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

// TestLimiterHelperProcess is not a real test: it is the body of the disposable
// child spawned by startLimiterTestChild. It blocks so the parent can apply (and
// tear down) limits against a live PID, then exits. Under a normal `go test` run
// GO_LIMITER_HELPER is unset, so it returns immediately and is a harmless no-op.
func TestLimiterHelperProcess(t *testing.T) {
	if os.Getenv("GO_LIMITER_HELPER") != "1" {
		return
	}
	time.Sleep(5 * time.Second)
}

func TestCheckDiskSpace_SufficientSpace(t *testing.T) {
	l := NewLimiter(slog.Default())
	// Check the temp dir — should have enough space for 1 MB.
	dir := t.TempDir()
	if err := l.CheckDiskSpace(dir, 1); err != nil {
		t.Errorf("CheckDiskSpace should succeed for 1 MB: %v", err)
	}
}

func TestCheckDiskSpace_ImpossibleRequirement(t *testing.T) {
	l := NewLimiter(slog.Default())
	dir := t.TempDir()
	// 1 PB — should fail on any real filesystem.
	err := l.CheckDiskSpace(dir, 1024*1024*1024)
	if err == nil {
		t.Error("CheckDiskSpace should fail for impossibly large requirement")
	}
	if !strings.Contains(err.Error(), "insufficient disk space") {
		t.Errorf("error should mention insufficient disk space, got: %v", err)
	}
}

func TestCheckDiskSpace_ZeroRequirement(t *testing.T) {
	l := NewLimiter(slog.Default())
	dir := t.TempDir()
	if err := l.CheckDiskSpace(dir, 0); err != nil {
		t.Errorf("CheckDiskSpace should succeed for 0 MB requirement: %v", err)
	}
}

func TestEnforce_ReturnsCleanup(t *testing.T) {
	l := NewLimiter(slog.Default())
	limits := &config.ResourceLimits{
		MaxCPUCores: 1,
		MaxMemoryMB: 256,
	}
	// Enforce against a disposable child, never the test process itself (see
	// startLimiterTestChild). It should return a non-nil cleanup without error
	// on every platform (Linux: prlimit/affinity, Windows: Job Object, macOS:
	// setpriority).
	cleanup, err := l.Enforce(startLimiterTestChild(t), limits)
	if err != nil {
		t.Skipf("Enforce returned error (may be expected on this platform): %v", err)
	}
	if cleanup == nil {
		t.Error("Enforce should return a non-nil cleanup function")
	} else {
		cleanup()
	}
}

func TestEnforce_ChildProcess(t *testing.T) {
	l := NewLimiter(slog.Default())
	limits := &config.ResourceLimits{
		MaxCPUCores: 1,
		MaxMemoryMB: 256,
	}
	// A live, non-self PID exercises the real enforce path (prlimit64 +
	// sched_setaffinity on Linux) without capping the test binary's own
	// address space — capping our own RLIMIT_AS is the #36 footgun.
	cleanup, err := l.Enforce(startLimiterTestChild(t), limits)
	if err != nil {
		t.Skipf("Enforce on child PID returned error (may be expected): %v", err)
	}
	if cleanup == nil {
		t.Error("Enforce should return a non-nil cleanup function")
	} else {
		cleanup()
	}
}

func TestEnforce_ZeroLimits(t *testing.T) {
	l := NewLimiter(slog.Default())
	limits := &config.ResourceLimits{
		MaxCPUCores: 0,
		MaxMemoryMB: 0,
	}
	pid := os.Getpid()
	cleanup, err := l.Enforce(pid, limits)
	if err != nil {
		t.Skipf("Enforce with zero limits returned error: %v", err)
	}
	if cleanup == nil {
		t.Error("Enforce should return a non-nil cleanup function even with zero limits")
	} else {
		cleanup()
	}
}

func TestApply_NoError(t *testing.T) {
	l := NewLimiter(slog.Default())
	limits := &config.ResourceLimits{
		MaxCPUCores: 2,
		MaxMemoryMB: 512,
	}
	cmd := exec.Command("echo", "test")
	if err := l.Apply(cmd, limits); err != nil {
		t.Errorf("Apply should not error: %v", err)
	}
}

func TestNewLimiter_NotNil(t *testing.T) {
	l := NewLimiter(slog.Default())
	if l == nil {
		t.Error("NewLimiter should not return nil")
	}
}
