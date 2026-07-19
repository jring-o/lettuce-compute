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

// TestFindDispatchableBatch_StagesWASM asserts the cache refill stages
// WASM-runtime units alongside every other runtime (PB-11), carrying the runtime
// so the in-memory capability gate can scope them to WASM-advertising volunteers.
// The pre-PB-11 exclusion ("WASM is dispatched by the immediate-assign browser
// path, not the cache") made the CLI's WASI runtime unreachable, because gRPC
// serves only from the cache.
func TestFindDispatchableBatch_StagesWASM(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-wasm")
	nativeLeaf := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	wasmLeaf := createActiveTestLeaf(t, pool, &userID, "", wasmExecConfig, valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	nativeWU := mustQueuedWU(t, ctx, repo, nativeLeaf)
	wasmWU := mustQueuedWU(t, ctx, repo, wasmLeaf)

	cands, err := repo.FindDispatchableBatch(ctx, 10, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected both the NATIVE and the WASM unit staged, got %d candidates", len(cands))
	}
	byID := make(map[types.ID]string, len(cands))
	for _, c := range cands {
		byID[c.WorkUnit.ID] = c.Runtime
	}
	if rt, ok := byID[nativeWU.ID]; !ok || rt != "NATIVE" {
		t.Fatalf("NATIVE unit missing or mis-labeled (runtime=%q, present=%v)", rt, ok)
	}
	if rt, ok := byID[wasmWU.ID]; !ok || rt != "WASM" {
		t.Fatalf("WASM unit missing from the refill or mis-labeled (runtime=%q, present=%v): the CLI's WASI runtime stays unreachable (PB-11)", rt, ok)
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
// lands a copy on a QUEUED unit with headroom (returning its (unit, volunteer) pair)
// and reports a conflict (pair NOT returned) when the unit's redundancy is already
// met by a live copy held by a DIFFERENT volunteer.
func TestFlushReservations_LandsAndConflicts(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-flush")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	free := mustQueuedWU(t, ctx, repo, leafID)
	taken := mustQueuedWU(t, ctx, repo, leafID)

	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)

	// Pre-reserve `taken` to volA via a per-copy RESERVED row (redundancy-1 met).
	if _, err := repo.ReserveCopy(ctx, taken.ID, volA, nil, time.Now().UTC().Add(time.Hour), 3600); err != nil {
		t.Fatalf("ReserveCopy(taken): %v", err)
	}

	// volB tries to flush both: `free` lands, `taken` conflicts (redundancy already
	// met by volA's live copy).
	until := time.Now().UTC().Add(15 * time.Minute)
	landed, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: free.ID, VolunteerID: volB, ReservedUntil: until, DeadlineSeconds: 3600},
		{WorkUnitID: taken.ID, VolunteerID: volB, ReservedUntil: until, DeadlineSeconds: 3600},
	}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations: %v", err)
	}
	if len(landed) != 1 || landed[0].WorkUnitID != free.ID || landed[0].VolunteerID != volB {
		t.Fatalf("expected only (%s, volB) to land, got %v", free.ID, landed)
	}

	// `free` now has a live copy held by volB; `taken` still by volA.
	if vols := liveCopyVolunteers(t, pool, free.ID); len(vols) != 1 || vols[0] != volB {
		t.Fatalf("free should have one live copy held by volB, got %v", vols)
	}
	if vols := liveCopyVolunteers(t, pool, taken.ID); len(vols) != 1 || vols[0] != volA {
		t.Fatalf("taken should remain held by volA, got %v", vols)
	}
}

