//go:build integration

package workunit

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// These DB-backed integration tests exercise the account-standing (BG-24b) enforcement the
// four requester-aware dispatch sites gained: the BENCHED dispatch gate (FindNextAssignable,
// FlushReservations, ReserveCopy) and the countable-coverage forced-replication rule
// (FindNextAssignable's redundancy headroom). Build tag `integration`; SKIP unless
// LETTUCE_TEST_DB_URL is set; safe under -p 1 (each test DELETE-cleans before it seeds).

// setStanding (writing a volunteer's raw standing + benched_until directly) is shared with
// standing_counts_integration_test.go in this package; enforcement always resolves those raw
// columns through the production standingExprSQL / volunteer.EffectiveStanding.

// setResultStanding stamps a PENDING result's submit-time effective standing
// (results.standing_at_submit) for author vol on wuID, so a probation-stamped result can be
// asserted NOT to cover redundancy. insertPendingResult leaves the column NULL (legacy = OK).
func setResultStanding(t *testing.T, pool *pgxpool.Pool, wuID, vol types.ID, standing string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE results SET standing_at_submit = $3 WHERE work_unit_id = $1 AND volunteer_id = $2`,
		wuID, vol, standing); err != nil {
		t.Fatalf("set result standing: %v", err)
	}
}

// standingOpts builds AssignmentOptions whose capabilities generously fit the default
// createActiveTestLeaf (NATIVE, min 1 CPU core), scoped to leafID so ONLY the target unit is
// a candidate. Standing is the dimension under test; every other predicate passes.
func standingOpts(vol, leafID types.ID) AssignmentOptions {
	return AssignmentOptions{
		VolunteerID:       vol,
		LeafIDs:           []types.ID{leafID},
		MaxCPUCores:       8,
		MaxMemoryMB:       16384,
		MaxDiskMB:         1 << 40,
		AvailableRuntimes: []string{"NATIVE"},
	}
}

// seedStandingUnit creates a fresh ACTIVE leaf with the given redundancy factor and one
// QUEUED work unit on it, returning both ids.
func seedStandingUnit(t *testing.T, pool *pgxpool.Pool, repo *PgxWorkUnitRepository, redundancy int) (leafID, unitID types.ID) {
	t.Helper()
	ctx := context.Background()
	userID := createTestUser(t, pool, "standing-"+uuid.New().String()[:8])
	valConfig := `{"redundancy_factor":` + strconv.Itoa(redundancy) + `,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`
	leafID = createActiveTestLeaf(t, pool, &userID, "", "", valConfig)
	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("create standing target unit: %v", err)
	}
	return leafID, wu.ID
}

// TestFindNextAssignable_BenchedRequesterRefused: a BENCHED account gets NO unit, while an OK
// account gets the same unit (redundancy 2, so coverage is never the reason for refusal —
// only the bench is). Also covers the indefinite bench (benched_until NULL).
func TestFindNextAssignable_BenchedRequesterRefused(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	future := time.Now().UTC().Add(time.Hour)
	for _, tc := range []struct {
		name         string
		benchedUntil *time.Time
	}{
		{"bench_future", &future},
		{"bench_indefinite", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanParityTables(t, pool)
			leafID, unitID := seedStandingUnit(t, pool, repo, 2)

			benched := createTestVolunteer(t, pool)
			setStanding(t, pool, benched, "BENCHED", tc.benchedUntil)
			ok := createTestVolunteer(t, pool) // default standing OK

			got, err := repo.FindNextAssignable(ctx, standingOpts(benched, leafID))
			if err != nil {
				t.Fatalf("FindNextAssignable(benched): %v", err)
			}
			if got != nil {
				t.Fatalf("benched requester was offered unit %s; want no dispatch", got.ID)
			}

			got, err = repo.FindNextAssignable(ctx, standingOpts(ok, leafID))
			if err != nil {
				t.Fatalf("FindNextAssignable(ok): %v", err)
			}
			if got == nil || got.ID != unitID {
				t.Fatalf("OK requester not offered the unit (got %v); the bench must not block an OK account", got)
			}
		})
	}
}

// TestFindNextAssignable_ExpiredBenchDispatches: a BENCHED standing whose benched_until has
// PASSED resolves to PROBATION, which is still dispatched — the account is offered the unit
// again (re-entry is neutralized, not blocked).
func TestFindNextAssignable_ExpiredBenchDispatches(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	cleanParityTables(t, pool)
	leafID, unitID := seedStandingUnit(t, pool, repo, 2)

	past := time.Now().UTC().Add(-time.Hour)
	vol := createTestVolunteer(t, pool)
	setStanding(t, pool, vol, "BENCHED", &past) // expired bench => effective PROBATION

	got, err := repo.FindNextAssignable(ctx, standingOpts(vol, leafID))
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if got == nil || got.ID != unitID {
		t.Fatalf("expired-bench requester not offered the unit (got %v); an expired bench must dispatch as PROBATION", got)
	}
}

// TestFlushReservations_BenchedReserverRefused: a benched account's reservation must not land
// (absent from RETURNING), while an OK account's does.
func TestFlushReservations_BenchedReserverRefused(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	cleanParityTables(t, pool)
	_, unitID := seedStandingUnit(t, pool, repo, 2)
	until := time.Now().UTC().Add(time.Hour)

	benched := createTestVolunteer(t, pool)
	setStanding(t, pool, benched, "BENCHED", nil)
	landed, err := repo.FlushReservations(ctx, []FlushReservation{{
		WorkUnitID: unitID, VolunteerID: benched, ReservedUntil: until, DeadlineSeconds: 3600,
	}}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations(benched): %v", err)
	}
	if containsFlushedPair(landed, unitID, benched) {
		t.Fatalf("benched reserver's copy landed; a benched account's reservation must not persist")
	}

	ok := createTestVolunteer(t, pool)
	landed, err = repo.FlushReservations(ctx, []FlushReservation{{
		WorkUnitID: unitID, VolunteerID: ok, ReservedUntil: until, DeadlineSeconds: 3600,
	}}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations(ok): %v", err)
	}
	if !containsFlushedPair(landed, unitID, ok) {
		t.Fatalf("OK reserver's copy did not land; the bench must not block an OK account")
	}
}

// TestReserveCopy_BenchedReserverRefused: a benched account's spot-check landing is refused
// with a 409 Conflict, while an OK account's copy is created.
func TestReserveCopy_BenchedReserverRefused(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	cleanParityTables(t, pool)
	_, unitID := seedStandingUnit(t, pool, repo, 2)
	until := time.Now().UTC().Add(time.Hour)

	benched := createTestVolunteer(t, pool)
	setStanding(t, pool, benched, "BENCHED", nil)
	if _, err := repo.ReserveCopy(ctx, unitID, benched, nil, until, 3600); err == nil {
		t.Fatalf("ReserveCopy(benched) succeeded; a benched account's copy must be refused")
	} else {
		var apiErr *apierror.APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus != 409 {
			t.Fatalf("ReserveCopy(benched) error = %v; want 409 Conflict", err)
		}
	}

	ok := createTestVolunteer(t, pool)
	cp, err := repo.ReserveCopy(ctx, unitID, ok, nil, until, 3600)
	if err != nil {
		t.Fatalf("ReserveCopy(ok): %v", err)
	}
	if cp == nil {
		t.Fatalf("OK reserver's copy was not created; the bench must not block an OK account")
	}
}

// TestFindNextAssignable_ProbationLiveCopyForcesReplication: on a redundancy-1 unit a live
// copy held by a PROBATION account does NOT cover redundancy (countable coverage 0), so a
// fresh OK requester is still offered the unit — full replication forced around the
// neutralized copy. The control (holder OK) closes the unit, proving the copy is what the
// standing filter, not something else, discounts.
func TestFindNextAssignable_ProbationLiveCopyForcesReplication(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	cleanParityTables(t, pool)
	leafID, unitID := seedStandingUnit(t, pool, repo, 1)

	holder := createTestVolunteer(t, pool)
	insertLiveCopy(t, pool, unitID, holder, nil)
	requester := createTestVolunteer(t, pool) // OK

	// Holder PROBATION: its live copy is non-countable, so headroom stays open.
	setStanding(t, pool, holder, "PROBATION", nil)
	got, err := repo.FindNextAssignable(ctx, standingOpts(requester, leafID))
	if err != nil {
		t.Fatalf("FindNextAssignable(probation holder): %v", err)
	}
	if got == nil || got.ID != unitID {
		t.Fatalf("unit not offered while its only live copy is PROBATION-held (got %v); forced replication should keep it dispatchable", got)
	}

	// Control — holder OK: the live copy is countable, redundancy 1 is met, unit closed.
	setStanding(t, pool, holder, "OK", nil)
	got, err = repo.FindNextAssignable(ctx, standingOpts(requester, leafID))
	if err != nil {
		t.Fatalf("FindNextAssignable(ok holder): %v", err)
	}
	if got != nil {
		t.Fatalf("unit offered while its live copy is OK-held (got %s); a countable copy must cover redundancy 1", got.ID)
	}
}

// TestFindNextAssignable_ProbationPendingResultDoesNotCover: on a redundancy-1 unit a PENDING
// result stamped PROBATION at submit does NOT cover redundancy, so a fresh OK requester is
// still offered the unit. The control (stamp OK) closes it.
func TestFindNextAssignable_ProbationPendingResultDoesNotCover(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	cleanParityTables(t, pool)
	leafID, unitID := seedStandingUnit(t, pool, repo, 1)

	author := createTestVolunteer(t, pool)
	insertPendingResult(t, pool, unitID, author)
	requester := createTestVolunteer(t, pool) // OK, distinct from author

	// Stamped PROBATION: the pending result is non-countable, headroom stays open.
	setResultStanding(t, pool, unitID, author, "PROBATION")
	got, err := repo.FindNextAssignable(ctx, standingOpts(requester, leafID))
	if err != nil {
		t.Fatalf("FindNextAssignable(probation-stamped result): %v", err)
	}
	if got == nil || got.ID != unitID {
		t.Fatalf("unit not offered while its only pending result is PROBATION-stamped (got %v); forced replication should keep it dispatchable", got)
	}

	// Control — stamped OK: the pending result is countable, redundancy 1 is met.
	setResultStanding(t, pool, unitID, author, "OK")
	got, err = repo.FindNextAssignable(ctx, standingOpts(requester, leafID))
	if err != nil {
		t.Fatalf("FindNextAssignable(ok-stamped result): %v", err)
	}
	if got != nil {
		t.Fatalf("unit offered while its pending result is OK-stamped (got %s); a countable result must cover redundancy 1", got.ID)
	}
}
