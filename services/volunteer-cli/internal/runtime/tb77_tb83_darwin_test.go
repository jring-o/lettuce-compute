//go:build darwin

package runtime

import (
	"errors"
	"math"
	"os/exec"
	"strings"
	"testing"
)

// TB-77: a Homebrew-installed osx-cpu-temp is found from a Finder-launched
// app whose PATH does not include the Homebrew prefix — the same search the
// engine detection does for Podman (TB-54).
func TestTB77_OSXCPUTempFoundInHomebrewPrefixWithoutPATH(t *testing.T) {
	origLook, origExists := lookPathFunc, fileExistsFunc
	t.Cleanup(func() { lookPathFunc, fileExistsFunc = origLook, origExists })

	lookPathFunc = func(string) (string, error) { return "", exec.ErrNotFound }
	fileExistsFunc = func(p string) bool { return p == "/usr/local/bin/osx-cpu-temp" }

	if got := osxCPUTempPath(); got != "/usr/local/bin/osx-cpu-temp" {
		t.Errorf("osxCPUTempPath = %q, want the Intel Homebrew prefix", got)
	}

	// The resolved path is what is executed, and its reading is used.
	var ran string
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		ran = name
		return []byte("67.5°C\n"), nil
	})
	if got := readCPUTemperature(); got != 67 {
		t.Errorf("readCPUTemperature = %d, want 67", got)
	}
	if ran != "/usr/local/bin/osx-cpu-temp" {
		t.Errorf("executed %q, want the resolved Homebrew path", ran)
	}

	cap := detectThermalCapability()
	if !cap.CPUReadable || cap.CPUSource != "osx-cpu-temp" || !strings.Contains(cap.Detail, "/usr/local/bin/osx-cpu-temp") {
		t.Errorf("capability = %+v, want readable via the Homebrew path", cap)
	}
}

// Without the helper anywhere: "none", with the brew remedy; with the helper
// but no reading (Apple silicon): "osx-cpu-temp" but unreadable, no remedy.
func TestTB77_OSXCPUTempCapabilityWithoutHelper(t *testing.T) {
	origLook, origExists := lookPathFunc, fileExistsFunc
	t.Cleanup(func() { lookPathFunc, fileExistsFunc = origLook, origExists })
	lookPathFunc = func(string) (string, error) { return "", exec.ErrNotFound }
	fileExistsFunc = func(string) bool { return false }

	cap := detectThermalCapability()
	if cap.CPUReadable || cap.CPUSource != "none" || !strings.Contains(cap.Remedy, "brew install osx-cpu-temp") {
		t.Errorf("capability = %+v, want none with the brew remedy", cap)
	}

	lookPathFunc = func(string) (string, error) { return "/opt/homebrew/bin/osx-cpu-temp", nil }
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) { return []byte("0.0°C\n"), nil })
	cap = detectThermalCapability()
	if cap.CPUReadable || cap.CPUSource != "osx-cpu-temp" || cap.Remedy != "" {
		t.Errorf("capability = %+v, want installed-but-unreadable with no remedy", cap)
	}
}

// TB-83: the macOS process-group reader sums `ps` rows by group.
func TestParsePSProcessGroups(t *testing.T) {
	listing := "  100   0:01.50\n  100   0:00.50\n  200  12:00.00\n  300  0:00.10\n"
	got, err := parsePSProcessGroups(listing, map[int]bool{100: true, 300: true})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got[100]-2.0) > 1e-9 || math.Abs(got[300]-0.1) > 1e-9 || len(got) != 2 {
		t.Errorf("got %v, want {100: 2.0, 300: 0.1}", got)
	}
}

// The `top` sampler reports the last sample's user + sys share.
func TestTopSampler(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name != "top" {
			return nil, errors.New("unexpected " + name)
		}
		return []byte("CPU usage: 50.0% user, 10.0% sys, 40.0% idle\nCPU usage: 20.0% user, 5.0% sys, 75.0% idle\n"), nil
	})
	got, err := NewMachineCPUSampler().Sample()
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-25) > 1e-9 {
		t.Errorf("top sampler = %v, want 25", got)
	}
}
