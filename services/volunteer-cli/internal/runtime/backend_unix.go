//go:build !windows

package runtime

import (
	"os"
	"strings"
)

// socketExistsFunc reports whether a Unix socket path exists on disk. It is a
// package-level seam so tests can drive the socket probes without touching
// real sockets under /run.
var socketExistsFunc = defaultSocketExists

func defaultSocketExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// socketFromEnv returns the Unix socket path from a CONTAINER_HOST or DOCKER_HOST
// override, or "" if neither names a usable Unix socket. Only "unix://" URLs and
// bare absolute paths yield a socket path; other schemes (tcp://, ssh://) are not
// reachable through the Unix-socket connection path used here and are ignored so
// detection falls through to the on-disk probe. Shared by the Linux and macOS
// resolvers (the macOS one ignored the override until TB-54).
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
