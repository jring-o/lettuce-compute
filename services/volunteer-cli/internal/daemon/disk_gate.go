package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// The disk gate (TB-24).
//
// max_disk_gb is a capacity OFFER: the head only dispatches leafs whose
// declared min_disk_mb fits under it, and Lettuce keeps its own footprint (work
// folders under the data dir plus cached container images) within it. It is NOT
// a free-space floor: what a fetch requires free is the LEAF's declared need,
// not the whole allowance. The old gate demanded the entire allowance free for
// any fresh image pull, with no credit for space the allowance had already
// spent — so pulling an image (the thing the allowance permits) pushed the
// machine toward failing its own gate, and raising the allowance to qualify
// for a big leaf made fetching strictly harder.
//
// Per leaf, the gate now asks three questions:
//
//  1. Free space on the data-dir volume >= the leaf's workspace need + a small
//     absolute floor. For a fresh pull the workspace need is the full declared
//     min_disk_mb (conservative: the declared need bundles image + workspace
//     and the split is not published). For an already-cached image it is the
//     declared need bounded by cachedImageWorkspaceHeadroomMB — a rerun writes
//     a workspace, not the image.
//  2. Lettuce's measured usage + the leaf's incremental need <= the allowance
//     (skipped when usage cannot be measured — unknown is not full).
//  3. For a fresh pull only: free space on every container image-store volume
//     >= the declared need + the floor (the image lands in the engine's store,
//     not the data dir; an unstattable store is noted and not gated on).
//
// doctor derives its disk verdicts from these same functions so the diagnostic
// and the live gate can never disagree (TODO #24).

// DiskFloorMB is the absolute free-space floor: below this on the data-dir
// volume nothing is fetched at all, and every per-leaf requirement adds it on
// top so a fetch can never aim the volume at zero bytes free.
const DiskFloorMB = 2048

// cachedImageWorkspaceHeadroomMB bounds the workspace requirement for a unit
// whose image is already pulled. A rerun only writes a workspace — the image is
// already on disk — so the requirement is the leaf's declared need bounded by
// this, never the full allowance.
const cachedImageWorkspaceHeadroomMB = 10 * 1024 // 10 GB

// imageCacheCheckTTL bounds how often the disk gate probes the container
// backend for image presence, so a disk-gated daemon doesn't spawn an "image
// exists" call on every loop iteration.
const imageCacheCheckTTL = 30 * time.Second

// diskUsageCheckTTL bounds how often Lettuce's own usage is re-measured; the
// measurement walks the data-dir tree, which is not free on big work dirs.
const diskUsageCheckTTL = 60 * time.Second

// UnknownLeafDiskNeedMB is the disk need assumed for a leaf that does not
// declare resource_requirements.min_disk_mb. It used to be the per-task /work
// ceiling default (runtime.DefaultPerTaskDiskMB, 10 GB), which exactly equals
// the default max_disk_gb allowance — usage + need could then only fit at zero
// measured usage, so a default-configured volunteer was silently disk-gated on
// every undeclared leaf the moment the daemon had written a log file (TB-31).
// 2 GB keeps "unknown is not needs-nothing" while leaving the default
// allowance ~8 GB of usage headroom; heads stamp min_disk_mb = 1024 at leaf
// creation (and migration 00029 backfills legacy leafs), so this fallback only
// governs heads that publish no requirements at all.
const UnknownLeafDiskNeedMB = 2048

// EffectiveLeafDiskNeedMB returns the disk need the gate uses for a leaf
// declaring declaredMB (resource_requirements.min_disk_mb; 0 = the head did
// not say). Exported because doctor's per-leaf verdicts must apply the same
// fallback, or the diagnostic and the live gate disagree on undeclared leafs.
func EffectiveLeafDiskNeedMB(declaredMB int64) int64 {
	if declaredMB > 0 {
		return declaredMB
	}
	return UnknownLeafDiskNeedMB
}

// leafDiskNeedMB returns the disk the leaf declares it needs on this machine
// (resource_requirements.min_disk_mb, the same number the head's dispatch gate
// compares against the advertised allowance), or the unknown-need fallback
// when the head did not say.
func leafDiskNeedMB(leaf CachedLeafInfo) int64 {
	var declared int64
	if leaf.ResourceRequirements != nil {
		declared = leaf.ResourceRequirements.MinDiskMB
	}
	return EffectiveLeafDiskNeedMB(declared)
}

