package resource

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
)

// PB-2: a CheckDiskSpace stat failure must be reported as ErrDiskSpaceUnknown —
// distinct from determined-but-insufficient — on every platform, using the REAL
// platform limiter. On Windows this exercises the exact mechanism of the filed
// bug: GetDiskFreeSpaceEx failing on a path this host cannot examine (there, a
// podman machine's VM-internal graphroot).
func TestCheckDiskSpace_StatFailureIsErrDiskSpaceUnknown(t *testing.T) {
	lim := NewLimiter(slog.Default())
	missing := filepath.Join(t.TempDir(), "no-such-dir", "no-such-sub")

	err := lim.CheckDiskSpace(missing, 1)
	if err == nil {
		t.Fatalf("CheckDiskSpace(%q) succeeded; expected a stat failure", missing)
	}
	if !errors.Is(err, ErrDiskSpaceUnknown) {
		t.Fatalf("stat failure not marked ErrDiskSpaceUnknown (callers would treat it as a full disk): %v", err)
	}
}

// A stattable path with an absurd requirement reports plain insufficiency, NOT
// ErrDiskSpaceUnknown — the two conditions must stay distinguishable.
func TestCheckDiskSpace_InsufficiencyIsNotUnknown(t *testing.T) {
	lim := NewLimiter(slog.Default())
	dir := t.TempDir()

	err := lim.CheckDiskSpace(dir, 1<<40) // ~1 EB in MB: no machine has this
	if err == nil {
		t.Fatalf("CheckDiskSpace(%q, 1<<40) succeeded; expected insufficiency", dir)
	}
	if errors.Is(err, ErrDiskSpaceUnknown) {
		t.Fatalf("insufficiency wrongly marked ErrDiskSpaceUnknown: %v", err)
	}
}
