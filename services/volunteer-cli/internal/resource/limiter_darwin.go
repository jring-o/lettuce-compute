//go:build darwin

package resource

import (
	"fmt"
	"log/slog"
	"os/exec"
	"syscall"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
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
func (d *DarwinLimiter) Apply(cmd *exec.Cmd, limits *config.ResourceLimits) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return nil
}

// Enforce lowers process priority as best-effort CPU management on macOS.
func (d *DarwinLimiter) Enforce(pid int, limits *config.ResourceLimits) (func(), error) {
	// Lower priority: nice value 10 (range -20 to 19, higher = lower priority).
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10); err != nil {
		d.logger.Warn("setpriority failed (best-effort)", "error", err, "pid", pid)
	} else {
		d.logger.Debug("set process priority", "pid", pid, "nice", 10)
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
