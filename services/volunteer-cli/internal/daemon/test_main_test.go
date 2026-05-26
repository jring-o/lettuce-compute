package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TestMain blocks real system commands that the daemon indirectly triggers
// through the thermal monitor (CPUTempReader) and GPU metrics (CommandExecutor).
// See runtime/test_main_test.go for the same pattern.
func TestMain(m *testing.M) {
	os.Setenv(runtime.SkipHardwareDetectionEnv, "1")

	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("BLOCKED: daemon test tried to execute real command %q", name)
	}

	runtime.CPUTempReader = func() int { return 0 }

	// Neutralize host RAM probing so admission tests are deterministic regardless
	// of the machine they run on. Tests that exercise the free-RAM path override
	// freeSystemMemoryMB locally.
	freeSystemMemoryMB = func() (int, bool) { return 0, false }

	os.Exit(m.Run())
}
