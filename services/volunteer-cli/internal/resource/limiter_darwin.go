//go:build darwin

package resource

import (
	"fmt"
	"log/slog"
	"os/exec"
	"syscall"
)

// DarwinLimiter enforces resource limits using setpriority (best-effort).
// macOS does not support CPU affinity, and RLIMIT_RSS is advisory.
type DarwinLimiter struct {
	logger *slog.Logger
}

func newPlatformLimiter(logger *slog.Logger) Limiter {
	return NewDarwinLimiter(logger)
}

// NewDarwinLimiter creates a limiter for macOS.
func NewDarwinLimiter(logger *slog.Logger) *DarwinLimiter {
	logger.Info("using setpriority for resource limits (macOS, best-effort)")
	return &DarwinLimiter{logger: logger}
}

// Apply configures the exec.Cmd before process start.
func (d *DarwinLimiter) Apply(cmd *exec.Cmd, limits *TaskLimits) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return nil
}

const (
	// lowestNice is the nice value every task runs at: the lowest priority the
	// ordinary scheduler offers. Child processes inherit it.
	lowestNice = 19

	// prioDarwinProcess and prioDarwinBG are setpriority(2)'s PRIO_DARWIN_PROCESS
	// and PRIO_DARWIN_BG (<sys/resource.h>): together they put a process in the
	// background state, where the system schedules its CPU and disk work behind
	// everything the user is doing.
	prioDarwinProcess = 4
	prioDarwinBG      = 0x1000
)

// Enforce runs the task at the lowest priority macOS offers, the only CPU
// management available here: the background state, and nice 19, which every
// child process inherits. Best-effort: a failure is logged and the task runs
// on.
func (d *DarwinLimiter) Enforce(pid int, limits *TaskLimits) (func(), error) {
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, pid, lowestNice); err != nil {
		d.logger.Warn("setpriority failed (best-effort)", "error", err, "pid", pid)
	} else {
		d.logger.Debug("set process priority", "pid", pid, "nice", lowestNice)
	}
	if err := syscall.Setpriority(prioDarwinProcess, pid, prioDarwinBG); err != nil {
		d.logger.Warn("could not put the task in the background state (best-effort)", "error", err, "pid", pid)
	} else {
		d.logger.Debug("task in the background state", "pid", pid)
	}

	return func() {}, nil
}

// CheckDiskSpace checks available disk space on the filesystem containing path.
func (d *DarwinLimiter) CheckDiskSpace(path string, requiredMB int) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("%w: statfs %s: %v", ErrDiskSpaceUnknown, path, err)
	}

	availableMB := (uint64(stat.Bavail) * uint64(stat.Bsize)) / (1024 * 1024)
	if availableMB < uint64(requiredMB) {
		return fmt.Errorf("insufficient disk space: %d MB available, %d MB required", availableMB, requiredMB)
	}

	return nil
}

// describeCPUEnforcement reports macOS's CPU posture.
//
// The Darwin limiter lowers the priority and nothing else: macOS offers no
// per-process CPU quota or affinity API comparable to cgroups or a Job Object,
// so a work unit is de-prioritised rather than capped. max_cpu_cores therefore
// serves only as the capability figure a head gates dispatch on.
func describeCPUEnforcement() CPUEnforcement {
	return CPUEnforcement{
		Mechanism:  "lowered process priority only (macOS has no per-process CPU cap)",
		Confinable: false,
	}
}
