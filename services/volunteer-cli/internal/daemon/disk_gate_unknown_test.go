package daemon

import (
	"fmt"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
)

// PB-2 regression coverage: the container engine can report an image-store path
// this host cannot stat at all — on the documented Windows/macOS setup a podman
// machine's graphroot is a VM-internal path. "Could not determine free space"
// must be handled distinctly from "0 MB free": the old gate treated the stat
// failure as insufficiency and failed closed FOREVER, so any
// CONTAINER-advertising Windows volunteer fetched no work of any runtime, while
// doctor called the same condition a non-blocking warning.

// unstattablePathLimiter models a host where some paths cannot be examined at
// all (stat fails), mirroring the platform limiters' ErrDiskSpaceUnknown
// wrapping, while other paths report normal per-path free space.
type unstattablePathLimiter struct {
	pathLimiter
	unstattable map[string]bool
}

func (l *unstattablePathLimiter) CheckDiskSpace(path string, requiredMB int) error {
	if l.unstattable[path] {
		return fmt.Errorf("%w: GetDiskFreeSpaceEx %s: The system cannot find the path specified", resource.ErrDiskSpaceUnknown, path)
	}
	return l.pathLimiter.CheckDiskSpace(path, requiredMB)
}

// The Windows+podman repro: roomy data dir, engine-reported store path the host
// cannot stat, no cached image. The gate must NOT fail closed — unknown is not
// "full", and doctor's verdict for the same condition is non-blocking.
func TestShouldFetch_UnstattableImageStoreDoesNotGate(t *testing.T) {
	const dataDir = "/data"
	const storePath = "/home/user/.local/share/containers/storage" // VM-internal
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	lim := &unstattablePathLimiter{
		pathLimiter: pathLimiter{availMB: map[string]int{dataDir: 200 * 1024}},
		unstattable: map[string]bool{storePath: true},
	}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, lim, scheduler)
	d.cfg.DataDir = dataDir
	d.cfg.ResourceLimits.MaxDiskGB = 100

	seedContainerLeaf(t, d, mc, &fakeDocker{exists: false, storePath: storePath}, "ghcr.io/example/big:1")

	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false, want true — an unstattable image-store path must not be treated as a full disk (PB-2)")
	}
}

// A genuinely low image store must STILL gate — the unknown carve-out must not
// weaken the real insufficiency path (already covered by
// TestShouldFetch_GatesImageStoreFilesystem; pinned here against the same
// limiter type used above so both behaviors are proven on one seam).
func TestShouldFetch_GenuinelyLowImageStoreStillGates(t *testing.T) {
	const dataDir = "/data"
	const storePath = "/var/lib/containers/storage"
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	lim := &unstattablePathLimiter{
		pathLimiter: pathLimiter{availMB: map[string]int{dataDir: 200 * 1024, storePath: 20 * 1024}},
	}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, lim, scheduler)
	d.cfg.DataDir = dataDir
	d.cfg.ResourceLimits.MaxDiskGB = 100

	seedContainerLeaf(t, d, mc, &fakeDocker{exists: false, storePath: storePath}, "ghcr.io/example/big:1")

	if d.shouldFetch() {
		t.Fatal("shouldFetch = true, want false — a determined-and-insufficient image store must still gate")
	}
}
