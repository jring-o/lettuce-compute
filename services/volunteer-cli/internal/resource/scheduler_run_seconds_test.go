package resource

import (
	"log/slog"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TestRunSecondsWithin: how much of a window the schedule lets work run —
// the deadline check counts a unit's deadline in these hours.
func TestRunSecondsWithin(t *testing.T) {
	everyDay := []int{0, 1, 2, 3, 4, 5, 6}
	// Monday 18:30 local.
	from := time.Date(2026, 9, 28, 18, 30, 0, 0, time.Local)

	for _, tc := range []struct {
		name  string
		sched config.Scheduling
		d     time.Duration
		want  time.Duration
		known bool
	}{
		{"always", config.Scheduling{Mode: "ALWAYS"}, 6 * time.Hour, 6 * time.Hour, true},
		{"overnight window, 20:00-06:00", config.Scheduling{Mode: "SCHEDULED",
			ScheduleRanges: []config.ScheduleRange{{Days: everyDay, StartHour: 20, EndHour: 6}}}, 6 * time.Hour, 4*time.Hour + 30*time.Minute, true},
		{"window already open, 18:00-20:00", config.Scheduling{Mode: "SCHEDULED",
			ScheduleRanges: []config.ScheduleRange{{Days: everyDay, StartHour: 18, EndHour: 20}}}, 6 * time.Hour, 90 * time.Minute, true},
		{"weekend only, from a Monday", config.Scheduling{Mode: "SCHEDULED",
			ScheduleRanges: []config.ScheduleRange{{Days: []int{5, 6}, StartHour: 0, EndHour: 0}}}, 6 * time.Hour, 0, true},
		{"cron, 22:00-23:59", config.Scheduling{Mode: "SCHEDULED", CronExpression: "* 22-23 * * *"}, 6 * time.Hour, 2 * time.Hour, true},
		{"when idle cannot be told", config.Scheduling{Mode: "WHEN_IDLE", IdleThresholdMins: 5}, 6 * time.Hour, 6 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewScheduler(&tc.sched, slog.Default())
			got, known := s.RunSecondsWithin(from, tc.d)
			if time.Duration(got*float64(time.Second)) != tc.want || known != tc.known {
				t.Errorf("RunSecondsWithin = %v (known %v), want %v (known %v)", time.Duration(got*float64(time.Second)), known, tc.want, tc.known)
			}
		})
	}
}
