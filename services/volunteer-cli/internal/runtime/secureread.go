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

// errOutputTooLarge is returned when a leaf-controlled file exceeds maxReadBytes.
// The unit fails rather than letting the daemon allocate unbounded RAM (BG-16f).
var errOutputTooLarge = errors.New("output file exceeds maximum readable size")

// maxReadBytes caps how much of a leaf-controlled file the shared reader will pull
// into the DAEMON's memory (BG-16f). Before this cap the read ended in a bare
// io.ReadAll, so a container leaf whose booked MEMORY was 512 MB could still make
// the daemon allocate up to its DISK ceiling (~10 GB) — or, for native/wasm, up to
// free disk — reading output.dat, a memory-amplification vector BookedMemMB never
// covered. 2 GiB unifies with the artifact-download copyCapped ceiling; a leaf that
// legitimately produces more must stream via an external reference, not inline.
const maxReadBytes int64 = 2 << 30 // 2 GiB

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
	return readRegularNoFollowLimited(path, maxReadBytes)
}

// readRegularNoFollowLimited is the core, with an explicit byte cap so tests can
// exercise the BG-16f overflow path without a multi-gigabyte fixture.
func readRegularNoFollowLimited(path string, maxBytes int64) ([]byte, error) {
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

	// BG-16f: bound the read into daemon RAM. Read at most maxBytes+1 so an overflow
	// is detectable (n > maxBytes) rather than silently truncated the way a bare
	// io.LimitReader would; fail the unit on overflow.
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %s (> %d bytes)", errOutputTooLarge, path, maxBytes)
	}
	return data, nil
}

// ReadRegularNoFollow is the exported entry point to the shared symlink-safe,
// size-capped reader for callers outside this package — the daemon's orphan-resume
// output reader and its progress reader (BG-15c), which previously used a plain
// symlink-following os.ReadFile. Routing them here means a future isolation change
// cannot silently reopen the exfiltration path readRegularNoFollow closes.
func ReadRegularNoFollow(path string) ([]byte, error) {
	return readRegularNoFollow(path)
}
