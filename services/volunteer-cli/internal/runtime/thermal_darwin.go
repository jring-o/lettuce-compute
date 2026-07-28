//go:build darwin

package runtime

import (
	"strconv"
	"strings"
)

// readCPUTemperature attempts to read CPU temperature on macOS.
// Uses osx-cpu-temp if installed. Returns 0 if unavailable.
func readCPUTemperature() int {
	out, err := CommandExecutor("osx-cpu-temp")
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
