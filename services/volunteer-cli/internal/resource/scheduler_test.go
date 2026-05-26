package resource

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

func TestScheduler_ShouldRun_Always(t *testing.T) {
	s := NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, slog.Default())
	if !s.ShouldRun() {
		t.Error("ALWAYS mode should always return true")
	}
}

func TestScheduler_ShouldRun_WhenIdle_Idle(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:              "WHEN_IDLE",
		IdleThresholdMins: 5,
	}, slog.Default())
	// Inject mock: user has been idle for 10 minutes.
	s.idleFunc = func() (int, error) { return 600, nil }
	if !s.ShouldRun() {
		t.Error("WHEN_IDLE should return true when idle >= threshold")
	}
}

func TestScheduler_ShouldRun_WhenIdle_Active(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:              "WHEN_IDLE",
		IdleThresholdMins: 5,
	}, slog.Default())
	// Inject mock: user was active 30 seconds ago.
	s.idleFunc = func() (int, error) { return 30, nil }
	if s.ShouldRun() {
		t.Error("WHEN_IDLE should return false when idle < threshold")
	}
}

func TestScheduler_ShouldRun_Scheduled_InWindow(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:           "SCHEDULED",
		CronExpression: "* 22-23 * * *", // 22:00 - 23:59 every day
	}, slog.Default())
	// Fix time to 22:30.
	s.nowFunc = func() time.Time {
		return time.Date(2026, 3, 13, 22, 30, 0, 0, time.UTC)
	}
	if !s.ShouldRun() {
		t.Error("SCHEDULED should return true during active window")
	}
}

func TestScheduler_ShouldRun_Scheduled_OutsideWindow(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:           "SCHEDULED",
		CronExpression: "* 22-23 * * *",
	}, slog.Default())
	// Fix time to 10:30.
	s.nowFunc = func() time.Time {
		return time.Date(2026, 3, 13, 10, 30, 0, 0, time.UTC)
	}
	if s.ShouldRun() {
		t.Error("SCHEDULED should return false outside active window")
	}
}

func TestScheduler_ShouldRun_Scheduled_Weekdays(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:           "SCHEDULED",
		CronExpression: "* 22-23 * * 1-5", // weekdays only
	}, slog.Default())
	// Wednesday at 22:30.
	s.nowFunc = func() time.Time {
		return time.Date(2026, 3, 11, 22, 30, 0, 0, time.UTC) // Wednesday
	}
	if !s.ShouldRun() {
		t.Error("SCHEDULED should return true on weekday in window")
	}
	// Sunday at 22:30.
	s.nowFunc = func() time.Time {
		return time.Date(2026, 3, 15, 22, 30, 0, 0, time.UTC) // Sunday
	}
	if s.ShouldRun() {
		t.Error("SCHEDULED should return false on weekend")
	}
}

func TestScheduler_WaitUntilActive_AlreadyActive(t *testing.T) {
	s := NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, slog.Default())
	ctx := context.Background()
	if err := s.WaitUntilActive(ctx); err != nil {
		t.Errorf("WaitUntilActive should return nil when already active: %v", err)
	}
}

func TestScheduler_WaitUntilActive_ContextCancelled(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:              "WHEN_IDLE",
		IdleThresholdMins: 5,
	}, slog.Default())
	// Never idle.
	s.idleFunc = func() (int, error) { return 0, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := s.WaitUntilActive(ctx)
	if err == nil {
		t.Error("WaitUntilActive should return error when context cancelled")
	}
}

func TestMatchesCron_AllWildcards(t *testing.T) {
	now := time.Now()
	match, err := matchesCron("* * * * *", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Error("all wildcards should match any time")
	}
}

func TestMatchesCron_SpecificMinute(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 30, 0, 0, time.UTC)
	match, err := matchesCron("30 * * * *", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Error("should match minute 30")
	}

	match, err = matchesCron("15 * * * *", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match {
		t.Error("should not match minute 15")
	}
}

