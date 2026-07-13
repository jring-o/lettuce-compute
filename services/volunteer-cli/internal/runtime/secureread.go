package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// errNotRegularFile is returned by readRegularNoFollow when the target is not a
// regular file — a symlink, directory, device, socket, or fifo. Callers treat it
// as "no readable output here" and skip the entry; the link is NEVER followed.
var errNotRegularFile = errors.New("not a regular file")

// readRegularNoFollow reads a file only if it is a regular file whose final path
// component is not a symlink. It is the single shared, symlink-safe output reader
// for every runtime (BG-15 / BG-15b): a malicious leaf that leaves output.dat as a
// symlink to the volunteer's signing key (~/.lettuce/identity.key) must read
// NOTHING, not the key. Two independent guards:
//
//   - an Lstat that refuses any non-regular entry (portable; also covers sockets,
//     devices and fifos), and
//   - where the OS supports it, O_NOFOLLOW on the open itself, so a symlink swapped
//     in between the Lstat and the open (a TOCTOU race) is still refused at open
//     time (openNoFollow is platform-specific; Windows relies on the Lstat guard,
//     since a reparse point reports as a non-regular mode there).
func readRegularNoFollow(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s", errNotRegularFile, path)
	}

	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Belt: re-check the handle we actually opened. On platforms without
	// O_NOFOLLOW this catches a final-component swap between the Lstat and the open.
	if fi2, statErr := f.Stat(); statErr == nil && !fi2.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s", errNotRegularFile, path)
	}
	return io.ReadAll(f)
}
