//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"

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
	})
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
	})
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
