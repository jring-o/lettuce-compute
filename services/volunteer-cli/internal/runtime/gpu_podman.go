package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
)

// cdiSpecPaths are standard locations for the NVIDIA CDI specification.
var cdiSpecPaths = []string{
	"/etc/cdi/nvidia.yaml",
	"/var/run/cdi/nvidia.yaml",
}

// ociHookPaths are standard locations for the NVIDIA OCI hook.
var ociHookPaths = []string{
	"/usr/share/containers/oci/hooks.d/oci-nvidia-hook.json",
}

// nvidiaCDIAvailable reports whether an NVIDIA CDI spec is present on the host.
// When present, GPUs can be requested via CDI device names (Driver "cdi"), which
// the NVIDIA Container Toolkit (>=1.17, default CDI/auto mode) honors directly —
// without the nvidia runtime being configured as the default in legacy mode.
// This lets GPU volunteers run on a stock host with no Docker-runtime reconfig.
func nvidiaCDIAvailable() bool {
	for _, p := range cdiSpecPaths {
		if fileExists(p) {
			return true
		}
	}
	return false
}

// VerifyPodmanGPUSupport checks if NVIDIA Container Toolkit is configured for Podman.
// Checks for CDI spec at standard locations, then falls back to OCI hook check.
// Returns nil if GPU support is available, error with guidance if not.
// Only meaningful on Linux — returns nil on other platforms (GPU passthrough deferred).
func VerifyPodmanGPUSupport() error {
	if goruntime.GOOS != "linux" {
		return nil
	}
	return verifyPodmanGPUSupport()
}

// verifyPodmanGPUSupport contains the platform-independent logic. Exported tests
// call this directly with mocked fileExists/lookPathFunc to verify behavior on
// any OS.
func verifyPodmanGPUSupport() error {
	// Check 1: CDI spec exists.
	for _, p := range cdiSpecPaths {
		if fileExists(p) {
			return nil
		}
	}

	// Check 2: OCI hook exists (system-wide or user-local).
	for _, p := range ociHookPaths {
		if fileExists(p) {
			return nil
		}
	}
	homeHookDir := filepath.Join(os.Getenv("HOME"), ".config/containers/oci/hooks.d/oci-nvidia-hook.json")
	if fileExists(homeHookDir) {
		return nil
	}

	// Check 3: nvidia-ctk available (can generate CDI spec).
	if _, err := lookPathFunc("nvidia-ctk"); err == nil {
		return fmt.Errorf("NVIDIA Container Toolkit installed but CDI spec not generated. Run: sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml")
	}

	return fmt.Errorf("NVIDIA Container Toolkit not installed. Install it for GPU container support: https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html")
}

// EnsurePodmanGPUReady is called during daemon startup when backend is Podman.
// Logs warnings if GPU support is misconfigured.
func EnsurePodmanGPUReady(logger *slog.Logger) {
	if err := VerifyPodmanGPUSupport(); err != nil {
		logger.Warn("GPU container support may not work through Podman",
			"error", err,
			"hint", "GPU leafs will fail. Fix the issue or use Docker for GPU projects.")
	} else {
		logger.Info("NVIDIA Container Toolkit configured for Podman (CDI or OCI hooks)")
	}
}

// fileExists is a helper that checks if a file exists. Overridable for testing.
var fileExists = func(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
