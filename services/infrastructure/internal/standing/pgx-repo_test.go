//go:build integration

package standing

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// setupTestDB connects to the integration Postgres and returns a pool plus a cleanup that
// empties the volunteers table between tests. The standing columns live on volunteers; this
// package only ever inserts volunteer rows, so clearing that table is sufficient.
func setupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}

	cleanup := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM trusted_runners")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		pool.Close()
	}
	// Start clean in case a prior aborted run left rows.
	_, _ = pool.Exec(ctx, "DELETE FROM trusted_runners")
	_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
	return pool, cleanup
}

// insertVolunteer creates a bare volunteer row (default OK/AUTO standing) and returns its id.
func insertVolunteer(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	pubKey := make([]byte, 32)
	copy(pubKey, uuid.New().NodeID())
	copy(pubKey[6:], uuid.New().NodeID())
	copy(pubKey[12:], uuid.New().NodeID())
	copy(pubKey[18:], uuid.New().NodeID())
	copy(pubKey[24:], uuid.New().NodeID())
	_, err := pool.Exec(ctx, `
		INSERT INTO volunteers (
			id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at
		) VALUES (
			$1, $2, $3, $4, 'ALWAYS', true, NOW()
		)`,
		id, pubKey,
		json.RawMessage(`{"cpu_cores":8,"max_cpu_cores":4,"memory_total_mb":32768,"max_memory_mb":16384,"disk_available_mb":102400,"max_disk_mb":10240}`),
		[]string{"NATIVE", "CONTAINER"},
	)
	if err != nil {
		t.Fatalf("failed to insert test volunteer: %v", err)
	}
	return id
}

func TestGetAbsentIsNil(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	e, err := repo.Get(ctx, types.NewID())
	if err != nil {
		t.Fatalf("Get absent: %v", err)
	}
	if e != nil {
		t.Errorf("absent volunteer entry = %+v, want nil", e)
	}
}

func TestSetOperatorBenchedAndGet(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	id := insertVolunteer(t, pool)
	until := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)

	e, err := repo.SetOperator(ctx, id, volunteer.StandingBenched, &until, "corroborated abuse")
	if err != nil {
		t.Fatalf("SetOperator: %v", err)
	}
	if e == nil {
		t.Fatal("SetOperator returned nil entry for existing volunteer")
	}
	if e.VolunteerID != id {
		t.Errorf("entry id = %s, want %s", e.VolunteerID, id)
	}
	if e.Standing != volunteer.StandingBenched {
		t.Errorf("standing = %q, want BENCHED", e.Standing)
	}
	if e.Source != volunteer.StandingSourceOperator {
		t.Errorf("source = %q, want OPERATOR", e.Source)
	}
	if e.BenchedUntil == nil || !e.BenchedUntil.Equal(until) {
		t.Errorf("benched_until = %v, want %v", e.BenchedUntil, until)
	}
	if e.Reason == nil || *e.Reason != "corroborated abuse" {
		t.Errorf("reason = %v, want 'corroborated abuse'", e.Reason)
	}
	if e.ChangedAt == nil {
		t.Error("standing_changed_at should be set")
	}

	// Get returns the same stored row.
	got, err := repo.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("Get after set: entry=%v err=%v", got, err)
	}
	if got.Standing != volunteer.StandingBenched || got.Source != volunteer.StandingSourceOperator {
		t.Errorf("Get entry = %+v, want BENCHED/OPERATOR", got)
	}
}

func TestSetOperatorClearsBenchedUntilForNonBenched(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	id := insertVolunteer(t, pool)
	until := time.Now().Add(24 * time.Hour).UTC()

	// Bench first (records a deadline).
	if _, err := repo.SetOperator(ctx, id, volunteer.StandingBenched, &until, "temp"); err != nil {
		t.Fatalf("SetOperator bench: %v", err)
	}
	// Move to PROBATION while still passing a deadline: it must be dropped.
	e, err := repo.SetOperator(ctx, id, volunteer.StandingProbation, &until, "downgraded")
	if err != nil {
		t.Fatalf("SetOperator probation: %v", err)
	}
	if e.Standing != volunteer.StandingProbation {
		t.Errorf("standing = %q, want PROBATION", e.Standing)
	}
	if e.BenchedUntil != nil {
		t.Errorf("benched_until = %v, want nil for non-BENCHED", e.BenchedUntil)
	}
	if e.Reason == nil || *e.Reason != "downgraded" {
		t.Errorf("reason = %v, want 'downgraded'", e.Reason)
	}
}

