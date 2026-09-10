//go:build darwin

package runtime

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// osxCPUTempCandidates are the Homebrew prefixes `osx-cpu-temp` installs to
// (Apple silicon, then Intel). They are probed when PATH lookup fails: the app
// launched from Finder or the Dock runs with the login PATH only
// (/usr/bin:/bin:/usr/sbin:/sbin — the TB-54 lineage), so a tool the volunteer
// installed with Homebrew was found by a daemon started from a terminal and
// missed by the same daemon started from the app (TB-77).
var osxCPUTempCandidates = []string{
	"/opt/homebrew/bin/osx-cpu-temp",
	"/usr/local/bin/osx-cpu-temp",
}

// fileExistsFunc reports whether a path exists; a seam for tests.
var fileExistsFunc = func(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// osxCPUTempPath resolves the helper: PATH first, then the Homebrew prefixes.
// Returns "" when it is not installed anywhere this client looks.
func osxCPUTempPath() string {
	if p, err := lookPathFunc("osx-cpu-temp"); err == nil && p != "" {
		return p
	}
	for _, candidate := range osxCPUTempCandidates {
		if fileExistsFunc(candidate) {
			return candidate
		}
	}
	return ""
}

// readCPUTemperature attempts to read CPU temperature on macOS.
//
// macOS exposes no CPU temperature to an ordinary program; the only source
// this client has is the third-party `osx-cpu-temp` helper (an SMC reader),
// where the volunteer has installed it. Returns 0 — "unknown" to the monitor —
// when it is absent or returns nothing usable, which is the case on every Mac
// without the helper and on Apple silicon Macs the helper does not support.
func readCPUTemperature() int {
	bin := osxCPUTempPath()
	if bin == "" {
		return 0
	}
	out, err := CommandExecutor(bin)
	if err == nil {
		// Output format: "65.0°C"
		temp := strings.TrimSpace(string(out))
		temp = strings.TrimSuffix(temp, "°C")
		temp = strings.TrimSpace(temp)
		if v, err := strconv.ParseFloat(temp, 64); err == nil && v > 0 {
			return int(v)
		}
	}

	return 0
}

// readSensors on macOS returns nothing.
//
// There is no sysfs equivalent; the only CPU reading available is the
// osx-cpu-temp shell-out above, which readCPUTemperature already handles. With
// no sensor list there is no non-CPU overheat check on this platform, which is
// correct rather than a gap: the class of bug it guards against (a drive's
// temperature judged by the CPU's threshold) cannot arise where no drive
// temperature is read.
func readSensors() []Sensor { return nil }

// detectThermalCapability reports whether a CPU temperature can be read here
// (TB-77): the helper must be installed somewhere this client looks AND answer
// with a plausible reading — an installed helper on an Apple silicon Mac
// prints 0.0°C, which is no reading at all.
func detectThermalCapability() ThermalCapability {
	bin := osxCPUTempPath()
	if bin == "" {
		return ThermalCapability{
			CPUSource: "none",
			Detail:    "macOS gives programs no CPU temperature without a helper tool, and osx-cpu-temp is not installed (looked on PATH, in /opt/homebrew/bin and in /usr/local/bin)",
			Remedy:    "install it with `brew install osx-cpu-temp` (Intel Macs) and restart Lettuce",
		}
	}
	if readCPUTemperature() <= 0 {
		return ThermalCapability{
			CPUSource: "osx-cpu-temp",
			Detail:    fmt.Sprintf("%s is installed but returned no reading — it does not support this Mac (Apple silicon Macs report nothing through it)", bin),
		}
	}
	return ThermalCapability{
		CPUSource:   "osx-cpu-temp",
		CPUReadable: true,
		Detail:      "reading the CPU temperature with " + bin,
	}
}
