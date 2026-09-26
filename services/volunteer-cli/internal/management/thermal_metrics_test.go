package management

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// GET /api/v1/metrics carries the temperature the thermal monitor reads. It
// used to fill only the disk figures, so cpu_temp_c was 0 on every machine and
// the app's CPU gauge never showed a temperature, even where the monitor was
// reading one every few seconds to decide whether to pause.
func TestMetrics_CarryTheThermalMonitorsReading(t *testing.T) {
	origTemp := runtime.CPUTempReader
	t.Cleanup(func() { runtime.CPUTempReader = origTemp })
	runtime.CPUTempReader = func() int { return 72 }
	origCap := runtime.ThermalCapabilityReader
	t.Cleanup(func() { runtime.ThermalCapabilityReader = origCap })
	runtime.ThermalCapabilityReader = func() runtime.ThermalCapability {
		return runtime.ThermalCapability{CPUSource: "sysfs", CPUReadable: true, Detail: "test sensor"}
	}
	origSensors := runtime.SensorReader
	t.Cleanup(func() { runtime.SensorReader = origSensors })
	runtime.SensorReader = func() []runtime.Sensor { return nil }

	env := setupTestEnv(t) // thermal protection is on by default
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.daemon.StartThermalMonitorForTest(ctx, 20*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for env.bridge.GetMetrics().CPUTempC != 72 {
		if time.Now().After(deadline) {
			t.Fatalf("GetMetrics().CPUTempC = %d after 3s, want the monitor's 72", env.bridge.GetMetrics().CPUTempC)
		}
		time.Sleep(10 * time.Millisecond)
	}

	body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/metrics", ""))
	if body["cpu_temp_c"] != float64(72) {
		t.Errorf("GET /api/v1/metrics cpu_temp_c = %v, want 72", body["cpu_temp_c"])
	}
	if body["gpu_temp_c"] != float64(0) {
		t.Errorf("gpu_temp_c = %v with no GPU, want 0 (not read)", body["gpu_temp_c"])
	}

	// No GPU was detected, so the machine capabilities say the GPU thresholds
	// have nothing to read.
	caps := env.bridge.MachineCaps()
	if caps.GPUTempReadable || caps.GPUTempSource != "none" || caps.GPUTempDetail != "no GPU detected" {
		t.Errorf("GPU temperature source = %q readable %v detail %q, want none / false / no GPU detected",
			caps.GPUTempSource, caps.GPUTempReadable, caps.GPUTempDetail)
	}
}
