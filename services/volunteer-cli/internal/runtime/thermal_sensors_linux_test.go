//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TB-17: readCPUTemperature took the maximum over EVERY thermal_zone and
// returned it as "the CPU temperature", never opening thermal_zone*/type. Linux
// exposes zones for NVMe controllers, WiFi chipsets, the PCH, batteries and
// ambient ACPI sensors, whose safe ranges differ from a CPU's by tens of
// degrees — 85C is danger for a CPU and routine for a drive. One threshold over
// all of them froze a tester's work for 2 h 28 min on a machine whose CPU was
// fine, and could not clear, because suspending Lettuce does not cool a disk
// something else is using.
//
// A CI host's real sensors are whatever that host happens to have (WSL2 and
// most containers expose no thermal_zone at all), so these drive a synthetic
// sysfs through the glob seams.

type fakeZone struct {
	kind     string
	tempC    int
	criticalC int // 0 = declares no critical trip point
}

// writeFakeSysfs builds a thermal_zone tree and points the reader at it.
func writeFakeSysfs(t *testing.T, zones []fakeZone) {
	t.Helper()
	root := t.TempDir()
	for i, z := range zones {
		dir := filepath.Join(root, "thermal_zone"+strconv.Itoa(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		write := func(name, content string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		write("type", z.kind)
		write("temp", strconv.Itoa(z.tempC*1000))
		if z.criticalC > 0 {
			write("trip_point_0_type", "critical")
			write("trip_point_0_temp", strconv.Itoa(z.criticalC*1000))
		}
	}

	origZone, origHwmon := thermalZoneGlob, hwmonGlob
	t.Cleanup(func() { thermalZoneGlob, hwmonGlob = origZone, origHwmon })
	thermalZoneGlob = filepath.Join(root, "thermal_zone*")
	hwmonGlob = filepath.Join(root, "nonexistent-hwmon*")
}

// The reported machine shape: a warm drive beside a cool CPU.
func TestReadCPUTemperature_IgnoresNonCPUSensors(t *testing.T) {
	writeFakeSysfs(t, []fakeZone{
		{kind: "acpitz", tempC: 27},
		{kind: "x86_pkg_temp", tempC: 62}, // the actual CPU
		{kind: "nvme", tempC: 86},         // an SSD at its ordinary working temperature
		{kind: "iwlwifi_1", tempC: 51},
	})

	got := readCPUTemperature()
	if got != 62 {
		t.Fatalf("readCPUTemperature() = %d, want 62 (the x86_pkg_temp zone)", got)
	}
	if got >= 85 {
		t.Errorf("returned %d, which trips the 85C pause threshold and freezes ALL work on a machine whose CPU is at 62C", got)
	}
}

func TestReadCPUTemperature_FindsHottestCPUZone(t *testing.T) {
	writeFakeSysfs(t, []fakeZone{
		{kind: "coretemp", tempC: 71},
		{kind: "x86_pkg_temp", tempC: 88},
		{kind: "nvme", tempC: 40},
	})
	if got := readCPUTemperature(); got != 88 {
		t.Errorf("readCPUTemperature() = %d, want 88 — a genuinely hot CPU must still trip the throttle", got)
	}
}

// With no identifiable CPU sensor we return 0, which disables the CPU check.
// Guessing would risk freezing a volunteer on a reading we cannot interpret.
func TestReadCPUTemperature_UnknownSensorsYieldZero(t *testing.T) {
	writeFakeSysfs(t, []fakeZone{
		{kind: "nvme", tempC: 90},
		{kind: "iwlwifi_1", tempC: 88},
		{kind: "pch_skylake", tempC: 91},
	})
	if got := readCPUTemperature(); got != 0 {
		t.Errorf("readCPUTemperature() = %d, want 0 — no CPU sensor exists on this machine, so there is nothing to judge", got)
	}
}

// Implausible readings are a documented sysfs failure mode: zones stuck at a
// constant, or reporting a trip point (200C) rather than a live value. Since a
// high reading suspends all work, an impossible one must not be believed.
func TestReadCPUTemperature_RejectsImplausibleReadings(t *testing.T) {
	writeFakeSysfs(t, []fakeZone{
		{kind: "x86_pkg_temp", tempC: 200}, // bogus
		{kind: "coretemp", tempC: 58},
	})
	if got := readCPUTemperature(); got != 58 {
		t.Errorf("readCPUTemperature() = %d, want 58 — a 200C reading is not a temperature", got)
	}
}

func TestClassifyZone(t *testing.T) {
	cases := map[string]SensorClass{
		"x86_pkg_temp":       SensorCPU,
		"coretemp":           SensorCPU,
		"coretemp-isa-0000":  SensorCPU,
		"k10temp":            SensorCPU,
		"cpu-thermal0":       SensorCPU,
		"amdgpu":             SensorGPU,
		"nouveau":            SensorGPU,
		"nvme":               SensorOther,
		"iwlwifi_1":          SensorOther,
		"acpitz":             SensorOther,
		"pch_skylake":        SensorOther,
		"BAT0":               SensorOther,
		"":                   SensorOther,
	}
	for kind, want := range cases {
		if got := classifyZone(kind); got != want {
			t.Errorf("classifyZone(%q) = %v, want %v", kind, got, want)
		}
	}
}

// A non-CPU sensor is honoured only against its OWN declared danger point.
func TestReadSensors_CarriesCriticalTripPoint(t *testing.T) {
	writeFakeSysfs(t, []fakeZone{
		{kind: "nvme", tempC: 86, criticalC: 95},
		{kind: "iwlwifi_1", tempC: 51}, // declares none
	})

	sensors := readSensors()
	if len(sensors) != 2 {
		t.Fatalf("read %d sensors, want 2", len(sensors))
	}
	byKind := map[string]Sensor{}
	for _, s := range sensors {
		byKind[s.Kind] = s
	}
	if got := byKind["nvme"].CriticalC; got != 95 {
		t.Errorf("nvme CriticalC = %d, want 95 (the kernel's own danger point for that part)", got)
	}
	if got := byKind["iwlwifi_1"].CriticalC; got != 0 {
		t.Errorf("iwlwifi CriticalC = %d, want 0 — it declares none, so it can never pause work", got)
	}
}

// The whole judgement, end to end: a drive at 86C with a 95C critical point is
// working normally and must not pause anything; the same drive at 96C has passed
// the number its own vendor declared and may.
func TestCriticalOverheats_OnlyPastTheSensorsOwnLimit(t *testing.T) {
	normal := []Sensor{{Zone: "thermal_zone2", Kind: "nvme", Class: SensorOther, TempC: 86, CriticalC: 95}}
	if pausing, _ := criticalOverheats(normal, criticalResumeMarginC); len(pausing) != 0 {
		t.Errorf("a drive at 86C against its own 95C limit paused work; this is the TB-17 freeze")
	}

	overheating := []Sensor{{Zone: "thermal_zone2", Kind: "nvme", Class: SensorOther, TempC: 96, CriticalC: 95}}
	if pausing, _ := criticalOverheats(overheating, criticalResumeMarginC); len(pausing) != 1 {
		t.Errorf("a drive past its own declared critical point did not pause work")
	}

	// No declared limit: nothing to judge against, so it never pauses.
	undeclared := []Sensor{{Zone: "thermal_zone3", Kind: "mystery", Class: SensorOther, TempC: 110}}
	if pausing, _ := criticalOverheats(undeclared, criticalResumeMarginC); len(pausing) != 0 {
		t.Errorf("a sensor with no declared danger point paused work on an invented threshold")
	}

	// CPU and GPU sensors are judged by the configured thresholds, not here.
	cpu := []Sensor{{Zone: "thermal_zone1", Kind: "x86_pkg_temp", Class: SensorCPU, TempC: 99, CriticalC: 100}}
	if pausing, _ := criticalOverheats(cpu, criticalResumeMarginC); len(pausing) != 0 {
		t.Errorf("a CPU sensor was double-counted by the critical-trip path")
	}
}
