//go:build linux

package resource

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// LinuxLimiter enforces resource limits using cgroups v2 (preferred) or
// prlimit/sched_setaffinity as fallback.
type LinuxLimiter struct {
	logger     *slog.Logger
	useCgroups bool
}

func newPlatformLimiter(logger *slog.Logger) Limiter {
	return NewLinuxLimiter(logger)
}

// NewLinuxLimiter creates a limiter, detecting cgroups v2 availability.
func NewLinuxLimiter(logger *slog.Logger) *LinuxLimiter {
	useCgroups := detectCgroupsV2()
	if useCgroups {
		logger.Info("using cgroups v2 for resource limits")
	} else {
		logger.Info("cgroups v2 not available, falling back to prlimit/affinity")
	}
	return &LinuxLimiter{
		logger:     logger,
		useCgroups: useCgroups,
	}
}

// detectCgroupsV2 checks if cgroups v2 is available and writable.
func detectCgroupsV2() bool {
	info, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return false
	}
	// Check if we can create a subdirectory (need cgroup delegation).
	testDir := "/sys/fs/cgroup/lettuce-probe"
	if err := os.Mkdir(testDir, 0o755); err != nil {
		return false
	}
	os.Remove(testDir)
	return info != nil
}

// Apply configures the exec.Cmd before process start.
func (l *LinuxLimiter) Apply(cmd *exec.Cmd, limits *config.ResourceLimits) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Create new process group for clean signal delivery.
	cmd.SysProcAttr.Setpgid = true
	return nil
}

// Enforce applies post-start resource limits (cgroups or prlimit+affinity).
func (l *LinuxLimiter) Enforce(pid int, limits *config.ResourceLimits) (func(), error) {
	if l.useCgroups {
		return l.enforceCgroups(pid, limits)
	}
	return l.enforceFallback(pid, limits)
}

// enforceCgroups creates a cgroup v2 scope for the process with memory and CPU limits.
func (l *LinuxLimiter) enforceCgroups(pid int, limits *config.ResourceLimits) (func(), error) {
	cgroupPath := fmt.Sprintf("/sys/fs/cgroup/lettuce-%d", pid)

	if err := os.MkdirAll(cgroupPath, 0o755); err != nil {
		return nil, fmt.Errorf("create cgroup: %w", err)
	}

	cleanup := func() {
		os.Remove(cgroupPath) // rmdir — only works if empty (after process exits)
	}

	// Set memory limit.
	if limits.MaxMemoryMB > 0 {
		memBytes := int64(limits.MaxMemoryMB) * 1024 * 1024
		memPath := filepath.Join(cgroupPath, "memory.max")
		if err := os.WriteFile(memPath, []byte(strconv.FormatInt(memBytes, 10)), 0o644); err != nil {
			cleanup()
			return nil, fmt.Errorf("set memory.max: %w", err)
		}
		l.logger.Debug("cgroup memory limit set", "bytes", memBytes)
	}

	// Set CPU limit: cpu.max = "{quota} {period}".
	// quota = cores * period; period = 100000 µs (100ms).
	if limits.MaxCPUCores > 0 {
		period := 100000
		quota := limits.MaxCPUCores * period
		cpuMax := fmt.Sprintf("%d %d", quota, period)
		cpuPath := filepath.Join(cgroupPath, "cpu.max")
		if err := os.WriteFile(cpuPath, []byte(cpuMax), 0o644); err != nil {
			cleanup()
			return nil, fmt.Errorf("set cpu.max: %w", err)
		}
		l.logger.Debug("cgroup CPU limit set", "quota", quota, "period", period, "cores", limits.MaxCPUCores)
	}

	// Assign the process to the cgroup.
	procsPath := filepath.Join(cgroupPath, "cgroup.procs")
	if err := os.WriteFile(procsPath, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		cleanup()
		return nil, fmt.Errorf("assign process to cgroup: %w", err)
	}

	l.logger.Info("process assigned to cgroup", "pid", pid, "cgroup", cgroupPath)
	return cleanup, nil
}

// rlimit64 matches the kernel's struct rlimit64.
type rlimit64 struct {
	Cur uint64
	Max uint64
}

// enforceFallback uses prlimit64 for memory and sched_setaffinity for CPU.
func (l *LinuxLimiter) enforceFallback(pid int, limits *config.ResourceLimits) (func(), error) {
	// Set RLIMIT_AS (virtual memory limit) via prlimit64 syscall.
	if limits.MaxMemoryMB > 0 {
		memBytes := uint64(limits.MaxMemoryMB) * 1024 * 1024
		lim := rlimit64{Cur: memBytes, Max: memBytes}
		const rlimitAS = 9 // RLIMIT_AS on Linux
		_, _, errno := syscall.RawSyscall6(
			syscall.SYS_PRLIMIT64,
			uintptr(pid),
			uintptr(rlimitAS),
			uintptr(unsafe.Pointer(&lim)),
			0, 0, 0,
		)
		if errno != 0 {
			l.logger.Warn("prlimit64 RLIMIT_AS failed", "error", errno, "pid", pid)
		} else {
			l.logger.Debug("set memory limit via prlimit64", "pid", pid, "mb", limits.MaxMemoryMB)
		}
	}

	// Set CPU affinity to restrict process to first N cores.
	if limits.MaxCPUCores > 0 {
		var mask [1024 / 64]uint64 // supports up to 1024 CPUs
		for i := 0; i < limits.MaxCPUCores && i < 1024; i++ {
			mask[i/64] |= 1 << (uint(i) % 64)
		}
		_, _, errno := syscall.RawSyscall(
			syscall.SYS_SCHED_SETAFFINITY,
			uintptr(pid),
			unsafe.Sizeof(mask),
			uintptr(unsafe.Pointer(&mask[0])),
		)
		if errno != 0 {
			l.logger.Warn("sched_setaffinity failed", "error", errno, "pid", pid)
		} else {
			l.logger.Debug("set CPU affinity", "pid", pid, "cores", limits.MaxCPUCores)
		}
	}

	return func() {}, nil
}

// CheckDiskSpace checks available disk space on the filesystem containing path.
func (l *LinuxLimiter) CheckDiskSpace(path string, requiredMB int) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("statfs %s: %w", path, err)
	}

	availableMB := (stat.Bavail * uint64(stat.Bsize)) / (1024 * 1024)
	if availableMB < uint64(requiredMB) {
		return fmt.Errorf("insufficient disk space: %d MB available, %d MB required", availableMB, requiredMB)
	}

	return nil
}
