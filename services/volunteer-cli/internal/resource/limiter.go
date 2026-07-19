package resource

import (
	"errors"
	"log/slog"
	"os/exec"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// ErrDiskSpaceUnknown marks a CheckDiskSpace failure where free space COULD NOT
// BE DETERMINED (the stat itself failed) — as opposed to a determined-but-
// insufficient result. The distinction is load-bearing: a container engine can
// report an image-store path the host cannot stat at all (a podman machine's
// graphroot is a VM-internal path on Windows/macOS), and treating that as
// "0 MB free" made the disk gate fail closed forever. Callers match it with
// errors.Is and decide per gate whether unknown means block or proceed.
var ErrDiskSpaceUnknown = errors.New("free disk space could not be determined")

// Limiter enforces resource limits on a subprocess.
type Limiter interface {
	// Apply sets resource limits on the exec.Cmd before it is started.
	Apply(cmd *exec.Cmd, limits *config.ResourceLimits) error

	// Enforce is called after the process starts. It sets up any post-start
	// enforcement (e.g., cgroups, job object assignment).
	Enforce(pid int, limits *config.ResourceLimits) (cleanup func(), err error)

	// CheckDiskSpace verifies enough disk space is available before execution.
	// A stat failure (the path cannot be examined from this host) is reported
	// as an error matching ErrDiskSpaceUnknown, distinct from insufficiency.
	CheckDiskSpace(path string, requiredMB int) error
}

// NewLimiter returns a platform-appropriate Limiter.
func NewLimiter(logger *slog.Logger) Limiter {
	return newPlatformLimiter(logger)
}
