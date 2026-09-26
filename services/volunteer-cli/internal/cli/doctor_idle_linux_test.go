//go:build linux

package cli

import (
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// On a Linux machine with no idle source (no dbus-send, no xprintidle),
// doctor listed a "run when idle" schedule as a plain information line and
// summed up "no blocking failures", although work could never start.
func TestDoctorFlagsUnreadableIdleTime(t *testing.T) {
	origCfg := cfg
	defer func() { cfg = origCfg }()
	cfg = config.Defaults()
	cfg.DataDir = t.TempDir() // no daemon.json: no running daemon to ask
	cfg.Scheduling.Mode = "WHEN_IDLE"
	cfg.Scheduling.IdleThresholdMins = 1
	t.Setenv("PATH", t.TempDir())

	var buf strings.Builder
	rep := &doctorReport{w: &buf}
	checkAccountInfo(rep)

	out := buf.String()
	if rep.warns+rep.fails == 0 {
		t.Fatalf("doctor raised nothing for a \"run when idle\" schedule that can never start:\n%s", out)
	}
	if !strings.Contains(out, "idle time") || !strings.Contains(out, "xprintidle") {
		t.Errorf("doctor does not say idle time cannot be read, or how to fix it:\n%s", out)
	}
}
