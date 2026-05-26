package config

import (
	"testing"
)

func TestValidate_ScheduleRanges_ValidNonWrapping(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0, 1, 2}, StartHour: 8, EndHour: 18},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid non-wrapping range should pass validation: %v", err)
	}
}

func TestValidate_ScheduleRanges_ValidWrapping(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0, 1, 2, 3, 4}, StartHour: 22, EndHour: 6},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid wrapping range (22-6) should pass validation: %v", err)
	}
}

func TestValidate_ScheduleRanges_AllDays(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHour: 0, EndHour: 23},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid all-days range should pass validation: %v", err)
	}
}

func TestValidate_ScheduleRanges_InvalidStartHourNegative(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0}, StartHour: -1, EndHour: 6},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("start_hour -1 should fail validation")
	}
}

func TestValidate_ScheduleRanges_InvalidStartHourTooHigh(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0}, StartHour: 24, EndHour: 6},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("start_hour 24 should fail validation")
	}
}

func TestValidate_ScheduleRanges_InvalidEndHourNegative(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0}, StartHour: 8, EndHour: -1},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("end_hour -1 should fail validation")
	}
}

func TestValidate_ScheduleRanges_InvalidEndHourTooHigh(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0}, StartHour: 8, EndHour: 24},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("end_hour 24 should fail validation")
	}
}

func TestValidate_ScheduleRanges_InvalidDayNegative(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{-1}, StartHour: 8, EndHour: 18},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("day -1 should fail validation")
	}
}

func TestValidate_ScheduleRanges_InvalidDayTooHigh(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{7}, StartHour: 8, EndHour: 18},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("day 7 should fail validation")
	}
}

func TestValidate_ScheduleRanges_BoundaryDays(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0, 6}, StartHour: 8, EndHour: 18},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("days 0 and 6 should be valid: %v", err)
	}
}

func TestValidate_ScheduleRanges_BoundaryHours(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0}, StartHour: 0, EndHour: 23},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("hours 0 and 23 should be valid: %v", err)
	}
}

func TestValidate_ScheduledMode_WithRangesNoCron(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.CronExpression = ""
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0}, StartHour: 8, EndHour: 18},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("SCHEDULED with ranges and no cron should pass: %v", err)
	}
}

func TestValidate_ScheduledMode_WithCronNoRanges(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.CronExpression = "* 22-23 * * *"
	cfg.Scheduling.ScheduleRanges = nil
	if err := cfg.Validate(); err != nil {
		t.Errorf("SCHEDULED with cron and no ranges should pass: %v", err)
	}
}

func TestValidate_ScheduledMode_NoCronNoRanges(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.CronExpression = ""
	cfg.Scheduling.ScheduleRanges = nil
	if err := cfg.Validate(); err == nil {
		t.Error("SCHEDULED with no cron and no ranges should fail validation")
	}
}

func TestValidate_ScheduleRanges_MultipleRanges(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0, 1, 2, 3, 4}, StartHour: 22, EndHour: 6},
		{Days: []int{5, 6}, StartHour: 0, EndHour: 23},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("multiple valid ranges should pass validation: %v", err)
	}
}

func TestValidate_ScheduleRanges_SecondRangeInvalid(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{0}, StartHour: 8, EndHour: 18},
		{Days: []int{8}, StartHour: 8, EndHour: 18}, // day 8 is invalid
	}
	if err := cfg.Validate(); err == nil {
		t.Error("second range with invalid day should fail validation")
	}
}

func TestValidate_ScheduleRanges_EmptyDays(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.ScheduleRanges = []ScheduleRange{
		{Days: []int{}, StartHour: 8, EndHour: 18},
	}
	// Empty days is valid (range just won't match anything)
	if err := cfg.Validate(); err != nil {
		t.Errorf("empty days should pass validation: %v", err)
	}
}
