package runtime

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// A Linux GPU thermal zone (amdgpu, gpu-thermal, ...) is judged against the GPU
// thresholds. It used to be classified as a GPU sensor and then read by nothing:
// the CPU check takes CPU sensors only, and the critical-point check skips the
// CPU and GPU classes, so a GPU with no vendor tool never paused work.
func TestThermalMonitor_GPUThermalZoneAboveThresholdPauses(t *testing.T) {
	withMockCPUTemp(t, 50)
	withMockExecutor(t, notFoundForAll) // no vendor tool answers
	withMockSensors(t, []Sensor{{Zone: "thermal_zone3", Kind: "gpu-thermal", Class: SensorGPU, TempC: 90}})

	pauseCh := make(chan bool, 1)
	cfg := defaultThermalConfig() // GPU pause 80, resume 70
	m := NewThermalMonitor(cfg, pauseCh, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	m.SetPollIntervalForTest(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	select {
	case paused := <-pauseCh:
		if !paused {
			t.Fatal("got resume, want pause")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a GPU thermal zone at 90°C over an 80°C GPU threshold did not pause work")
	}
	if cap := m.Capability(); !cap.GPUReadable || cap.GPUSource != "sysfs" {
		t.Errorf("capability GPU source = %q readable %v, want sysfs readable", cap.GPUSource, cap.GPUReadable)
	}
	if r := m.Readings(); r.GPUTempC != 90 || r.CPUTempC != 50 {
		t.Errorf("readings = %+v, want CPU 50 and GPU 90", r)
	}
}

// rocm-smi reports edge, junction and memory temperatures. The junction and
// memory sensors run 10-20 °C hotter by design; only the edge reading compares
// with the GPU thresholds (and with nvidia-smi's single figure). The parser used
// to keep whichever temperature column came last — the memory sensor.
func TestParseRocmMetrics_UsesTheEdgeSensor(t *testing.T) {
	out := "device,Temperature (Sensor edge) (C),Temperature (Sensor junction) (C),Temperature (Sensor memory) (C),GPU use (%)\n" +
		"card0,64.0,79.0,92.0,99%\n"
	snap, err := parseRocmMetrics(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.TemperatureC != 64 {
		t.Errorf("TemperatureC = %d, want the edge sensor's 64 (not junction 79 or memory 92)", snap.TemperatureC)
	}
	if snap.UtilizationPct != 99 {
		t.Errorf("UtilizationPct = %d, want 99", snap.UtilizationPct)
	}

	// Without an edge column the first temperature is used.
	snap, err = parseRocmMetrics("device,Temperature (Sensor junction) (C),Temperature (Sensor memory) (C)\ncard0,70.0,88.0\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.TemperatureC != 70 {
		t.Errorf("no edge column: TemperatureC = %d, want the first temperature 70", snap.TemperatureC)
	}
}

// Each card gets a collector addressed by its position among its own vendor's
// cards, and only where this client may start that vendor's tool: never AMD's
// on Windows, never for makes with no tool.
func TestGPUThermalCollectors_PerVendorIndexAndPlatformRule(t *testing.T) {
	gpus := []*GpuDetectionResult{
		{Model: "RTX A", Vendor: "nvidia"},
		{Model: "RX 7800", Vendor: "amd"},
		{Model: "RTX B", Vendor: "nvidia"},
		{Model: "Arc", Vendor: "intel"},
		nil,
	}
	cs := GPUThermalCollectors(gpus, slog.Default())
	var got []string
	for _, c := range cs {
		got = append(got, c.vendor+":"+string(rune('0'+c.deviceIdx)))
	}
	want := []string{"nvidia:0", "amd:0", "nvidia:1"}
	if goruntime.GOOS == "windows" {
		want = []string{"nvidia:0", "nvidia:1"}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("collectors = %v, want %v", got, want)
	}
}

func TestDetectGPUThermal_SaysWhereTheReadingComesFromOrWhyNone(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			return []byte("61, 40, 1000, 12288, 90.00\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	nvidia := []*GpuDetectionResult{{Model: "RTX 3060", Vendor: "nvidia"}}
	src, ok, detail := DetectGPUThermal(nvidia, GPUThermalCollectors(nvidia, slog.Default()), nil)
	if !ok || src != "nvidia-smi" || !strings.Contains(detail, "61°C") {
		t.Errorf("readable NVIDIA card: source %q readable %v detail %q", src, ok, detail)
	}

	src, ok, detail = DetectGPUThermal(nil, nil, nil)
	if ok || src != "none" || detail != "no GPU detected" {
		t.Errorf("no GPU: source %q readable %v detail %q", src, ok, detail)
	}

	apple := []*GpuDetectionResult{{Model: "Apple M2", Vendor: "apple"}}
	src, ok, detail = DetectGPUThermal(apple, GPUThermalCollectors(apple, slog.Default()), nil)
	if ok || src != "none" || !strings.Contains(detail, "Apple M2") || !strings.Contains(detail, "report no temperature") {
		t.Errorf("Apple GPU: source %q readable %v detail %q, want it named with the reason", src, ok, detail)
	}

	withMockExecutor(t, notFoundForAll)
	src, ok, detail = DetectGPUThermal(nvidia, GPUThermalCollectors(nvidia, slog.Default()), nil)
	if ok || src != "none" || !strings.Contains(detail, "nvidia-smi reported no temperature") {
		t.Errorf("nvidia-smi missing: source %q readable %v detail %q", src, ok, detail)
	}

	// A GPU zone alone makes the thresholds act.
	src, ok, _ = DetectGPUThermal(nil, nil, []Sensor{{Zone: "thermal_zone4", Kind: "amdgpu", Class: SensorGPU, TempC: 55}})
	if !ok || src != "sysfs" {
		t.Errorf("GPU zone: source %q readable %v, want sysfs readable", src, ok)
	}
}

// When the CPU cannot be read, the notice says whether the GPU threshold still
// acts rather than promising it does. It used to say "GPU thresholds still apply
// where a GPU tool reports a temperature" on every machine, including those with
// no GPU at all.
func TestThermalMonitor_UnreadableCPUNoticeTellsTheTruthAboutTheGPU(t *testing.T) {
	withMockCPUTemp(t, 0)
	withMockThermalCapability(t, ThermalCapability{CPUSource: "none", Detail: "the platform does not expose it"})
	withMockSensors(t, nil)

	m, sink := newCapabilityTestMonitor(t, true) // no GPU tool answers, no GPU detected
	startAndStop(t, m)
	got := sink.snapshot()
	if len(got) != 1 || !strings.Contains(got[0].message, "No GPU temperature can be read here either") || strings.Contains(got[0].message, "still apply") {
		t.Errorf("no GPU: notice = %+v, want it to say the GPU thresholds have no effect either", got)
	}

	m, sink = newCapabilityTestMonitor(t, true)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			return []byte("58, 10, 500, 8192, 40.00\n"), nil
		}
		return nil, exec.ErrNotFound
	})
	m.SetDetectedGPUs([]*GpuDetectionResult{{Model: "RTX 4070", Vendor: "nvidia"}})
	startAndStop(t, m)
	got = sink.snapshot()
	if len(got) != 1 || !strings.Contains(got[0].message, "The GPU pause threshold of 80°C still applies") {
		t.Errorf("readable GPU: notice = %+v, want it to say the GPU threshold still applies", got)
	}
}
