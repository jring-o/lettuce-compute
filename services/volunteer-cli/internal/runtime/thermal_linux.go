//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readCPUTemperature reads the highest CPU temperature from Linux hwmon.
// Returns temperature in degrees Celsius, or 0 if unavailable.
func readCPUTemperature() int {
	matches, err := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	if err != nil || len(matches) == 0 {
		return 0
	}

	maxTemp := 0
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// Value is in millidegrees Celsius.
		milliC, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			continue
		}
		tempC := milliC / 1000
		if tempC > maxTemp {
			maxTemp = tempC
		}
	}
	return maxTemp
}
