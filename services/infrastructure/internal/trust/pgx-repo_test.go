//go:build integration

package trust

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// setupTestDB connects to the integration Postgres and returns a pool plus a cleanup that
// empties volunteer_trust between tests. The table has no foreign keys (subjects are
// identities, not volunteer rows), so no other tables need seeding.
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
		_, _ = pool.Exec(ctx, "DELETE FROM volunteer_trust")
		pool.Close()
	}
	// Start clean in case a prior aborted run left rows.
	_, _ = pool.Exec(ctx, "DELETE FROM volunteer_trust")
	return pool, cleanup
}

func TestGetScoreAbsentIsZero(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	score, err := repo.GetScore(ctx, "did:plc:absent")
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if score != 0 {
		t.Errorf("absent subject score = %d, want 0", score)
	}

	entry, err := repo.Get(ctx, "did:plc:absent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry != nil {
		t.Errorf("absent subject entry = %+v, want nil", entry)
	}
}

func TestSetScoreUpsert(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	const subj = "did:plc:seed"

	// Insert.
	if err := repo.SetScore(ctx, subj, 30); err != nil {
		t.Fatalf("SetScore insert: %v", err)
	}
	e, err := repo.Get(ctx, subj)
	if err != nil || e == nil {
		t.Fatalf("Get after insert: entry=%v err=%v", e, err)
	}
	if e.Score != 30 {
		t.Errorf("score = %d, want 30", e.Score)
	}
	if e.CleanUnits != 0 {
		t.Errorf("clean_units = %d, want 0 (seeding must not fabricate earned work)", e.CleanUnits)
	}

	// Update (upsert) — score changes, clean_units still untouched.
	if err := repo.SetScore(ctx, subj, 5); err != nil {
		t.Fatalf("SetScore update: %v", err)
	}
	e, _ = repo.Get(ctx, subj)
	if e.Score != 5 {
		t.Errorf("score after update = %d, want 5", e.Score)
	}
	if e.CleanUnits != 0 {
		t.Errorf("clean_units after update = %d, want 0", e.CleanUnits)
	}

	got, err := repo.GetScore(ctx, subj)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if got != 5 {
		t.Errorf("GetScore = %d, want 5", got)
	}
}

func TestAccrueCleanUnit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	const subj = "vol:11111111-1111-1111-1111-111111111111"

	// New subject starts at 1/1.
	if err := repo.AccrueCleanUnit(ctx, subj); err != nil {
		t.Fatalf("AccrueCleanUnit new: %v", err)
	}
	e, _ := repo.Get(ctx, subj)
	if e == nil || e.Score != 1 || e.CleanUnits != 1 {
		t.Fatalf("after first accrual entry = %+v, want score=1 clean_units=1", e)
	}

	// Second accrual increments BOTH.
	if err := repo.AccrueCleanUnit(ctx, subj); err != nil {
		t.Fatalf("AccrueCleanUnit second: %v", err)
	}
	e, _ = repo.Get(ctx, subj)
	if e.Score != 2 || e.CleanUnits != 2 {
		t.Errorf("after second accrual entry = %+v, want score=2 clean_units=2", e)
	}
}

func TestSlashZeroesScoreRetainsCleanUnits(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	const subj = "did:plc:bad"

	// Accrue some clean units first.
	for i := 0; i < 3; i++ {
		if err := repo.AccrueCleanUnit(ctx, subj); err != nil {
			t.Fatalf("AccrueCleanUnit: %v", err)
		}
	}

	if err := repo.Slash(ctx, subj); err != nil {
		t.Fatalf("Slash: %v", err)
	}
	e, _ := repo.Get(ctx, subj)
	if e == nil {
		t.Fatal("entry missing after slash")
	}
	if e.Score != 0 {
		t.Errorf("score after slash = %d, want 0", e.Score)
	}
	if e.CleanUnits != 3 {
		t.Errorf("clean_units after slash = %d, want 3 (retained for audit)", e.CleanUnits)
	}
	if e.SlashedAt == nil {
		t.Error("slashed_at should be set after slash")
	}

	// Slashing an absent subject creates a zeroed, slashed row.
	const absent = "did:plc:neverseen"
	if err := repo.Slash(ctx, absent); err != nil {
		t.Fatalf("Slash absent: %v", err)
	}
	e2, _ := repo.Get(ctx, absent)
	if e2 == nil || e2.Score != 0 || e2.SlashedAt == nil {
		t.Errorf("slashed absent subject = %+v, want zeroed row with slashed_at set", e2)
	}
}

func TestListOrderingLimitOffset(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxRepository(pool)
	ctx := context.Background()

	// Seed subjects with distinct scores; two share a score to exercise the subject ASC
	// tiebreak.
	seed := map[string]int{
		"did:plc:aaa": 10,
		"did:plc:bbb": 30,
		"did:plc:ccc": 20,
		"did:plc:ddd": 20,
	}
	for s, sc := range seed {
		if err := repo.SetScore(ctx, s, sc); err != nil {
			t.Fatalf("SetScore %s: %v", s, err)
		}
	}

	all, err := repo.List(ctx, 100, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("List returned %d, want 4", len(all))
	}
	// Expected order: score DESC, then subject ASC on ties.
	want := []string{"did:plc:bbb", "did:plc:ccc", "did:plc:ddd", "did:plc:aaa"}
	for i, e := range all {
		if e.Subject != want[i] {
			t.Errorf("order[%d] = %q, want %q", i, e.Subject, want[i])
		}
	}

	// Limit.
	page1, err := repo.List(ctx, 2, 0)
	if err != nil {
		t.Fatalf("List limit: %v", err)
	}
	if len(page1) != 2 || page1[0].Subject != "did:plc:bbb" || page1[1].Subject != "did:plc:ccc" {
		t.Errorf("page1 = %v, want [bbb ccc]", subjects(page1))
	}

	// Offset.
	page2, err := repo.List(ctx, 2, 2)
	if err != nil {
		t.Fatalf("List offset: %v", err)
	}
	if len(page2) != 2 || page2[0].Subject != "did:plc:ddd" || page2[1].Subject != "did:plc:aaa" {
		t.Errorf("page2 = %v, want [ddd aaa]", subjects(page2))
	}
}

func subjects(es []*Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Subject
	}
	return out
}
