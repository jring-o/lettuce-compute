//go:build !windows

package runtime

import (
	"os"
	"syscall"
)

// openNoFollow opens path read-only with O_NOFOLLOW so the kernel refuses the open
// outright when the final path component is a symlink — closing the Lstat→open
// TOCTOU window that a portable Lstat-only check cannot.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
