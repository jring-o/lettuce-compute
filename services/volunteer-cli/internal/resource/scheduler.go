package resource

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// Scheduler determines whether the daemon should be active based on the
// configured scheduling mode (ALWAYS, WHEN_IDLE, SCHEDULED).
type Scheduler struct {
	mode              string
	idleThresholdMins int
	cronExpr          string
	scheduleRanges    []config.ScheduleRange
	logger            *slog.Logger
	nowFunc           func() time.Time   // injectable for tests
	idleFunc          func() (int, error) // injectable for tests
	pollInterval      time.Duration       // overridable for tests
}

// NewScheduler creates a Scheduler from the config scheduling section.
func NewScheduler(cfg *config.Scheduling, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		mode:              cfg.Mode,
		idleThresholdMins: cfg.IdleThresholdMins,
		cronExpr:          cfg.CronExpression,
		scheduleRanges:    cfg.ScheduleRanges,
		logger:            logger,
		nowFunc:           time.Now,
		idleFunc:          GetIdleSeconds,
		pollInterval:      10 * time.Second,
	}
}

// SetIdleFunc replaces the idle detection function (for testing from other packages).
func (s *Scheduler) SetIdleFunc(fn func() (int, error)) {
	s.idleFunc = fn
}

// ShouldRun checks if the daemon should be active right now.
func (s *Scheduler) ShouldRun() bool {
	switch s.mode {
	case "ALWAYS":
		return true

	case "WHEN_IDLE":
		idle, err := s.idleFunc()
		if err != nil {
			s.logger.Warn("idle detection failed, assuming not idle", "error", err)
			return false
		}
		thresholdSecs := s.idleThresholdMins * 60
		return idle >= thresholdSecs

	case "SCHEDULED":
		now := s.nowFunc()
		// Prefer schedule ranges (from desktop visual builder) over cron.
		if len(s.scheduleRanges) > 0 {
			return matchesScheduleRanges(s.scheduleRanges, now)
		}
		if s.cronExpr == "" {
			s.logger.Warn("no cron expression or schedule ranges for SCHEDULED mode")
			return false
		}
		match, err := matchesCron(s.cronExpr, now)
		if err != nil {
			s.logger.Warn("cron expression parse error", "error", err, "expr", s.cronExpr)
			return false
		}
		return match

	default:
		s.logger.Warn("unknown scheduling mode, defaulting to always", "mode", s.mode)
		return true
	}
}

// WaitUntilActive blocks until the scheduler says the daemon should run.
// It polls every 10 seconds. Returns immediately if already active.
// Returns error if the context is cancelled.
func (s *Scheduler) WaitUntilActive(ctx context.Context) error {
	if s.ShouldRun() {
		return nil
	}

	s.logger.Info("waiting for schedule to become active", "mode", s.mode)

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if s.ShouldRun() {
				s.logger.Info("schedule became active")
				return nil
			}
		}
	}
}

// matchesCron checks if the given time matches a cron expression.
// Format: minute hour day-of-month month day-of-week
// Supports: * (any), N (value), N-M (range), N,M (list), */N (step), N-M/S (range+step).
func matchesCron(expr string, t time.Time) (bool, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false, fmt.Errorf("cron expression must have 5 fields, got %d", len(fields))
	}

	checks := []struct {
		value    int
		min, max int
	}{
		{t.Minute(), 0, 59},
		{t.Hour(), 0, 23},
		{t.Day(), 1, 31},
		{int(t.Month()), 1, 12},
		{int(t.Weekday()), 0, 6}, // Sunday = 0
	}

	for i, check := range checks {
		allowed, err := parseCronField(fields[i], check.min, check.max)
		if err != nil {
			return false, fmt.Errorf("field %d (%q): %w", i, fields[i], err)
		}
		found := false
		for _, v := range allowed {
			if v == check.value {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}

	return true, nil
}

// matchesScheduleRanges checks if the current time falls within any of the
// configured schedule ranges. Uses Monday=0 convention from the desktop app,
// while Go's time.Weekday() uses Sunday=0, so we convert.
func matchesScheduleRanges(ranges []config.ScheduleRange, t time.Time) bool {
	// Convert Go weekday (Sun=0) to our convention (Mon=0, Sun=6).
	goDay := int(t.Weekday())
	day := (goDay + 6) % 7 // Mon=0, Tue=1, ..., Sun=6
	hour := t.Hour()

	for _, r := range ranges {
		dayMatch := false
		for _, d := range r.Days {
			if d == day {
				dayMatch = true
				break
			}
		}
		if !dayMatch {
			continue
		}

		if r.StartHour == r.EndHour {
			// Same start and end = all 24 hours
			return true
		} else if r.StartHour < r.EndHour {
			// Non-wrapping: e.g., 08:00-18:00
			if hour >= r.StartHour && hour < r.EndHour {
				return true
			}
		} else {
			// Wrapping: e.g., 22:00-06:00 means 22,23,0,1,2,3,4,5
			if hour >= r.StartHour || hour < r.EndHour {
				return true
			}
		}
	}
	return false
}

// parseCronField parses a single cron field into a set of matching values.
func parseCronField(field string, min, max int) ([]int, error) {
	var values []int

	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)

		if part == "*" {
			for i := min; i <= max; i++ {
				values = append(values, i)
			}
			continue
		}

		// */step
		if strings.HasPrefix(part, "*/") {
			step, err := strconv.Atoi(part[2:])
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("invalid step: %s", part)
			}
			for i := min; i <= max; i += step {
				values = append(values, i)
			}
			continue
		}

		// range with optional step: N-M or N-M/S
		if strings.Contains(part, "-") {
			rangeParts := strings.SplitN(part, "/", 2)
			bounds := strings.SplitN(rangeParts[0], "-", 2)
			lo, err := strconv.Atoi(bounds[0])
			if err != nil {
				return nil, fmt.Errorf("invalid range start: %s", part)
			}
			hi, err := strconv.Atoi(bounds[1])
			if err != nil {
				return nil, fmt.Errorf("invalid range end: %s", part)
			}
			step := 1
			if len(rangeParts) > 1 {
				step, err = strconv.Atoi(rangeParts[1])
				if err != nil || step <= 0 {
					return nil, fmt.Errorf("invalid range step: %s", part)
				}
			}
			for i := lo; i <= hi; i += step {
				values = append(values, i)
			}
			continue
		}

		// single value
		v, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid value: %s", part)
		}
		values = append(values, v)
	}

	return values, nil
}
