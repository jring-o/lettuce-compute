package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A run whose result the head did not need (the unit was already finalized) is
// recorded with "outcome":"not_needed". `history` must say so in the row and
// count it apart in the footer, rather than showing it as a head rejection. The
// lines are written raw so the test runs unchanged against the code before the
// outcome field existed.
func TestHistoryShowsAndCountsNotNeededRuns(t *testing.T) {
	dir := t.TempDir()
	lines := `{"work_unit_id":"unit-acc","leaf_id":"leaf-a","leaf_name":"Alpha","server_name":"Head","completed_at":"2026-09-25T10:00:00Z","wall_clock_seconds":100,"cpu_seconds":100,"result_accepted":true}` + "\n" +
		`{"work_unit_id":"unit-rej","leaf_id":"leaf-a","leaf_name":"Alpha","server_name":"Head","completed_at":"2026-09-25T11:00:00Z","wall_clock_seconds":100,"cpu_seconds":100,"result_accepted":false}` + "\n" +
		`{"work_unit_id":"unit-notneed","leaf_id":"leaf-a","leaf_name":"Alpha","server_name":"Head","completed_at":"2026-09-25T12:00:00Z","wall_clock_seconds":100,"cpu_seconds":100,"result_accepted":false,"outcome":"not_needed"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}

	out := tb46Run(t, dir, 0)

	rows := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		for _, unit := range []string{"unit-acc", "unit-rej", "unit-notneed"} {
			if strings.HasPrefix(line, unit) {
				rows[unit] = line
			}
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(rows["unit-notneed"]), "not needed") {
		t.Errorf("the not-needed run's HEAD ACCEPTED cell must say \"not needed\"; row: %q\n%s", rows["unit-notneed"], out)
	}
	if !strings.HasSuffix(strings.TrimSpace(rows["unit-rej"]), "no") {
		t.Errorf("a head rejection still shows \"no\"; row: %q", rows["unit-rej"])
	}
	if !strings.Contains(out, "Showing 3 of 3 completed units; the head accepted 1 on submission.") {
		t.Errorf("footer must still count every run and the accepted ones; got:\n%s", out)
	}
	if !strings.Contains(out, "1 was not needed: the head had already finalized the unit") {
		t.Errorf("footer must count the not-needed run apart and say why; got:\n%s", out)
	}
}
