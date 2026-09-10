//go:build !windows

package daemon

import (
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

const terminateGracePeriod = 5 * time.Second

// pgidGroup implements ProcessGroup on Unix using process groups.
// Each child process becomes its own process group leader (Setpgid=true).
// On Terminate(), we send SIGTERM, wait 5 seconds, then SIGKILL survivors.
type pgidGroup struct {
	mu     sync.Mutex
	pgids  map[int]struct{} // tracked process group IDs (== child PIDs with Setpgid)
	logger *slog.Logger
}

// NewProcessGroup creates a Unix process group tracker.
func NewProcessGroup(logger *slog.Logger) (ProcessGroup, error) {
	logger.Info("process group created (Unix setpgid)")
	return &pgidGroup{
		pgids:  make(map[int]struct{}),
		logger: logger,
	}, nil
}

func (g *pgidGroup) ConfigureCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	setPdeathsig(cmd)
}

func (g *pgidGroup) Add(pid int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pgids[pid] = struct{}{}
	g.logger.Debug("tracking process group", "pgid", pid)
	return nil
}

func (g *pgidGroup) Terminate() {
	g.mu.Lock()
	pgids := make(map[int]struct{}, len(g.pgids))
	for pgid := range g.pgids {
		pgids[pgid] = struct{}{}
	}
	g.mu.Unlock()

	// Phase 1: SIGTERM — give processes a chance to save state and exit.
	alive := g.signalAll(pgids, syscall.SIGTERM, "SIGTERM")
	if len(alive) == 0 {
		g.clearPgids()
		return
	}

	// Wait for grace period, then force-kill survivors.
	g.logger.Info("waiting for processes to exit gracefully",
		"count", len(alive), "grace_period", terminateGracePeriod)
	time.Sleep(terminateGracePeriod)

	// Phase 2: SIGKILL — force-kill anything still running.
	g.signalAll(alive, syscall.SIGKILL, "SIGKILL")
	g.clearPgids()
}

// signalAll sends a signal to all process groups and returns those that were still alive.
func (g *pgidGroup) signalAll(pgids map[int]struct{}, sig syscall.Signal, name string) map[int]struct{} {
	alive := make(map[int]struct{})
	for pgid := range pgids {
		if err := syscall.Kill(-pgid, sig); err != nil {
			if err != syscall.ESRCH {
				g.logger.Warn("failed to signal process group", "pgid", pgid, "signal", name, "error", err)
			}
			// ESRCH = already exited, not alive
		} else {
			g.logger.Debug("sent signal to process group", "pgid", pgid, "signal", name)
			alive[pgid] = struct{}{}
		}
	}
	return alive
}

func (g *pgidGroup) clearPgids() {
	g.mu.Lock()
	g.pgids = make(map[int]struct{})
	g.mu.Unlock()
}

// CPUSeconds sums each tracked group's process tree from the process table
// (TB-83). A group the table no longer has any process for has exited and is
// dropped from tracking, so a later process that happens to reuse its id is
// not mistaken for it.
func (g *pgidGroup) CPUSeconds() (map[string]float64, error) {
	g.mu.Lock()
	pgids := make([]int, 0, len(g.pgids))
	for pgid := range g.pgids {
		pgids = append(pgids, pgid)
	}
	g.mu.Unlock()

	found, err := runtime.ProcessGroupsCPUSeconds(pgids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(found))
	g.mu.Lock()
	for _, pgid := range pgids {
		secs, alive := found[pgid]
		if !alive {
			delete(g.pgids, pgid)
			continue
		}
		out[strconv.Itoa(pgid)] = secs
	}
	g.mu.Unlock()
	return out, nil
}

func (g *pgidGroup) ReleaseChildren() {
	// On Unix, suspended (SIGSTOP'd) processes naturally survive parent exit.
	// Just clear tracked pgids so Terminate() won't kill them.
	g.mu.Lock()
	g.pgids = make(map[int]struct{})
	g.mu.Unlock()
	g.logger.Info("released children from process group tracking")
}

func (g *pgidGroup) Close() {
	// No OS resources to release on Unix.
}
