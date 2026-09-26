//go:build darwin

package resource

import (
	"os/exec"
)

// IdleDetectionRemedy says what makes this computer's idle time readable
// when GetIdleSeconds cannot read it. macOS has no alternative source.
const IdleDetectionRemedy = ""

// GetIdleSeconds returns the number of seconds since the last user input.
// On macOS, it parses the HIDIdleTime from ioreg (nanoseconds). When ioreg
// fails or reports no HIDIdleTime it returns an error wrapping
// ErrIdleUnknown; the scheduler then treats the machine as not idle and says
// why.
func GetIdleSeconds() (int, error) {
	return firstIdleReading([]idleSource{{name: "ioreg HIDIdleTime", read: ioregIdleSeconds}})
}

func ioregIdleSeconds() (int, error) {
	out, err := exec.Command("ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return 0, err
	}
	return parseHIDIdleTime(string(out))
}
