//go:build integration

package workunit

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TB-39: dead-letter parks a unit FAILED + flagged-for-review "for a human to review",
// but the review had no verb — the requeue endpoint refuses FAILED, the state chart gave
// FAILED no exit, and (correctly, since TB-35) dispatch refuses any unit whose copy
// budget reads exhausted, so even a raw state flip left the unit QUEUED and
// undispatchable forever. The TB-35 remediation of 42 healthy units was hand-written SQL
// on both production databases. These tests pin the productized surgery:
// ReviveDeadLettered re-judges verified give-backs RETURNED, refuses a still-exhausted
// poison unit unless the operator explicitly refunds, resurrects the disposed results,
// and requeues — atomically, and to a state dispatch will actually serve.

// insertClosedCopyWithReason is insertClosedCopy plus the recorded outcome_reason —
// started_at stays NULL (the copy never run-started), which is half of the give-back
// verification the revive predicate applies.
func insertClosedCopyWithReason(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, outcome, reason string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at, outcome, outcome_at, outcome_reason)
		VALUES ($1, $2, NOW(), $3::assignment_outcome, NOW(), $4)`, wuID, volID, outcome, reason); err != nil {
		t.Fatalf("insert closed copy with reason: %v", err)
	}
}

// deadLetterForTest drives a unit to the parked state through the REAL executor and
// verifies the shape the revive starts from: FAILED, flagged, results SUPERSEDED.
func deadLetterForTest(t *testing.T, ctx context.Context, repo *PgxWorkUnitRepository, pool *pgxpool.Pool, wuID types.ID) {
	t.Helper()
	failed, err := repo.DeadLetterIfExhausted(ctx, wuID)
	if err != nil {
		t.Fatalf("DeadLetterIfExhausted: %v", err)
	}
	if !failed {
		t.Fatal("test setup: unit did not dead-letter")
	}
	wu, err := repo.GetByID(ctx, wuID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if wu.State != WorkUnitStateFailed || !wu.FlaggedForReview {
		t.Fatalf("test setup: state=%s flagged=%v, want FAILED/flagged", wu.State, wu.FlaggedForReview)
	}
}

// resultStatus reads the single result row's validation_status for a unit.
func resultStatus(t *testing.T, pool *pgxpool.Pool, wuID types.ID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT validation_status FROM results WHERE work_unit_id = $1`, wuID).Scan(&s); err != nil {
		t.Fatalf("read result status: %v", err)
	}
	return s
}

// stagedByRefill reports whether the volunteer-agnostic refill would stage the unit —
// the capsNotExhaustedSQL gate every dispatch read embeds since TB-35. This is the
// assert that separates a real revive from a bare state flip.
func stagedByRefill(t *testing.T, ctx context.Context, repo *PgxWorkUnitRepository, wuID types.ID) bool {
	t.Helper()
	cands, err := repo.FindDispatchableBatch(ctx, 50, nil, nil)
	if err != nil {
		t.Fatalf("FindDispatchableBatch: %v", err)
	}
	for _, c := range cands {
		if c.WorkUnit.ID == wuID {
			return true
		}
	}
	return false
}

