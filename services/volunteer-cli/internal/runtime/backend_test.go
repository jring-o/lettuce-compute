package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// withMockLookPath temporarily overrides lookPathFunc for testing.
func withMockLookPath(t *testing.T, mock func(file string) (string, error)) {
	t.Helper()
	orig := lookPathFunc
	t.Cleanup(func() { lookPathFunc = orig })
	lookPathFunc = mock
}

// withMockDockerAvailable temporarily overrides isDockerAvailableFunc for testing.
func withMockDockerAvailable(t *testing.T, available bool) {
	t.Helper()
	orig := isDockerAvailableFunc
	t.Cleanup(func() { isDockerAvailableFunc = orig })
	isDockerAvailableFunc = func() bool { return available }
}

// withMockPodmanInstallPath temporarily overrides podmanInstallPathFunc.
func withMockPodmanInstallPath(t *testing.T, path string) {
	t.Helper()
	orig := podmanInstallPathFunc
	t.Cleanup(func() { podmanInstallPathFunc = orig })
	podmanInstallPathFunc = func() string { return path }
}

// detectPodman must find a podman that's installed in a standard location even
// when it's not on PATH and not bundled next to the sidecar — this is the
// wizard-installed-podman case that previously fell through to Docker.
func TestDetectPodman_FindsInstallPathWhenNotOnPath(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "podman.exe")
	if err := os.WriteFile(installed, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Not bundled, not on PATH — only present at the install location.
	withMockLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	withMockPodmanInstallPath(t, installed)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == installed && len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 5.7.0\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	info, ok := detectPodman("")
	if !ok {
		t.Fatal("expected podman to be detected via install path")
	}
	if info.BinaryPath != installed {
		t.Errorf("BinaryPath = %q, want %q", info.BinaryPath, installed)
	}
	if info.Backend != BackendPodman {
		t.Errorf("Backend = %s, want podman", info.Backend)
	}
}

// With podman installed (install path) but config preferring Docker absent,
// the auto/podman path picks podman over Docker.
func TestDetectContainerBackend_InstallPathPodmanBeatsDocker(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "podman.exe")
	if err := os.WriteFile(installed, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	withMockLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	withMockPodmanInstallPath(t, installed)
	withMockDockerAvailable(t, true)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == installed && len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 5.7.0\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	info := DetectContainerBackendPreferred("", "")
	if info.Backend != BackendPodman {
		t.Fatalf("expected podman (install path) to win over docker, got %s", info.Backend)
	}
}

func TestDetectContainerBackendPreferred_DockerPreferredAndAvailable(t *testing.T) {
	withMockDockerAvailable(t, true)
	// Podman must NOT be probed when Docker is preferred and available.
	withMockLookPath(t, func(string) (string, error) {
		t.Error("LookPath should not be called when Docker is preferred and available")
		return "", exec.ErrNotFound
	})

	info := DetectContainerBackendPreferred("", BackendDocker)
	if info.Backend != BackendDocker {
		t.Fatalf("expected BackendDocker, got %s", info.Backend)
	}
}

func TestDetectContainerBackendPreferred_DockerPreferredFallsBackToPodman(t *testing.T) {
	withMockDockerAvailable(t, false)
	withMockLookPath(t, func(file string) (string, error) {
		if file == "podman" {
			return "/usr/bin/podman", nil
		}
		return "", exec.ErrNotFound
	})
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/podman" && len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	info := DetectContainerBackendPreferred("", BackendDocker)
	if info.Backend != BackendPodman {
		t.Fatalf("expected fallback to BackendPodman, got %s", info.Backend)
	}
}

func TestDetectContainerBackendPreferred_DockerPreferredNeitherAvailable(t *testing.T) {
	withMockDockerAvailable(t, false)
	withMockLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	withMockExecutor(t, notFoundForAll)

	info := DetectContainerBackendPreferred("", BackendDocker)
	if info.Backend != BackendNone {
		t.Fatalf("expected BackendNone, got %s", info.Backend)
	}
}

func TestDetectContainerBackendPreferred_PodmanFirstWhenBothPresent(t *testing.T) {
	// Auto/Podman preference: Podman wins even if Docker is also available.
	withMockDockerAvailable(t, true)
	withMockLookPath(t, func(file string) (string, error) {
		if file == "podman" {
			return "/usr/bin/podman", nil
		}
		return "", exec.ErrNotFound
	})
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/podman" && len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	for _, pref := range []ContainerBackend{"", BackendPodman} {
		info := DetectContainerBackendPreferred("", pref)
		if info.Backend != BackendPodman {
			t.Fatalf("preferred=%q: expected BackendPodman, got %s", pref, info.Backend)
		}
	}
}

