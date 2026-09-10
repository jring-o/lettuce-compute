package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-77 / TB-83: doctor says what the thermal thresholds can see and whether
// Lettuce yields to other programs. Neither had a row before.

func doctorRows(fn func(rep *doctorReport)) (string, *doctorReport) {
	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	fn(rep)
	return buf.String(), rep
}

func TestTB77_DoctorThermalRow(t *testing.T) {
	th := config.Defaults().Thermal

	out, rep := doctorRows(func(rep *doctorReport) {
		checkThermal(rep, th, runtime.ThermalCapability{CPUSource: "sysfs", CPUReadable: true, Detail: "reading thermal_zone1 (x86_pkg_temp)"})
	})
	if !strings.Contains(out, "ok  ") || !strings.Contains(out, "thermal") || !strings.Contains(out, "x86_pkg_temp") || !strings.Contains(out, "85°C") {
		t.Errorf("readable row = %q, want an ok row naming the sensor and threshold", out)
	}
	if rep.warns != 0 {
		t.Errorf("readable source must not warn; warns = %d", rep.warns)
	}

	out, rep = doctorRows(func(rep *doctorReport) {
		checkThermal(rep, th, runtime.ThermalCapability{CPUSource: "none", Detail: "no helper installed", Remedy: "install it with brew"})
	})
	if !strings.Contains(out, "warn") || !strings.Contains(out, "cannot be read") || !strings.Contains(out, "no helper installed") || !strings.Contains(out, "-> install it with brew") {
		t.Errorf("fixable unreadable row = %q, want a warning with the detail and remedy", out)
	}
	if rep.warns != 1 {
		t.Errorf("fixable gap must count as a warning; warns = %d", rep.warns)
	}

	out, rep = doctorRows(func(rep *doctorReport) {
		checkThermal(rep, th, runtime.ThermalCapability{CPUSource: "none", Detail: "the platform does not expose it"})
	})
	if !strings.Contains(out, "info") || strings.Contains(out, "->") {
		t.Errorf("unfixable unreadable row = %q, want information with no remedy line", out)
	}
	if rep.warns != 0 {
		t.Errorf("unfixable gap must not count as a warning (TI-19); warns = %d", rep.warns)
	}

	off := th
	off.Enabled = false
	out, _ = doctorRows(func(rep *doctorReport) {
		checkThermal(rep, off, runtime.ThermalCapability{CPUSource: "none"})
	})
	if !strings.Contains(out, "off (thermal.enabled false)") {
		t.Errorf("disabled row = %q, want it to say protection is off", out)
	}
}

func TestTB83_DoctorYieldRow(t *testing.T) {
	y := config.Defaults().Yield

	out, rep := doctorRows(func(rep *doctorReport) { checkYield(rep, y, nil) })
	if !strings.Contains(out, "off (yield.enabled false)") || !strings.Contains(out, "config set yield.enabled true") {
		t.Errorf("off row = %q, want it to say yield is off and how to turn it on", out)
	}
	if rep.warns != 0 {
		t.Errorf("off must not warn; warns = %d", rep.warns)
	}

	y.Enabled = true
	out, rep = doctorRows(func(rep *doctorReport) { checkYield(rep, y, nil) })
	if !strings.Contains(out, "ok  ") || !strings.Contains(out, "25%") || !strings.Contains(out, "15%") || !strings.Contains(out, "30 s") {
		t.Errorf("on row = %q, want an ok row with both thresholds and the window", out)
	}

	out, rep = doctorRows(func(rep *doctorReport) { checkYield(rep, y, errors.New("no counters")) })
	if !strings.Contains(out, "warn") || !strings.Contains(out, "cannot be measured") || !strings.Contains(out, "no counters") {
		t.Errorf("unmeasurable row = %q, want a warning naming the failure", out)
	}
	if rep.warns != 1 {
		t.Errorf("unmeasurable must warn; warns = %d", rep.warns)
	}
}

// The probe treats a counter reader's baseline as success: only a real
// failure means the load cannot be measured here.
func TestTB83_ProbeYieldMeasurableOnThisHost(t *testing.T) {
	if err := probeYieldMeasurable(); err != nil {
		t.Errorf("probe on this host: %v (Linux, macOS and Windows all have a reader)", err)
	}
}
