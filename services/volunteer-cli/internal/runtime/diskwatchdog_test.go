package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDirsSizeBytes: the size walk sums regular files across dirs and ignores
// symlinks/directories.
func TestDirsSizeBytes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	_ = os.Mkdir(sub, 0o755)
	if err := os.WriteFile(filepath.Join(sub, "b"), make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := dirsSizeBytes(dir); got != 1500 {
		t.Errorf("dirsSizeBytes = %d, want 1500", got)
	}
}

// TestDiskWatchdogFiresAtCeiling is the BG-16c exit test (i): the watchdog terminates
// a unit when /work exceeds the CONFIG-derived ceiling (BookedDiskMB), NOT the
// attacker-declared MaxDiskMB. A unit that over-declares MaxDiskMB (~50 TB) with a
// configured disk budget of ~50 MB is bounded at ~50 MB. Fails on pre-fix code, which
// keyed enforcement on the raw declared value (or did not enforce disk at all).
func TestDiskWatchdogFiresAtCeiling(t *testing.T) {
	dir := t.TempDir()

	// The unit declares a 50 TB disk; the volunteer's configured budget is 50 MB.
	const declaredMaxDiskMB = 50_000_000
	const configCeilingMB = 50
	bookedMB := BookedDiskMB(declaredMaxDiskMB, configCeilingMB)
	if bookedMB != configCeilingMB {
		t.Fatalf("BookedDiskMB(%d, %d) = %d, want %d (must clamp to the config ceiling, not the declared value)",
			declaredMaxDiskMB, configCeilingMB, bookedMB, configCeilingMB)
	}

	fired := make(chan int64, 1)
	stop := startDiskWatchdog(context.Background(), int64(bookedMB)*1024*1024, []string{dir}, func(size int64) {
		fired <- size
	})
	defer stop()

	// Fill /work past the booked ceiling (write ~60 MB > 50 MB).
	if err := os.WriteFile(filepath.Join(dir, "big.dat"), make([]byte, 60*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case size := <-fired:
		if size <= int64(bookedMB)*1024*1024 {
			t.Errorf("watchdog fired at %d bytes, want > %d (the booked ceiling)", size, int64(bookedMB)*1024*1024)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("disk watchdog did not fire when /work exceeded the booked ceiling")
	}
}

// TestDiskWatchdogDoesNotFireUnderCeiling: a well-behaved unit is never terminated.
func TestDiskWatchdogDoesNotFireUnderCeiling(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small.dat"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	fired := make(chan int64, 1)
	stop := startDiskWatchdog(context.Background(), 50*1024*1024, []string{dir}, func(size int64) {
		fired <- size
	})
	defer stop()

	select {
	case <-fired:
		t.Fatal("watchdog fired for a unit well under the ceiling")
	case <-time.After(3 * diskWatchdogInterval):
		// expected: no fire
	}
}
