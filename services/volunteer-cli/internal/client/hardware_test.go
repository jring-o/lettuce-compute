package client

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	gpudetect "github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

func TestDetectHardwareCPUCores(t *testing.T) {
	withMockHardware(t)
	cfg := config.Defaults()
	hw := DetectHardware(cfg)

	if hw.CpuCores != int32(runtime.NumCPU()) {
		t.Errorf("CpuCores = %d, want %d", hw.CpuCores, runtime.NumCPU())
	}
}

func TestDetectHardwareCPUModel(t *testing.T) {
	withMockHardware(t)
	cfg := config.Defaults()
	hw := DetectHardware(cfg)

	if hw.CpuModel != "Mock CPU" {
		t.Errorf("CpuModel = %q, want %q", hw.CpuModel, "Mock CPU")
	}
}

func TestDetectHardwareMemory(t *testing.T) {
	withMockHardware(t)
	cfg := config.Defaults()
	hw := DetectHardware(cfg)

	if hw.MemoryTotalMb != 16384 {
		t.Errorf("MemoryTotalMb = %d, want 16384", hw.MemoryTotalMb)
	}
}

func TestDetectHardwareConfigLimits(t *testing.T) {
	withMockHardware(t)
	cfg := config.Defaults()
	cfg.ResourceLimits.MaxCPUCores = 4
	cfg.ResourceLimits.MaxMemoryMB = 8192
	cfg.ResourceLimits.MaxDiskGB = 50
	cfg.ResourceLimits.MaxBandwidthMbps = 100

	hw := DetectHardware(cfg)

	if hw.MaxCpuCores != 4 {
		t.Errorf("MaxCpuCores = %d, want 4", hw.MaxCpuCores)
	}
	if hw.MaxMemoryMb != 8192 {
		t.Errorf("MaxMemoryMb = %d, want 8192", hw.MaxMemoryMb)
	}
	if hw.MaxDiskMb != 50*1024 {
		t.Errorf("MaxDiskMb = %d, want %d", hw.MaxDiskMb, 50*1024)
	}
	if hw.MaxBandwidthMbps != 100 {
		t.Errorf("MaxBandwidthMbps = %d, want 100", hw.MaxBandwidthMbps)
	}
}

// disableSkipHardwareDetection clears the global skip env-var for the duration
// of a test that needs DetectHardware/DetectGPUs to walk the full mocked path.
// TestMain sets the env var as a safety net; tests that mock the detect vars
// must clear it so the real DetectHardware code runs against those mocks.
func disableSkipHardwareDetection(t *testing.T) {
	t.Helper()
	prev, had := os.LookupEnv(gpudetect.SkipHardwareDetectionEnv)
	os.Unsetenv(gpudetect.SkipHardwareDetectionEnv)
	t.Cleanup(func() {
		if had {
			os.Setenv(gpudetect.SkipHardwareDetectionEnv, prev)
		} else {
			os.Unsetenv(gpudetect.SkipHardwareDetectionEnv)
		}
	})
}

// withMockHardware mocks all platform detection (CPU, memory, disk, GPU)
// so tests never execute real system commands.
func withMockHardware(t *testing.T) {
	t.Helper()
	disableSkipHardwareDetection(t)
	origCPU := detectCPUModel
	origMem := detectTotalMemoryMB
	origDisk := detectDiskAvailableMB
	origGPU := gpudetect.CommandExecutor
	origGPUCtx := gpudetect.CommandExecutorCtx
	t.Cleanup(func() {
		detectCPUModel = origCPU
		detectTotalMemoryMB = origMem
		detectDiskAvailableMB = origDisk
		gpudetect.CommandExecutor = origGPU
		gpudetect.CommandExecutorCtx = origGPUCtx
	})
	detectCPUModel = func() string { return "Mock CPU" }
	detectTotalMemoryMB = func() int32 { return 16384 }
	detectDiskAvailableMB = func(path string) int64 { return 500000 }
	gpudetect.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return nil, exec.ErrNotFound
	}
	gpudetect.CommandExecutorCtx = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return nil, exec.ErrNotFound
	}
}

