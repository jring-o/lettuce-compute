package client

import (
	"runtime"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TB-85 regression test, advertisement half: the advertisement registration
// sends carries the configured limits as host_max_memory_mb /
// host_max_cpu_cores — the budgets a head compares a native or WASM leaf with
// — beside max_memory_mb / max_cpu_cores, which start-up clips to the
// container engine's VM. Each host figure is capped at the machine's detected
// total, since a head refuses a registration claiming more than the machine
// has. Pre-fix the fields did not exist and the head saw only the clipped
// figures.
func TestTB85_AdvertisementCarriesTheHostBudgets(t *testing.T) {
	withMockHardware(t)
	cfg := config.Defaults()
	cfg.ResourceLimits.MaxCPUCores = 1
	cfg.ResourceLimits.MaxMemoryMB = 8192
	hw := DetectHardware(cfg)
	if hw.HostMaxMemoryMb != 8192 || hw.HostMaxCpuCores != 1 {
		t.Errorf("host budgets = %d MB / %d cores, want the configured 8192 / 1", hw.HostMaxMemoryMb, hw.HostMaxCpuCores)
	}

	// A limit above the machine is advertised as the machine.
	cfg.ResourceLimits.MaxMemoryMB = 32768
	cfg.ResourceLimits.MaxCPUCores = runtime.NumCPU() + 4
	hw = DetectHardware(cfg)
	if hw.HostMaxMemoryMb != hw.MemoryTotalMb || hw.MemoryTotalMb != 16384 {
		t.Errorf("host_max_memory_mb = %d with 16384 MB detected, want it capped at the total", hw.HostMaxMemoryMb)
	}
	if hw.HostMaxCpuCores != int32(runtime.NumCPU()) {
		t.Errorf("host_max_cpu_cores = %d with %d CPUs, want it capped at the count", hw.HostMaxCpuCores, runtime.NumCPU())
	}
}
