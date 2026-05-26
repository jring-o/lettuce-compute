//go:build windows

package runtime

import "strings"

// podmanSocketPath returns the Podman API named pipe on Windows.
// Tries `podman machine inspect` first, then falls back to the default pipe name.
func podmanSocketPath(binaryPath string) string {
	bin := binaryPath
	if bin == "" {
		bin = "podman"
	}
	// Try podman machine inspect for the actual pipe name.
	out, err := CommandExecutor(bin, "machine", "inspect", "--format", "{{.ConnectionInfo.PodmanPipe.Path}}")
	if err == nil {
		p := strings.TrimSpace(string(out))
		if p != "" {
			return p
		}
	}

	// Default named pipe.
	return `//./pipe/podman-machine-default`
}
