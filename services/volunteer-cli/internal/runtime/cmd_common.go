package runtime

import (
	"os"
	"strings"
	"time"
)

// SkipHardwareDetectionEnv, when truthy ("1"/"true"/"yes"/"on"), bypasses
// every platform detection call (CPU model, memory, disk, GPUs, thermal).
// Tests set this in TestMain so `go test ./...` cannot trigger DiskPart UAC
// prompts or vendor-CLI hangs on Windows. The client package's
// DetectHardware and this package's DetectGPUs both honor it.
const SkipHardwareDetectionEnv = "LETTUCE_SKIP_HARDWARE_DETECTION"

// SkipHardwareDetection reports whether the env var is set to a truthy value.
func SkipHardwareDetection() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(SkipHardwareDetectionEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// DefaultCommandTimeout is the maximum time any single external command can run
// when invoked via CommandExecutor. It is intentionally generous because some
// callers (e.g. `podman machine start/init`) legitimately take tens of seconds.
const DefaultCommandTimeout = 30 * time.Second

// DetectionCommandTimeout is the per-command upper bound used by hardware/GPU
// detection paths. Detection CLIs (nvidia-smi, rocm-smi, amd-smi, wmic,
// system_profiler, sysctl, ...) should respond in well under a second on a
// healthy host. Anything beyond a few seconds is almost certainly a hung tool
// (e.g. amd-smi probing a missing driver), and we'd rather degrade to "no GPU"
// than block volunteer registration.
const DetectionCommandTimeout = 5 * time.Second

// DetectHardwareTimeout caps the total wall time spent in DetectHardware,
// regardless of how many sub-detections are in flight. Sub-detections run in
// parallel and each has its own per-command timeout, but this is the hard
// ceiling so a pathological host can never block Register past it.
const DetectHardwareTimeout = 10 * time.Second
