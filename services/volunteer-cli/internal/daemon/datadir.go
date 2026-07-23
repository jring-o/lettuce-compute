package daemon

import (
	"fmt"
	"os"
	"runtime"
)

// EnsureDataDirPrivate creates the data dir if needed and enforces the 0o700
// owner-only mode the sandbox containment model assumes (PB-30).
//
// The container runtime's rw bind dirs (and restored checkpoint contents) are
// deliberately world-writable so the hardened 65534 container user can write
// them (PB-23/PB-29); what keeps other LOCAL users away from those dirs is
// solely the 0o700 data dir above them. MkdirAll never tightens a PRE-EXISTING
// looser directory, so pointing --data-dir at an existing 0o755 dir silently
// voided that shield — another local user could poison a result or plant a
// checkpoint. Tightening (rather than refusing) preserves the operator's
// intent while restoring the invariant; the return reports whether a loose
// mode was actually corrected so the caller can WARN visibly.
//
// On Windows the POSIX group/other bits are not the access-control model
// (directories are governed by ACLs; Go reports synthetic modes), so only
// creation is performed there.
func EnsureDataDirPrivate(dataDir string) (tightened bool, err error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return false, fmt.Errorf("creating data directory: %w", err)
	}
	if runtime.GOOS == "windows" {
		return false, nil
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		return false, fmt.Errorf("checking data directory mode: %w", err)
	}
	if info.Mode().Perm()&0o077 == 0 {
		return false, nil
	}
	// A pre-existing dir with group/other access: tighten it. Failing to do so
	// must be fatal to the caller — running with the shield down silently is
	// exactly the PB-30 failure mode.
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return false, fmt.Errorf("tightening data directory to 0700 (it was %04o): %w", info.Mode().Perm(), err)
	}
	return true, nil
}
