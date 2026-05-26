package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
)

// ContainerBackend identifies which container engine is available.
type ContainerBackend string

const (
	BackendPodman ContainerBackend = "podman"
	BackendDocker ContainerBackend = "docker"
	BackendNone   ContainerBackend = "none"
)

// BackendInfo holds detection results for a container backend.
type BackendInfo struct {
	Backend    ContainerBackend
	SocketPath string // Unix socket path or Windows named pipe
	Version    string // e.g., "4.9.0"
	BinaryPath string // Path to podman/docker binary (empty if using socket only)
}

// lookPathFunc is overridden in tests to mock exec.LookPath.
var lookPathFunc = exec.LookPath

// isDockerAvailableFunc is overridden in tests to mock Docker detection.
var isDockerAvailableFunc = IsDockerAvailable

// podmanInstallPathFunc resolves a podman binary in a standard install location.
// Overridden in tests.
var podmanInstallPathFunc = defaultPodmanInstallPath

// DetectContainerBackend probes for available container runtimes using the
// historical default priority (Podman first, Docker second). Equivalent to
// DetectContainerBackendPreferred with no preference.
func DetectContainerBackend(bundledPodmanPath string) BackendInfo {
	return DetectContainerBackendPreferred(bundledPodmanPath, "")
}

// DetectContainerBackendPreferred probes for available container runtimes,
// honoring an optional preferred backend (from config's container_backend).
//
//   - preferred == BackendDocker: choose Docker if available, falling back to
//     Podman, then none. Preferring Docker matters on Windows/macOS because its
//     storage lives on the host (Docker Desktop's WSL2 backend / host volume)
//     rather than inside a Podman-machine VM, so large images are not subject to
//     a separate VM disk.
//   - preferred == BackendPodman or "" (auto): try Podman first (bundled binary,
//     then PATH), then Docker, then none — the historical default.
func DetectContainerBackendPreferred(bundledPodmanPath string, preferred ContainerBackend) BackendInfo {
	if preferred == BackendDocker {
		if isDockerAvailableFunc() {
			return BackendInfo{Backend: BackendDocker}
		}
		// Preferred Docker isn't available — fall back to Podman if present.
		if info, ok := detectPodman(bundledPodmanPath); ok {
			return info
		}
		return BackendInfo{Backend: BackendNone}
	}

	// Auto / preferred Podman: Podman first, then Docker.
	if info, ok := detectPodman(bundledPodmanPath); ok {
		return info
	}
	if isDockerAvailableFunc() {
		return BackendInfo{Backend: BackendDocker}
	}
	return BackendInfo{Backend: BackendNone}
}

// detectPodman looks for a Podman binary and resolves its socket path and version.
func detectPodman(bundledPath string) (BackendInfo, bool) {
	var binaryPath string

	// Check bundled binary first.
	if bundledPath != "" {
		if _, err := os.Stat(bundledPath); err == nil {
			binaryPath = bundledPath
		}
	}

	// Fall back to system PATH.
	if binaryPath == "" {
		if p, err := lookPathFunc("podman"); err == nil {
			binaryPath = p
		}
	}

	// Fall back to standard install locations. The desktop wizard installs podman
	// via MSI to %LOCALAPPDATA%\Programs\Podman, but the already-running app's PATH
	// isn't refreshed for that session, so lookPath misses it and the daemon would
	// otherwise fall through to Docker. Mirror the Rust-side find_podman so the
	// bundled-podman flow works out of the box, no PATH or restart required.
	if binaryPath == "" {
		binaryPath = podmanInstallPathFunc()
	}

	if binaryPath == "" {
		return BackendInfo{}, false
	}

	// Resolve socket path (platform-specific).
	socketPath := podmanSocketPath(binaryPath)

	// Get version.
	version := podmanVersion(binaryPath)

	return BackendInfo{
		Backend:    BackendPodman,
		SocketPath: socketPath,
		Version:    version,
		BinaryPath: binaryPath,
	}, true
}

// podmanVersion runs `podman --version` and parses the version string.
func podmanVersion(binaryPath string) string {
	out, err := CommandExecutor(binaryPath, "--version")
	if err != nil {
		return ""
	}
	return parseVersionOutput(string(out))
}

// BundledPodmanPath returns the expected location of the bundled Podman binary
// relative to the running executable. Returns empty string if not found.
func BundledPodmanPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	podman := filepath.Join(dir, "podman")
	if goruntime.GOOS == "windows" {
		podman += ".exe"
	}
	if _, err := os.Stat(podman); err == nil {
		return podman
	}
	return ""
}

// defaultPodmanInstallPath returns the path to a podman binary in a standard
// install location, or "" if none is found. On Windows the per-user MSI installs
// to %LOCALAPPDATA%\Programs\Podman and the machine-scope MSI to
// C:\Program Files\RedHat\Podman. This mirrors the desktop app's Rust-side
// find_podman so the daemon can use a freshly wizard-installed podman even when
// the running process's PATH predates the install.
func defaultPodmanInstallPath() string {
	if goruntime.GOOS != "windows" {
		return ""
	}
	var candidates []string
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		candidates = append(candidates, filepath.Join(la, "Programs", "Podman", "podman.exe"))
	}
	candidates = append(candidates, `C:\Program Files\RedHat\Podman\podman.exe`)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// podmanHostString converts a socket path to a Docker client host string.
func podmanHostString(socketPath string) string {
	if goruntime.GOOS == "windows" {
		return fmt.Sprintf("npipe://%s", socketPath)
	}
	return fmt.Sprintf("unix://%s", socketPath)
}
