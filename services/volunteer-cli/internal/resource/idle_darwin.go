//go:build darwin

package resource

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// idleTimeRe matches HIDIdleTime output from ioreg (value in nanoseconds).
var idleTimeRe = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)

// GetIdleSeconds returns the number of seconds since the last user input.
// On macOS, it parses the HIDIdleTime from ioreg (nanoseconds).
// Returns 0 (never idle) if detection fails.
func GetIdleSeconds() (int, error) {
	out, err := exec.Command("ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return 0, nil // safe fallback
	}

	for _, line := range strings.Split(string(out), "\n") {
		if m := idleTimeRe.FindStringSubmatch(line); len(m) == 2 {
			ns, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				continue
			}
			return int(ns / 1_000_000_000), nil
		}
	}

	return 0, nil // safe fallback
}
