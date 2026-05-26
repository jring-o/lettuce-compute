//go:build linux

package resource

import (
	"os/exec"
	"strconv"
	"strings"
)

// GetIdleSeconds returns the number of seconds since the last user input.
// On Linux, it tries D-Bus ScreenSaver API first, then xprintidle.
// Returns 0 (never idle) if detection fails — safe fallback that keeps the
// daemon paused in WHEN_IDLE mode.
func GetIdleSeconds() (int, error) {
	// Try D-Bus org.freedesktop.ScreenSaver.GetSessionIdleTime.
	if ms, err := dbusIdleMillis(); err == nil {
		return int(ms / 1000), nil
	}

	// Try xprintidle (returns milliseconds).
	if out, err := exec.Command("xprintidle").Output(); err == nil {
		ms, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err == nil {
			return int(ms / 1000), nil
		}
	}

	// All detection failed — return 0 (never idle, safe fallback).
	return 0, nil
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

	return 0, exec.ErrNotFound
}
