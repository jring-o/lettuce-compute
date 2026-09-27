package runtime

import (
	"context"
	"errors"
	"sync"
	"time"
)

// A container result's CPU time and peak memory. The engine reports a
// container's usage only while it runs: once it exits, Docker answers a stats
// request with an empty reading and Podman with an error, and inspect carries
// only the configured limits. So a running container's stats are read every
// containerUsageInterval, and the result reports the last reading taken
// before it exited. At most one interval of CPU time at the end of a run goes
// unmeasured, and a burst of memory shorter than an interval can be missed;
// neither is ever made up.

// containerUsageInterval is how often a running container's stats are read.
const containerUsageInterval = 2 * time.Second

// containerUsageTimeout bounds one stats read, so a hung engine socket cannot
// hold a finished run's result back for longer than this.
const containerUsageTimeout = 5 * time.Second

// containerUsage accumulates one run's stats readings: the CPU counters of
// the latest reading, less those at the start of the run, and the highest
// working set seen.
type containerUsage struct {
	mu sync.Mutex
	// base is the CPU the container had used before this run: nil for a
	// container created for the run, whose counters start at zero.
	base *ContainerStats
	// baseUnknown marks an adopted container whose counters at adoption could
	// not be read, so its CPU for this run cannot be told apart.
	baseUnknown bool
	last        *ContainerStats
	peakBytes   uint64
}

// setBaseline records the counters an adopted container brought from its
// previous session; a nil reading (the engine did not answer) leaves the
// run's CPU unmeasured rather than counting the earlier session's.
func (u *containerUsage) setBaseline(s *ContainerStats) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.base = s
	u.baseUnknown = s == nil
	if s != nil && s.MemoryBytes > u.peakBytes {
		u.peakBytes = s.MemoryBytes
	}
}

// add folds one reading in. The CPU counters are cumulative, so a reading
// below the latest one (an engine that answered with less) never replaces it.
func (u *containerUsage) add(s *ContainerStats) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.last == nil || s.CPUUsageUser+s.CPUUsageKernel >= u.last.CPUUsageUser+u.last.CPUUsageKernel {
		u.last = s
	}
	if s.MemoryBytes > u.peakBytes {
		u.peakBytes = s.MemoryBytes
	}
}

// cpu is the user and kernel CPU time of the run in nanoseconds, and whether
// it was measured at all.
func (u *containerUsage) cpu() (user, kernel uint64, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.last == nil || u.baseUnknown {
		return 0, 0, false
	}
	user, kernel = u.last.CPUUsageUser, u.last.CPUUsageKernel
	if u.base != nil {
		user = counterDelta(user, u.base.CPUUsageUser)
		kernel = counterDelta(kernel, u.base.CPUUsageKernel)
	}
	return user, kernel, true
}

// peakMemory is the highest working set read during the run, in bytes; 0
// when no reading arrived.
func (u *containerUsage) peakMemory() uint64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.peakBytes
}

func counterDelta(now, before uint64) uint64 {
	if now < before {
		return 0
	}
	return now - before
}

// readContainerUsage takes one stats reading of a running container, or nil
// with its error when the engine gave none.
func (c *ContainerRuntime) readContainerUsage(ctx context.Context, containerID string) (*ContainerStats, error) {
	readCtx, cancel := context.WithTimeout(ctx, containerUsageTimeout)
	defer cancel()
	return c.dockerClient.ContainerUsage(readCtx, containerID)
}

// startContainerUsageSampler reads the container's stats into usage every
// interval until stop is called. stop cancels a reading in flight and waits
// for the sampler to finish, so the caller can read usage as soon as it
// returns; calling it again is harmless. Failed readings are logged when they
// start and when they stop, not on every retry.
func (c *ContainerRuntime) startContainerUsageSampler(ctx context.Context, containerID string, usage *containerUsage) (stop func()) {
	interval := containerUsageInterval
	if c.usagePollOverride > 0 {
		interval = c.usagePollOverride
	}
	sampleCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		failing := false
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				s, err := c.readContainerUsage(sampleCtx, containerID)
				switch {
				case err == nil:
					usage.add(s)
					if failing {
						failing = false
						c.logger.Debug("container stats readable again", "container", shortImageID(containerID))
					}
				case sampleCtx.Err() != nil, errors.Is(err, errContainerNotRunning):
					// Stopped mid-reading, or the container has just exited and
					// the wait is about to return.
				case !failing:
					failing = true
					c.logger.Debug("container stats unavailable; its CPU time and peak memory may be incomplete",
						"container", shortImageID(containerID), "error", err)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}
