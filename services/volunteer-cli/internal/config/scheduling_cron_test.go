package config

import (
	"strings"
	"testing"
)

// theTestersAnswer is what a beta tester typed at init's bare "Cron expression"
// prompt: the flags of the `schedule` command, which is a perfectly reasonable
// guess at what the prompt wanted. It was stored verbatim and the volunteer never
// ran again (TB-3).
const theTestersAnswer = "--from 06:00 --to 04:00 --days mon-fri"

// TestSchedulingValidateRejectsUnparseableCron is the TB-3 write-path regression:
// validation used to check only that cron_expression was NON-EMPTY, so every path
// that writes a config — init, `config set`, the management API — accepted a
// string the scheduler could never parse.
func TestSchedulingValidateRejectsUnparseableCron(t *testing.T) {
	s := Scheduling{Mode: "SCHEDULED", CronExpression: theTestersAnswer}
	err := s.Validate()
	if err == nil {
		t.Fatalf("Scheduling.Validate() accepted %q, want an error", theTestersAnswer)
	}
	if !strings.Contains(err.Error(), "cron_expression") {
		t.Errorf("error %q does not name the offending field", err)
	}
	// The remedy has to be actionable for someone who is not a developer.
	if !strings.Contains(err.Error(), "schedule set") {
		t.Errorf("error %q does not point at the command that fixes it", err)
	}

	// A real expression still passes.
	ok := Scheduling{Mode: "SCHEDULED", CronExpression: "0 19-23 * * 1-5"}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate() rejected a valid cron expression: %v", err)
	}
}

// TestConfigValidateRejectsUnparseableCron checks the same through the whole-config
// entry point that init and `config set` actually call.
func TestConfigValidateRejectsUnparseableCron(t *testing.T) {
	c := Defaults()
	c.Scheduling.Mode = "SCHEDULED"
	c.Scheduling.CronExpression = theTestersAnswer
	if err := c.Validate(); err == nil {
		t.Fatalf("Config.Validate() accepted cron %q, want an error", theTestersAnswer)
	}
}

// TestSchedulingValidateChecksCronEvenWhenRangesWin guards the quiet case: schedule
// ranges take precedence at runtime, so a broken cron sitting behind them runs
// nothing today and detonates the day the ranges are removed.
func TestSchedulingValidateChecksCronEvenWhenRangesWin(t *testing.T) {
	s := Scheduling{
		Mode:           "SCHEDULED",
		CronExpression: theTestersAnswer,
		ScheduleRanges: []ScheduleRange{{Days: []int{0, 1, 2, 3, 4}, StartHour: 19, EndHour: 7}},
	}
	if err := s.Validate(); err == nil {
		t.Error("Validate() accepted a broken cron expression because ranges were present")
	}
}

// TestSchedulingNeverRuns is the read-path predicate used by `start` and `doctor`.
// It must mirror the scheduler's SCHEDULED branch exactly — flagging only a
// schedule that can genuinely never become active, so nothing that works today is
// newly refused.
func TestSchedulingNeverRuns(t *testing.T) {
	dead := []struct {
		name string
		s    Scheduling
	}{
		{"unparseable cron, no ranges", Scheduling{Mode: "SCHEDULED", CronExpression: theTestersAnswer}},
		{"SCHEDULED with nothing configured", Scheduling{Mode: "SCHEDULED"}},
	}
	for _, tc := range dead {
		if err := tc.s.NeverRuns(); err == nil {
			t.Errorf("%s: NeverRuns() = nil, want a reason", tc.name)
		}
	}

	alive := []struct {
		name string
		s    Scheduling
	}{
		{"ALWAYS", Scheduling{Mode: "ALWAYS"}},
		{"WHEN_IDLE", Scheduling{Mode: "WHEN_IDLE", IdleThresholdMins: 5}},
		{"valid cron", Scheduling{Mode: "SCHEDULED", CronExpression: "0 19-23 * * 1-5"}},
		{"ranges win over a broken cron", Scheduling{
			Mode:           "SCHEDULED",
			CronExpression: theTestersAnswer,
			ScheduleRanges: []ScheduleRange{{Days: []int{0}, StartHour: 19, EndHour: 7}},
		}},
		// A mode the config file spells oddly still runs today (the scheduler
		// defaults an unknown mode to always), so the read path must not block it.
		{"unknown mode falls through to always", Scheduling{Mode: "always"}},
	}
	for _, tc := range alive {
		if err := tc.s.NeverRuns(); err != nil {
			t.Errorf("%s: NeverRuns() = %v, want nil", tc.name, err)
		}
	}
}
