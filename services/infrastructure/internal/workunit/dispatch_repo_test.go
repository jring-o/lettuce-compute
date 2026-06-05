//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Layer 2 dispatch-cache repo methods: FindDispatchableBatch (bulk refill),
// FlushReservations (batched async reservation write with optimistic guard), and
// CountActiveByVolunteer (inflight reconcile). These mirror the reservation-columns
// model the in-memory dispatch cache shadows.

const wasmExecConfig = `{"runtime":"WASM","gpu_required":false}`

// TestFindDispatchableBatch_BulkAndExclude stages several QUEUED units on an ACTIVE
// leaf and asserts the bulk refill returns them (up to LIMIT) while honoring the
// exclude-set (the DB-level backstop against re-staging an in-flight unit).
func TestFindDispatchableBatch_BulkAndExclude(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-bulk")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	var staged []types.ID
	for i := 0; i < 5; i++ {
		wu := mustQueuedWU(t, ctx, repo, leafID)
		staged = append(staged, wu.ID)
	}

	// LIMIT clamps the batch.
	cands, err := repo.FindDispatchableBatch(ctx, 3, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("expected LIMIT 3 candidates, got %d", len(cands))
	}
	for _, c := range cands {
		if c.WorkUnit == nil || c.WorkUnit.State != WorkUnitStateQueued {
			t.Fatalf("candidate is not a QUEUED unit: %+v", c)
		}
		if c.RedundancyFactor != 1 {
			t.Fatalf("expected redundancy 1, got %d", c.RedundancyFactor)
		}
		if c.ActiveAssignments != 0 {
			t.Fatalf("expected 0 active assignments, got %d", c.ActiveAssignments)
		}
		if c.Runtime != "NATIVE" {
			t.Fatalf("expected NATIVE runtime, got %q", c.Runtime)
		}
	}

	// Exclude all 5: nothing is returned (the in-memory-held backstop).
	excluded, err := repo.FindDispatchableBatch(ctx, 10, staged, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch (excluded): %v", err)
	}
	if len(excluded) != 0 {
		t.Fatalf("exclude-set should hide every staged unit, got %d", len(excluded))
	}
}

// TestFindDispatchableBatch_ExcludesWASM asserts the cache refill never stages a
// WASM-runtime unit (those are dispatched by the immediate-assign browser path,
// partitioned by runtime).
func TestFindDispatchableBatch_ExcludesWASM(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-wasm")
	nativeLeaf := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	wasmLeaf := createActiveTestLeaf(t, pool, &userID, "", wasmExecConfig, valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	nativeWU := mustQueuedWU(t, ctx, repo, nativeLeaf)
	_ = mustQueuedWU(t, ctx, repo, wasmLeaf)

	cands, err := repo.FindDispatchableBatch(ctx, 10, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected only the NATIVE unit, got %d candidates", len(cands))
	}
	if cands[0].WorkUnit.ID != nativeWU.ID {
		t.Fatalf("WASM unit leaked into the dispatch cache refill")
	}
}

// TestFindDispatchableBatch_LeafScoped asserts the leaf-scoped refill (Blocker 2)
// returns only units from the requested leafs, so a leaf-filtered requester can be
// served even when the global pool is monopolized by a different leaf.
func TestFindDispatchableBatch_LeafScoped(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-leaf-scope")
	leafA := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	leafB := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_ = mustQueuedWU(t, ctx, repo, leafA)
	}
	bUnit := mustQueuedWU(t, ctx, repo, leafB)

	// Leaf-agnostic refill returns units from both leafs.
	all, err := repo.FindDispatchableBatch(ctx, 10, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch (all): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 candidates across both leafs, got %d", len(all))
	}

	// Leaf-scoped to B returns only B's unit.
	scoped, err := repo.FindDispatchableBatch(ctx, 10, nil, []types.ID{leafB})
	if err != nil {
		t.Fatalf("FindDispatchableBatch (leaf-scoped): %v", err)
	}
	if len(scoped) != 1 {
		t.Fatalf("expected only leaf B's unit, got %d candidates", len(scoped))
	}
	if scoped[0].WorkUnit.ID != bUnit.ID {
		t.Fatalf("leaf-scoped refill returned the wrong unit")
	}
}