// LeafDiskThresholds returns the free-space thresholds (MB) the fetch gate
// applies for one leaf: dataDirMB on the data-dir volume, and storeMB on each
// container image-store volume (0 = the store is not gated, i.e. no fresh pull
// is needed). Shared with doctor (TODO #24).
func LeafDiskThresholds(needMB int64, isContainer, imageCached bool) (dataDirMB, storeMB int64) {
	if isContainer && imageCached {
		ws := needMB
		if ws > cachedImageWorkspaceHeadroomMB {
			ws = cachedImageWorkspaceHeadroomMB
		}
		return ws + DiskFloorMB, 0
	}
	if isContainer {
		return needMB + DiskFloorMB, needMB + DiskFloorMB
	}
	return needMB + DiskFloorMB, 0
}

// leafIncrementalNeedMB is the budget-side counterpart of LeafDiskThresholds:
// how much NEW space running this leaf can consume. A cached image is already
// counted inside the measured usage, so only the workspace is incremental.
func leafIncrementalNeedMB(needMB int64, isContainer, imageCached bool) int64 {
	if isContainer && imageCached {
		if needMB > cachedImageWorkspaceHeadroomMB {
			return cachedImageWorkspaceHeadroomMB
		}
	}
	return needMB
}

// DiskBudgetVerdict applies the allowance budget: Lettuce's measured usage plus
// the leaf's incremental need must fit under max_disk_gb. Returns ok and, when
// blocked, a reason naming the numbers and the setting (the refusal must name
// the setting or the confusion outlives the fix — TB-24's prior-art lesson).
func DiskBudgetVerdict(needMB int64, isContainer, imageCached bool, usedMB, allowanceMB int64) (bool, string) {
	incr := leafIncrementalNeedMB(needMB, isContainer, imageCached)
	if usedMB+incr <= allowanceMB {
		return true, ""
	}
	return false, fmt.Sprintf(
		"disk budget: Lettuce already uses %d MB (work folders + cached images) and this leaf needs %d MB more, exceeding the %d MB max_disk_gb allowance — free space (superseded images are reclaimed automatically), disable an unused leaf, or raise resource_limits.max_disk_gb",
		usedMB, incr, allowanceMB)
}

// leafIsContainer reports whether the leaf executes as a container, and whether
// that is even knowable. A nil execution spec (a head that doesn't publish it,
// or the synthetic any-leaf request) is unknown — the caller treats unknown as
// container iff a container runtime is registered, which is the conservative
// reading (gates the image store it might fill).
func leafIsContainer(leaf CachedLeafInfo) (isContainer, known bool) {
	if leaf.ExecutionSpec == nil {
		return false, false
	}
	return leaf.ExecutionSpec.Image != "", true
}

// leafDiskGate is the live per-leaf disk gate: whether fetching work for this
// leaf is currently allowed, and if not, why. Called by shouldFetch (fetch iff
// at least one enabled leaf passes) and by the fetcher (skip exactly the leafs
// that don't).
func (d *Daemon) leafDiskGate(leaf CachedLeafInfo) (bool, string) {
	if d.limiter == nil {
		return true, ""
	}

	need := leafDiskNeedMB(leaf)
	isContainer, known := leafIsContainer(leaf)
	if !known {
		isContainer = d.runtimeRegistry != nil && d.runtimeRegistry.GetRuntime("container") != nil
	}
	cached := false
	if isContainer && known {
		cached = d.leafImageCached(leaf)
	}

	dataDirMB, storeMB := LeafDiskThresholds(need, isContainer, cached)

	if err := d.limiter.CheckDiskSpace(d.cfg.DataDir, int(dataDirMB)); err != nil {
		return false, fmt.Sprintf("data dir %s: %v (leaf needs %d MB + %d MB floor)", d.cfg.DataDir, err, need, DiskFloorMB)
	}

	if usedMB, ok := d.lettuceDiskUsageMB(); ok {
		if ok, reason := DiskBudgetVerdict(need, isContainer, cached, usedMB, int64(d.cfg.ResourceLimits.MaxDiskGB)*1024); !ok {
			return false, reason
		}
	}

	if storeMB > 0 {
		if paths, ok := d.imageStorePaths(); ok {
			for _, path := range paths {
				if err := d.limiter.CheckDiskSpace(path, int(storeMB)); err != nil {
					// Unknown is not "full" — see noteUnstattableImageStore.
					if errors.Is(err, resource.ErrDiskSpaceUnknown) {
						d.noteUnstattableImageStore(path)
						continue
					}
					return false, fmt.Sprintf("image store %s: %v (a fresh pull of this leaf's image needs %d MB + %d MB floor there)", path, err, need, DiskFloorMB)
				}
			}
		}
	}

	return true, ""
}

