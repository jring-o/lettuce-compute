// Package cron parses and matches the 5-field cron expressions accepted by
// scheduling.cron_expression.
//
// It exists as its own leaf package so the ONE parser is reachable from both
// ends: the resource scheduler, which matches an expression every poll, and
// config validation, which must refuse an unparseable expression where it
// enters (TB-3). Keeping a second copy in the config package would let the two
// drift, and a validator that accepts what the scheduler rejects re-opens the
// bug it was written to close.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// fields describes the five cron fields in order, with the values each accepts.
//
// Day-of-week accepts 7 as well as 0. Both mean Sunday in standard cron and in
// the online cron builders a volunteer is most likely to copy from, so refusing 7
// would reject a correct expression — while silently never matching it, which is
// what this code used to do, is worse still. normalizeDayOfWeek folds it onto
// Go's Sunday=0.
var fields = []struct {
	name     string
	min, max int
}{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 7},
}

// parse turns a whole expression into the allowed value set of each field.
func parse(expr string) ([][]int, error) {
	split := strings.Fields(expr)
	if len(split) != len(fields) {
		return nil, fmt.Errorf("a cron expression has 5 space-separated fields (minute hour day-of-month month day-of-week), got %d", len(split))
	}
	out := make([][]int, len(fields))
	for i, f := range fields {
		allowed, err := ParseField(split[i], f.min, f.max)
		if err != nil {
			return nil, fmt.Errorf("%s field %q: %w", f.name, split[i], err)
		}
		if f.name == "day-of-week" {
			allowed = normalizeDayOfWeek(allowed)
		}
		out[i] = allowed
	}
	return out, nil
}

// normalizeDayOfWeek rewrites the second spelling of Sunday (7) as Go's (0).
func normalizeDayOfWeek(allowed []int) []int {
	out := make([]int, 0, len(allowed))
	for _, v := range allowed {
		if v == 7 {
			v = 0
		}
		out = append(out, v)
	}
	return out
}

// Matches reports whether t falls in the window described by expr.
// Format: minute hour day-of-month month day-of-week.
// Supports: * (any), N (value), N-M (range), N,M (list), */N (step), N-M/S (range+step).
func Matches(expr string, t time.Time) (bool, error) {
	allowed, err := parse(expr)
	if err != nil {
		return false, err
	}

	now := []int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	for i, value := range now {
		found := false
		for _, v := range allowed[i] {
			if v == value {
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

// Validate reports whether expr is a cron expression the scheduler can act on,
// without reference to any particular instant. It is exactly the parse Matches
// performs, so anything Validate accepts the scheduler can evaluate — an
// expression that only fails at poll time is what made a bad cron silently
// unrunnable (TB-3).
func Validate(expr string) error {
	_, err := parse(expr)
	return err
}

// ParseField parses a single cron field into the set of values it matches.
//
// Every returned value is guaranteed to lie within [min,max] and the set is
// guaranteed non-empty. Both guarantees are load-bearing: this parser used to
// accept "99" as a minute, "25" as an hour and "22-2" as a reversed hour range,
// each of which produced a field that matches NOTHING. The expression then
// validated cleanly and the volunteer never ran — the exact failure TB-3 is
// about, reached by a different door.
func ParseField(field string, min, max int) ([]int, error) {
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
			// A backwards range produced no values at all and matched nothing,
			// forever. This parser does not wrap, so say how to write the
			// intent instead of accepting a range that can never fire.
			if hi < lo {
				return nil, fmt.Errorf("range %s runs backwards; write a wrapping range as two parts, e.g. 22-2 as 22-%d,%d-2", part, max, min)
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

	// Out-of-range values used to survive parsing and simply never match.
	for _, v := range values {
		if v < min || v > max {
			return nil, fmt.Errorf("%d is out of range (allowed: %d-%d)", v, min, max)
		}
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("matches no values")
	}

	return values, nil
}
