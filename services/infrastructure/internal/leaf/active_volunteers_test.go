package leaf

import (
	"fmt"
	"strings"
	"testing"
)

// The real counting behaviour is exercised by the DB-backed stats tests
// (engine_test.go / handler_test.go). These unit tests guard the SQL shape so a
// future edit can't silently revert the rolling window back to live-copy-only
// counting — the regression that made long-running, thin-pool leaves read "0
// active volunteers" while a volunteer was actively crunching them.

func TestActiveVolunteerWindowIsPositive(t *testing.T) {
	if ActiveVolunteerWindowMinutes <= 0 {
		t.Fatalf("ActiveVolunteerWindowMinutes = %d, want > 0", ActiveVolunteerWindowMinutes)
	}
}

func TestActiveVolunteerSubqueryShape(t *testing.T) {
	sql := ActiveVolunteerSubquery()
	wantFragments := []string{
		"COUNT(DISTINCT h.volunteer_id)",
		"work_unit_assignment_history",
		"h.outcome IS NULL",            // currently-live copies
		"h.outcome_at >= now() -",      // recently-closed copies (the rolling window)
		"GROUP BY wu.leaf_id",
	}
	for _, f := range wantFragments {
		if !strings.Contains(sql, f) {
			t.Errorf("ActiveVolunteerSubquery() missing %q\ngot: %s", f, sql)
		}
	}
	// The window constant must actually appear in the generated SQL.
	if !strings.Contains(sql, fmt.Sprintf("mins => %d", ActiveVolunteerWindowMinutes)) {
		t.Errorf("ActiveVolunteerSubquery() does not interpolate the window constant %d\ngot: %s",
			ActiveVolunteerWindowMinutes, sql)
	}
}
