package runtime

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// TestMain sets up safety nets that prevent any real system commands from
// executing during tests. This blocks tools like nvidia-smi, rocm-smi,
// powershell, and any other external command from actually running.
//
// Individual tests that need specific mock responses use withMockExecutor()
// which temporarily overrides CommandExecutor and restores this blocker
// when the test finishes.
func TestMain(m *testing.M) {
	// Default-on: no test should ever hit real platform detection. Individual
	// tests that exercise DetectGPUs against mocked executors clear this via
	// the withMockExecutor helper.
	os.Setenv(SkipHardwareDetectionEnv, "1")

	CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("BLOCKED: test tried to execute real command %q (use withMockExecutor to mock)", name)
	}
	CommandExecutorCtx = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("BLOCKED: test tried to execute real command %q (use withMockExecutorCtx or withMockExecutor to mock)", name)
	}

	CPUTempReader = func() int { return 0 }

	// Default to "no podman in a standard install location" so detection tests
	// don't accidentally find a podman that's actually installed on the host
	// running the suite. Individual tests override this to exercise the fallback.
	podmanInstallPathFunc = func() string { return "" }

	os.Exit(m.Run())
}
