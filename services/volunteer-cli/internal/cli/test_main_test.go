package cli

import (
	"fmt"
	"os"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TestMain blocks real system commands to prevent UAC prompts and system dialogs.
// See CLAUDE.md test safety rules.
func TestMain(m *testing.M) {
	os.Setenv(runtime.SkipHardwareDetectionEnv, "1")

	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("BLOCKED: cli test tried to execute real command %q", name)
	}

	runtime.CPUTempReader = func() int { return 0 }

	os.Exit(m.Run())
}
