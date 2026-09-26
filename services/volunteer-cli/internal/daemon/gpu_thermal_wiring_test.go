package daemon

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// The GPU pause threshold acts on a machine whose GPU the daemon detected.
// The daemon used to build its thermal monitor with no GPU source at all, so a
// card at any temperature never paused work, while the config file, the app,
// doctor and the guide all said it would. Here nvidia-smi reports the detected
// card at 85 °C against an 80 °C pause threshold, and the CPU cannot be read,
// so only the GPU can cause the pause.
func TestNewDaemon_ThermalMonitorPausesForAHotDetectedGPU(t *testing.T) {
	var asked atomic.Int32
	origExec := runtime.CommandExecutorCtx
	t.Cleanup(func() { runtime.CommandExecutorCtx = origExec })
	runtime.CommandExecutorCtx = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" && strings.Contains(strings.Join(args, " "), "temperature.gpu") {
			asked.Add(1)
			// temperature, utilisation, VRAM used, VRAM total, power
			return []byte("85, 97, 4096, 12288, 170.00\n"), nil
		}
		return nil, exec.ErrNotFound
	}
	origCap := runtime.ThermalCapabilityReader
	t.Cleanup(func() { runtime.ThermalCapabilityReader = origCap })
	runtime.ThermalCapabilityReader = func() runtime.ThermalCapability {
		return runtime.ThermalCapability{CPUSource: "none", Detail: "test: no CPU sensor"}
	}
	origSensors := runtime.SensorReader
	t.Cleanup(func() { runtime.SensorReader = origSensors })
	runtime.SensorReader = func() []runtime.Sensor { return nil }

	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.Thermal.Enabled = true
	cfg.Thermal.GPUPauseThresholdC = 80
	cfg.Thermal.GPUResumeThresholdC = 70
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	d := NewDaemon(DaemonConfig{
		Config:       cfg,
		Logger:       logger,
		Hardware:     &lettucev1.HardwareCapabilities{CpuModel: "test-cpu"},
		DetectedGPUs: []*runtime.GpuDetectionResult{{Model: "NVIDIA GeForce RTX 3060", Vendor: "nvidia", VRAMMB: 12288}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.StartThermalMonitorForTest(ctx, 20*time.Millisecond)

	select {
	case paused := <-d.thermalPauseCh:
		if !paused {
			t.Fatal("the monitor signalled resume, want pause")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no thermal pause within 3s with the detected GPU at 85°C over an 80°C pause threshold (nvidia-smi asked %d times)", asked.Load())
	}

	cap := d.ThermalCapability()
	if !cap.GPUReadable || cap.GPUSource != "nvidia-smi" {
		t.Errorf("GPU source = %q readable %v, want nvidia-smi readable (detail %q)", cap.GPUSource, cap.GPUReadable, cap.GPUDetail)
	}
	if r := d.ThermalReadings(); r.GPUTempC != 85 || r.GPUUsePct != 97 {
		t.Errorf("readings = %+v, want the card's 85°C and 97%% use", r)
	}
}
