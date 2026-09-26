package cli

import (
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// doctor states the bandwidth limit and names what it cannot reach, so a
// volunteer watching an image pull run at full speed under a limit knows that
// is expected rather than a fault.
func TestDoctorBandwidthRow(t *testing.T) {
	out, rep := doctorRows(func(rep *doctorReport) { checkBandwidth(rep, 0) })
	if !strings.Contains(out, "bandwidth") || !strings.Contains(out, "unlimited") {
		t.Errorf("0 Mbps row = %q, want it to say unlimited", out)
	}
	out, rep = doctorRows(func(rep *doctorReport) { checkBandwidth(rep, 20) })
	for _, want := range []string{"20 Mbps", "downloads together stay under it", "so do its uploads", "container image pulls", "not limited"} {
		if !strings.Contains(out, want) {
			t.Errorf("20 Mbps row = %q, want %q", out, want)
		}
	}
	if rep.warns != 0 || rep.fails != 0 {
		t.Errorf("the bandwidth row is information; warns %d fails %d", rep.warns, rep.fails)
	}
}

// The thermal row reports the GPU thresholds by what the GPU source found. It
// used to print "(GPU: 80/70°C where a GPU tool reports one)" on every machine,
// although nothing ever read a GPU temperature.
func TestDoctorThermalRow_GPUHalf(t *testing.T) {
	th := config.Defaults().Thermal

	out, _ := doctorRows(func(rep *doctorReport) {
		checkThermal(rep, th, runtime.ThermalCapability{
			CPUSource: "sysfs", CPUReadable: true, Detail: "reading thermal_zone1 (x86_pkg_temp)",
			GPUSource: "nvidia-smi", GPUReadable: true, GPUDetail: "reading NVIDIA card 0 with nvidia-smi (61°C now)",
		})
	})
	for _, want := range []string{"GPU temperature from nvidia-smi", "61°C now", "pauses at 87°C", "below 77°C"} {
		if !strings.Contains(out, want) {
			t.Errorf("readable GPU row = %q, want %q", out, want)
		}
	}
	if strings.Contains(out, "where a GPU tool reports") {
		t.Errorf("row still hedges with the old wording: %q", out)
	}

	out, _ = doctorRows(func(rep *doctorReport) {
		checkThermal(rep, th, runtime.ThermalCapability{
			CPUSource: "sysfs", CPUReadable: true, Detail: "reading thermal_zone1 (x86_pkg_temp)",
			GPUSource: "none", GPUDetail: "no GPU detected",
		})
	})
	if !strings.Contains(out, "the GPU thresholds (87/77°C) have no effect: no GPU detected") {
		t.Errorf("no-GPU row = %q, want it to say the GPU thresholds have no effect and why", out)
	}

	out, _ = doctorRows(func(rep *doctorReport) {
		checkThermal(rep, th, runtime.ThermalCapability{
			CPUSource: "none", Detail: "the platform does not expose it",
			GPUSource: "none", GPUDetail: "no GPU temperature can be read: Radeon (AMD's temperature tool asks for administrator rights on Windows, so Lettuce does not start it)",
		})
	})
	if !strings.Contains(out, "have no effect either") || !strings.Contains(out, "administrator rights") {
		t.Errorf("nothing-readable row = %q, want both halves to say they have no effect", out)
	}
}
