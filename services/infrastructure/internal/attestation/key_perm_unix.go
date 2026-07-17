//go:build unix

package attestation

import (
	"fmt"
	"os"
	"syscall"
)

// keyPermRemediation is the exact host-side fix appended to every refusal. The
// head runs as a non-root uid against a read-only mount, so it can never repair
// the key itself — the operator must. The operator guide's troubleshooting row
// greps this file for "insecure permissions" and "not owned by", so those
// substrings are a pinned contract; do not reword them.
const keyPermRemediation = "in the bundled production deploy run: " +
	"sudo chown 10001:10001 keys/signing.key && chmod 600 keys/signing.key on the host; " +
	"the file must be owned by the uid the head runs as, mode 0600"

// checkKeyFilePermissions enforces that the signing key file is a regular file,
// readable only by its owner (mode 0600 or tighter), and owned by the uid the
// head process runs as. The signing key is the platform's external trust
// anchor; a group- or world-readable key, or one owned by a different local
// user, is refused so the head fails closed instead of trusting a key that
// another account could have read or replaced.
//
// A missing file returns the os.Lstat error unchanged so callers can still use
// os.IsNotExist to fall through to the missing-file (autogen or fail-closed)
// path. os.Lstat (not Stat) is used so a symlink is reported as a symlink rather
// than followed to its target.
func checkKeyFilePermissions(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if fi.Mode()&os.ModeType != 0 {
		return fmt.Errorf(
			"signing key %q is not a regular file (mode %v); the signing key must be a regular file. %s",
			path, fi.Mode(), keyPermRemediation)
	}

	if perm := fi.Mode().Perm(); perm&0077 != 0 {
		return fmt.Errorf(
			"signing key %q has insecure permissions %04o: it is readable by group or others. %s",
			path, perm, keyPermRemediation)
	}

	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf(
			"signing key %q: cannot determine file owner to verify ownership. %s",
			path, keyPermRemediation)
	}
	if owner, euid := int(stat.Uid), processEUID(); owner != euid {
		return fmt.Errorf(
			"signing key %q is not owned by the head process user (owner uid %d, process uid %d). %s",
			path, owner, euid, keyPermRemediation)
	}

	return nil
}
