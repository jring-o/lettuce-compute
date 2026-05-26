//go:build linux

package runtime

import (
	"fmt"
	"os"
)

// podmanSocketPath returns the Podman API socket path on Linux (rootless).
// Uses $XDG_RUNTIME_DIR/podman/podman.sock, falling back to /run/user/{uid}/podman/podman.sock.
func podmanSocketPath(binaryPath string) string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return xdg + "/podman/podman.sock"
	}
	return fmt.Sprintf("/run/user/%d/podman/podman.sock", os.Getuid())
}
