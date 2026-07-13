//go:build windows

package runtime

import "os"

// openNoFollow on Windows relies on the caller's Lstat/IsRegular guard: Windows has
// no O_NOFOLLOW, but Go's os.Lstat reports a reparse point (symlink or junction) as
// a non-regular mode, so a symlinked final component is already refused before this
// function is reached. A plain read-only open is therefore the safe second read.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}