func TestDetectContainerBackendPreferred_AutoFallsBackToDocker(t *testing.T) {
	// No Podman, Docker available, no explicit preference → Docker.
	withMockDockerAvailable(t, true)
	withMockLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	withMockExecutor(t, notFoundForAll)

	info := DetectContainerBackendPreferred("", "")
	if info.Backend != BackendDocker {
		t.Fatalf("expected BackendDocker, got %s", info.Backend)
	}
}

func TestDetectContainerBackend_PodmanFound(t *testing.T) {
	withMockLookPath(t, func(file string) (string, error) {
		if file == "podman" {
			return "/usr/bin/podman", nil
		}
		return "", exec.ErrNotFound
	})

	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/podman" && len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.0\n"), nil
		}
		// podman machine inspect fallback (macOS/Windows).
		return nil, exec.ErrNotFound
	})

	info := DetectContainerBackend("")
	if info.Backend != BackendPodman {
		t.Fatalf("expected BackendPodman, got %s", info.Backend)
	}
	if info.BinaryPath != "/usr/bin/podman" {
		t.Errorf("expected binary path /usr/bin/podman, got %s", info.BinaryPath)
	}
	if info.Version != "4.9.0" {
		t.Errorf("expected version 4.9.0, got %s", info.Version)
	}
	if info.SocketPath == "" {
		t.Error("expected non-empty socket path")
	}
}

func TestDetectContainerBackend_BundledPodmanPreferred(t *testing.T) {
	dir := t.TempDir()
	bundledPath := dir + "/podman"
	if runtime.GOOS == "windows" {
		bundledPath = dir + "\\podman.exe"
	}
	if err := os.WriteFile(bundledPath, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	// LookPath should NOT be called if bundled binary exists.
	withMockLookPath(t, func(file string) (string, error) {
		t.Error("LookPath should not be called when bundled binary exists")
		return "", exec.ErrNotFound
	})

	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == bundledPath && len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 5.0.0\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	info := DetectContainerBackend(bundledPath)
	if info.Backend != BackendPodman {
		t.Fatalf("expected BackendPodman, got %s", info.Backend)
	}
	if info.BinaryPath != bundledPath {
		t.Errorf("expected bundled path %s, got %s", bundledPath, info.BinaryPath)
	}
	if info.Version != "5.0.0" {
		t.Errorf("expected version 5.0.0, got %s", info.Version)
	}
}

func TestDetectContainerBackend_NoPodman_FallsThrough(t *testing.T) {
	withMockLookPath(t, func(file string) (string, error) {
		return "", exec.ErrNotFound
	})
	withMockExecutor(t, notFoundForAll)

	info := DetectContainerBackend("")
	if info.Backend == BackendPodman {
		t.Fatal("should not detect podman when not in PATH")
	}
	// Backend is either Docker (if daemon running) or None — both valid.
}

func TestPodmanVersion_Parsing(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.9.3\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	v := podmanVersion("/usr/bin/podman")
	if v != "4.9.3" {
		t.Errorf("expected 4.9.3, got %s", v)
	}
}

func TestPodmanVersion_Error(t *testing.T) {
	withMockExecutor(t, notFoundForAll)

	v := podmanVersion("/usr/bin/podman")
	if v != "" {
		t.Errorf("expected empty version on error, got %s", v)
	}
}

func TestPodmanHostString(t *testing.T) {
	if runtime.GOOS == "windows" {
		got := podmanHostString(`//./pipe/podman-machine-default`)
		expected := `npipe:////./pipe/podman-machine-default`
		if got != expected {
			t.Errorf("expected %s, got %s", expected, got)
		}
	} else {
		got := podmanHostString("/run/user/1000/podman/podman.sock")
		expected := "unix:///run/user/1000/podman/podman.sock"
		if got != expected {
			t.Errorf("expected %s, got %s", expected, got)
		}
	}
}

func TestPodmanSocketPath(t *testing.T) {
	withMockExecutor(t, notFoundForAll)

	path := podmanSocketPath("")
	if path == "" {
		t.Error("expected non-empty socket path")
	}
}

func TestPodmanSocketPath_WithBinaryPath(t *testing.T) {
	var capturedBin string
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		capturedBin = name
		return nil, exec.ErrNotFound
	})

	podmanSocketPath("/opt/bundled/podman")
	if capturedBin == "" {
		// On Linux, no shell-out happens — binary path is unused.
		if runtime.GOOS != "linux" {
			t.Error("expected command executor to be called with binary path")
		}
	} else if capturedBin != "/opt/bundled/podman" {
		t.Errorf("expected binary /opt/bundled/podman, got %s", capturedBin)
	}
}

func TestBundledPodmanPath_NotFound(t *testing.T) {
	path := BundledPodmanPath()
	if path != "" {
		t.Errorf("expected empty path when no bundled binary exists, got %s", path)
	}
}
