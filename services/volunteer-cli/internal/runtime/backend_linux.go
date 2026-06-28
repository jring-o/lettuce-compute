//go:build linux

package runtime

import (
	"fmt"
	"os"
	"strings"
)

// rootfulPodmanSocket is the system/rootful Podman API socket, owned by root.
// A host running only rootful Podman (e.g. the easier path under an unprivileged
// LXC guest) exposes this and no rootless user socket.
const rootfulPodmanSocket = "/run/podman/podman.sock"

// socketExistsFunc reports whether a Unix socket path exists on disk. It is a
// package-level seam so tests can drive the rootless/rootful probe without
// touching real sockets under /run.
var socketExistsFunc = defaultSocketExists

func defaultSocketExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// podmanSocketPath returns the Podman API socket path on Linux, resolved in order:
//
//  1. An explicit CONTAINER_HOST / DOCKER_HOST unix override, mirroring the Docker
//     SDK's client.FromEnv — so an operator can point the client at any socket
//     without symlink hacks.
//  2. The rootless user socket ($XDG_RUNTIME_DIR/podman/podman.sock, falling back
//     to /run/user/<uid>/podman/podman.sock) when it exists — the recommended,
//     unprivileged setup, kept as the preferred choice.
//  3. The rootful/system socket /run/podman/podman.sock when it exists — a host
//     running ONLY rootful Podman exposes this and no rootless socket; it was
//     never probed before, so such hosts were not auto-detected.
//  4. Otherwise the rootless default path, preserving the prior behavior so the
//     downstream connect attempt and its error message are unchanged.
func podmanSocketPath(binaryPath string) string {
	if sock := socketFromEnv(); sock != "" {
		return sock
	}

	rootless := rootlessPodmanSocket()
	if socketExistsFunc(rootless) {
		return rootless
	}
	if socketExistsFunc(rootfulPodmanSocket) {
		return rootfulPodmanSocket
	}
	return rootless
}

// rootlessPodmanSocket returns the conventional rootless user socket path.
func rootlessPodmanSocket() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return xdg + "/podman/podman.sock"
	}
	return fmt.Sprintf("/run/user/%d/podman/podman.sock", os.Getuid())
}

// socketFromEnv returns the Unix socket path from a CONTAINER_HOST or DOCKER_HOST
// override, or "" if neither names a usable Unix socket. Only "unix://" URLs and
// bare absolute paths yield a socket path; other schemes (tcp://, ssh://) are not
// reachable through the Unix-socket connection path used here and are ignored so
// detection falls through to the on-disk probe.
func socketFromEnv() string {
	for _, key := range []string{"CONTAINER_HOST", "DOCKER_HOST"} {
		if p := unixSocketPath(strings.TrimSpace(os.Getenv(key))); p != "" {
			return p
		}
	}
	return ""
}

// unixSocketPath extracts a filesystem socket path from a Docker/Podman host
// string. It accepts "unix:///path/to.sock" and bare absolute paths ("/path");
// anything else returns "".
func unixSocketPath(host string) string {
	if strings.HasPrefix(host, "unix://") {
		return strings.TrimPrefix(host, "unix://")
	}
	if strings.HasPrefix(host, "/") {
		return host
	}
	return ""
}