// TestFlushReservations_ParallelCopiesUpToRedundancy asserts the per-copy flush:
// under a redundancy-2 leaf a SECOND distinct volunteer lands a parallel copy on a
// unit already holding one, an idempotent re-flush for a volunteer that already
// holds a live copy is silently dropped (ON CONFLICT DO NOTHING — not returned),
// and a THIRD volunteer is rejected once redundancy is met.
func TestFlushReservations_ParallelCopiesUpToRedundancy(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-rereserve")
	// Default valConfig has redundancy_factor 2.
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	unit := mustQueuedWU(t, ctx, repo, leafID)

	first := createTestVolunteer(t, pool)
	second := createTestVolunteer(t, pool)
	third := createTestVolunteer(t, pool)

	// `first` already holds a live copy.
	if _, err := repo.ReserveCopy(ctx, unit.ID, first, nil, time.Now().UTC().Add(time.Hour), 3600); err != nil {
		t.Fatalf("ReserveCopy(first): %v", err)
	}

	until := time.Now().UTC().Add(15 * time.Minute)

	// `second` lands a parallel copy (1 live < redundancy 2); a re-flush for `first`
	// is dropped (it already holds a live copy → ON CONFLICT DO NOTHING).
	landed, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: unit.ID, VolunteerID: second, ReservedUntil: until, DeadlineSeconds: 3600},
		{WorkUnitID: unit.ID, VolunteerID: first, ReservedUntil: until, DeadlineSeconds: 3600},
	}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations: %v", err)
	}
	if len(landed) != 1 || landed[0].VolunteerID != second {
		t.Fatalf("expected only the parallel copy for `second` to land, got %v", landed)
	}
	if vols := liveCopyVolunteers(t, pool, unit.ID); len(vols) != 2 {
		t.Fatalf("expected two distinct live copies after parallel flush, got %v", vols)
	}

	// `third` is rejected: redundancy 2 is now met (2 live copies).
	landed2, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: unit.ID, VolunteerID: third, ReservedUntil: until, DeadlineSeconds: 3600},
	}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations(third): %v", err)
	}
	if len(landed2) != 0 {
		t.Fatalf("expected redundancy cap to reject `third`, got %v", landed2)
	}
}

