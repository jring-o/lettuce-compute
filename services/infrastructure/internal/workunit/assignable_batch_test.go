//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// FindAssignableBatch is the dispatch cache's per-requester fallback query: FindNextAssignable's
// predicate for one requester, in batches, bounded by the machine's in-flight room, skipping the
// cache's own units, and — under scale-out — honouring and taking dispatch claims. Its predicate
// is pinned beside FindNextAssignable's by the dispatch-predicate parity suite; these tests pin
// what the batch form adds.

// queuedAged creates a QUEUED unit created ageSecs ago, so tests control the dispatch order.
func queuedAged(t *testing.T, ctx context.Context, pool *pgxpool.Pool, repo *PgxWorkUnitRepository, leafID types.ID, ageSecs int) *WorkUnit {
	t.Helper()
	wu := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := pool.Exec(ctx, `UPDATE work_units SET created_at = NOW() - make_interval(secs => $2) WHERE id = $1`, wu.ID, ageSecs); err != nil {
		t.Fatalf("age unit: %v", err)
	}
	return wu
}

func batchIDs(units []*WorkUnit) []types.ID {
	out := make([]types.ID, len(units))
	for i, u := range units {
		out[i] = u.ID
	}
	return out
}

func sameIDs(got, want []types.ID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestFindAssignableBatch_ServesUntouchedUnitsPastTheRequestersOwn is the field shape in SQL:
// the leaf's oldest units all carry the requester's result and wait for a second account; the
// volunteer-agnostic refill returns exactly those, while the per-requester batch returns the
// untouched units behind them, in dispatch order.
func TestFindAssignableBatch_ServesUntouchedUnitsPastTheRequestersOwn(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	userID := createTestUser(t, pool, "assignable-batch-own")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy2)
	repo := NewPgxWorkUnitRepository(pool)
	vol := createTestVolunteer(t, pool)

	var own []types.ID
	for i := 0; i < 4; i++ {
		wu := queuedAged(t, ctx, pool, repo, leafID, 1000-i)
		insertPendingResult(t, pool, wu.ID, vol)
		own = append(own, wu.ID)
	}
	var untouched []types.ID
	for i := 0; i < 3; i++ {
		untouched = append(untouched, queuedAged(t, ctx, pool, repo, leafID, 500-i).ID)
	}

	pooled, err := repo.FindDispatchableBatch(ctx, 4, nil, []types.ID{leafID})
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	var pooledIDs []types.ID
	for _, c := range pooled {
		pooledIDs = append(pooledIDs, c.WorkUnit.ID)
	}
	if !sameIDs(pooledIDs, own) {
		t.Fatalf("the refill staged %v, want the requester's own four (the pool's account-blind shape)", pooledIDs)
	}

	opts := reserveOpts(vol, 0)
	opts.LeafIDs = []types.ID{leafID}
	got, err := repo.FindAssignableBatch(ctx, opts, 64, nil, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FindAssignableBatch: %v", err)
	}
	if !sameIDs(batchIDs(got), untouched) {
		t.Fatalf("per-requester batch = %v, want the untouched units %v in dispatch order", batchIDs(got), untouched)
	}
}

// TestFindAssignableBatch_BoundedByTheMachinesRoom: the whole batch fits in the machine's
// remaining in-flight room, counted from its live copies; no cap means only limit bounds it.
func TestFindAssignableBatch_BoundedByTheMachinesRoom(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	userID := createTestUser(t, pool, "assignable-batch-room")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	otherLeaf := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	vol := createTestVolunteer(t, pool)
	host := types.NewID()

	for i := 0; i < 6; i++ {
		queuedAged(t, ctx, pool, repo, leafID, 100-i)
	}
	// The machine already holds one copy, of another leaf's unit.
	held := mustQueuedWU(t, ctx, repo, otherLeaf)
	insertLiveCopy(t, pool, held.ID, vol, &host)

	opts := reserveOpts(vol, 3)
	opts.LeafIDs = []types.ID{leafID}
	opts.HostID = &host
	got, err := repo.FindAssignableBatch(ctx, opts, 10, nil, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FindAssignableBatch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("batch of %d for a machine with cap 3 holding 1, want 2", len(got))
	}
	opts.MaxInflightPerVolunteer = 1
	if got, err = repo.FindAssignableBatch(ctx, opts, 10, nil, types.ID{}, 0); err != nil || len(got) != 0 {
		t.Fatalf("batch of %d (err %v) for a machine at its cap, want none", len(got), err)
	}
	opts.MaxInflightPerVolunteer = 0
	if got, err = repo.FindAssignableBatch(ctx, opts, 4, nil, types.ID{}, 0); err != nil || len(got) != 4 {
		t.Fatalf("batch of %d (err %v) with no cap and limit 4, want 4", len(got), err)
	}
}

