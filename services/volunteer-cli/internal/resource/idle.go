package resource

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ErrIdleUnknown means no source on this machine could say how long it has
// been idle. GetIdleSeconds wraps it with what each source answered.
//
// It is an error, not a reading of 0: "never idle" is indistinguishable from
// a person who has just touched the keyboard, so a "run when idle" volunteer
// on a machine with no idle source used to wait forever while every surface
// reported an ordinary schedule pause and nothing was logged.
var ErrIdleUnknown = errors.New("this computer's idle time cannot be read")

// DescribeIdleUnavailable is the volunteer-facing explanation of failed idle
// readings in "run when idle" mode: what it means and what fixes it. The
// daemon's notice, its pause detail and `doctor` all say it this way.
func DescribeIdleUnavailable() string {
	fix := "choose to run always or during set hours instead (Settings in the app, or `lettuce-volunteer schedule clear` / `schedule set --from 20:00 --to 06:00`)"
	if IdleDetectionRemedy != "" {
		fix = IdleDetectionRemedy + ", or " + fix
	}
	return `Lettuce cannot tell when this computer is idle, so "run when idle" will never start work. To fix it, ` + fix + "."
}

// idleSource is one way of reading the seconds since the last user input.
type idleSource struct {
	name string
	read func() (int, error)
}

// firstIdleReading returns the first reading any source gives, trying them
// in order. When every source fails the error wraps ErrIdleUnknown and names
// what each one answered, so the log says which tools were tried and why they
// gave nothing.
func firstIdleReading(sources []idleSource) (int, error) {
	failures := make([]string, 0, len(sources))
	for _, s := range sources {
		secs, err := s.read()
		if err == nil {
			return secs, nil
		}
		failures = append(failures, s.name+": "+commandFailure(err))
	}
	return 0, fmt.Errorf("%w (%s)", ErrIdleUnknown, strings.Join(failures, "; "))
}

// commandFailure describes a failed helper command in one line, adding the
// first line of what it printed on stderr ("Unable to autolaunch a
// dbus-daemon without a $DISPLAY") when exec captured it, since the exit
// status alone does not say why.
func commandFailure(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if line, _, _ := strings.Cut(strings.TrimSpace(string(ee.Stderr)), "\n"); line != "" {
			return err.Error() + ": " + strings.TrimSpace(line)
		}
	}
	return err.Error()
}

// hidIdleTimeRe matches the HIDIdleTime property in `ioreg -c IOHIDSystem`
// output (a value in nanoseconds).
var hidIdleTimeRe = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)

// parseHIDIdleTime reads the seconds since the last input from macOS ioreg
// output. Output without a HIDIdleTime value is an error, not 0 seconds.
func parseHIDIdleTime(out string) (int, error) {
	for _, line := range strings.Split(out, "\n") {
		if m := hidIdleTimeRe.FindStringSubmatch(line); len(m) == 2 {
			ns, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				continue
			}
			return int(ns / 1_000_000_000), nil
		}
	}
	return 0, errors.New("no HIDIdleTime in the ioreg output")
}