// TestTB39_ReviveRestoresGivebackKilledUnit is the TB-35 kill shape end to end: a
// healthy unit whose budget was burned entirely by un-run give-backs (billed ABANDONED
// by pre-v0.10.8 clients), dead-lettered with one honest PENDING result disposed
// SUPERSEDED. The revive must re-judge both give-back reasons RETURNED, resurrect the
// result, requeue, and — the half a bare state flip cannot do — leave the unit staged
// by the real dispatch refill.
func TestTB39_ReviveRestoresGivebackKilledUnit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "tb39-revive")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	volC := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	setMaxTotalCopies(t, pool, wu.ID, 2)
	// The two give-back shapes the predicate must catch: the buffer give-back and the
	// graceful shutdown — both un-started, both billed ABANDONED by an old client.
	insertClosedCopyWithReason(t, pool, wu.ID, volA, "ABANDONED", "work buffer full (over the hours target)")
	insertClosedCopyWithReason(t, pool, wu.ID, volB, "ABANDONED", "volunteer shutdown")
	// The orphaned honest compute (8 of the 33 filed units carried one).
	insertPendingResult(t, pool, wu.ID, volC)

	deadLetterForTest(t, ctx, repo, pool, wu.ID)
	if got := resultStatus(t, pool, wu.ID); got != "SUPERSEDED" {
		t.Fatalf("test setup: result %s, want SUPERSEDED (the ★BG-21i disposal)", got)
	}
	if stagedByRefill(t, ctx, repo, wu.ID) {
		t.Fatal("test setup: a FAILED unit must not be staged")
	}

	out, err := repo.ReviveDeadLettered(ctx, wu.ID, false, 0, 0)
	if err != nil {
		t.Fatalf("ReviveDeadLettered: %v", err)
	}
	if out.ReclassifiedGivebacks != 2 {
		t.Errorf("reclassified = %d, want 2 (both give-back reasons)", out.ReclassifiedGivebacks)
	}
	if out.ResurrectedResults != 1 {
		t.Errorf("resurrected = %d, want 1", out.ResurrectedResults)
	}
	if out.Refunded {
		t.Error("refunded = true on a give-back-only revive, want false")
	}

	got, err := repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != WorkUnitStateQueued || got.FlaggedForReview {
		t.Errorf("state=%s flagged=%v, want QUEUED/unflagged", got.State, got.FlaggedForReview)
	}
	for _, vol := range []types.ID{volA, volB} {
		if o := copyOutcome(t, pool, wu.ID, vol); o != "RETURNED" {
			t.Errorf("give-back copy outcome = %s, want RETURNED", o)
		}
	}
	if got := resultStatus(t, pool, wu.ID); got != "PENDING" {
		t.Errorf("result = %s, want PENDING (the honest compute holds its slot again)", got)
	}
	if n, err := repo.CountTotalCopies(ctx, wu.ID); err != nil || n != 0 {
		t.Errorf("CountTotalCopies = %d (err %v), want 0 — the budget freed", n, err)
	}
	// The money assert: revived means DISPATCHABLE, not just QUEUED.
	if !stagedByRefill(t, ctx, repo, wu.ID) {
		t.Error("revived unit not staged by the dispatch refill — a state flip without budget relief (the TB-39 trap)")
	}
}

// TestTB39_PoisonUnitStaysDeadWithoutOverride: a unit whose billed copies are REAL
// failures keeps its budget exhausted after the give-back re-judgment, so the revive
// refuses — rolling back everything it touched — unless the operator passes the
// explicit refund override, which rebases a fresh budget on top of what is billed.
func TestTB39_PoisonUnitStaysDeadWithoutOverride(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "tb39-poison")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	volC := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	setMaxTotalCopies(t, pool, wu.ID, 2)
	// Real failures: a timeout, and an un-started ABANDONED with no recorded reason —
	// the remediation's "ambiguous rows left billed" class, which the predicate must
	// NOT whitewash.
	insertClosedCopy(t, pool, wu.ID, volA, "EXPIRED")
	insertClosedCopy(t, pool, wu.ID, volB, "ABANDONED")
	insertPendingResult(t, pool, wu.ID, volC)
	deadLetterForTest(t, ctx, repo, pool, wu.ID)

	_, err := repo.ReviveDeadLettered(ctx, wu.ID, false, 0, 0)
	if err == nil {
		t.Fatal("poison unit revived without the override")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("refusal = %T (%v), want *apierror.APIError", err, err)
	}
	if details, _ := apiErr.Details.(map[string]string); details["code"] != "REVIVE_BUDGET_EXHAUSTED" {
		t.Fatalf("refusal = %v, want REVIVE_BUDGET_EXHAUSTED", err)
	}
	if !strings.Contains(apiErr.Message, "refund_real_failures") {
		t.Errorf("the refusal must name the override that would proceed; got: %s", apiErr.Message)
	}
	// The refusal is a pure read: nothing moved.
	got, err := repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != WorkUnitStateFailed || !got.FlaggedForReview {
		t.Errorf("after refusal state=%s flagged=%v, want FAILED/flagged untouched", got.State, got.FlaggedForReview)
	}
	if s := resultStatus(t, pool, wu.ID); s != "SUPERSEDED" {
		t.Errorf("after refusal result = %s, want SUPERSEDED untouched", s)
	}

	// The explicit override: a fresh budget rebased on top of the 2 billed copies.
	out, err := repo.ReviveDeadLettered(ctx, wu.ID, true, 8, 0)
	if err != nil {
		t.Fatalf("ReviveDeadLettered(refund): %v", err)
	}
	if !out.Refunded || out.BudgetCeiling != 10 || out.BudgetSpent != 2 {
		t.Errorf("refund outcome = %+v, want refunded with ceiling 2+8=10 over spent 2", out)
	}
	got, err = repo.GetByID(ctx, wu.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != WorkUnitStateQueued || got.FlaggedForReview {
		t.Errorf("state=%s flagged=%v, want QUEUED/unflagged", got.State, got.FlaggedForReview)
	}
	if s := resultStatus(t, pool, wu.ID); s != "PENDING" {
		t.Errorf("result = %s, want PENDING", s)
	}
	if !stagedByRefill(t, ctx, repo, wu.ID) {
		t.Error("refund-revived unit not staged by the dispatch refill")
	}
}