// TestFindAssignableBatch_SkipsExcludedIDs: units the cache stages or holds are never returned,
// since a held unit's copy may not have landed yet.
func TestFindAssignableBatch_SkipsExcludedIDs(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	userID := createTestUser(t, pool, "assignable-batch-exclude")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	vol := createTestVolunteer(t, pool)
	a := queuedAged(t, ctx, pool, repo, leafID, 30)
	b := queuedAged(t, ctx, pool, repo, leafID, 20)
	c := queuedAged(t, ctx, pool, repo, leafID, 10)

	opts := reserveOpts(vol, 0)
	opts.LeafIDs = []types.ID{leafID}
	got, err := repo.FindAssignableBatch(ctx, opts, 10, []types.ID{a.ID, c.ID}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FindAssignableBatch: %v", err)
	}
	if !sameIDs(batchIDs(got), []types.ID{b.ID}) {
		t.Fatalf("batch = %v, want only the unexcluded %s", batchIDs(got), b.ID)
	}
}

// TestFindAssignableBatch_HonoursAndTakesDispatchClaims is the scale-out guarantee: a unit
// another replica holds a live claim on is never returned, and every unit returned is claimed
// for this head, so no other replica's refill can stage it while this one hands it out. The
// claim-blind FindNextAssignable, by contrast, returns the claimed unit, which is why the
// fallback could not use it as it stands.
func TestFindAssignableBatch_HonoursAndTakesDispatchClaims(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	userID := createTestUser(t, pool, "assignable-batch-claims")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	vol := createTestVolunteer(t, pool)
	self, other := types.NewID(), types.NewID()

	claimedLive := queuedAged(t, ctx, pool, repo, leafID, 40)
	claimedExpired := queuedAged(t, ctx, pool, repo, leafID, 30)
	claimedBySelf := queuedAged(t, ctx, pool, repo, leafID, 20)
	unclaimed := queuedAged(t, ctx, pool, repo, leafID, 10)
	setClaim := func(id, head types.ID, expiresIn string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE work_units SET dispatch_claimed_by = $2,
			dispatch_claim_expires_at = NOW() + $3::interval WHERE id = $1`, id, head, expiresIn); err != nil {
			t.Fatalf("seed claim: %v", err)
		}
	}
	setClaim(claimedLive.ID, other, "5 minutes")
	setClaim(claimedExpired.ID, other, "-1 minute")
	setClaim(claimedBySelf.ID, self, "5 minutes")

	opts := reserveOpts(vol, 0)
	opts.LeafIDs = []types.ID{leafID}

	if wu, err := repo.FindNextAssignable(ctx, opts); err != nil || wu == nil || wu.ID != claimedLive.ID {
		t.Fatalf("FindNextAssignable = %v (err %v); expected the claim-blind query to return the unit another replica claimed", wu, err)
	}

	got, err := repo.FindAssignableBatch(ctx, opts, 10, nil, self, 2*time.Minute)
	if err != nil {
		t.Fatalf("FindAssignableBatch: %v", err)
	}
	want := []types.ID{claimedExpired.ID, claimedBySelf.ID, unclaimed.ID}
	if !sameIDs(batchIDs(got), want) {
		t.Fatalf("batch = %v, want %v (never the unit under another replica's live claim)", batchIDs(got), want)
	}
	for _, id := range want {
		owner, exp := claimOf(t, pool, id)
		if owner == nil || *owner != self || exp == nil || !exp.After(time.Now()) {
			t.Fatalf("returned unit %s is not claimed for this head", id)
		}
	}
	if owner, _ := claimOf(t, pool, claimedLive.ID); owner == nil || *owner != other {
		t.Fatalf("the other replica's live claim was disturbed")
	}

	// Another replica's refill now sees only what nobody holds: nothing.
	cands, err := repo.ClaimDispatchableBatch(ctx, other, time.Minute, 10, nil, nil)
	if err != nil {
		t.Fatalf("ClaimDispatchableBatch: %v", err)
	}
	for _, c := range cands {
		if c.WorkUnit.ID != claimedLive.ID {
			t.Fatalf("another replica staged %s, which this head just claimed", c.WorkUnit.ID)
		}
	}

	// Single-replica (no head id): no claim is read or written.
	if _, err := pool.Exec(ctx, `UPDATE work_units SET dispatch_claimed_by = NULL, dispatch_claim_expires_at = NULL`); err != nil {
		t.Fatalf("clear claims: %v", err)
	}
	setClaim(claimedLive.ID, other, "5 minutes")
	got, err = repo.FindAssignableBatch(ctx, opts, 10, nil, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FindAssignableBatch (single replica): %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("single-replica batch = %d units, want all 4 (claims are a scale-out concept)", len(got))
	}
	if owner, _ := claimOf(t, pool, unclaimed.ID); owner != nil {
		t.Fatalf("single-replica batch wrote a claim")
	}
}