func TestSetOperatorUnknownVolunteerIsNil(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	e, err := repo.SetOperator(ctx, types.NewID(), volunteer.StandingBenched, nil, "x")
	if err != nil {
		t.Fatalf("SetOperator unknown: %v", err)
	}
	if e != nil {
		t.Errorf("unknown volunteer entry = %+v, want nil", e)
	}
}

func TestSetOperatorInvalidStanding(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	id := insertVolunteer(t, pool)
	if _, err := repo.SetOperator(ctx, id, "NOPE", nil, ""); err == nil {
		t.Error("SetOperator with invalid standing: err = nil, want validation error")
	}
}

func TestClearRoundTrip(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	id := insertVolunteer(t, pool)
	until := time.Now().Add(48 * time.Hour).UTC()
	if _, err := repo.SetOperator(ctx, id, volunteer.StandingBenched, &until, "benched"); err != nil {
		t.Fatalf("SetOperator: %v", err)
	}

	e, err := repo.Clear(ctx, id)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if e == nil {
		t.Fatal("Clear returned nil for existing volunteer")
	}
	if e.Standing != volunteer.StandingOK {
		t.Errorf("standing after clear = %q, want OK", e.Standing)
	}
	if e.Source != volunteer.StandingSourceAuto {
		t.Errorf("source after clear = %q, want AUTO", e.Source)
	}
	if e.BenchedUntil != nil {
		t.Errorf("benched_until after clear = %v, want nil", e.BenchedUntil)
	}
	if e.Reason != nil {
		t.Errorf("reason after clear = %v, want nil", e.Reason)
	}

	// Clearing an unknown volunteer returns (nil, nil).
	ne, err := repo.Clear(ctx, types.NewID())
	if err != nil {
		t.Fatalf("Clear unknown: %v", err)
	}
	if ne != nil {
		t.Errorf("Clear unknown entry = %+v, want nil", ne)
	}
}

func TestListNonOKAndAllNonOK(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Three volunteers: one benched, one on probation, one left OK.
	benched := insertVolunteer(t, pool)
	probation := insertVolunteer(t, pool)
	okVol := insertVolunteer(t, pool)

	until := time.Now().Add(6 * time.Hour).UTC()
	if _, err := repo.SetOperator(ctx, benched, volunteer.StandingBenched, &until, "b"); err != nil {
		t.Fatalf("SetOperator benched: %v", err)
	}
	if _, err := repo.SetOperator(ctx, probation, volunteer.StandingProbation, nil, "p"); err != nil {
		t.Fatalf("SetOperator probation: %v", err)
	}

	// ListNonOK returns exactly the two non-OK rows (okVol excluded).
	list, err := repo.ListNonOK(ctx, 100, 0)
	if err != nil {
		t.Fatalf("ListNonOK: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListNonOK returned %d, want 2", len(list))
	}
	for _, e := range list {
		if e.VolunteerID == okVol {
			t.Error("ListNonOK included the OK volunteer")
		}
		if e.Standing == volunteer.StandingOK {
			t.Errorf("ListNonOK entry has OK standing: %+v", e)
		}
	}

	// AllNonOK is the same population, keyed by id.
	all, err := repo.AllNonOK(ctx)
	if err != nil {
		t.Fatalf("AllNonOK: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("AllNonOK returned %d, want 2", len(all))
	}
	if _, ok := all[benched]; !ok {
		t.Error("AllNonOK missing benched volunteer")
	}
	if _, ok := all[probation]; !ok {
		t.Error("AllNonOK missing probation volunteer")
	}
	if _, ok := all[okVol]; ok {
		t.Error("AllNonOK included the OK volunteer")
	}

	// After clearing the benched one, both views drop it.
	if _, err := repo.Clear(ctx, benched); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	list, _ = repo.ListNonOK(ctx, 100, 0)
	if len(list) != 1 || list[0].VolunteerID != probation {
		t.Errorf("ListNonOK after clear = %+v, want only probation", list)
	}
	all, _ = repo.AllNonOK(ctx)
	if len(all) != 1 {
		t.Errorf("AllNonOK after clear = %d entries, want 1", len(all))
	}
}
