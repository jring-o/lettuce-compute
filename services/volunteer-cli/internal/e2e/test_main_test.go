package e2e

import (
	"fmt"
	"os"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TestMain blocks all real command execution in E2E tests.
// Individual tests mock CommandExecutor as needed via withMockExecutor.
func TestMain(m *testing.M) {
	os.Setenv(runtime.SkipHardwareDetectionEnv, "1")
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("BLOCKED: e2e test tried to execute real command %q (use withMockExecutor to mock)", name)
	}
	runtime.CPUTempReader = func() int { return 0 }
	os.Exit(m.Run())
}