func TestMatchesCron_StepValues(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) // minute 0
	match, err := matchesCron("*/15 * * * *", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Error("minute 0 should match */15")
	}

	now = time.Date(2026, 1, 1, 12, 7, 0, 0, time.UTC) // minute 7
	match, err = matchesCron("*/15 * * * *", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match {
		t.Error("minute 7 should not match */15")
	}
}

func TestMatchesCron_InvalidExpression(t *testing.T) {
	_, err := matchesCron("* * *", time.Now())
	if err == nil {
		t.Error("should error on 3-field expression")
	}
}

func TestParseCronField_List(t *testing.T) {
	vals, err := parseCronField("1,3,5", 0, 59)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := map[int]bool{1: true, 3: true, 5: true}
	for _, v := range vals {
		if !expected[v] {
			t.Errorf("unexpected value: %d", v)
		}
	}
	if len(vals) != 3 {
		t.Errorf("expected 3 values, got %d", len(vals))
	}
}

func TestParseCronField_RangeWithStep(t *testing.T) {
	vals, err := parseCronField("0-10/2", 0, 59)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []int{0, 2, 4, 6, 8, 10}
	if len(vals) != len(expected) {
		t.Fatalf("expected %d values, got %d", len(expected), len(vals))
	}
	for i, v := range vals {
		if v != expected[i] {
			t.Errorf("index %d: expected %d, got %d", i, expected[i], v)
		}
	}
}

// --- Additional coverage tests ---

func TestScheduler_ShouldRun_WhenIdle_IdleDetectionError(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:              "WHEN_IDLE",
		IdleThresholdMins: 5,
	}, slog.Default())
	// Inject mock: idle detection fails.
	s.idleFunc = func() (int, error) { return 0, fmt.Errorf("no display") }
	if s.ShouldRun() {
		t.Error("WHEN_IDLE should return false when idle detection fails")
	}
}

func TestScheduler_ShouldRun_WhenIdle_ExactThreshold(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:              "WHEN_IDLE",
		IdleThresholdMins: 5,
	}, slog.Default())
	// Idle for exactly 5 minutes = 300 seconds.
	s.idleFunc = func() (int, error) { return 300, nil }
	if !s.ShouldRun() {
		t.Error("WHEN_IDLE should return true when idle == threshold")
	}
}

func TestScheduler_ShouldRun_Scheduled_EmptyCronExpression(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:           "SCHEDULED",
		CronExpression: "",
	}, slog.Default())
	if s.ShouldRun() {
		t.Error("SCHEDULED should return false when cron expression is empty")
	}
}

func TestScheduler_ShouldRun_Scheduled_InvalidCronExpression(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:           "SCHEDULED",
		CronExpression: "invalid cron",
	}, slog.Default())
	if s.ShouldRun() {
		t.Error("SCHEDULED should return false when cron expression is invalid")
	}
}

func TestScheduler_ShouldRun_UnknownMode(t *testing.T) {
	s := NewScheduler(&config.Scheduling{Mode: "UNKNOWN_MODE"}, slog.Default())
	if !s.ShouldRun() {
		t.Error("unknown mode should default to always (return true)")
	}
}

