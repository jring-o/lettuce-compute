package procmetrics

import (
	"fmt"
	"os"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

func TestMain(m *testing.M) {
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("BLOCKED: test tried to execute real command %q", name)
	}
	runtime.CPUTempReader = func() int { return 0 }
	os.Exit(m.Run())
}
