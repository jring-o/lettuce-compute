//go:build darwin

package runtime

import (
	"os"
	"path/filepath"
	"strings"
)

// podmanSocketPath returns the Podman API socket path on macOS, resolved in order:
//
//  1. An explicit CONTAINER_HOST / DOCKER_HOST unix override, as on Linux and
//     as the Docker SDK's client.FromEnv does — the macOS resolver ignored it
//     before TB-54.
//  2. What `podman machine inspect` reports for the running machine, asked of
//     the binary detection found (an absolute path, so this works from a
//     Finder-launched app whose PATH does not include Podman).
//  3. The default machine socket location under the user's home.
func podmanSocketPath(binaryPath string) string {
	if sock := socketFromEnv(); sock != "" {
		return sock
	}

	bin := binaryPath
	if bin == "" {
		bin = "podman"
	}
	out, err := CommandExecutor(bin, "machine", "inspect", "--format", "{{.ConnectionInfo.PodmanSocket.Path}}")
	if err == nil {
		p := strings.TrimSpace(string(out))
		if p != "" {
			return p
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "containers", "podman", "machine", "podman.sock")
}
