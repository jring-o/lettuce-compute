//go:build darwin

package runtime

import (
	"os"
	"path/filepath"
	"strings"
)

// podmanSocketPath returns the Podman API socket path on macOS.
// Tries `podman machine inspect` first, then falls back to the default location.
func podmanSocketPath(binaryPath string) string {
	bin := binaryPath
	if bin == "" {
		bin = "podman"
	}
	// Try podman machine inspect for the actual socket path.
	out, err := CommandExecutor(bin, "machine", "inspect", "--format", "{{.ConnectionInfo.PodmanSocket.Path}}")
	if err == nil {
		p := strings.TrimSpace(string(out))
		if p != "" {
			return p
		}
	}

	// Fallback to default location.
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "containers", "podman", "machine", "podman.sock")
}