// leafImageCached reports whether this leaf's container image is already in the
// engine's store, from the TTL-cached per-image sweep.
func (d *Daemon) leafImageCached(leaf CachedLeafInfo) bool {
	if leaf.ExecutionSpec == nil || leaf.ExecutionSpec.Image == "" {
		return false
	}
	return d.enabledImagesCached()[leaf.ExecutionSpec.Image]
}

// enabledImagesCached returns image ref -> already-pulled for every enabled
// leaf's image, cached for imageCacheCheckTTL. Empty when no container runtime
// is registered (native-only volunteers, or tests using mock runtimes — which
// keeps the disk gate from ever shelling out under test).
func (d *Daemon) enabledImagesCached() map[string]bool {
	d.imgCacheMu.Lock()
	if !d.imgCacheChecked.IsZero() && time.Since(d.imgCacheChecked) < imageCacheCheckTTL {
		m := d.imgCached
		d.imgCacheMu.Unlock()
		return m
	}
	d.imgCacheMu.Unlock()

	result := d.checkEnabledImagesCached()

	d.imgCacheMu.Lock()
	d.imgCacheChecked = time.Now()
	d.imgCached = result
	d.imgCacheMu.Unlock()
	return result
}

func (d *Daemon) checkEnabledImagesCached() map[string]bool {
	result := make(map[string]bool)
	if d.runtimeRegistry == nil || d.multiClient == nil {
		return result
	}
	cr, ok := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime)
	if !ok || cr == nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range d.multiClient.Servers() {
		for _, lf := range d.enabledLeafs(srv.Name) {
			if lf.ExecutionSpec == nil || lf.ExecutionSpec.Image == "" {
				continue
			}
			img := lf.ExecutionSpec.Image
			if _, seen := result[img]; seen {
				continue
			}
			exists, err := cr.Client().ImageExists(ctx, img)
			result[img] = err == nil && exists
		}
	}
	return result
}

// lettuceDiskUsageMB measures Lettuce's own disk footprint: everything under
// the data dir (work dirs, checkpoints, artifact caches, logs, identity) plus
// the cached container images of the wanted repositories — superseded copies
// included, because they occupy the allowance until the image reaper reclaims
// them. TTL-cached. ok=false means the measurement failed; callers must treat
// that as unknown, never as zero.
func (d *Daemon) lettuceDiskUsageMB() (int64, bool) {
	d.diskUsageMu.Lock()
	if !d.diskUsageChecked.IsZero() && time.Since(d.diskUsageChecked) < diskUsageCheckTTL {
		mb, ok := d.diskUsageMB, d.diskUsageOK
		d.diskUsageMu.Unlock()
		return mb, ok
	}
	d.diskUsageMu.Unlock()

	mb, ok := d.measureDiskUsageMB()

	d.diskUsageMu.Lock()
	d.diskUsageChecked = time.Now()
	d.diskUsageMB, d.diskUsageOK = mb, ok
	d.diskUsageMu.Unlock()
	return mb, ok
}

func (d *Daemon) measureDiskUsageMB() (int64, bool) {
	wsMB, err := DirSizeMB(d.cfg.DataDir)
	if err != nil {
		return 0, false
	}
	total := wsMB
	if d.runtimeRegistry != nil {
		if cr, ok := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime); ok && cr != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if bytes, ok := cr.WantedImageBytes(ctx); ok {
				total += bytes / (1024 * 1024)
			}
			// Image bytes unknown (engine unreachable): report the workspace part
			// alone — an underestimate fails open, matching unknown-is-not-full.
		}
	}
	return total, true
}

// DiskUsage returns Lettuce's measured disk usage and the configured allowance,
// both in MB, for the management metrics endpoint. ok=false when usage cannot
// be measured right now.
func (d *Daemon) DiskUsage() (usedMB, allowanceMB int64, ok bool) {
	usedMB, ok = d.lettuceDiskUsageMB()
	return usedMB, int64(d.cfg.ResourceLimits.MaxDiskGB) * 1024, ok
}

// DirSizeMB sums the sizes of all regular files under root, in MB. Unreadable
// entries are skipped rather than failing the whole walk — a work dir being
// deleted mid-walk must not turn the measurement into an error.
func DirSizeMB(root string) (int64, error) {
	var bytes int64
	err := filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err // an unreadable root is a failed measurement, not zero usage
			}
			return fs.SkipDir
		}
		if de.Type().IsRegular() {
			if info, ierr := de.Info(); ierr == nil {
				bytes += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return bytes / (1024 * 1024), nil
}