func TestMatchesCron_MonthField(t *testing.T) {
	// March 15th should match month=3.
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	match, err := matchesCron("0 12 15 3 *", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Error("should match specific month/day/hour/minute")
	}

	// Same time in April should not match.
	now = time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	match, err = matchesCron("0 12 15 3 *", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match {
		t.Error("should not match wrong month")
	}
}

func TestMatchesCron_DayOfMonth(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	match, err := matchesCron("0 0 1 * *", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Error("should match 1st of month at midnight")
	}

	now = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	match, err = matchesCron("0 0 1 * *", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match {
		t.Error("should not match 2nd of month")
	}
}

func TestParseCronField_PlainRange(t *testing.T) {
	vals, err := parseCronField("3-7", 0, 59)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []int{3, 4, 5, 6, 7}
	if len(vals) != len(expected) {
		t.Fatalf("expected %d values, got %d", len(expected), len(vals))
	}
	for i, v := range vals {
		if v != expected[i] {
			t.Errorf("index %d: expected %d, got %d", i, expected[i], v)
		}
	}
}

func TestParseCronField_InvalidStep(t *testing.T) {
	_, err := parseCronField("*/abc", 0, 59)
	if err == nil {
		t.Error("should error on invalid step value")
	}
}

func TestParseCronField_ZeroStep(t *testing.T) {
	_, err := parseCronField("*/0", 0, 59)
	if err == nil {
		t.Error("should error on zero step value")
	}
}

func TestParseCronField_InvalidRangeStart(t *testing.T) {
	_, err := parseCronField("abc-5", 0, 59)
	if err == nil {
		t.Error("should error on invalid range start")
	}
}

func TestParseCronField_InvalidRangeEnd(t *testing.T) {
	_, err := parseCronField("0-abc", 0, 59)
	if err == nil {
		t.Error("should error on invalid range end")
	}
}

func TestParseCronField_InvalidRangeStep(t *testing.T) {
	_, err := parseCronField("0-10/abc", 0, 59)
	if err == nil {
		t.Error("should error on invalid range step")
	}
}

func TestParseCronField_ZeroRangeStep(t *testing.T) {
	_, err := parseCronField("0-10/0", 0, 59)
	if err == nil {
		t.Error("should error on zero range step")
	}
}

func TestParseCronField_InvalidSingleValue(t *testing.T) {
	_, err := parseCronField("xyz", 0, 59)
	if err == nil {
		t.Error("should error on non-numeric value")
	}
}

func TestMatchesCron_InvalidFieldInExpression(t *testing.T) {
	_, err := matchesCron("abc * * * *", time.Now())
	if err == nil {
		t.Error("should error on invalid field in expression")
	}
}

func TestScheduler_SetIdleFunc(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode:              "WHEN_IDLE",
		IdleThresholdMins: 5,
	}, slog.Default())
	// Default idle function is the real one; override it.
	s.SetIdleFunc(func() (int, error) { return 999, nil })
	if !s.ShouldRun() {
		t.Error("WHEN_IDLE should return true after SetIdleFunc returns high idle time")
	}
}

// --- Schedule ranges tests (S67) ---

func TestMatchesScheduleRanges_NonWrapping_InWindow(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0, 1, 2, 3, 4}, StartHour: 8, EndHour: 18}, // Mon-Fri 08:00-18:00
	}
	// Wednesday at 10:00 (Go Weekday: Wednesday=3, our convention: Wed=2)
	tm := time.Date(2026, 3, 18, 10, 0, 0, 0, time.UTC) // Wednesday
	if !matchesScheduleRanges(ranges, tm) {
		t.Error("Wednesday 10:00 should match Mon-Fri 08:00-18:00")
	}
}

func TestMatchesScheduleRanges_NonWrapping_OutsideWindow(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0, 1, 2, 3, 4}, StartHour: 8, EndHour: 18},
	}
	// Wednesday at 20:00
	tm := time.Date(2026, 3, 18, 20, 0, 0, 0, time.UTC)
	if matchesScheduleRanges(ranges, tm) {
		t.Error("Wednesday 20:00 should NOT match Mon-Fri 08:00-18:00")
	}
}

func TestMatchesScheduleRanges_NonWrapping_AtStartHour(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0}, StartHour: 8, EndHour: 18},
	}
	// Monday at 08:00 — should match (>= start)
	tm := time.Date(2026, 3, 16, 8, 0, 0, 0, time.UTC) // Monday
	if !matchesScheduleRanges(ranges, tm) {
		t.Error("Monday 08:00 should match (hour >= startHour)")
	}
}

func TestMatchesScheduleRanges_NonWrapping_AtEndHour(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0}, StartHour: 8, EndHour: 18},
	}
	// Monday at 18:00 — should NOT match (< end, not <=)
	tm := time.Date(2026, 3, 16, 18, 0, 0, 0, time.UTC)
	if matchesScheduleRanges(ranges, tm) {
		t.Error("Monday 18:00 should NOT match (hour must be < endHour)")
	}
}

