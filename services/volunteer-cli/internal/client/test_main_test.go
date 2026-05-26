package client

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TestMain is the safety net that prevents ANY client test from triggering
// real platform hardware detection. Two layers of defense:
//
//  1. LETTUCE_SKIP_HARDWARE_DETECTION=1 is set process-wide. Any code path
//     that goes through DetectHardware or runtime.DetectGPUs short-circuits
//     to zero/empty values without spawning a vendor CLI or reading the
//     registry.
//  2. The package-level detect{CPUModel,TotalMemoryMB,DiskAvailableMB} vars
//     are pointed at stubs, and runtime.CommandExecutor[Ctx] are pointed at
//     blockers, so a test that explicitly bypasses the env-var skip still
//     cannot fire real detection.
//
// Individual tests that exercise specific behaviors (e.g. validating that
// the aggregate timeout fires) restore detection locally via withMockHardware/
// the per-test CommandExecutorCtx override.
//
// This guards against the Windows DiskPart-UAC prompts that wmic-based
// detection used to surface during `go test ./...` runs.
func TestMain(m *testing.M) {
	os.Setenv(runtime.SkipHardwareDetectionEnv, "1")

	detectCPUModel = func() string { return "unknown" }
	detectTotalMemoryMB = func() int32 { return 0 }
	detectDiskAvailableMB = func(string) int64 { return 0 }

	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("BLOCKED: client test tried to execute real command %q (use withMockHardware to mock)", name)
	}
	runtime.CommandExecutorCtx = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("BLOCKED: client test tried to execute real command %q (use withMockHardware to mock)", name)
	}

	os.Exit(m.Run())
}
