//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// valConfigTarget3Quorum2 is a target>quorum leaf (TODO #50): dispatch up to 3 copies, validate
// when 2 agree. redundancy_factor is retained (it is the back-compat alias) but target_copies /
// min_quorum override it.
const valConfigTarget3Quorum2 = `{"redundancy_factor":2,"target_copies":3,"min_quorum":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`

// valConfigRedundancy2 is a plain redundancy-2 leaf (target == quorum == 2 by the back-compat
// alias) — the "existing leaf" shape that must behave exactly as before #50.
const valConfigRedundancy2 = `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`

// reserveLiveCopies reserves `n` live copies on a unit, each by a DISTINCT volunteer (the
// per-volunteer distinctness the reserve path enforces), returning the copies.
func reserveLiveCopies(t *testing.T, ctx context.Context, repo *PgxWorkUnitRepository, pool *pgxpool.Pool, wuID types.ID, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		vol := createTestVolunteer(t, pool)
		if _, err := repo.ReserveCopy(ctx, wuID, vol, nil, time.Now().Add(time.Hour), 3600); err != nil {
			t.Fatalf("ReserveCopy %d: %v", i, err)
		}
	}
}

// TestDispatch_TargetCopies_OverDispatch asserts the dispatch predicate uses TARGET (not quorum):
// a target=3/quorum=2 unit stays dispatchable while it has fewer than 3 live copies, so the head
// over-dispatches toward target and validates at quorum (the serial-reassignment tail removed by
// #50). It also asserts the effective_redundancy column the cache reads equals the target.
func TestDispatch_TargetCopies_OverDispatch(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	userID := createTestUser(t, pool, "dispatch-target")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigTarget3Quorum2)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// Units with 0, 1, 2, 3 live copies respectively.
	wu := make([]types.ID, 4)
	for live := 0; live < 4; live++ {
		u := mustQueuedWU(t, ctx, repo, leafID)
		wu[live] = u.ID
		reserveLiveCopies(t, ctx, repo, pool, u.ID, live)
	}

	cands, err := repo.FindDispatchableBatch(ctx, 100, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	got := map[types.ID]DispatchCandidate{}
	for _, c := range cands {
		got[c.WorkUnit.ID] = c
	}

	// 0/1/2 live copies are still dispatchable (live < target 3); 3 live is NOT (target reached).
	for live := 0; live < 3; live++ {
		c, ok := got[wu[live]]
		if !ok {
			t.Errorf("unit with %d live copies should be dispatchable (target 3), but was excluded", live)
			continue
		}
		if c.RedundancyFactor != 3 {
			t.Errorf("unit with %d live: effective_redundancy(target) = %d, want 3", live, c.RedundancyFactor)
		}
		if c.ActiveAssignments != live {
			t.Errorf("unit with %d live: active_assignments = %d, want %d", live, c.ActiveAssignments, live)
		}
	}
	if _, ok := got[wu[3]]; ok {
		t.Errorf("unit with 3 live copies (target reached) should NOT be dispatchable")
	}
}

// TestDispatch_RedundancyFactor_BackCompat asserts a plain redundancy-2 leaf (no target/quorum)
// resolves target == redundancy_factor == 2 — byte-for-byte the pre-#50 dispatch behavior.
func TestDispatch_RedundancyFactor_BackCompat(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	userID := createTestUser(t, pool, "dispatch-backcompat")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy2)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	// 1 live copy: still dispatchable (1 < 2). 2 live copies: not (redundancy reached).
	u1 := mustQueuedWU(t, ctx, repo, leafID)
	reserveLiveCopies(t, ctx, repo, pool, u1.ID, 1)
	u2 := mustQueuedWU(t, ctx, repo, leafID)
	reserveLiveCopies(t, ctx, repo, pool, u2.ID, 2)

	cands, err := repo.FindDispatchableBatch(ctx, 100, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	got := map[types.ID]int{}
	for _, c := range cands {
		got[c.WorkUnit.ID] = c.RedundancyFactor
	}
	if target, ok := got[u1.ID]; !ok || target != 2 {
		t.Errorf("redundancy-2 unit with 1 live copy should be dispatchable with target 2; ok=%v target=%d", ok, target)
	}
	if _, ok := got[u2.ID]; ok {
		t.Errorf("redundancy-2 unit with 2 live copies should NOT be dispatchable (target reached)")
	}
}

// TestDeadLetter_TargetQuorumCaps asserts the dead-letter SQL reads the new quorum + ceiling
// fragments: a target=3/quorum=2 unit with the default ceiling (target+6=9) dead-letters only
// once total copies reach 9 with quorum (2) unmet and no live copy.
func TestDeadLetter_TargetQuorumCaps(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	userID := createTestUser(t, pool, "dl-target")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigTarget3Quorum2)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	u := mustQueuedWU(t, ctx, repo, leafID)
	// Create 8 closed (EXPIRED) copies, each a distinct volunteer: total=8 < ceiling 9.
	for i := 0; i < 8; i++ {
		vol := createTestVolunteer(t, pool)
		cp, err := repo.ReserveCopy(ctx, u.ID, vol, nil, time.Now().Add(time.Hour), 3600)
		if err != nil {
			t.Fatalf("reserve copy %d: %v", i, err)
		}
		if err := repo.CloseCopy(ctx, cp.ID, "EXPIRED"); err != nil {
			t.Fatalf("close copy %d: %v", i, err)
		}
	}
	failed, err := repo.DeadLetterIfExhausted(ctx, u.ID)
	if err != nil {
		t.Fatalf("dead-letter probe (8 copies): %v", err)
	}
	if failed {
		t.Fatal("should NOT dead-letter at 8 copies (ceiling for target 3 is target+6=9)")
	}

	// 9th copy → total reaches the ceiling 9 → dead-letter.
	vol := createTestVolunteer(t, pool)
	cp, err := repo.ReserveCopy(ctx, u.ID, vol, nil, time.Now().Add(time.Hour), 3600)
	if err != nil {
		t.Fatalf("reserve copy 9: %v", err)
	}
	if err := repo.CloseCopy(ctx, cp.ID, "EXPIRED"); err != nil {
		t.Fatalf("close copy 9: %v", err)
	}
	failed, err = repo.DeadLetterIfExhausted(ctx, u.ID)
	if err != nil {
		t.Fatalf("dead-letter probe (9 copies): %v", err)
	}
	if !failed {
		t.Fatal("should dead-letter once total copies reach the ceiling (9 for target 3)")
	}
}