func TestMatchesScheduleRanges_Wrapping_InLateWindow(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0, 1, 2, 3, 4}, StartHour: 22, EndHour: 6}, // 22:00-06:00
	}
	// Monday at 23:00
	tm := time.Date(2026, 3, 16, 23, 0, 0, 0, time.UTC)
	if !matchesScheduleRanges(ranges, tm) {
		t.Error("Monday 23:00 should match 22:00-06:00 wrapping range")
	}
}

func TestMatchesScheduleRanges_Wrapping_InEarlyWindow(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0, 1, 2, 3, 4}, StartHour: 22, EndHour: 6},
	}
	// Monday at 03:00
	tm := time.Date(2026, 3, 16, 3, 0, 0, 0, time.UTC)
	if !matchesScheduleRanges(ranges, tm) {
		t.Error("Monday 03:00 should match 22:00-06:00 wrapping range")
	}
}

func TestMatchesScheduleRanges_Wrapping_OutsideWindow(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0, 1, 2, 3, 4}, StartHour: 22, EndHour: 6},
	}
	// Monday at 12:00 — between 06:00 and 22:00, should NOT match
	tm := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	if matchesScheduleRanges(ranges, tm) {
		t.Error("Monday 12:00 should NOT match 22:00-06:00 wrapping range")
	}
}

func TestMatchesScheduleRanges_Wrapping_AtStartHour(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0}, StartHour: 22, EndHour: 6},
	}
	// Monday at 22:00 — should match
	tm := time.Date(2026, 3, 16, 22, 0, 0, 0, time.UTC)
	if !matchesScheduleRanges(ranges, tm) {
		t.Error("Monday 22:00 should match wrapping range (hour >= startHour)")
	}
}

func TestMatchesScheduleRanges_Wrapping_AtEndHour(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0}, StartHour: 22, EndHour: 6},
	}
	// Monday at 06:00 — should NOT match (< end, not <=)
	tm := time.Date(2026, 3, 16, 6, 0, 0, 0, time.UTC)
	if matchesScheduleRanges(ranges, tm) {
		t.Error("Monday 06:00 should NOT match wrapping range (hour must be < endHour)")
	}
}

func TestMatchesScheduleRanges_DayMismatch(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{5, 6}, StartHour: 0, EndHour: 23}, // Sat-Sun only
	}
	// Wednesday at 12:00
	tm := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC) // Wednesday
	if matchesScheduleRanges(ranges, tm) {
		t.Error("Wednesday should NOT match Sat-Sun range")
	}
}

func TestMatchesScheduleRanges_WeekendMatch(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{5, 6}, StartHour: 0, EndHour: 23}, // Sat-Sun only
	}
	// Saturday at 12:00 (Go Weekday: Saturday=6, our convention: Sat=5)
	tm := time.Date(2026, 3, 21, 12, 0, 0, 0, time.UTC) // Saturday
	if !matchesScheduleRanges(ranges, tm) {
		t.Error("Saturday 12:00 should match Sat-Sun range")
	}
}

func TestMatchesScheduleRanges_SundayMapping(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{6}, StartHour: 0, EndHour: 23}, // Sunday=6 in our convention
	}
	// Sunday at 12:00 (Go Weekday: Sunday=0, our convention: Sun=6)
	tm := time.Date(2026, 3, 22, 12, 0, 0, 0, time.UTC) // Sunday
	if !matchesScheduleRanges(ranges, tm) {
		t.Error("Sunday should map to day=6 in our convention")
	}
}

func TestMatchesScheduleRanges_MondayMapping(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0}, StartHour: 0, EndHour: 23}, // Monday=0 in our convention
	}
	// Monday at 12:00 (Go Weekday: Monday=1, our convention: Mon=0)
	tm := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC) // Monday
	if !matchesScheduleRanges(ranges, tm) {
		t.Error("Monday should map to day=0 in our convention")
	}
}

func TestMatchesScheduleRanges_EmptyRanges(t *testing.T) {
	ranges := []config.ScheduleRange{}
	tm := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	if matchesScheduleRanges(ranges, tm) {
		t.Error("empty ranges should never match")
	}
}

