package resource

import (
	"log/slog"
	"os/exec"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// Limiter enforces resource limits on a subprocess.
type Limiter interface {
	// Apply sets resource limits on the exec.Cmd before it is started.
	Apply(cmd *exec.Cmd, limits *config.ResourceLimits) error

	// Enforce is called after the process starts. It sets up any post-start
	// enforcement (e.g., cgroups, job object assignment).
	Enforce(pid int, limits *config.ResourceLimits) (cleanup func(), err error)

	// CheckDiskSpace verifies enough disk space is available before execution.
	CheckDiskSpace(path string, requiredMB int) error
}

// NewLimiter returns a platform-appropriate Limiter.
func NewLimiter(logger *slog.Logger) Limiter {
	return newPlatformLimiter(logger)
}
