package runtime

import (
	"context"
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

// diskWatchdogInterval is how often the /work size watchdog samples on-disk usage.
// A short interval bounds the overshoot beyond the booked ceiling to a small multiple
// of what a unit can write in one tick.
const diskWatchdogInterval = 2 * time.Second

// dirsSizeBytes returns the total size of all regular files under the given
// directories. Symlinks and special files are not counted (their size is not the
// leaf's own disk use), and unreadable entries are skipped rather than failing the
// whole walk.
func dirsSizeBytes(dirs ...string) int64 {
	var total int64
	for _, d := range dirs {
		if d == "" {
			continue
		}
		_ = filepath.WalkDir(d, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries, keep walking
			}
			if entry.Type().IsRegular() {
				if info, e := entry.Info(); e == nil {
					total += info.Size()
				}
			}
			return nil
		})
	}
	return total
}

// startDiskWatchdog polls the total size of dirs every diskWatchdogInterval and calls
// onExceed exactly once, the first time the total exceeds capBytes, then stops. It
// also stops when ctx is done or when the returned stop func is called. This is the
// portable BG-16c enforcement: a leaf's /work growth is bounded at bookedDiskMB (the
// volunteer's configured ceiling), never at the attacker-declared MaxDiskMB. It is a
// poll, so a unit can overshoot by up to one interval's writes before termination —
// bounded, not zero (design §6).
//
// capBytes <= 0 disables the watchdog (returns a no-op stop func).
func startDiskWatchdog(ctx context.Context, capBytes int64, dirs []string, onExceed func(sizeBytes int64)) (stop func()) {
	if capBytes <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { close(done) }) }

	go func() {
		ticker := time.NewTicker(diskWatchdogInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if size := dirsSizeBytes(dirs...); size > capBytes {
					onExceed(size)
					return
				}
			}
		}
	}()
	return stop
}
