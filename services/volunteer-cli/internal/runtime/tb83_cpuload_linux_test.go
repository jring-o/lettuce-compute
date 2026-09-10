//go:build linux

package runtime

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TB-83: the Linux readers — /proc/stat for the machine and a /proc scan by
// process group for Lettuce's native task trees — against synthetic trees.

func TestParseProcPIDStat(t *testing.T) {
	// pid (comm with spaces and a paren) state ppid pgrp session tty tpgid
	// flags minflt cminflt majflt cmajflt utime stime cutime cstime …
	stat := "4242 (my (odd) comm) S 1 4200 4200 0 -1 4194560 100 200 0 0 150 50 30 20 20 0 1 0 12345 0 0\n"
	pgrp, secs, ok := parseProcPIDStat(stat)
	if !ok {
		t.Fatal("parseProcPIDStat returned !ok")
	}
	if pgrp != 4200 {
		t.Errorf("pgrp = %d, want 4200", pgrp)
	}
	// (150 + 50 + 30 + 20) ticks at 100 Hz = 2.5 s: reaped children count.
	if math.Abs(secs-2.5) > 1e-9 {
		t.Errorf("cpu seconds = %v, want 2.5", secs)
	}
	if _, _, ok := parseProcPIDStat("garbage"); ok {
		t.Error("garbage must not parse")
	}
}

func TestProcessGroupsCPUSeconds_SyntheticProc(t *testing.T) {
	root := t.TempDir()
	write := func(pid int, stat string) {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Group 100: leader and one child (a forked helper), 1.0 s + 0.5 s.
	write(100, "100 (leaf) R 1 100 100 0 -1 0 0 0 0 0 60 40 0 0 20 0 1 0 1 0 0")
	write(101, "101 (helper) R 100 100 100 0 -1 0 0 0 0 0 30 20 0 0 20 0 1 0 1 0 0")
	// Group 200: a foreign process, not ours.
	write(200, "200 (browser) R 1 200 200 0 -1 0 0 0 0 0 900 900 0 0 20 0 1 0 1 0 0")
	// Not a pid directory.
	if err := os.MkdirAll(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = orig })

	got, err := ProcessGroupsCPUSeconds([]int{100, 300})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want only group 100 (300 has no process)", got)
	}
	if math.Abs(got[100]-1.5) > 1e-9 {
		t.Errorf("group 100 = %v s, want 1.5 (leader + helper)", got[100])
	}

	empty, err := ProcessGroupsCPUSeconds(nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("no groups → %v, %v; want empty, nil", empty, err)
	}
}

func TestMachineCPUSampler_ProcStatFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	orig := procStatPath
	procStatPath = path
	t.Cleanup(func() { procStatPath = orig })

	if err := os.WriteFile(path, []byte("cpu  100 0 0 900 0 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewMachineCPUSampler()
	if _, err := s.Sample(); err == nil {
		t.Fatal("first sample must be a baseline")
	}
	if err := os.WriteFile(path, []byte("cpu  175 0 0 925 0 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-75) > 1e-9 {
		t.Errorf("busy = %v, want 75 (75 busy of 100 elapsed)", got)
	}
}

// TB-77: the Linux CPU temperature source is detected from the same synthetic
// sysfs tree the sensor reader is tested against.
func TestTB77_LinuxThermalCapability(t *testing.T) {
	root := t.TempDir()
	zones := filepath.Join(root, "thermal")
	hwmon := filepath.Join(root, "hwmon")
	origZone, origHwmon := thermalZoneGlob, hwmonGlob
	thermalZoneGlob = filepath.Join(zones, "thermal_zone*")
	hwmonGlob = filepath.Join(hwmon, "hwmon*")
	t.Cleanup(func() { thermalZoneGlob, hwmonGlob = origZone, origHwmon })

	// Only an NVMe zone: no CPU source.
	nvme := filepath.Join(zones, "thermal_zone0")
	if err := os.MkdirAll(nvme, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(nvme, "type"), []byte("nvme\n"), 0o644)
	_ = os.WriteFile(filepath.Join(nvme, "temp"), []byte("45000\n"), 0o644)

	cap := detectThermalCapability()
	if cap.CPUReadable || cap.CPUSource != "none" || cap.Remedy == "" {
		t.Errorf("without a CPU zone: %+v, want none + a driver remedy", cap)
	}

	// Add a package sensor: readable, naming the zone and its kind.
	pkg := filepath.Join(zones, "thermal_zone1")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(pkg, "type"), []byte("x86_pkg_temp\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pkg, "temp"), []byte("52000\n"), 0o644)

	cap = detectThermalCapability()
	if !cap.CPUReadable || cap.CPUSource != "sysfs" {
		t.Errorf("with a CPU zone: %+v, want readable sysfs", cap)
	}
	for _, want := range []string{"thermal_zone1", "x86_pkg_temp"} {
		if !strings.Contains(cap.Detail, want) {
			t.Errorf("detail %q lacks %q", cap.Detail, want)
		}
	}
}