// withMockGPUs mocks all platform detection and provides 2 NVIDIA GPUs.
func withMockGPUs(t *testing.T) {
	t.Helper()
	disableSkipHardwareDetection(t)
	origCPU := detectCPUModel
	origMem := detectTotalMemoryMB
	origDisk := detectDiskAvailableMB
	origGPU := gpudetect.CommandExecutor
	origGPUCtx := gpudetect.CommandExecutorCtx
	t.Cleanup(func() {
		detectCPUModel = origCPU
		detectTotalMemoryMB = origMem
		detectDiskAvailableMB = origDisk
		gpudetect.CommandExecutor = origGPU
		gpudetect.CommandExecutorCtx = origGPUCtx
	})
	detectCPUModel = func() string { return "Mock CPU" }
	detectTotalMemoryMB = func() int32 { return 16384 }
	detectDiskAvailableMB = func(path string) int64 { return 500000 }
	gpuMock := func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			return []byte("NVIDIA GeForce RTX 3080, 10240, 8.6\nNVIDIA GeForce RTX 3070, 8192, 8.6\n"), nil
		}
		return nil, exec.ErrNotFound
	}
	gpudetect.CommandExecutor = gpuMock
	gpudetect.CommandExecutorCtx = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return gpuMock(name, args...)
	}
}

func TestDetectHardwareGPUsNoGPUs(t *testing.T) {
	withMockHardware(t)
	cfg := config.Defaults()
	hw := DetectHardware(cfg)

	if len(hw.Gpus) != 0 {
		t.Errorf("Gpus length = %d, want 0", len(hw.Gpus))
	}
}

func TestDetectHardwareGPUsDetected(t *testing.T) {
	withMockGPUs(t)
	cfg := config.Defaults()
	hw := DetectHardware(cfg)

	if len(hw.Gpus) != 2 {
		t.Fatalf("Gpus length = %d, want 2", len(hw.Gpus))
	}
	if hw.Gpus[0].Model != "NVIDIA GeForce RTX 3080" {
		t.Errorf("GPU 0 Model = %q, want %q", hw.Gpus[0].Model, "NVIDIA GeForce RTX 3080")
	}
	if hw.Gpus[0].MaxVramPct != 50 {
		t.Errorf("GPU 0 MaxVramPct = %d, want 50 (default)", hw.Gpus[0].MaxVramPct)
	}
}

func TestDetectHardwareGPUVRAMPctZeroDisablesGPU(t *testing.T) {
	withMockGPUs(t)
	cfg := config.Defaults()
	cfg.ResourceLimits.MaxGPUVRAMPct = 0
	hw := DetectHardware(cfg)

	if len(hw.Gpus) != 0 {
		t.Errorf("Gpus length = %d, want 0 when MaxGPUVRAMPct=0", len(hw.Gpus))
	}
}

func TestDetectHardwareGPUGlobalVRAMPct(t *testing.T) {
	withMockGPUs(t)
	cfg := config.Defaults()
	cfg.ResourceLimits.MaxGPUVRAMPct = 75
	hw := DetectHardware(cfg)

	if len(hw.Gpus) != 2 {
		t.Fatalf("Gpus length = %d, want 2", len(hw.Gpus))
	}
	for i, g := range hw.Gpus {
		if g.MaxVramPct != 75 {
			t.Errorf("GPU %d MaxVramPct = %d, want 75", i, g.MaxVramPct)
		}
	}
}

