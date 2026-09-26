//go:build linux

package resource

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// IdleDetectionRemedy says what makes this computer's idle time readable
// when GetIdleSeconds cannot read it.
const IdleDetectionRemedy = "install xprintidle (X11 desktops), or run Lettuce inside a desktop session that provides the org.freedesktop.ScreenSaver idle time"

// GetIdleSeconds returns the number of seconds since the last user input.
// On Linux, it tries the D-Bus ScreenSaver API first, then xprintidle. When
// neither answers — a headless machine, a daemon started over SSH or as a
// service, a desktop without either — it returns an error wrapping
// ErrIdleUnknown; the scheduler then treats the machine as not idle and says
// why.
func GetIdleSeconds() (int, error) {
	return firstIdleReading([]idleSource{
		{name: "D-Bus ScreenSaver", read: dbusIdleSeconds},
		{name: "xprintidle", read: xprintidleSeconds},
	})
}

// xprintidleSeconds runs xprintidle, which prints milliseconds and needs an
// X display.
func xprintidleSeconds() (int, error) {
	out, err := exec.Command("xprintidle").Output()
	if err != nil {
		return 0, err
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, err
	}
	return int(ms / 1000), nil
}

// dbusIdleSeconds queries the session bus's ScreenSaver interface.
func dbusIdleSeconds() (int, error) {
	ms, err := dbusIdleMillis()
	if err != nil {
		return 0, err
	}
	return int(ms / 1000), nil
}

// dbusIdleMillis queries the D-Bus ScreenSaver interface for idle time in ms.
func dbusIdleMillis() (int64, error) {
	out, err := exec.Command(
		"dbus-send",
		"--session",
		"--dest=org.freedesktop.ScreenSaver",
		"--type=method_call",
		"--print-reply",
		"/org/freedesktop/ScreenSaver",
		"org.freedesktop.ScreenSaver.GetSessionIdleTime",
	).Output()
	if err != nil {
		return 0, err
	}

	// Parse dbus-send output: "   uint32 12345\n"
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "uint32") || strings.HasPrefix(line, "uint64") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return strconv.ParseInt(parts[1], 10, 64)
			}
		}
	}

	return 0, errors.New("no idle time in the dbus-send reply")
}
