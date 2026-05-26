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