func TestDetectHardwareGPUPerGPUOverride(t *testing.T) {
	withMockGPUs(t)
	cfg := config.Defaults()
	cfg.ResourceLimits.MaxGPUVRAMPct = 50
	cfg.GPUOverrides = []config.GPUOverride{
		{Index: 1, MaxVRAMPct: 80},
	}
	hw := DetectHardware(cfg)

	if len(hw.Gpus) != 2 {
		t.Fatalf("Gpus length = %d, want 2", len(hw.Gpus))
	}
	if hw.Gpus[0].MaxVramPct != 50 {
		t.Errorf("GPU 0 MaxVramPct = %d, want 50 (global default)", hw.Gpus[0].MaxVramPct)
	}
	if hw.Gpus[1].MaxVramPct != 80 {
		t.Errorf("GPU 1 MaxVramPct = %d, want 80 (override)", hw.Gpus[1].MaxVramPct)
	}
}

func TestDetectHardwareGPUDisabledOverride(t *testing.T) {
	withMockGPUs(t)
	cfg := config.Defaults()
	cfg.GPUOverrides = []config.GPUOverride{
		{Index: 0, Disabled: true},
	}
	hw := DetectHardware(cfg)

	if len(hw.Gpus) != 1 {
		t.Fatalf("Gpus length = %d, want 1 (GPU 0 disabled)", len(hw.Gpus))
	}
	if hw.Gpus[0].Model != "NVIDIA GeForce RTX 3070" {
		t.Errorf("remaining GPU Model = %q, want RTX 3070", hw.Gpus[0].Model)
	}
}

func TestDetectHardwareDiskAvailable(t *testing.T) {
	withMockHardware(t)
	cfg := config.Defaults()
	hw := DetectHardware(cfg)

	if hw.DiskAvailableMb != 500000 {
		t.Errorf("DiskAvailableMb = %d, want 500000", hw.DiskAvailableMb)
	}
}

func TestDetectHardwareZeroConfigLimits(t *testing.T) {
	withMockHardware(t)
	cfg := config.Defaults()
	cfg.ResourceLimits.MaxCPUCores = 0
	cfg.ResourceLimits.MaxMemoryMB = 0
	cfg.ResourceLimits.MaxDiskGB = 0
	cfg.ResourceLimits.MaxBandwidthMbps = 0

	hw := DetectHardware(cfg)

	if hw.MaxCpuCores != 0 {
		t.Errorf("MaxCpuCores = %d, want 0", hw.MaxCpuCores)
	}
	if hw.MaxMemoryMb != 0 {
		t.Errorf("MaxMemoryMb = %d, want 0", hw.MaxMemoryMb)
	}
	if hw.MaxDiskMb != 0 {
		t.Errorf("MaxDiskMb = %d, want 0", hw.MaxDiskMb)
	}
	if hw.MaxBandwidthMbps != 0 {
		t.Errorf("MaxBandwidthMbps = %d, want 0", hw.MaxBandwidthMbps)
	}
}

func TestDetectHardwareDiskGBToMBConversion(t *testing.T) {
	withMockHardware(t)
	cfg := config.Defaults()
	cfg.ResourceLimits.MaxDiskGB = 1

	hw := DetectHardware(cfg)

	if hw.MaxDiskMb != 1024 {
		t.Errorf("MaxDiskMb = %d, want 1024 (1 GB * 1024)", hw.MaxDiskMb)
	}
}

func TestDetectHardwareDiskEmptyPath(t *testing.T) {
	withMockHardware(t)
	cfg := config.Defaults()
	cfg.DataDir = ""
	hw := DetectHardware(cfg)

	// Mock always returns 500000 regardless of path.
	if hw.DiskAvailableMb != 500000 {
		t.Errorf("DiskAvailableMb = %d, want 500000", hw.DiskAvailableMb)
	}
}

