package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Lettuce's own CPU use, for the yield monitor (TB-83).
//
// "Other programs are using N % of the CPU" is only honest if Lettuce's own
// load is subtracted first — the daemon process (which also runs every WASM
// task in-process), every native task's process tree, and every running
// container. Counting our own tasks as foreign is the trap the incumbent's
// users file bugs about (a task in a VM counted as someone else's load, so the
// client paused itself), and the one thing the operator's specification names
// as a test: two units at full quota must never trigger the pause.
//
// The figure is a cumulative CPU-seconds counter the sampler differentiates.
// It must never decrease, or the interval in which a task finishes would show
// its last seconds of work as foreign load. ownCPUMeter keeps it monotonic:
// a source that disappears (a finished task's process group, a removed
// container) keeps the seconds it had used in a retired total.

// ownCPUStatsTimeout bounds one round of container stats reads; a hung
// engine socket must not stall the monitor for longer than a poll.
const ownCPUStatsTimeout = 4 * time.Second

// ownCPUMeter turns per-source cumulative CPU seconds — some of which vanish
// when their source exits — into one total that never decreases.
type ownCPUMeter struct {
	mu      sync.Mutex
	last    map[string]float64
	retired float64
}

// total folds the live sources' current readings in and returns the
// monotonic total. A source missing from live is retired at its last value;
// a source whose reading fell (a counter that restarted) keeps its high mark.
func (m *ownCPUMeter) total(live map[string]float64) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = make(map[string]float64)
	}
	for key, v := range m.last {
		if _, ok := live[key]; !ok {
			m.retired += v
			delete(m.last, key)
		}
	}
	sum := m.retired
	for key, v := range live {
		if v < m.last[key] {
			v = m.last[key]
		}
		m.last[key] = v
		sum += v
	}
	return sum
}

// OwnContainer is one running container task and the engine client that can
// read its stats.
type OwnContainer struct {
	Client      runtime.DockerClient
	ContainerID string
}

// ownCPUSeconds is Lettuce's own cumulative CPU time on this machine: this
// process, the process groups of its native tasks, and its containers. An
// error means the figure is incomplete and the caller must not treat the
// machine's remaining load as other programs'.
func (d *Daemon) ownCPUSeconds() (float64, error) {
	self, err := runtime.SelfCPUSeconds()
	if err != nil {
		return 0, err
	}
	live := make(map[string]float64)

	var containers []OwnContainer
	nativeTasks := 0
	if d.slotManager != nil {
		containers, nativeTasks = d.slotManager.OwnProcesses()
	}

	if d.processGroup != nil {
		groups, err := d.processGroup.CPUSeconds()
		if err != nil {
			return 0, fmt.Errorf("native task CPU time: %w", err)
		}
		for key, secs := range groups {
			live["group:"+key] = secs
		}
	} else if nativeTasks > 0 {
		return 0, errors.New("native tasks are running but no process group tracks them")
	}

	if len(containers) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), ownCPUStatsTimeout)
		defer cancel()
		for _, c := range containers {
			nanos, err := c.Client.ContainerCPUNanos(ctx, c.ContainerID)
			if err != nil {
				if runtime.IsContainerNotFound(err) {
					continue // just finished and removed: its seconds are retired below
				}
				return 0, fmt.Errorf("container %s CPU time: %w", shortContainerID(c.ContainerID), err)
			}
			live["container:"+c.ContainerID] = float64(nanos) / 1e9
		}
	}

	return self + d.ownMeter.total(live), nil
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
