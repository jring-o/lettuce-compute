package runtime

import (
	"bytes"
	"fmt"
	"log/slog"
	goruntime "runtime"
	"strings"
	"testing"
)

// withMockFileExists temporarily overrides fileExists for the duration of a test.
func withMockFileExists(t *testing.T, mock func(path string) bool) {
	t.Helper()
	orig := fileExists
	t.Cleanup(func() { fileExists = orig })
	fileExists = mock
}

// noFilesExist is a fileExists mock that reports no files found.
func noFilesExist(path string) bool { return false }

// lookPathNotFound is a lookPathFunc mock that always returns not-found.
func lookPathNotFound(name string) (string, error) {
	return "", fmt.Errorf("executable not found: %s", name)
}

func TestVerifyPodmanGPUSupport_CDIExists(t *testing.T) {
	withMockFileExists(t, func(path string) bool {
		return path == "/etc/cdi/nvidia.yaml"
	})
	withMockLookPath(t, lookPathNotFound)

	if err := verifyPodmanGPUSupport(); err != nil {
		t.Errorf("expected nil when CDI spec exists, got: %v", err)
	}
}

func TestVerifyPodmanGPUSupport_CDIExistsVarRun(t *testing.T) {
	withMockFileExists(t, func(path string) bool {
		return path == "/var/run/cdi/nvidia.yaml"
	})
	withMockLookPath(t, lookPathNotFound)

	if err := verifyPodmanGPUSupport(); err != nil {
		t.Errorf("expected nil when CDI spec at /var/run exists, got: %v", err)
	}
}

func TestVerifyPodmanGPUSupport_OCIHookExists(t *testing.T) {
	withMockFileExists(t, func(path string) bool {
		return path == "/usr/share/containers/oci/hooks.d/oci-nvidia-hook.json"
	})
	withMockLookPath(t, lookPathNotFound)

	if err := verifyPodmanGPUSupport(); err != nil {
		t.Errorf("expected nil when OCI hook exists, got: %v", err)
	}
}

func TestVerifyPodmanGPUSupport_NvidiaCTKAvailable(t *testing.T) {
	withMockFileExists(t, noFilesExist)
	withMockLookPath(t, func(name string) (string, error) {
		if name == "nvidia-ctk" {
			return "/usr/bin/nvidia-ctk", nil
		}
		return "", fmt.Errorf("executable not found: %s", name)
	})

	err := verifyPodmanGPUSupport()
	if err == nil {
		t.Fatal("expected error when nvidia-ctk available but CDI not generated")
	}
	if !strings.Contains(err.Error(), "CDI spec not generated") {
		t.Errorf("expected CDI generation guidance, got: %v", err)
	}
	if !strings.Contains(err.Error(), "nvidia-ctk cdi generate") {
		t.Errorf("expected nvidia-ctk command in error, got: %v", err)
	}
}

func TestVerifyPodmanGPUSupport_NothingInstalled(t *testing.T) {
	withMockFileExists(t, noFilesExist)
	withMockLookPath(t, lookPathNotFound)

	err := verifyPodmanGPUSupport()
	if err == nil {
		t.Fatal("expected error when nothing installed")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("expected install guidance, got: %v", err)
	}
	if !strings.Contains(err.Error(), "docs.nvidia.com") {
		t.Errorf("expected install URL, got: %v", err)
	}
}

func TestVerifyPodmanGPUSupport_UserLocalOCIHook(t *testing.T) {
	withMockFileExists(t, func(path string) bool {
		// Match the user-local hook path (filepath.Join uses OS separators).
		return strings.Contains(path, "oci-nvidia-hook.json") &&
			strings.Contains(path, ".config")
	})
	withMockLookPath(t, lookPathNotFound)

	if err := verifyPodmanGPUSupport(); err != nil {
		t.Errorf("expected nil when user-local OCI hook exists, got: %v", err)
	}
}

func TestVerifyPodmanGPUSupport_PublicGOOSGate(t *testing.T) {
	// On non-Linux (this machine is Windows), the public function returns nil immediately.
	// On Linux, it delegates to verifyPodmanGPUSupport().
	err := VerifyPodmanGPUSupport()
	if goruntime.GOOS != "linux" {
		if err != nil {
			t.Errorf("expected nil on non-Linux platform, got: %v", err)
		}
	}
}

func TestEnsurePodmanGPUReady_LogsWarning(t *testing.T) {
	withMockFileExists(t, noFilesExist)
	withMockLookPath(t, lookPathNotFound)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	EnsurePodmanGPUReady(logger)

	// On non-Linux, VerifyPodmanGPUSupport returns nil → info log (not captured at Warn level).
	// The internal function would produce a warning.
	if goruntime.GOOS == "linux" {
		if !strings.Contains(buf.String(), "GPU container support may not work") {
			t.Errorf("expected warning log, got: %s", buf.String())
		}
	}
}

func TestEnsurePodmanGPUReady_LogsSuccess(t *testing.T) {
	withMockFileExists(t, func(path string) bool {
		return path == "/etc/cdi/nvidia.yaml"
	})
	withMockLookPath(t, lookPathNotFound)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	EnsurePodmanGPUReady(logger)

	// On non-Linux, VerifyPodmanGPUSupport returns nil → info log.
	if !strings.Contains(buf.String(), "NVIDIA Container Toolkit configured") {
		t.Errorf("expected success info log, got: %s", buf.String())
	}
}
