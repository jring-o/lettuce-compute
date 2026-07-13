package safego

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestSafeGo_PanicIsRecoveredAndWorkerRestarts proves the wrapper's contract
// in-process: a job that panics on its first two runs is restarted and keeps
// running, the panic is logged with the job name and a stack, and the
// goroutine stops without restart once the job returns normally.
func TestSafeGo_PanicIsRecoveredAndWorkerRestarts(t *testing.T) {
	restore := compressRestartSchedule(t)
	defer restore()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	var runs atomic.Int32
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Go(ctx, logger, "test-crasher", func(ctx context.Context) {
		n := runs.Add(1)
		if n <= 2 {
			panic(fmt.Sprintf("tick bomb %d", n))
		}
		close(done)
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("worker was not restarted past its panics; runs=%d", runs.Load())
	}

	if got := runs.Load(); got != 3 {
		t.Fatalf("expected exactly 3 runs (2 panics + 1 clean), got %d", got)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "test-crasher") {
		t.Error("panic log must carry the job name")
	}
	if !strings.Contains(logs, "tick bomb 1") || !strings.Contains(logs, "tick bomb 2") {
		t.Error("each panic value must be logged")
	}
	if !strings.Contains(logs, "safego_test.go") {
		t.Error("panic log must carry a stack trace")
	}
}

// TestSafeGo_CtxCancelStopsRestarts proves a cancelled ctx ends the restart
// loop even for a job that panics every run.
func TestSafeGo_CtxCancelStopsRestarts(t *testing.T) {
	restore := compressRestartSchedule(t)
	defer restore()

	var runs atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())

	Go(ctx, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), "always-crashes", func(ctx context.Context) {
		runs.Add(1)
		panic("always")
	})

	// Let it crash at least once, then cancel and observe the run count settle.
	deadline := time.Now().Add(2 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	settled := runs.Load()
	time.Sleep(100 * time.Millisecond)
	if got := runs.Load(); got > settled+1 {
		t.Fatalf("restarts continued after ctx cancel: %d -> %d", settled, got)
	}
}

// compressRestartSchedule shrinks the restart backoff so tests run in
// milliseconds, restoring the production values afterwards.
func compressRestartSchedule(t *testing.T) func() {
	t.Helper()
	origInitial, origMax, origReset := initialRestartDelay, maxRestartDelay, cleanRunReset
	initialRestartDelay = time.Millisecond
	maxRestartDelay = 10 * time.Millisecond
	cleanRunReset = time.Hour
	return func() {
		initialRestartDelay, maxRestartDelay, cleanRunReset = origInitial, origMax, origReset
	}
}

// TestBG19_BackgroundPanicDoesNotKillProcess is the BG-19 attack re-run, in a
// subprocess so the parent test survives observing a process death. The child
// launches a background worker whose tick panics:
//
//   - mode "bare" reproduces the pre-fix wiring (`go worker(ctx)`, the
//     main.go:618-671 pattern) — the panic must kill the child process. This is
//     the attack control proving the defect is real.
//   - mode "safego" launches the same worker via safego.Go — the child must
//     survive the panic, prove a post-panic restart ran, and exit 0. This is
//     the refutation.
func TestBG19_BackgroundPanicDoesNotKillProcess(t *testing.T) {
	if mode := os.Getenv("LETTUCE_SAFEGO_CRASHER_MODE"); mode != "" {
		crasherChild(mode)
		return
	}

	t.Run("bare launch kills the process (pre-fix wiring)", func(t *testing.T) {
		out, err := runCrasherChild(t, "bare")
		if err == nil {
			t.Fatalf("bare-launched panicking worker must kill the process; child survived with output:\n%s", out)
		}
		if !strings.Contains(out, "panic: tick bomb") {
			t.Errorf("child death must be the worker panic; output:\n%s", out)
		}
		if strings.Contains(out, "SURVIVED-AFTER-RESTART") {
			t.Errorf("bare mode must never reach the survival marker; output:\n%s", out)
		}
	})

	t.Run("safego launch survives the panic and keeps ticking", func(t *testing.T) {
		out, err := runCrasherChild(t, "safego")
		if err != nil {
			t.Fatalf("safego-launched worker must not kill the process; child failed (%v) with output:\n%s", err, out)
		}
		if !strings.Contains(out, "SURVIVED-AFTER-RESTART") {
			t.Errorf("child must prove a post-panic restart ran; output:\n%s", out)
		}
	})
}

// runCrasherChild re-execs this test binary in crasher-child mode.
func runCrasherChild(t *testing.T, mode string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "TestBG19_BackgroundPanicDoesNotKillProcess", "-test.v")
	cmd.Env = append(os.Environ(), "LETTUCE_SAFEGO_CRASHER_MODE="+mode)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// crasherChild is the child-process body: launch a panicking ticker worker in
// the requested mode, then report survival. Never returns to the test harness
// normally — it exits the process explicitly so the parent reads a clean signal.
func crasherChild(mode string) {
	initialRestartDelay = time.Millisecond
	maxRestartDelay = 10 * time.Millisecond

	var runs atomic.Int32
	worker := func(ctx context.Context) {
		// A background sweeper whose tick hits a poison row: panics every run.
		n := runs.Add(1)
		panic(fmt.Sprintf("tick bomb %d", n))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	switch mode {
	case "bare":
		go worker(ctx) // the pre-fix main.go wiring: no recovery
	case "safego":
		Go(ctx, logger, "crasher", worker)
	default:
		fmt.Fprintf(os.Stderr, "unknown crasher mode %q\n", mode)
		os.Exit(3)
	}

	// Wait for evidence of a post-panic restart (>=2 runs). In bare mode the
	// process dies on the first panic before this loop can succeed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runs.Load() >= 2 {
			fmt.Println("SURVIVED-AFTER-RESTART")
			os.Exit(0)
		}
		time.Sleep(5 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "no restart observed before deadline")
	os.Exit(4)
}
