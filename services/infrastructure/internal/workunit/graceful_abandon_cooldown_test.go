//go:build integration

package workunit

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// The post-failure cooldown benches a volunteer whose recent copy of a unit ended
// EXPIRED/ABANDONED so a *fresh* volunteer gets first crack on the requeue. That is a
// RELIABILITY signal — it only makes sense for a copy the volunteer actually STARTED
// (run-start set started_at) and then failed to finish before the deadline. A volunteer
// that GRACEFULLY hands back UN-STARTED buffered work (AbandonWorkUnit, reason
// "volunteer shutdown" — the copy is closed ABANDONED with started_at still NULL, and the
// same shape is produced by the buffer reconciler reaping a dropped prefetch) is not
// unreliable, so on a small/dominated pool benching it for a full deadline needlessly
// strands the work. These tests pin that a never-started ABANDONED copy does NOT bench,
// while a started copy (EXPIRED or ABANDONED) still does. (ROADMAP #59)

// FindNextAssignable (the desktop reserve read-side, the SQL reference for the cooldown)
// must offer the unit straight back to a volunteer that gracefully returned an un-started
// buffered copy of it.
func TestFindNextAssignable_GracefulBufferReturnNotBenched(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "find-graceful")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "") // default redundancy 2
	volA := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)

	// volA buffers the unit (a RESERVED copy, started_at NULL) and never run-starts it.
	if _, err := repo.ReserveNextAssignable(ctx, reserveOpts(volA, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve volA: %v", err)
	}
	// volA gracefully abandons the un-started buffered copy (restart / shutdown path):
	// closed ABANDONED, started_at still NULL.
	if err := repo.CloseCopyByVolunteer(ctx, wu.ID, volA, "ABANDONED", nil); err != nil {
		t.Fatalf("close volA copy ABANDONED: %v", err)
	}

	got, err := repo.FindNextAssignable(ctx, reserveOpts(volA, 0))
	if err != nil {
		t.Fatalf("FindNextAssignable: %v", err)
	}
	if got == nil || got.ID != wu.ID {
		t.Fatalf("FindNextAssignable must offer the unit back to a graceful never-started abandoner, got %v", got)
	}
}

// ReserveCopy (the authoritative spot-check landing gate) must NOT bench a graceful
// never-started abandon: the volunteer can re-reserve the very unit it returned.
func TestReserveCopy_GracefulBufferReturnNotBenched(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-graceful")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "") // default redundancy 2
	volA := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)

	if _, err := repo.ReserveNextAssignable(ctx, reserveOpts(volA, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve volA: %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, wu.ID, volA, "ABANDONED", nil); err != nil {
		t.Fatalf("close volA copy ABANDONED: %v", err)
	}

	if _, err := repo.ReserveCopy(ctx, wu.ID, volA, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
		t.Fatalf("ReserveCopy must NOT bench a graceful never-started abandon: %v", err)
	}
}

// FlushReservations (the production hand-out landing path) must LAND a graceful
// never-started abandoner's reservation rather than skipping it as a benched copy.
func TestFlushReservations_GracefulBufferReturnLands(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "flush-graceful")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "") // default redundancy 2
	volA := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)

	if _, err := repo.ReserveNextAssignable(ctx, reserveOpts(volA, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve volA: %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, wu.ID, volA, "ABANDONED", nil); err != nil {
		t.Fatalf("close volA copy ABANDONED: %v", err)
	}

	until := time.Now().UTC().Add(time.Hour)
	landed, err := repo.FlushReservations(ctx, []FlushReservation{
		{WorkUnitID: wu.ID, VolunteerID: volA, ReservedUntil: until, DeadlineSeconds: wu.DeadlineSeconds},
	}, types.ID{}, 0)
	if err != nil {
		t.Fatalf("FlushReservations: %v", err)
	}
	if !containsFlushedPair(landed, wu.ID, volA) {
		t.Fatalf("a graceful never-started abandon must NOT be benched: volA's reservation must land")
	}
}

// FindDispatchableBatch carries the per-unit benched snapshot the in-memory dispatch
// cache reads at hand-out (BenchedVolunteerIDs). A graceful never-started abandoner must
// NOT be in that snapshot.
func TestFindDispatchableBatch_GracefulBufferReturnNotBenched(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "batch-graceful")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "") // default redundancy 2
	volA := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)

	if _, err := repo.ReserveNextAssignable(ctx, reserveOpts(volA, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve volA: %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, wu.ID, volA, "ABANDONED", nil); err != nil {
		t.Fatalf("close volA copy ABANDONED: %v", err)
	}

	cands, err := repo.FindDispatchableBatch(ctx, 10, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	var cand *DispatchCandidate
	for i := range cands {
		if cands[i].WorkUnit.ID == wu.ID {
			cand = &cands[i]
			break
		}
	}
	if cand == nil {
		t.Fatalf("unit not staged by FindDispatchableBatch after a graceful abandon (it should be re-dispatchable)")
	}
	for _, b := range cand.Benched {
		if b.VolunteerID == volA {
			t.Fatalf("graceful never-started abandoner volA must NOT be in the benched snapshot, got %v", cand.Benched)
		}
	}
}

// Guard the other half: a copy the volunteer actually STARTED and then abandoned (a real
// reliability signal) MUST still bench it for ~one deadline, while a fresh volunteer is
// admitted. This pins that the #59 relaxation is scoped to never-started copies only.
func TestReserveCopy_StartedThenAbandonStillBenched(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "reserve-started-abandon")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "") // default redundancy 2
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)

	if _, err := repo.ReserveNextAssignable(ctx, reserveOpts(volA, 0), 60*time.Second); err != nil {
		t.Fatalf("reserve volA: %v", err)
	}
	// volA RUN-STARTS the copy (started_at set), then abandons it mid-run.
	if _, err := repo.Assign(ctx, wu.ID, volA); err != nil {
		t.Fatalf("run-start volA copy: %v", err)
	}
	if err := repo.CloseCopyByVolunteer(ctx, wu.ID, volA, "ABANDONED", nil); err != nil {
		t.Fatalf("close volA copy ABANDONED: %v", err)
	}

	if _, err := repo.ReserveCopy(ctx, wu.ID, volA, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err == nil {
		t.Fatalf("ReserveCopy must STILL bench a started-then-abandoned copy (reliability signal)")
	}
	if _, err := repo.ReserveCopy(ctx, wu.ID, volB, nil, time.Now().UTC().Add(time.Hour), wu.DeadlineSeconds); err != nil {
		t.Fatalf("a fresh volunteer must be admitted during the benched volunteer's cooldown: %v", err)
	}
}