// TestDetectHardwareGPUSlowExecutorTimesOut simulates a hung vendor CLI (e.g.
// the ~3min amd-smi hang observed on this Windows host) and verifies that:
//
//  1. GPU detection bails out via the per-command timeout instead of waiting
//     for the fake command to return.
//  2. DetectHardware returns within the aggregate cap, not the sum of every
//     per-vendor timeout.
//  3. The GPU list degrades gracefully to empty instead of failing the flow.
//
// The hung command sleeps far longer than DetectHardwareTimeout; we expect the
// call to return shortly after the aggregate cap fires.
func TestDetectHardwareGPUSlowExecutorTimesOut(t *testing.T) {
	withMockHardware(t)

	hangFor := 30 * time.Second // far beyond DetectHardwareTimeout
	var calls int32

	origCtx := gpudetect.CommandExecutorCtx
	orig := gpudetect.CommandExecutor
	t.Cleanup(func() {
		gpudetect.CommandExecutorCtx = origCtx
		gpudetect.CommandExecutor = orig
	})
	gpudetect.CommandExecutorCtx = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(hangFor):
			return nil, nil
		}
	}
	// Catch any stray callers that don't pass a context — they would otherwise
	// hang for the full duration.
	gpudetect.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return nil, exec.ErrNotFound
	}

	cfg := config.Defaults() // MaxGPUVRAMPct > 0 so GPU detection runs
	// Sanity: this test is meaningful only while the cap is small enough that
	// it would clearly fail the old (serialized, untimed) code path.
	cap := gpudetect.DetectHardwareTimeout
	margin := 3 * time.Second

	start := time.Now()
	hw := DetectHardware(cfg)
	elapsed := time.Since(start)

	if elapsed > cap+margin {
		t.Fatalf("DetectHardware took %v, want <= %v (cap %v + margin %v)", elapsed, cap+margin, cap, margin)
	}
	if got := atomic.LoadInt32(&calls); got == 0 {
		t.Fatal("expected at least one detection call to be attempted")
	}
	if len(hw.Gpus) != 0 {
		t.Errorf("Gpus = %d, want 0 (timed-out detection should degrade to empty)", len(hw.Gpus))
	}
}

// TestDetectHardwareCPUSlowDegrades verifies that a hung CPU-model detector
// does NOT block the whole DetectHardware call — the field falls back to its
// zero value while the other sub-detections complete normally.
func TestDetectHardwareCPUSlowDegrades(t *testing.T) {
	// Don't use withMockHardware — we want a slow CPU detector specifically.
	disableSkipHardwareDetection(t)
	origCPU := detectCPUModel
	origMem := detectTotalMemoryMB
	origDisk := detectDiskAvailableMB
	origGPU := gpudetect.CommandExecutor
	origGPUCtx := gpudetect.CommandExecutorCtx
	t.Cleanup(func() {
		detectCPUModel = origCPU
		detectTotalMemoryMB = origMem
		detectDiskAvailableMB = origDisk
		gpudetect.CommandExecutor = origGPU
		gpudetect.CommandExecutorCtx = origGPUCtx
	})

	detectCPUModel = func() string {
		time.Sleep(30 * time.Second) // far beyond DetectHardwareTimeout
		return "should never appear"
	}
	detectTotalMemoryMB = func() int32 { return 16384 }
	detectDiskAvailableMB = func(path string) int64 { return 500000 }
	gpudetect.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return nil, exec.ErrNotFound
	}
	gpudetect.CommandExecutorCtx = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return nil, exec.ErrNotFound
	}

	cfg := config.Defaults()
	cap := gpudetect.DetectHardwareTimeout
	margin := 3 * time.Second

	start := time.Now()
	hw := DetectHardware(cfg)
	elapsed := time.Since(start)

	if elapsed > cap+margin {
		t.Fatalf("DetectHardware took %v, want <= %v (cap %v + margin %v)", elapsed, cap+margin, cap, margin)
	}
	// CpuModel raced past the cap → should be the zero value ("").
	if hw.CpuModel == "should never appear" {
		t.Errorf("CpuModel = %q, expected slow detector to be abandoned", hw.CpuModel)
	}
	// Other fields completed promptly and should be populated normally.
	if hw.MemoryTotalMb != 16384 {
		t.Errorf("MemoryTotalMb = %d, want 16384 (fast detector should have completed)", hw.MemoryTotalMb)
	}
	if hw.DiskAvailableMb != 500000 {
		t.Errorf("DiskAvailableMb = %d, want 500000 (fast detector should have completed)", hw.DiskAvailableMb)
	}
}