func TestMatchesScheduleRanges_EmptyDays(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{}, StartHour: 0, EndHour: 23},
	}
	tm := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	if matchesScheduleRanges(ranges, tm) {
		t.Error("range with empty days should never match")
	}
}

func TestMatchesScheduleRanges_MultipleRanges(t *testing.T) {
	ranges := []config.ScheduleRange{
		{Days: []int{0, 1, 2, 3, 4}, StartHour: 22, EndHour: 6},  // Weekday nights
		{Days: []int{5, 6}, StartHour: 0, EndHour: 23},            // All weekend
	}

	// Saturday at 14:00 — matches weekend range
	tm := time.Date(2026, 3, 21, 14, 0, 0, 0, time.UTC)
	if !matchesScheduleRanges(ranges, tm) {
		t.Error("Saturday 14:00 should match weekend range")
	}

	// Monday at 23:00 — matches weekday night range
	tm = time.Date(2026, 3, 16, 23, 0, 0, 0, time.UTC)
	if !matchesScheduleRanges(ranges, tm) {
		t.Error("Monday 23:00 should match weekday night range")
	}

	// Monday at 14:00 — matches neither
	tm = time.Date(2026, 3, 16, 14, 0, 0, 0, time.UTC)
	if matchesScheduleRanges(ranges, tm) {
		t.Error("Monday 14:00 should NOT match any range")
	}
}

func TestScheduler_ShouldRun_Scheduled_WithScheduleRanges(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode: "SCHEDULED",
		ScheduleRanges: []config.ScheduleRange{
			{Days: []int{0, 1, 2, 3, 4}, StartHour: 22, EndHour: 6},
		},
	}, slog.Default())
	// Monday at 23:00
	s.nowFunc = func() time.Time {
		return time.Date(2026, 3, 16, 23, 0, 0, 0, time.UTC)
	}
	if !s.ShouldRun() {
		t.Error("SCHEDULED with schedule ranges should return true when in active window")
	}
}

func TestScheduler_ShouldRun_Scheduled_WithScheduleRanges_Outside(t *testing.T) {
	s := NewScheduler(&config.Scheduling{
		Mode: "SCHEDULED",
		ScheduleRanges: []config.ScheduleRange{
			{Days: []int{0, 1, 2, 3, 4}, StartHour: 22, EndHour: 6},
		},
	}, slog.Default())
	// Monday at 12:00
	s.nowFunc = func() time.Time {
		return time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	}
	if s.ShouldRun() {
		t.Error("SCHEDULED with schedule ranges should return false when outside active window")
	}
}

func TestScheduler_ShouldRun_Scheduled_RangesPrioritizedOverCron(t *testing.T) {
	// When both are set, ranges take priority
	s := NewScheduler(&config.Scheduling{
		Mode:           "SCHEDULED",
		CronExpression: "* * * * *", // would always match
		ScheduleRanges: []config.ScheduleRange{
			{Days: []int{5, 6}, StartHour: 0, EndHour: 23}, // weekends only
		},
	}, slog.Default())
	// Monday at 12:00 — cron would match, but ranges should take priority
	s.nowFunc = func() time.Time {
		return time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC) // Monday
	}
	if s.ShouldRun() {
		t.Error("schedule ranges should take priority over cron; Monday should not match weekend-only ranges")
	}
}

func TestScheduler_WaitUntilActive_BecomesActive(t *testing.T) {
	callCount := 0
	s := NewScheduler(&config.Scheduling{
		Mode:              "WHEN_IDLE",
		IdleThresholdMins: 5,
	}, slog.Default())
	s.pollInterval = 50 * time.Millisecond
	// First two calls: not idle. Third call: idle.
	// Note: ShouldRun() is called once before entering the loop, plus once per tick.
	s.idleFunc = func() (int, error) {
		callCount++
		if callCount >= 3 {
			return 600, nil
		}
		return 0, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.WaitUntilActive(ctx)
	if err != nil {
		t.Errorf("WaitUntilActive should return nil when schedule becomes active: %v", err)
	}
	if callCount < 3 {
		t.Errorf("expected at least 3 idle calls, got %d", callCount)
	}
}
