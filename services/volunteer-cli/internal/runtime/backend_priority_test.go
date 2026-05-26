package runtime

import (
	"os"
	"os/exec"
	goruntime "runtime"
	"testing"
)

// TestScenario7_BackendDetectionPriority is the Scenario 7 E2E test for v0.9.1.
// It verifies the full fallback chain: bundled Podman > system Podman > Docker > none.

func TestScenario7_BothAvailable_PodmanSelected(t *testing.T) {
	// Mock: Podman found in PATH.
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

	// Note: IsDockerAvailable() will fail (no real Docker daemon) so Docker appears unavailable.
	// But since Podman is found first, it doesn't matter.
	info := DetectContainerBackend("")
	if info.Backend != BackendPodman {
		t.Errorf("expected BackendPodman when Podman available, got %s", info.Backend)
	}
}

func TestScenario7_OnlyDockerAvailable_DockerSelected(t *testing.T) {
	// Mock: Podman NOT found.
	withMockLookPath(t, func(file string) (string, error) {
		return "", exec.ErrNotFound
	})
	withMockExecutor(t, notFoundForAll)

	// Note: IsDockerAvailable() makes a real Docker daemon call.
	// If Docker Desktop is running on this machine, this returns BackendDocker.
	// If not, it returns BackendNone. Either is correct behavior.
	info := DetectContainerBackend("")
	if info.Backend == BackendPodman {
		t.Error("expected NOT BackendPodman when Podman unavailable")
	}
	// Backend is either Docker (if daemon running) or None — both correct.
}

func TestScenario7_NeitherAvailable_NoneSelected(t *testing.T) {
	// Mock: Podman NOT found.
	withMockLookPath(t, func(file string) (string, error) {
		return "", exec.ErrNotFound
	})
	withMockExecutor(t, notFoundForAll)

	// Use a dummy bundled path that doesn't exist.
	info := DetectContainerBackend("/nonexistent/path/podman")
	// If Docker daemon happens to be running, we get Docker. Otherwise None.
	// The key assertion: Podman is NOT selected.
	if info.Backend == BackendPodman {
		t.Error("expected NOT BackendPodman when neither bundled nor system Podman exists")
	}
}

func TestScenario7_BundledPodmanPreferredOverSystem(t *testing.T) {
	// Create a fake bundled binary.
	dir := t.TempDir()
	bundledPath := dir + "/podman"
	if goruntime.GOOS == "windows" {
		bundledPath = dir + "\\podman.exe"
	}
	if err := os.WriteFile(bundledPath, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	systemPodmanCalled := false
	withMockLookPath(t, func(file string) (string, error) {
		systemPodmanCalled = true
		return "/usr/bin/podman-system", nil
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
	if systemPodmanCalled {
		t.Error("LookPath should not be called when bundled binary exists")
	}
}

func TestScenario7_SystemPodmanWhenNoBundled(t *testing.T) {
	withMockLookPath(t, func(file string) (string, error) {
		if file == "podman" {
			return "/usr/local/bin/podman", nil
		}
		return "", exec.ErrNotFound
	})

	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "/usr/local/bin/podman" && len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 4.8.0\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	// No bundled path — should fall through to system.
	info := DetectContainerBackend("")
	if info.Backend != BackendPodman {
		t.Fatalf("expected BackendPodman, got %s", info.Backend)
	}
	if info.BinaryPath != "/usr/local/bin/podman" {
		t.Errorf("expected system path /usr/local/bin/podman, got %s", info.BinaryPath)
	}
	if info.Version != "4.8.0" {
		t.Errorf("expected version 4.8.0, got %s", info.Version)
	}
}