// TestFlushReservations_LandsAndConflicts asserts the batched reservation write
// lands a reservation on a QUEUED unit (returning its id) and reports a conflict
// (id NOT returned) when the unit is already reserved to a DIFFERENT volunteer.
func TestFlushReservations_LandsAndConflicts(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-flush")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	free := mustQueuedWU(t, ctx, repo, leafID)
	taken := mustQueuedWU(t, ctx, repo, leafID)

	volA := types.NewID()
	volB := types.NewID()

	// Pre-reserve `taken` to volA via the existing stamp path.
	if _, err := repo.StampReservation(ctx, taken.ID, volA, time.Hour); err != nil {
		t.Fatalf("StampReservation: %v", err)
	}

	// volB tries to flush both: `free` lands, `taken` conflicts (held by volA).
	until := time.Now().UTC().Add(15 * time.Minute)
	landed, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: free.ID, VolunteerID: volB, ReservedUntil: until},
		{WorkUnitID: taken.ID, VolunteerID: volB, ReservedUntil: until},
	}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations: %v", err)
	}
	if len(landed) != 1 || landed[0] != free.ID {
		t.Fatalf("expected only %s to land, got %v", free.ID, landed)
	}

	// `free` is now reserved to volB; `taken` is still reserved to volA.
	got, err := repo.GetByID(ctx, free.ID)
	if err != nil {
		t.Fatalf("GetByID(free): %v", err)
	}
	if got.ReservedVolunteerID == nil || *got.ReservedVolunteerID != volB {
		t.Fatalf("free should be reserved to volB")
	}
	gotTaken, err := repo.GetByID(ctx, taken.ID)
	if err != nil {
		t.Fatalf("GetByID(taken): %v", err)
	}
	if gotTaken.ReservedVolunteerID == nil || *gotTaken.ReservedVolunteerID != volA {
		t.Fatalf("taken should remain reserved to volA")
	}
}

// TestFlushReservations_ReReservesOwnAndLapsed asserts the optimistic guard allows
// re-reserving a unit already held by the SAME volunteer (idempotent re-flush) and
// a unit whose lease has lapsed.
func TestFlushReservations_ReReservesOwnAndLapsed(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-rereserve")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	own := mustQueuedWU(t, ctx, repo, leafID)
	lapsed := mustQueuedWU(t, ctx, repo, leafID)

	vol := types.NewID()
	other := types.NewID()

	// `own` already reserved to vol; `lapsed` reserved to `other` but expired.
	if _, err := repo.StampReservation(ctx, own.ID, vol, time.Hour); err != nil {
		t.Fatalf("StampReservation(own): %v", err)
	}
	if _, err := repo.StampReservation(ctx, lapsed.ID, other, -time.Minute); err != nil {
		t.Fatalf("StampReservation(lapsed): %v", err)
	}

	until := time.Now().UTC().Add(15 * time.Minute)
	landed, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: own.ID, VolunteerID: vol, ReservedUntil: until},
		{WorkUnitID: lapsed.ID, VolunteerID: vol, ReservedUntil: until},
	}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations: %v", err)
	}
	if len(landed) != 2 {
		t.Fatalf("re-reserve-own + lapsed-takeover should both land, got %v", landed)
	}
}

// TestCountActiveByVolunteer counts live reservations + active history rows per
// volunteer (the inflight reconcile source).
func TestCountActiveByVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-count")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	vol := types.NewID()

	// Two live reservations for vol.
	r1 := mustQueuedWU(t, ctx, repo, leafID)
	r2 := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.StampReservation(ctx, r1.ID, vol, time.Hour); err != nil {
		t.Fatalf("StampReservation(r1): %v", err)
	}
	if _, err := repo.StampReservation(ctx, r2.ID, vol, time.Hour); err != nil {
		t.Fatalf("StampReservation(r2): %v", err)
	}

	// One LAPSED reservation must NOT be counted.
	rl := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.StampReservation(ctx, rl.ID, vol, -time.Minute); err != nil {
		t.Fatalf("StampReservation(rl): %v", err)
	}

	counts, err := repo.CountActiveByVolunteer(ctx)
	if err != nil {
		t.Fatalf("CountActiveByVolunteer: %v", err)
	}
	if counts[vol] != 2 {
		t.Fatalf("expected 2 live reservations for vol, got %d", counts[vol])
	}
}