// TestTB39_MixedHistoryKeepsRealFailuresBilled: the re-judgment is surgical — only
// verified un-started give-back rows flip RETURNED; a real timeout stays billed, and
// the unit revives on the headroom the give-backs alone free.
func TestTB39_MixedHistoryKeepsRealFailuresBilled(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "tb39-mixed")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	volA := createTestVolunteer(t, pool)
	volB := createTestVolunteer(t, pool)
	volC := createTestVolunteer(t, pool)
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	setMaxTotalCopies(t, pool, wu.ID, 3)
	insertClosedCopyWithReason(t, pool, wu.ID, volA, "ABANDONED", "work buffer full (over the hours target)")
	insertClosedCopyWithReason(t, pool, wu.ID, volB, "ABANDONED", "work buffer full and the unit cannot start in the idle slot (memory)")
	insertClosedCopy(t, pool, wu.ID, volC, "EXPIRED")
	deadLetterForTest(t, ctx, repo, pool, wu.ID)

	out, err := repo.ReviveDeadLettered(ctx, wu.ID, false, 0, 0)
	if err != nil {
		t.Fatalf("ReviveDeadLettered: %v", err)
	}
	if out.ReclassifiedGivebacks != 2 {
		t.Errorf("reclassified = %d, want 2 (the EXPIRED row is not a give-back)", out.ReclassifiedGivebacks)
	}
	if o := copyOutcome(t, pool, wu.ID, volC); o != "EXPIRED" {
		t.Errorf("real timeout re-judged to %s — must stay EXPIRED (billed)", o)
	}
	if n, _ := repo.CountTotalCopies(ctx, wu.ID); n != 1 {
		t.Errorf("CountTotalCopies = %d, want 1 (only the real failure billed)", n)
	}
	if !stagedByRefill(t, ctx, repo, wu.ID) {
		t.Error("revived unit not staged by the dispatch refill")
	}
}

// TestTB39_ReviveRefusesNonFailedUnits: the verb is for the dead-letter parking only —
// every other state keeps its existing paths (requeue, the transitioner).
func TestTB39_ReviveRefusesNonFailedUnits(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	userID := createTestUser(t, pool, "tb39-states")
	leafID := createActiveTestLeaf(t, pool, &userID, "", "", "")
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	wu := mustQueuedWU(t, ctx, repo, leafID)
	_, err := repo.ReviveDeadLettered(ctx, wu.ID, false, 0, 0)
	if err == nil {
		t.Fatal("QUEUED unit revived")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("refusal = %T (%v), want *apierror.APIError", err, err)
	}
	if details, _ := apiErr.Details.(map[string]string); details["code"] != "INVALID_REVIVE_STATE" {
		t.Fatalf("refusal = %v, want INVALID_REVIVE_STATE", err)
	}

	if _, err := repo.ReviveDeadLettered(ctx, types.NewID(), false, 0, 0); err == nil {
		t.Fatal("nonexistent unit revived")
	}
}
