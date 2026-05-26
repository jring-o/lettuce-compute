package resource

import (
	"context"
	"log/slog"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// Monitor watches scheduler state and disk space, sending pause/resume
// signals to the daemon via a channel.
type Monitor struct {
	limiter          Limiter
	scheduler        *Scheduler
	limits           *config.ResourceLimits
	dataDir          string
	logger           *slog.Logger
	scheduleInterval time.Duration // overridable for tests
	diskInterval     time.Duration // overridable for tests
}

// NewMonitor creates a resource monitor.
func NewMonitor(limiter Limiter, scheduler *Scheduler, limits *config.ResourceLimits, dataDir string, logger *slog.Logger) *Monitor {
	return &Monitor{
		limiter:          limiter,
		scheduler:        scheduler,
		limits:           limits,
		dataDir:          dataDir,
		logger:           logger,
		scheduleInterval: 10 * time.Second,
		diskInterval:     60 * time.Second,
	}
}

// Run starts the monitoring loop. It sends true to pauseCh when the daemon
// should pause (user became active or disk space low) and false when it
// should resume. Exits when ctx is cancelled.
func (m *Monitor) Run(ctx context.Context, pauseCh chan<- bool) {
	scheduleTicker := time.NewTicker(m.scheduleInterval)
	defer scheduleTicker.Stop()

	diskTicker := time.NewTicker(m.diskInterval)
	defer diskTicker.Stop()

	paused := false
	diskPaused := false

	for {
		select {
		case <-ctx.Done():
			return

		case <-scheduleTicker.C:
			shouldRun := m.scheduler.ShouldRun()
			if paused && shouldRun && !diskPaused {
				m.logger.Info("schedule became active, resuming")
				paused = false
				select {
				case pauseCh <- false:
				case <-ctx.Done():
					return
				}
			} else if !paused && !shouldRun {
				m.logger.Info("schedule became inactive, pausing")
				paused = true
				select {
				case pauseCh <- true:
				case <-ctx.Done():
					return
				}
			}

		case <-diskTicker.C:
			// Check disk space — pause if below 1 GB free.
			err := m.limiter.CheckDiskSpace(m.dataDir, 1024)
			if err != nil && !diskPaused {
				m.logger.Warn("low disk space, pausing execution", "error", err)
				diskPaused = true
				if !paused {
					paused = true
					select {
					case pauseCh <- true:
					case <-ctx.Done():
						return
					}
				}
			} else if err == nil && diskPaused {
				m.logger.Info("disk space recovered")
				diskPaused = false
				if paused && m.scheduler.ShouldRun() {
					paused = false
					select {
					case pauseCh <- false:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}
}
