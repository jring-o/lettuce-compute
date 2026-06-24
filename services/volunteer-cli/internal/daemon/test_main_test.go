package daemon

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// spinChildEnv, when set in the environment, turns a re-exec of the test binary
// into a long-lived, suspendable child process. It writes a monotonically
// increasing counter to the named file roughly every 10ms, so a parent test can
// (a) confirm the child is alive and progressing, and (b) confirm OS-level
// suspension by observing the counter stop advancing. Used by the resumed-orphan
// suspension reproduction in slot_orphan_suspend_test.go.
const spinChildEnv = "LETTUCE_TEST_SPIN_FILE"

func runSpinChild(path string) {
	for n := 1; ; n++ {
		_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0644)
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMain blocks real system commands that the daemon indirectly triggers
// through the thermal monitor (CPUTempReader) and GPU metrics (CommandExecutor).
// See runtime/test_main_test.go for the same pattern.
func TestMain(m *testing.M) {
	// Re-exec hook: act as the spin child instead of running the suite when asked.
	if spinFile := os.Getenv(spinChildEnv); spinFile != "" {
		runSpinChild(spinFile)
		os.Exit(0)
	}

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
