// Package safego launches long-lived background goroutines with panic
// containment. A panic in any goroutine kills the whole Go process, so a single
// poison row hit by a background sweeper tick would crash the head, be
// restarted by the container runtime, hit the same row, and crash-loop the
// entire service. Request handlers are protected by the gRPC/HTTP recovery
// wrappers; this package is the equivalent for the background jobs (leader
// monitors, sweepers, dispatch-cache loops, rate-limit reapers).
package safego

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Restart pacing for a panicked job. The delay doubles per consecutive panic up
// to the cap, and resets once a run survives cleanRunReset, so a deterministic
// early panic cannot spin hot while a rare once-a-day panic still restarts
// promptly. Package vars (not consts) only so tests can compress the schedule.
var (
	initialRestartDelay = time.Second
	maxRestartDelay     = time.Minute
	cleanRunReset       = 5 * time.Minute
)

// Go runs fn on a new goroutine. A panic inside fn is recovered and logged
// (with the job name and stack), and fn is restarted after a backoff until ctx
// is done — so a panicking ticker loop resumes from the top of its loop at the
// next restart instead of silently dying or killing the process. When fn
// returns normally the goroutine ends without restart (that is fn's own
// shutdown path, e.g. ctx cancellation or a closed stop channel).
func Go(ctx context.Context, logger *slog.Logger, name string, fn func(context.Context)) {
	if logger == nil {
		logger = slog.Default()
	}
	go func() {
		delay := initialRestartDelay
		for {
			started := time.Now()
			panicked := runRecovered(ctx, logger, name, fn)
			if !panicked || ctx.Err() != nil {
				return
			}
			if time.Since(started) >= cleanRunReset {
				delay = initialRestartDelay
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay *= 2
			if delay > maxRestartDelay {
				delay = maxRestartDelay
			}
		}
	}()
}

// runRecovered executes one run of fn, converting a panic into a logged,
// reported return instead of process death.
func runRecovered(ctx context.Context, logger *slog.Logger, name string, fn func(context.Context)) (panicked bool) {
	defer func() {
		if rec := recover(); rec != nil {
			panicked = true
			logger.Error("background job panicked; recovered, restarting after backoff",
				"job", name,
				"panic", fmt.Sprintf("%v", rec),
				"stack", string(debug.Stack()),
			)
		}
	}()
	fn(ctx)
	return false
}
