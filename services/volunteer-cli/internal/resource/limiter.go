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

const (
	// goHeapArenaGranuleMB is the granule the Go runtime maps heap arenas in.
	// Every managed runtime commits memory in chunks rather than by the byte;
	// this is the largest granule we have measured and the one our own leaves
	// hit, so it sets the scale of the headroom below.
	goHeapArenaGranuleMB = 64

	// minFallbackMemoryLimitMB is the floor for the fallback ceiling. Measured:
	// no Go program starts under 64 MiB of RLIMIT_DATA whatever its working set,
	// because it cannot map even one arena granule. The floor keeps a small
	// per-unit declaration from landing below the point where a managed runtime
	// can reach main() at all.
	minFallbackMemoryLimitMB = 128
)

// fallbackMemoryLimitMB converts a work unit's declared memory ceiling into the
// value the non-cgroups Linux path actually enforces. A declared 0 (unspecified)
// stays 0, meaning no ceiling.
//
// The declared value must not be enforced verbatim. An rlimit bounds MAPPED
// memory, whereas cgroups and the container engines bound RESIDENT memory, and a
// managed runtime maps considerably more than it touches. Measured against a Go
// leaf on Linux 6.6:
//
//   - a 100 MiB working set dies under a 128 MiB RLIMIT_DATA, because Go maps
//     arenas in 64 MiB granules and so has 128 MiB mapped before the arena index
//     and runtime structures are counted;
//   - no Go program at all starts below 64 MiB.
//
// Enforcing the declaration exactly would therefore kill leaves behaving exactly
// as declared — a subtler rerun of the RLIMIT_AS failure this replaced (TB-11).
// The ceiling is headroomed instead: twice the declaration plus one granule.
//
// This is deliberately a backstop against a runaway leaf on the least-sandboxed
// runtime we have, not an accounting boundary; precise accounting is what the
// cgroups path is for. Measured: a unit declaring 128 MiB runs at its full
// declaration and is still stopped at 400 MiB.
func fallbackMemoryLimitMB(declaredMB int) int {
	if declaredMB <= 0 {
		return 0
	}
	limit := declaredMB*2 + goHeapArenaGranuleMB
	if limit < minFallbackMemoryLimitMB {
		limit = minFallbackMemoryLimitMB
	}
	return limit
}
