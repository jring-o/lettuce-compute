package resource

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// mockLimiter implements Limiter for testing.
type mockLimiter struct {
	mu      sync.Mutex
	diskErr error
}

func (m *mockLimiter) Apply(_ *exec.Cmd, _ *config.ResourceLimits) error { return nil }
func (m *mockLimiter) Enforce(_ int, _ *config.ResourceLimits) (func(), error) {
	return func() {}, nil
}
func (m *mockLimiter) CheckDiskSpace(_ string, _ int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.diskErr
}

func newTestMonitor(limiter Limiter, sched *Scheduler, dataDir string) *Monitor {
	limits := &config.ResourceLimits{MaxDiskGB: 10}
	mon := NewMonitor(limiter, sched, limits, dataDir, slog.Default())
	mon.scheduleInterval = 50 * time.Millisecond
	mon.diskInterval = 50 * time.Millisecond
	return mon
}

func TestMonitor_PausesWhenSchedulerInactive(t *testing.T) {
	sched := NewScheduler(&config.Scheduling{
		Mode:              "WHEN_IDLE",
		IdleThresholdMins: 5,
	}, slog.Default())
	sched.idleFunc = func() (int, error) { return 0, nil }

	mon := newTestMonitor(&mockLimiter{}, sched, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pauseCh := make(chan bool, 10)
	go mon.Run(ctx, pauseCh)

	select {
	case paused := <-pauseCh:
		if !paused {
			t.Error("expected pause=true signal")
		}
	case <-ctx.Done():
		t.Error("timeout waiting for pause signal")
	}
}

func TestMonitor_ResumesWhenSchedulerBecomesActive(t *testing.T) {
	var idle atomic.Int64
	sched := NewScheduler(&config.Scheduling{
		Mode:              "WHEN_IDLE",
		IdleThresholdMins: 5,
	}, slog.Default())
	sched.idleFunc = func() (int, error) { return int(idle.Load()), nil }

	mon := newTestMonitor(&mockLimiter{}, sched, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pauseCh := make(chan bool, 10)
	go mon.Run(ctx, pauseCh)

	// Wait for pause.
	select {
	case <-pauseCh:
	case <-ctx.Done():
		t.Fatal("timeout waiting for initial pause")
	}

	// Simulate user going idle.
	idle.Store(600)

	// Should receive resume signal.
	select {
	case paused := <-pauseCh:
		if paused {
			t.Error("expected pause=false (resume) signal")
		}
	case <-ctx.Done():
		t.Error("timeout waiting for resume signal")
	}
}

func TestMonitor_StopsOnContextCancel(t *testing.T) {
	sched := NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, slog.Default())
	mon := newTestMonitor(&mockLimiter{}, sched, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	pauseCh := make(chan bool, 10)

	done := make(chan struct{})
	go func() {
		mon.Run(ctx, pauseCh)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// success
	case <-time.After(1 * time.Second):
		t.Error("monitor did not stop after context cancel")
	}
}

func TestMonitor_PausesOnDiskSpaceLow(t *testing.T) {
	sched := NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, slog.Default())
	mon := newTestMonitor(&mockLimiter{diskErr: fmt.Errorf("insufficient disk space")}, sched, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pauseCh := make(chan bool, 10)
	go mon.Run(ctx, pauseCh)

	select {
	case paused := <-pauseCh:
		if !paused {
			t.Error("expected pause=true signal from disk space check")
		}
	case <-ctx.Done():
		t.Error("timeout waiting for disk space pause signal")
	}
}

func TestMonitor_ResumesWhenDiskSpaceRecovers(t *testing.T) {
	ml := &mockLimiter{diskErr: fmt.Errorf("insufficient disk space")}
	sched := NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, slog.Default())
	mon := newTestMonitor(ml, sched, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pauseCh := make(chan bool, 10)
	go mon.Run(ctx, pauseCh)

	// Wait for the initial disk pause.
	select {
	case paused := <-pauseCh:
		if !paused {
			t.Fatal("expected initial pause from disk space")
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for disk pause")
	}

	// Simulate disk space recovery.
	ml.mu.Lock()
	ml.diskErr = nil
	ml.mu.Unlock()

	// Should receive resume signal.
	select {
	case paused := <-pauseCh:
		if paused {
			t.Error("expected pause=false (resume) after disk recovery")
		}
	case <-ctx.Done():
		t.Error("timeout waiting for disk recovery resume")
	}
}

func TestMonitor_NoPauseWhenAlwaysActiveAndDiskOK(t *testing.T) {
	sched := NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, slog.Default())
	mon := newTestMonitor(&mockLimiter{}, sched, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	pauseCh := make(chan bool, 10)
	go mon.Run(ctx, pauseCh)

	// Should not receive any signals when scheduler is ALWAYS and disk is OK.
	select {
	case paused := <-pauseCh:
		t.Errorf("unexpected pause signal: %v", paused)
	case <-ctx.Done():
		// expected: no signals
	}
}
