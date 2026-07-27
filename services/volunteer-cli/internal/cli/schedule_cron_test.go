package cli

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// writeCronConfig lays down a valid volunteer config whose schedule is a CRON
// expression — the shape init used to produce — and returns its path.
func writeCronConfig(t *testing.T, dir, cronExpr string) string {
	t.Helper()
	cfgFile := filepath.Join(dir, "config.yaml")
	c := config.Defaults()
	c.DataDir = dir
	c.Scheduling.Mode = "SCHEDULED"
	c.Scheduling.CronExpression = cronExpr
	c.Scheduling.ScheduleRanges = nil
	if err := c.Save(cfgFile); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return cfgFile
}

// TestScheduleAddRefusesToDeleteCronExpression is the TB-2 regression. A volunteer
// whose schedule was a cron expression (init's only offer) ran `schedule add` to
// layer on a window; `add` DELETED the cron expression, saved, and said nothing.
// Their configured schedule was simply gone.
func TestScheduleAddRefusesToDeleteCronExpression(t *testing.T) {
	dir := t.TempDir()
	const cronExpr = "0 19-23 * * 1-5"
	cfgFile := writeCronConfig(t, dir, cronExpr)

	cmd := newRootCmd()
	cmd.SetArgs([]string{"schedule", "add", "--from", "04:00", "--to", "02:00", "--days", "sat-sun",
		"--config", cfgFile, "--data-dir", dir})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("`schedule add` succeeded against a cron schedule; it must refuse rather than silently discard it")
	}
	// The refusal has to tell a non-developer what to do instead.
	for _, want := range []string{"schedule set", "schedule clear"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}

	// The whole point: nothing on disk changed.
	loaded, lerr := config.Load(cfgFile)
	if lerr != nil {
		t.Fatalf("reloading config: %v", lerr)
	}
	if loaded.Scheduling.CronExpression != cronExpr {
		t.Errorf("cron_expression = %q, want it preserved as %q", loaded.Scheduling.CronExpression, cronExpr)
	}
	if len(loaded.Scheduling.ScheduleRanges) != 0 {
		t.Errorf("schedule_ranges = %v, want none written", loaded.Scheduling.ScheduleRanges)
	}
}

// TestScheduleAddStillLayersWindows guards the neighbouring behavior TB-2 was
// careful to note is NOT broken: set-then-add must still produce two windows.
func TestScheduleAddStillLayersWindows(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	c := config.Defaults()
	c.DataDir = dir
	if err := c.Save(cfgFile); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	for _, args := range [][]string{
		{"schedule", "set", "--from", "19:00", "--to", "07:00", "--days", "mon-fri"},
		{"schedule", "add", "--from", "00:00", "--to", "00:00", "--days", "sat,sun"},
	} {
		cmd := newRootCmd()
		cmd.SetArgs(append(args, "--config", cfgFile, "--data-dir", dir))
		captureStdout(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
		})
	}

	loaded, err := config.Load(cfgFile)
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	if len(loaded.Scheduling.ScheduleRanges) != 2 {
		t.Fatalf("schedule_ranges = %d, want 2", len(loaded.Scheduling.ScheduleRanges))
	}
}

// TestScheduleSetAnnouncesTheCronItReplaced covers the other half of TB-2: `set`
// is entitled to replace the schedule, but not to make a volunteer's cron
// expression disappear without a word.
func TestScheduleSetAnnouncesTheCronItReplaced(t *testing.T) {
	dir := t.TempDir()
	const cronExpr = "0 19-23 * * 1-5"
	cfgFile := writeCronConfig(t, dir, cronExpr)

	cmd := newRootCmd()
	cmd.SetArgs([]string{"schedule", "set", "--from", "20:00", "--to", "06:00",
		"--config", cfgFile, "--data-dir", dir})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("`schedule set` failed: %v", err)
		}
	})

	if !strings.Contains(out, cronExpr) {
		t.Errorf("output did not name the cron expression it removed:\n%s", out)
	}
	loaded, err := config.Load(cfgFile)
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	if loaded.Scheduling.CronExpression != "" {
		t.Errorf("cron_expression = %q, want cleared by `set`", loaded.Scheduling.CronExpression)
	}
	if len(loaded.Scheduling.ScheduleRanges) != 1 {
		t.Errorf("schedule_ranges = %v, want the one new window", loaded.Scheduling.ScheduleRanges)
	}
}

// TestScheduleShowFlagsAnUnrunnableCron is a TB-3 surfacing regression: `schedule
// show` printed whatever string was stored as though it were a working schedule,
// which is exactly how the volunteer concluded they were correctly configured.
func TestScheduleShowFlagsAnUnrunnableCron(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeCronConfig(t, dir, "--from 06:00 --to 04:00 --days mon-fri")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"schedule", "show", "--config", cfgFile, "--data-dir", dir})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("`schedule show` failed: %v", err)
		}
	})

	if !strings.Contains(out, "INVALID") || !strings.Contains(out, "NEVER runs") {
		t.Errorf("`schedule show` presented an unrunnable cron as legitimate:\n%s", out)
	}
}

// TestStartRefusesAScheduleThatCanNeverRun is the TB-3 headline regression: the
// daemon used to start happily on an unparseable cron, warn once per 10-second
// poll into a log nobody reads, and contribute nothing forever while looking
// configured.
func TestStartRefusesAScheduleThatCanNeverRun(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeCronConfig(t, dir, "--from 06:00 --to 04:00 --days mon-fri")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"start", "--config", cfgFile, "--data-dir", dir})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("`start` accepted a schedule that can never become active")
	}
	if !strings.Contains(err.Error(), "never become active") {
		t.Errorf("error %q does not explain that the schedule is the problem", err)
	}
	if !strings.Contains(err.Error(), "schedule set") {
		t.Errorf("error %q does not point at the command that fixes it", err)
	}
}

// TestDoctorFailsOnAScheduleThatCanNeverRun: doctor is the primary diagnostic, and
// it reported a dead cron schedule as an ordinary informational line among the
// passing checks (TB-3).
func TestDoctorFailsOnAScheduleThatCanNeverRun(t *testing.T) {
	dir := t.TempDir()
	origCfg := cfg
	defer func() { cfg = origCfg }()

	cfg = config.Defaults()
	cfg.DataDir = dir
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.CronExpression = "--from 06:00 --to 04:00 --days mon-fri"

	var buf strings.Builder
	rep := &doctorReport{w: &buf}
	checkAccountInfo(rep)

	if rep.fails == 0 {
		t.Errorf("doctor reported no failure for a schedule that can never run:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "never run") {
		t.Errorf("doctor's report does not say the volunteer will never run:\n%s", buf.String())
	}

	// A healthy schedule must still be a plain info line, not a failure.
	buf.Reset()
	rep = &doctorReport{w: &buf}
	cfg.Scheduling.CronExpression = "0 19-23 * * 1-5"
	checkAccountInfo(rep)
	if rep.fails != 0 {
		t.Errorf("doctor failed a valid cron schedule:\n%s", buf.String())
	}
}
