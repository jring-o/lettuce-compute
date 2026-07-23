//go:build integration

package workunit

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// PB-38 regression: the dispatch selection queries had NO visibility predicate, so a
// head whose only leafs are UNLISTED handed their units to volunteers that never
// pinned them (the volunteer any-leaf fallback requests with no leaf filter),
// contradicting the visibility semantics the catalog enforces (GetHeadInfo lists
// PUBLIC ACTIVE leafs only). The rule now: non-PUBLIC leafs are excluded from
// any-leaf/catalog-driven selection everywhere, and still served when the request
// names the leaf id explicitly (the pin-by-id opt-in).

func visibilityAnyLeafOpts(requester types.ID) AssignmentOptions {
	return AssignmentOptions{
		VolunteerID:       requester,
		MaxCPUCores:       4,
		MaxMemoryMB:       4096,
		MaxDiskMB:         1 << 40,
		AvailableRuntimes: []string{"NATIVE"},
		HRClass:           "unknown/unknown/unknown",
	}
}

// TestFindDispatchableBatch_GlobalRefillExcludesNonPublic pins the cache-refill half:
// the GLOBAL refill (no leaf scope) must not stage UNLISTED/PRIVATE leafs' units into
// the shared ready pool, while a LEAF-SCOPED refill (the pin-by-id path) still stages
// them.
func TestFindDispatchableBatch_GlobalRefillExcludesNonPublic(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-vis")
	publicLeaf := createActiveTestLeafVis(t, pool, &userID, "", "", valConfigRedundancy1, "PUBLIC")
	unlistedLeaf := createActiveTestLeafVis(t, pool, &userID, "", "", valConfigRedundancy1, "UNLISTED")
	privateLeaf := createActiveTestLeafVis(t, pool, &userID, "", "", valConfigRedundancy1, "PRIVATE")
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	publicWU := mustQueuedWU(t, ctx, repo, publicLeaf)
	unlistedWU := mustQueuedWU(t, ctx, repo, unlistedLeaf)
	privateWU := mustQueuedWU(t, ctx, repo, privateLeaf)

	// Global refill: PUBLIC only.
	cands, err := repo.FindDispatchableBatch(ctx, 10, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch (global): %v", err)
	}
	staged := map[types.ID]bool{}
	for _, c := range cands {
		staged[c.WorkUnit.ID] = true
	}
	if !staged[publicWU.ID] {
		t.Fatalf("global refill must stage the PUBLIC leaf's unit")
	}
	if staged[unlistedWU.ID] {
		t.Fatalf("global refill staged an UNLISTED leaf's unit: any-leaf volunteers would be handed work from a leaf hidden from the catalog (PB-38)")
	}
	if staged[privateWU.ID] {
		t.Fatalf("global refill staged a PRIVATE leaf's unit (PB-38)")
	}

	// Leaf-scoped refill (pin-by-id): the named non-PUBLIC leaf still stages.
	scoped, err := repo.FindDispatchableBatch(ctx, 10, nil, []types.ID{unlistedLeaf})
	if err != nil {
		t.Fatalf("FindDispatchableBatch (scoped): %v", err)
	}
	if len(scoped) != 1 || scoped[0].WorkUnit.ID != unlistedWU.ID {
		t.Fatalf("leaf-scoped refill must stage the pinned UNLISTED leaf's unit, got %d candidates", len(scoped))
	}
}

// TestClaimDispatchableBatch_GlobalRefillExcludesNonPublic is the Layer-3
// (scale-out claim-on-refill) twin of the test above: same predicate set, plus the
// atomic claim stamp.
func TestClaimDispatchableBatch_GlobalRefillExcludesNonPublic(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-vis-claim")
	unlistedLeaf := createActiveTestLeafVis(t, pool, &userID, "", "", valConfigRedundancy1, "UNLISTED")
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	unlistedWU := mustQueuedWU(t, ctx, repo, unlistedLeaf)
	headID := types.NewID()

	cands, err := repo.ClaimDispatchableBatch(ctx, headID, 0, 10, nil, nil)
	if err != nil {
		t.Fatalf("ClaimDispatchableBatch (global): %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("global claim-refill claimed %d unit(s) of an UNLISTED leaf (PB-38)", len(cands))
	}

	scoped, err := repo.ClaimDispatchableBatch(ctx, headID, 0, 10, nil, []types.ID{unlistedLeaf})
	if err != nil {
		t.Fatalf("ClaimDispatchableBatch (scoped): %v", err)
	}
	if len(scoped) != 1 || scoped[0].WorkUnit.ID != unlistedWU.ID {
		t.Fatalf("leaf-scoped claim-refill must claim the pinned UNLISTED leaf's unit, got %d candidates", len(scoped))
	}
}

// TestFindNextAssignable_NonPublicRequiresExplicitPin pins the read-side gate the
// browser immediate-assign path and the gRPC Layer-1 fallback share: an any-leaf
// request must not receive a non-PUBLIC leaf's unit; a request naming the leaf id
// still does. (The parity suite's visibility_* scenarios pin the same rule across
// all four predicate layers; this is the direct filed-repro shape.)
func TestFindNextAssignable_NonPublicRequiresExplicitPin(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "dispatch-vis-fna")
	unlistedLeaf := createActiveTestLeafVis(t, pool, &userID, "", "", valConfigRedundancy1, "UNLISTED")
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	unlistedWU := mustQueuedWU(t, ctx, repo, unlistedLeaf)
	requester := createTestVolunteer(t, pool)

	// Any-leaf request (the volunteer fallback with no leaf filter): nothing.
	anyLeaf := visibilityAnyLeafOpts(requester)
	got, err := repo.FindNextAssignable(ctx, anyLeaf)
	if err != nil {
		t.Fatalf("FindNextAssignable (any-leaf): %v", err)
	}
	if got != nil {
		t.Fatalf("any-leaf request was served unit %s of an UNLISTED leaf the volunteer never pinned (PB-38)", got.ID)
	}

	// Pin-by-id request: served.
	pinned := visibilityAnyLeafOpts(requester)
	pinned.LeafIDs = []types.ID{unlistedLeaf}
	got, err = repo.FindNextAssignable(ctx, pinned)
	if err != nil {
		t.Fatalf("FindNextAssignable (pinned): %v", err)
	}
	if got == nil || got.ID != unlistedWU.ID {
		t.Fatalf("pin-by-id request must be served the UNLISTED leaf's unit, got %v", got)
	}
}