// --- Layer 3 (claim-on-refill) integration tests ------------------------------

// claimOf reads the dispatch-claim columns of a unit directly.
func claimOf(t *testing.T, pool *pgxpool.Pool, id types.ID) (claimedBy *types.ID, expires *time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT dispatch_claimed_by, dispatch_claim_expires_at FROM work_units WHERE id = $1`, id,
	).Scan(&claimedBy, &expires); err != nil {
		t.Fatalf("read claim columns: %v", err)
	}
	return claimedBy, expires
}

// TestClaimDispatchableBatch_StampsAndExcludes proves claim-on-refill: the claim
// UPDATE stamps the head as owner, a second head's claim is EXCLUDED while the first
// is live, and the SAME head re-claims its own units.
func TestClaimDispatchableBatch_StampsAndExcludes(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-claim")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	headA := types.NewID()
	headB := types.NewID()

	var ids []types.ID
	for i := 0; i < 4; i++ {
		ids = append(ids, mustQueuedWU(t, ctx, repo, leafID).ID)
	}

	// Head A claims all 4. Each carries A's owner + a future expiry; units stay QUEUED.
	candsA, err := repo.ClaimDispatchableBatch(ctx, headA, 5*time.Minute, 10, nil, nil)
	if err != nil {
		t.Fatalf("ClaimDispatchableBatch(A): %v", err)
	}
	if len(candsA) != 4 {
		t.Fatalf("head A should claim 4 units, got %d", len(candsA))
	}
	for _, c := range candsA {
		if c.WorkUnit.State != WorkUnitStateQueued {
			t.Fatalf("claimed unit must stay QUEUED, got %s", c.WorkUnit.State)
		}
		owner, exp := claimOf(t, pool, c.WorkUnit.ID)
		if owner == nil || *owner != headA {
			t.Fatalf("unit %s not claimed by head A", c.WorkUnit.ID)
		}
		if exp == nil || !exp.After(time.Now()) {
			t.Fatalf("unit %s has no future claim expiry", c.WorkUnit.ID)
		}
	}

	// Head B's refill sees nothing: every unit is under head A's LIVE claim.
	candsB, err := repo.ClaimDispatchableBatch(ctx, headB, 5*time.Minute, 10, nil, nil)
	if err != nil {
		t.Fatalf("ClaimDispatchableBatch(B): %v", err)
	}
	if len(candsB) != 0 {
		t.Fatalf("head B must not claim units under head A's live claim, got %d", len(candsB))
	}

	// Head A re-claims its OWN units (a re-stage / renew) without conflict.
	candsA2, err := repo.ClaimDispatchableBatch(ctx, headA, 5*time.Minute, 10, nil, nil)
	if err != nil {
		t.Fatalf("ClaimDispatchableBatch(A re-claim): %v", err)
	}
	if len(candsA2) != 4 {
		t.Fatalf("head A should re-claim its own 4 units, got %d", len(candsA2))
	}
}

// TestClaimDispatchableBatch_ExpiredIsReclaimable: once a claim's expiry passes, a
// DIFFERENT head re-claims the unit (the passive-expiry crash-reclaim guarantee).
func TestClaimDispatchableBatch_ExpiredIsReclaimable(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-claim-expired")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	headA := types.NewID()
	headB := types.NewID()
	wu := mustQueuedWU(t, ctx, repo, leafID)

	// Seed an ALREADY-EXPIRED claim owned by head A directly (a crashed replica that
	// stopped renewing). ClaimDispatchableBatch clamps a non-positive lease to the
	// default, so we set the expiry in the past via SQL.
	if _, err := pool.Exec(ctx,
		`UPDATE work_units SET dispatch_claimed_by = $2, dispatch_claim_expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`,
		wu.ID, headA,
	); err != nil {
		t.Fatalf("seed expired claim: %v", err)
	}
	owner, _ := claimOf(t, pool, wu.ID)
	if owner == nil || *owner != headA {
		t.Fatalf("unit not initially claimed by head A")
	}

	// Head B re-claims the expired unit.
	candsB, err := repo.ClaimDispatchableBatch(ctx, headB, 5*time.Minute, 10, nil, nil)
	if err != nil {
		t.Fatalf("ClaimDispatchableBatch(B reclaim): %v", err)
	}
	if len(candsB) != 1 || candsB[0].WorkUnit.ID != wu.ID {
		t.Fatalf("head B should re-claim the expired unit, got %v", candsB)
	}
	owner, _ = claimOf(t, pool, wu.ID)
	if owner == nil || *owner != headB {
		t.Fatalf("expired claim should now belong to head B")
	}
}

// TestClaimReleaseOnAssignAndReservationClear: Assign and ClearReservation both NULL
// the dispatch-claim columns so a unit leaving the dispatchable universe (run-start)
// or abandoned never strands its claim.
func TestClaimReleaseOnAssignAndReservationClear(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-claim-release")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()
	head := types.NewID()
	vol := createTestVolunteer(t, pool)

	// Assign clears the claim.
	a := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ClaimDispatchableBatch(ctx, head, 5*time.Minute, 10, nil, nil); err != nil {
		t.Fatalf("claim(a): %v", err)
	}
	if _, err := repo.Assign(ctx, a.ID, vol); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if owner, exp := claimOf(t, pool, a.ID); owner != nil || exp != nil {
		t.Fatalf("Assign must NULL the claim columns, got owner=%v exp=%v", owner, exp)
	}

	// ClearReservation (abandon path) clears the claim on a reserved+claimed unit.
	b := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ClaimDispatchableBatch(ctx, head, 5*time.Minute, 10, nil, nil); err != nil {
		t.Fatalf("claim(b): %v", err)
	}
	if _, err := repo.StampReservation(ctx, b.ID, vol, time.Hour); err != nil {
		t.Fatalf("StampReservation(b): %v", err)
	}
	if _, err := repo.ClearReservation(ctx, b.ID, vol); err != nil {
		t.Fatalf("ClearReservation(b): %v", err)
	}
	if owner, exp := claimOf(t, pool, b.ID); owner != nil || exp != nil {
		t.Fatalf("ClearReservation must NULL the claim columns, got owner=%v exp=%v", owner, exp)
	}
}

// TestUpdateStateClearsClaim: the requeue path (EXPIRED/REJECTED -> QUEUED via
// UpdateState) clears the dispatch claim, so a re-QUEUED unit is immediately
// re-claimable independent of Assign ordering.
func TestUpdateStateClearsClaim(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-claim-requeue")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()
	head := types.NewID()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	// Seed an EXPIRED-state unit that still carries a live dispatch claim (the
	// stranded-claim case the requeue path must self-heal): the claim was, e.g.,
	// stamped before the unit was dispatched and the run later expired.
	if _, err := pool.Exec(ctx,
		`UPDATE work_units SET state = 'EXPIRED', dispatch_claimed_by = $2, dispatch_claim_expires_at = NOW() + INTERVAL '5 minutes' WHERE id = $1`,
		wu.ID, head,
	); err != nil {
		t.Fatalf("seed expired-state claimed unit: %v", err)
	}
	// Reassign's requeue path: EXPIRED -> QUEUED via UpdateState.
	if _, err := repo.UpdateState(ctx, wu.ID, WorkUnitStateExpired, WorkUnitStateQueued); err != nil {
		t.Fatalf("UpdateState(EXPIRED->QUEUED): %v", err)
	}
	if owner, exp := claimOf(t, pool, wu.ID); owner != nil || exp != nil {
		t.Fatalf("requeue (UpdateState) must NULL the claim, got owner=%v exp=%v", owner, exp)
	}
}

// TestFlushReservationsRenewsClaim: the reservation flush extends THIS head's claim
// (so a held-but-unflushed unit's claim never expires under it) and never touches
// another head's claim.
func TestFlushReservationsRenewsClaim(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-claim-renew")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()
	headA := types.NewID()
	headB := types.NewID()
	vol := types.NewID()

	// Unit owned by head A with a SHORT (about-to-expire) claim.
	ownUnit := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ClaimDispatchableBatch(ctx, headA, time.Second, 10, nil, nil); err != nil {
		t.Fatalf("claim(own): %v", err)
	}
	_, expBefore := claimOf(t, pool, ownUnit.ID)

	// Unit owned by head B (a different replica) — head A's flush must NOT touch it.
	otherUnit := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ClaimDispatchableBatch(ctx, headB, 5*time.Minute, 10, []types.ID{ownUnit.ID}, nil); err != nil {
		t.Fatalf("claim(other): %v", err)
	}
	_, otherExpBefore := claimOf(t, pool, otherUnit.ID)

	until := time.Now().UTC().Add(15 * time.Minute)
	landed, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: ownUnit.ID, VolunteerID: vol, ReservedUntil: until},
		{WorkUnitID: otherUnit.ID, VolunteerID: vol, ReservedUntil: until},
	}, headA, 10*time.Minute)
	if err != nil {
		t.Fatalf("FlushReservations: %v", err)
	}
	if len(landed) != 2 {
		t.Fatalf("both reservations should land, got %v", landed)
	}

	// Head A's own claim is RENEWED (expiry pushed far out, owner unchanged).
	ownerOwn, expAfter := claimOf(t, pool, ownUnit.ID)
	if ownerOwn == nil || *ownerOwn != headA {
		t.Fatalf("own unit claim owner changed unexpectedly: %v", ownerOwn)
	}
	if expBefore != nil && expAfter != nil && !expAfter.After(*expBefore) {
		t.Fatalf("flush should renew (extend) head A's claim; before=%v after=%v", expBefore, expAfter)
	}

	// Head B's claim is UNTOUCHED by head A's flush (owner + expiry unchanged).
	ownerOther, otherExpAfter := claimOf(t, pool, otherUnit.ID)
	if ownerOther == nil || *ownerOther != headB {
		t.Fatalf("head A's flush hijacked head B's claim: %v", ownerOther)
	}
	if otherExpBefore != nil && otherExpAfter != nil && !otherExpAfter.Equal(*otherExpBefore) {
		t.Fatalf("head A's flush must not renew head B's claim; before=%v after=%v", otherExpBefore, otherExpAfter)
	}
}

// TestClearExpiredDispatchClaims: the hygiene sweep NULLs only EXPIRED claims and
// leaves live claims intact.
func TestClearExpiredDispatchClaims(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-claim-hygiene")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()
	head := types.NewID()

	expired := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := pool.Exec(ctx,
		`UPDATE work_units SET dispatch_claimed_by = $2, dispatch_claim_expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`,
		expired.ID, head,
	); err != nil {
		t.Fatalf("seed expired claim: %v", err)
	}
	live := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ClaimDispatchableBatch(ctx, head, 5*time.Minute, 10, []types.ID{expired.ID}, nil); err != nil {
		t.Fatalf("claim(live): %v", err)
	}

	cleared, err := repo.ClearExpiredDispatchClaims(ctx)
	if err != nil {
		t.Fatalf("ClearExpiredDispatchClaims: %v", err)
	}
	if cleared != 1 {
		t.Fatalf("expected exactly 1 expired claim cleared, got %d", cleared)
	}
	if owner, _ := claimOf(t, pool, expired.ID); owner != nil {
		t.Fatalf("expired claim should be NULLed")
	}
	if owner, _ := claimOf(t, pool, live.ID); owner == nil || *owner != head {
		t.Fatalf("live claim must remain intact")
	}
}
