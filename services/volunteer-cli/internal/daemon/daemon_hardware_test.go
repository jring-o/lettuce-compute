package daemon

import (
	"log/slog"
	"os"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// The daemon adopts the hardware start-up already detected instead of
// probing the machine a second time. HasGPU and the GPU budget read from
// that same object, so the advertised capabilities and the local
// eligibility checks can never disagree.
func TestNewDaemon_ReusesDetectedHardware(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.ResourceLimits.MaxGPUVRAMPct = 70

	hw := &lettucev1.HardwareCapabilities{
		CpuModel: "cached-cpu",
		Gpus: []*lettucev1.GpuInfo{
			{Model: "AMD Radeon RX 7800 XT", Vendor: "amd", VramMb: 16384, MaxVramPct: 70},
		},
	}
	d := NewDaemon(DaemonConfig{Config: cfg, Logger: logger, Hardware: hw})

	if d.cachedHW != hw {
		t.Fatalf("daemon cachedHW = %p, want the exact object passed in (%p); a second detection ran", d.cachedHW, hw)
	}
	if !d.HasGPU() {
		t.Error("HasGPU = false with a GPU in the supplied hardware")
	}
	vramMB, cardVRAMMB, pct, vendors, _ := d.GPUBudget()
	if vramMB != 16384*70/100 || cardVRAMMB != 16384 || pct != 70 || len(vendors) != 1 || vendors[0] != "AMD" {
		t.Errorf("GPUBudget = (%d, %d, %d, %v), want the budget of the supplied card", vramMB, cardVRAMMB, pct, vendors)
	}
}

// Without supplied hardware the daemon still detects for itself (the test
// process has detection disabled, so the result is the placeholder set).
func TestNewDaemon_DetectsWhenNoHardwareSupplied(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()

	d := NewDaemon(DaemonConfig{Config: cfg, Logger: logger})
	if d.cachedHW == nil {
		t.Fatal("cachedHW is nil; the daemon must detect hardware when none is supplied")
	}
}
