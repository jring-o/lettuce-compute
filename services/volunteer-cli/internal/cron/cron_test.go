package cron

import (
	"strings"
	"testing"
	"time"
)

func TestValidateAcceptsRealCronExpressions(t *testing.T) {
	for _, expr := range []string{
		"* * * * *",
		"0 * * * *",
		"30 2 * * *",
		"*/15 * * * *",
		"0 19-23 * * 1-5",
		"0 0 1 1 0",
		"0,30 8-18/2 * * *",
		"  0   *   *   *   *  ", // surrounding and repeated whitespace
		"0 2 * * 7",             // 7 is the other standard spelling of Sunday
	} {
		if err := Validate(expr); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", expr, err)
		}
	}
}

// TestValidateRejectsWhatIsNotACronExpression covers TB-3: an answer that is not a
// 5-field cron — the tester typed the `schedule` command's own flags at init's
// "Cron expression" prompt — was stored verbatim and only failed later, invisibly,
// on every scheduler poll.
func TestValidateRejectsWhatIsNotACronExpression(t *testing.T) {
	for _, expr := range []string{
		"--from 06:00 --to 04:00 --days mon-fri", // the tester's exact answer
		"20:00-06:00",
		"every night",
		"* * * *",       // four fields
		"* * * * * *",   // six fields
		"abc * * * *",   // unparseable minute
		"*/0 * * * *",   // zero step
		"0-abc * * * *", // broken range
		"",
	} {
		if err := Validate(expr); err == nil {
			t.Errorf("Validate(%q) = nil, want an error", expr)
		}
	}
}

// TestValidateRejectsExpressionsThatCanNeverFire covers the second door into
// TB-3's failure, found while hardening the first: this parser never checked that
// a field's values were within that field's range, and never checked that a field
// matched anything at all. So "99 * * * *" (minute 99), "0 25 * * *" (hour 25),
// "0 2 32 * *" (day 32) and "0 22-2 * * *" (a backwards hour range, which yields
// an EMPTY set) all parsed cleanly and then matched no instant, ever — a
// validated-looking config whose volunteer silently never runs, which is the
// whole of the bug.
func TestValidateRejectsExpressionsThatCanNeverFire(t *testing.T) {
	// A Sunday at 02:00 — chosen so a correct "0 2 * * 7" would match here.
	when := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)

	for _, expr := range []string{
		"99 * * * *",   // minute out of range
		"0 25 * * *",   // hour out of range
		"0 2 32 * *",   // day-of-month out of range
		"0 2 * 13 *",   // month out of range
		"0 2 * * 8",    // day-of-week out of range (7 is Sunday; 8 is nothing)
		"0 22-2 * * *", // backwards range: matches nothing
		"0-70 * * * *", // range end out of bounds
	} {
		if err := Validate(expr); err == nil {
			t.Errorf("Validate(%q) = nil, but this expression can never fire", expr)
			continue
		}
		// Matches must agree, so nothing slips past validation and dies at poll time.
		if _, err := Matches(expr, when); err == nil {
			t.Errorf("Matches(%q) returned no error; it must refuse what Validate refuses", expr)
		}
	}
}

// TestSundayIsSevenOrZero: standard cron, and every cron builder a volunteer is
// likely to copy from, spells Sunday as either 0 or 7. This parser accepted 7 and
// then never matched it — a CORRECT expression that silently never ran.
func TestSundayIsSevenOrZero(t *testing.T) {
	sunday := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	if sunday.Weekday() != time.Sunday {
		t.Fatalf("test date is a %v, not a Sunday", sunday.Weekday())
	}
	for _, expr := range []string{"0 2 * * 0", "0 2 * * 7", "0 2 * * 6-7"} {
		match, err := Matches(expr, sunday)
		if err != nil {
			t.Errorf("Matches(%q) error: %v", expr, err)
			continue
		}
		if !match {
			t.Errorf("Matches(%q) on a Sunday = false, want true", expr)
		}
	}
	// And a weekday-only expression must still not match a Sunday.
	if match, err := Matches("0 2 * * 1-5", sunday); err != nil || match {
		t.Errorf("Matches(\"0 2 * * 1-5\") on a Sunday = %v/%v, want false/nil", match, err)
	}
}

// TestValidateNamesTheOffendingField keeps the error usable by a non-developer:
// it must say which of the five fields is wrong, not just that something is.
func TestValidateNamesTheOffendingField(t *testing.T) {
	err := Validate("0 abc * * *")
	if err == nil {
		t.Fatal("expected an error for a bad hour field")
	}
	if !strings.Contains(err.Error(), "hour") {
		t.Errorf("error %q does not name the hour field", err)
	}
}

// TestValidateAgreesWithMatches is the anti-drift guard that motivates this
// package existing at all: anything Validate accepts, Matches must be able to
// evaluate, and anything Validate rejects, Matches must refuse. A validator that
// disagreed with the runtime parser would re-open TB-3 by letting a config pass
// validation and then fail on every poll.
func TestValidateAgreesWithMatches(t *testing.T) {
	now := time.Date(2026, 7, 27, 21, 30, 0, 0, time.UTC)
	exprs := []string{
		"* * * * *", "30 21 * * *", "*/15 * * * *", "0 19-23 * * 1-5", "0 2 * * 7",
		"--from 06:00 --to 04:00 --days mon-fri", "20:00-06:00", "* * * *",
		"abc * * * *", "*/0 * * * *", "", "99 * * * *", "0 22-2 * * *", "0 2 * * 8",
	}
	for _, expr := range exprs {
		validErr := Validate(expr)
		_, matchErr := Matches(expr, now)
		if (validErr == nil) != (matchErr == nil) {
			t.Errorf("disagreement on %q: Validate=%v, Matches=%v", expr, validErr, matchErr)
		}
	}
}
