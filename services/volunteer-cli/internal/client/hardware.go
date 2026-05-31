package client

import (
	"log/slog"
	"runtime"
	"sync"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	gpudetect "github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// SkipHardwareDetectionEnv re-exports the runtime package's env var name so
// callers that only depend on this package can set it without an extra
// import. The actual check lives in runtime.SkipHardwareDetection() so
// DetectHardware and DetectGPUs share one source of truth.
const SkipHardwareDetectionEnv = gpudetect.SkipHardwareDetectionEnv

// Platform detection functions — overridable for testing.
// Defaults are set to platform-specific implementations in hardware_{linux,darwin,windows}.go.
var (
	detectCPUModel      = defaultDetectCPUModel
	detectTotalMemoryMB = defaultDetectTotalMemoryMB
	detectDiskAvailableMB = defaultDetectDiskAvailableMB
)

// DiskAvailableMB returns the disk space (in MB) available to the current user
// on the filesystem that contains path, or 0 if it can't be determined. It
// reuses the same platform detection as hardware registration, so callers (the
// daemon's disk gate, the startup readiness banner) can report real free space
// without duplicating the per-OS syscalls.
func DiskAvailableMB(path string) int64 {
	return detectDiskAvailableMB(path)
}

// DetectHardware detects system hardware and builds a HardwareCapabilities proto.
// Config-specified limits override detected hardware maximums.
// Platform-specific detection is in hardware_{linux,darwin,windows}.go.
//
// Sub-detections (CPU model, memory, disk, GPUs) run in parallel and the total
// wall time is capped at gpudetect.DetectHardwareTimeout. Any sub-detection
// that hangs past the cap is treated as "unavailable" and the corresponding
// field falls back to its zero value (e.g. "unknown" CPU model, 0 MB memory,
// no GPUs). This is required so that quirky vendor CLIs (the canonical
// offender: amd-smi taking minutes on hosts without a working ROCm driver)
// cannot block volunteer Register past its RPC deadline.
func DetectHardware(cfg *config.Config) *lettucev1.HardwareCapabilities {
	if gpudetect.SkipHardwareDetection() {
		return &lettucev1.HardwareCapabilities{
			CpuCores:         int32(runtime.NumCPU()),
			CpuModel:         "unknown",
			MaxCpuCores:      int32(cfg.ResourceLimits.MaxCPUCores),
			MaxMemoryMb:      int32(cfg.ResourceLimits.MaxMemoryMB),
			MaxDiskMb:        int64(cfg.ResourceLimits.MaxDiskGB) * 1024,
			MaxBandwidthMbps: int32(cfg.ResourceLimits.MaxBandwidthMbps),
			Gpus:             []*lettucev1.GpuInfo{},
		}
	}

	var (
		cpuModel string
		memMB    int32
		diskMB   int64
		gpus     []*lettucev1.GpuInfo
	)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); cpuModel = runWithFallback("cpu_model", detectCPUModel, "unknown") }()
	go func() { defer wg.Done(); memMB = runWithFallback("memory_mb", detectTotalMemoryMB, int32(0)) }()
	go func() {
		defer wg.Done()
		diskMB = runWithFallback("disk_mb", func() int64 { return detectDiskAvailableMB(cfg.DataDir) }, int64(0))
	}()
	go func() {
		defer wg.Done()
		gpus = runWithFallback("gpus", func() []*lettucev1.GpuInfo { return detectAndApplyGPUConfig(cfg) }, []*lettucev1.GpuInfo{})
	}()

	// Wait with an overall ceiling — individual sub-detections each have their
	// own per-command timeout, but this is the hard outer wall.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(gpudetect.DetectHardwareTimeout):
		slog.Warn("DetectHardware exceeded aggregate timeout, returning partial results",
			"timeout", gpudetect.DetectHardwareTimeout)
	}

	if gpus == nil {
		gpus = []*lettucev1.GpuInfo{}
	}

	return &lettucev1.HardwareCapabilities{
		CpuCores:         int32(runtime.NumCPU()),
		CpuModel:         cpuModel,
		MaxCpuCores:      int32(cfg.ResourceLimits.MaxCPUCores),
		MemoryTotalMb:    memMB,
		MaxMemoryMb:      int32(cfg.ResourceLimits.MaxMemoryMB),
		DiskAvailableMb:  diskMB,
		MaxDiskMb:        int64(cfg.ResourceLimits.MaxDiskGB) * 1024,
		MaxBandwidthMbps: int32(cfg.ResourceLimits.MaxBandwidthMbps),
		Gpus:             gpus,
	}
}

// runWithFallback invokes fn and recovers from any panic, returning fallback
// instead. Sub-detections call into platform CLIs and DLLs, and we never want
// a misbehaving tool to take down the whole registration flow.
func runWithFallback[T any](label string, fn func() T, fallback T) (out T) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("hardware detection panicked, using fallback", "step", label, "panic", r)
			out = fallback
		}
	}()
	return fn()
}

// detectAndApplyGPUConfig detects GPUs and applies config limits.
func detectAndApplyGPUConfig(cfg *config.Config) []*lettucev1.GpuInfo {
	if cfg.ResourceLimits.MaxGPUVRAMPct == 0 {
		return []*lettucev1.GpuInfo{}
	}

	detected := gpudetect.DetectGPUs()
	var gpus []*lettucev1.GpuInfo

	for i, g := range detected {
		maxVRAMPct := cfg.ResourceLimits.MaxGPUVRAMPct
		disabled := false

		for _, ov := range cfg.GPUOverrides {
			if ov.Index == i {
				if ov.Disabled {
					disabled = true
				} else if ov.MaxVRAMPct > 0 {
					maxVRAMPct = ov.MaxVRAMPct
				}
				break
			}
		}

		if disabled {
			continue
		}

		gpus = append(gpus, &lettucev1.GpuInfo{
			Model:             g.Model,
			Vendor:            g.Vendor,
			VramMb:            g.VRAMMB,
			MaxVramPct:        int32(maxVRAMPct),
			ComputeCapability: g.ComputeCapability,
		})
	}

	if gpus == nil {
		return []*lettucev1.GpuInfo{}
	}
	return gpus
}
