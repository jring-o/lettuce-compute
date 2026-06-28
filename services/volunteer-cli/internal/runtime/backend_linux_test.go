//go:build linux

package runtime

import "testing"

// withMockSocketExists temporarily overrides socketExistsFunc for a test so the
// resolver's "which socket actually exists" probe can be driven deterministically
// without touching real sockets under /run.
func withMockSocketExists(t *testing.T, mock func(path string) bool) {
	t.Helper()
	orig := socketExistsFunc
	t.Cleanup(func() { socketExistsFunc = orig })
	socketExistsFunc = mock
}

// A CONTAINER_HOST unix override must point the Podman backend at that socket,
// mirroring the Docker SDK's client.FromEnv (which honors DOCKER_HOST). Before
// this fix the Linux resolver ignored the override entirely and always returned
// the rootless user socket.
func TestPodmanSocketPath_HonorsContainerHost(t *testing.T) {
	// Set a rootless dir that is NOT the override target, so a pass can only mean
	// the override was honored (not that we happened to return the default).
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("CONTAINER_HOST", "unix:///run/podman/podman.sock")

	got := podmanSocketPath("")
	if got != "/run/podman/podman.sock" {
		t.Fatalf("podmanSocketPath ignored CONTAINER_HOST: got %q, want /run/podman/podman.sock", got)
	}
}

// DOCKER_HOST is honored as a fallback when CONTAINER_HOST is unset, matching the
// Docker backend's client.FromEnv behavior.
func TestPodmanSocketPath_HonorsDockerHost(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("CONTAINER_HOST", "")
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")

	got := podmanSocketPath("")
	if got != "/var/run/docker.sock" {
		t.Fatalf("podmanSocketPath ignored DOCKER_HOST: got %q, want /var/run/docker.sock", got)
	}
}

// On a host running ONLY rootful Podman the rootless user socket is absent and
// only the system socket /run/podman/podman.sock exists; the resolver must probe
// and select it. Before this fix it returned the (non-existent) rootless path, so
// such hosts were never auto-detected.
func TestPodmanSocketPath_RootfulSystemSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("CONTAINER_HOST", "")
	t.Setenv("DOCKER_HOST", "")
	withMockSocketExists(t, func(path string) bool {
		return path == "/run/podman/podman.sock"
	})

	got := podmanSocketPath("")
	if got != "/run/podman/podman.sock" {
		t.Fatalf("podmanSocketPath did not probe the rootful system socket: got %q, want /run/podman/podman.sock", got)
	}
}

// The rootless socket is preferred over the rootful one when both exist (the
// recommended, unprivileged setup keeps working unchanged).
func TestPodmanSocketPath_PrefersRootlessWhenBothExist(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("CONTAINER_HOST", "")
	t.Setenv("DOCKER_HOST", "")
	withMockSocketExists(t, func(string) bool { return true })

	got := podmanSocketPath("")
	want := "/run/user/1000/podman/podman.sock"
	if got != want {
		t.Fatalf("podmanSocketPath did not prefer the rootless socket: got %q, want %q", got, want)
	}
}

// When no override is set and neither socket exists, the resolver preserves the
// historical behavior: return the rootless user socket, so the downstream connect
// attempt and its error message are unchanged from before this fix.
func TestPodmanSocketPath_FallsBackToRootlessDefault(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("CONTAINER_HOST", "")
	t.Setenv("DOCKER_HOST", "")
	withMockSocketExists(t, func(string) bool { return false })

	got := podmanSocketPath("")
	want := "/run/user/1000/podman/podman.sock"
	if got != want {
		t.Fatalf("podmanSocketPath fallback wrong: got %q, want %q", got, want)
	}
}
