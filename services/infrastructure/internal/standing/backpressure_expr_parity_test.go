//go:build integration

package standing

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// TestEffectiveStandingExpr_MatchesEffectiveStanding pins the two twins of the effective
// account standing (BG-24b) against each other: the Go source of truth
// volunteer.EffectiveStanding, and effectiveStandingExpr, the SQL expression the
// backpressure fold UPDATE embeds to decide transitions on a volunteer's EFFECTIVE
// standing. For each combination of stored standing × benched_until state it writes a
// volunteer row, computes the Go verdict, then asks the DB for the verdict the shared SQL
// expression yields for the same row and asserts they are byte-identical. A change to
// EITHER side that drifts from the other fails here, so the machine's SQL standing rule can
// never silently diverge from the validation-side rule the two must agree on. This mirrors
// workunit.TestStandingExprSQL_MatchesEffectiveStanding — the two copies of the rule are
// each independently pinned to the same Go function.
//
// The benched_until "past"/"future" cases sit a full hour from now so the small skew
// between the Go clock (EffectiveStanding's now argument) and the DB clock
// (effectiveStandingExpr's NOW()) can never flip a verdict — the only place the two evaluate
// a live vs. expired bench.
func TestEffectiveStandingExpr_MatchesEffectiveStanding(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ts := func(d time.Duration) *time.Time { v := time.Now().UTC().Add(d); return &v }

	cases := []struct {
		name         string
		standing     string
		benchedUntil *time.Time // nil => benched_until column NULL
	}{
		{"ok_null", volunteer.StandingOK, nil},
		{"ok_past", volunteer.StandingOK, ts(-time.Hour)},
		{"ok_future", volunteer.StandingOK, ts(time.Hour)},
		{"probation_null", volunteer.StandingProbation, nil},
		{"probation_past", volunteer.StandingProbation, ts(-time.Hour)},
		{"probation_future", volunteer.StandingProbation, ts(time.Hour)},
		// The load-bearing rows: only a BENCHED standing with a live (NULL or future) bench
		// resolves BENCHED; a BENCHED standing whose bench has passed resolves PROBATION.
		{"benched_null", volunteer.StandingBenched, nil},
		{"benched_past", volunteer.StandingBenched, ts(-time.Hour)},
		{"benched_future", volunteer.StandingBenched, ts(time.Hour)},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Each subtest inserts its own row (unique id), so no inter-case cleanup is needed.
			volID := insertVolunteer(t, pool)
			if _, err := pool.Exec(ctx,
				`UPDATE volunteers SET standing = $2, benched_until = $3 WHERE id = $1`,
				volID, tc.standing, tc.benchedUntil); err != nil {
				t.Fatalf("set standing columns: %v", err)
			}

			// The fetched values, carrying exactly the fields EffectiveStanding reads.
			var standing string
			var benchedUntil *time.Time
			if err := pool.QueryRow(ctx,
				`SELECT standing, benched_until FROM volunteers WHERE id = $1`, volID).
				Scan(&standing, &benchedUntil); err != nil {
				t.Fatalf("fetch volunteer standing: %v", err)
			}
			goStanding := volunteer.EffectiveStanding(standing, benchedUntil, time.Now().UTC())

			// The standing the DB computes via the shared expression for the same row.
			var sqlStanding string
			if err := pool.QueryRow(ctx,
				`SELECT `+effectiveStandingExpr("v")+` FROM volunteers v WHERE v.id = $1`, volID).
				Scan(&sqlStanding); err != nil {
				t.Fatalf("compute SQL standing: %v", err)
			}

			if goStanding != sqlStanding {
				t.Fatalf("standing twin drift (%s): volunteer.EffectiveStanding=%q, effectiveStandingExpr=%q\n"+
					"The Go rule and the SQL expression must stay identical — update both together.",
					tc.name, goStanding, sqlStanding)
			}
		})
	}
}
