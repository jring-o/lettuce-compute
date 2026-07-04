//go:build integration

package workunit

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Tests for CountTrustStarvedUnits, the observability probe behind the fault monitor's
// trust-starved WARN: QUEUED units whose remaining redundancy headroom the
// trusted-corroborator reservation is withholding for trusted subjects none have taken,
// aged over the fixed one-hour threshold.

// starvedTestPolicy is the gate-on head policy the probe tests run under: K = 1, floor 25.
var starvedTestPolicy = TrustDispatchPolicy{
	GateEnabled:             true,
	DefaultMinCorroborators: 1,
	DefaultFloor:            25,
}

// insertStarvedFixtureUnit creates a QUEUED unit on leafID and backdates its created_at
// past the probe's one-hour age threshold when backdate is true.
func insertStarvedFixtureUnit(t *testing.T, pool *pgxpool.Pool, repo *PgxWorkUnitRepository, leafID types.ID, backdate bool) types.ID {
	t.Helper()
	ctx := context.Background()
	wu := newTestWorkUnit(leafID, nil)
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("create fixture unit: %v", err)
	}
	age := "NOW()"
	if backdate {
		age = "NOW() - INTERVAL '2 hours'"
	}
	if _, err := pool.Exec(ctx,
		`UPDATE work_units SET state = 'QUEUED', created_at = `+age+` WHERE id = $1`, wu.ID); err != nil {
		t.Fatalf("queue/backdate fixture unit: %v", err)
	}
	return wu.ID
}

// insertStarvedPendingResult inserts a PENDING result for (wuID, vol). trusted stamps it
// with a submission-time score far above any floor; untrusted leaves the stamps NULL (a
// legacy/unscored row — never trusted).
func insertStarvedPendingResult(t *testing.T, pool *pgxpool.Pool, wuID, vol types.ID, trusted bool) {
	t.Helper()
	ctx := context.Background()
	if trusted {
		if _, err := pool.Exec(ctx, `
			INSERT INTO results
				(work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata,
				 validation_status, trust_subject, trust_score_at_submit)
			VALUES ($1, $2, '{"x":1}'::jsonb, $3, '{}'::jsonb, 'PENDING', $4, 1000000)`,
			wuID, vol, strings.Repeat("a", 64), "vol:"+vol.String(),
		); err != nil {
			t.Fatalf("insert trusted pending result: %v", err)
		}
		return
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO results
			(work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata, validation_status)
		VALUES ($1, $2, '{"x":1}'::jsonb, $3, '{}'::jsonb, 'PENDING')`,
		wuID, vol, strings.Repeat("b", 64),
	); err != nil {
		t.Fatalf("insert untrusted pending result: %v", err)
	}
}

func TestCountTrustStarvedUnits(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := createTestUser(t, pool, "truststarved")
	leafID := createTestLeaf(t, pool, &userID)
	// The probe only scans ACTIVE leafs (an inactive leaf's units are not dispatchable at
	// all, so the reservation cannot be what is holding them).
	if _, err := pool.Exec(ctx, `UPDATE leafs SET state = 'ACTIVE' WHERE id = $1`, leafID); err != nil {
		t.Fatalf("activate leaf: %v", err)
	}

	gateOn := NewPgxWorkUnitRepository(pool).WithTrustDispatch(starvedTestPolicy)

	// STARVED: aged, one untrusted PENDING result on a target-2 leaf. Covered (1) < target
	// (2), trusted present 0 < K 1, and 1 + (1-0) >= 2 — the last slot is reserved.
	starved := insertStarvedFixtureUnit(t, pool, gateOn, leafID, true)
	insertStarvedPendingResult(t, pool, starved, createTestVolunteer(t, pool), false)

	// NOT starved (young): same shape but created now — under the one-hour age threshold.
	young := insertStarvedFixtureUnit(t, pool, gateOn, leafID, false)
	insertStarvedPendingResult(t, pool, young, createTestVolunteer(t, pool), false)

	// NOT starved (trusted present): aged, but its pending result is STAMPED trusted, so
	// the trusted-corroborator need is already met and nothing is being withheld.
	satisfied := insertStarvedFixtureUnit(t, pool, gateOn, leafID, true)
	insertStarvedPendingResult(t, pool, satisfied, createTestVolunteer(t, pool), true)

	// NOT starved (full): aged, two untrusted PENDING results — covered (2) == target (2).
	// No headroom remains for the reservation to withhold; whatever happens next is
	// validation's call (corroborate/reject), not dispatch starvation.
	full := insertStarvedFixtureUnit(t, pool, gateOn, leafID, true)
	insertStarvedPendingResult(t, pool, full, createTestVolunteer(t, pool), false)
	insertStarvedPendingResult(t, pool, full, createTestVolunteer(t, pool), false)

	count, sample, err := gateOn.CountTrustStarvedUnits(ctx, 5)
	if err != nil {
		t.Fatalf("CountTrustStarvedUnits (gate on): %v", err)
	}
	if count != 1 {
		t.Fatalf("starved count = %d, want 1 (only the aged, untrusted-pending, headroom-remaining unit)", count)
	}
	if len(sample) != 1 || sample[0] != starved {
		t.Fatalf("starved sample = %v, want exactly [%s]", sample, starved)
	}

	// Gate off: the probe short-circuits to zero without touching the database, no matter
	// what starved shapes exist.
	gateOff := NewPgxWorkUnitRepository(pool)
	count, sample, err = gateOff.CountTrustStarvedUnits(ctx, 5)
	if err != nil {
		t.Fatalf("CountTrustStarvedUnits (gate off): %v", err)
	}
	if count != 0 || sample != nil {
		t.Fatalf("gate off: count = %d, sample = %v, want 0 and nil (short-circuit)", count, sample)
	}
}