// TestCountActiveByVolunteer counts live copies (RESERVED + RUNNING history rows,
// outcome IS NULL) per volunteer (the inflight reconcile source). A CLOSED copy is
// not counted.
func TestCountActiveByVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-count")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	vol := createTestVolunteer(t, pool)
	until := time.Now().UTC().Add(time.Hour)

	// Two live copies for vol.
	r1 := mustQueuedWU(t, ctx, repo, leafID)
	r2 := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveCopy(ctx, r1.ID, vol, nil, until, 3600); err != nil {
		t.Fatalf("ReserveCopy(r1): %v", err)
	}
	if _, err := repo.ReserveCopy(ctx, r2.ID, vol, nil, until, 3600); err != nil {
		t.Fatalf("ReserveCopy(r2): %v", err)
	}

	// One CLOSED copy must NOT be counted (a lapsed reserved_until alone keeps the
	// copy live — only a set outcome retires it from the inflight count).
	rc := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveCopy(ctx, rc.ID, vol, nil, until, 3600); err != nil {
		t.Fatalf("ReserveCopy(rc): %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, rc.ID, vol, "ABANDONED", nil); err != nil {
		t.Fatalf("CloseCopyByVolunteer(rc): %v", err)
	}

	counts, err := repo.CountActiveByVolunteer(ctx)
	if err != nil {
		t.Fatalf("CountActiveByVolunteer: %v", err)
	}
	if counts[vol] != 2 {
		t.Fatalf("expected 2 live copies for vol, got %d", counts[vol])
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

// TestClaimPersistsThroughRunStart: in the per-copy model a unit stays QUEUED through
// run-start (Assign), so the dispatching head KEEPS its claim — the unit may still
// need more redundancy copies dispatched, and the claim is what keeps every other
// replica from also staging it. (The claim is released by a state transition via
// UpdateState — see TestUpdateStateClearsClaim — or simply expires.)
func TestClaimPersistsThroughRunStart(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-claim-runstart")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()
	head := types.NewID()
	vol := createTestVolunteer(t, pool)

	a := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ClaimDispatchableBatch(ctx, head, 5*time.Minute, 10, nil, nil); err != nil {
		t.Fatalf("claim(a): %v", err)
	}
	// Buffer a copy, then run-start it. The unit stays QUEUED.
	if _, err := repo.ReserveCopy(ctx, a.ID, vol, nil, time.Now().UTC().Add(time.Hour), 3600); err != nil {
		t.Fatalf("ReserveCopy: %v", err)
	}
	if _, err := repo.Assign(ctx, a.ID, vol); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	got, err := repo.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != WorkUnitStateQueued {
		t.Fatalf("run-start must keep the unit QUEUED, got %s", got.State)
	}
	if owner, exp := claimOf(t, pool, a.ID); owner == nil || *owner != head || exp == nil {
		t.Fatalf("run-start must keep the head's dispatch claim, got owner=%v exp=%v", owner, exp)
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
	// Seed a real volunteer: FlushReservations inserts work_unit_assignment_history rows
	// with a volunteer_id FK, so the holder must exist (other tests use this helper too).
	vol := createTestVolunteer(t, pool)

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

// TestReleaseStaleBufferedCopies asserts the buffer reconcile closes a volunteer's
// buffered (RESERVED, un-started) copies it no longer reports holding, while leaving
// the ones it still holds, leaving RUNNING copies untouched, and respecting the grace
// window (a copy newer than the cutoff is never released).
func TestReleaseStaleBufferedCopies(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "release-stale-buffered")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", valConfigRedundancy1)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()
	vol := createTestVolunteer(t, pool)

	until := time.Now().UTC().Add(time.Hour)
	heldUnit := mustQueuedWU(t, ctx, repo, leafID)
	droppedUnit := mustQueuedWU(t, ctx, repo, leafID)
	runningUnit := mustQueuedWU(t, ctx, repo, leafID)

	for _, wu := range []*WorkUnit{heldUnit, droppedUnit, runningUnit} {
		if _, err := repo.ReserveCopy(ctx, wu.ID, vol, nil, until, 3600); err != nil {
			t.Fatalf("ReserveCopy(%s): %v", wu.ID, err)
		}
	}
	// Run-start the running unit so its copy is no longer buffered (started_at set).
	if _, err := repo.Assign(ctx, runningUnit.ID, vol); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	// Volunteer reports holding only heldUnit. Future cutoff so the grace window does
	// not protect the just-created copies.
	released, err := repo.ReleaseStaleBufferedCopies(ctx, vol, []types.ID{heldUnit.ID}, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatalf("ReleaseStaleBufferedCopies: %v", err)
	}
	if len(released) != 1 || released[0] != droppedUnit.ID {
		t.Fatalf("expected only droppedUnit released, got %v", released)
	}
	if n, _ := repo.CountLiveCopies(ctx, droppedUnit.ID); n != 0 {
		t.Errorf("droppedUnit should have 0 live copies after release, got %d", n)
	}
	if n, _ := repo.CountLiveCopies(ctx, heldUnit.ID); n != 1 {
		t.Errorf("heldUnit must remain live (still reported held), got %d", n)
	}
	if n, _ := repo.CountLiveCopies(ctx, runningUnit.ID); n != 1 {
		t.Errorf("runningUnit must remain live (started copies ride their deadline), got %d", n)
	}

	// Grace window: a buffered copy newer than the cutoff is NOT released even when not
	// held (empty held set = volunteer holds nothing).
	graceUnit := mustQueuedWU(t, ctx, repo, leafID)
	if _, err := repo.ReserveCopy(ctx, graceUnit.ID, vol, nil, until, 3600); err != nil {
		t.Fatalf("ReserveCopy(grace): %v", err)
	}
	releasedGrace, err := repo.ReleaseStaleBufferedCopies(ctx, vol, nil, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ReleaseStaleBufferedCopies (grace): %v", err)
	}
	if len(releasedGrace) != 0 {
		t.Fatalf("grace window must protect a freshly-created copy, got %v", releasedGrace)
	}
}
