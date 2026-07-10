//go:build integration

package credit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// randChecksum returns a fresh 64-hex-char string for a result's output_checksum.
func randChecksum() string {
	return strings.ReplaceAll(uuid.New().String()+uuid.New().String(), "-", "")[:64]
}

// createLedgerEntry stands up the full prerequisite chain (user/leaf/work unit/volunteer/
// result) and inserts one credit_ledger grant of the given amount, returning the populated
// entry. It reuses the fixture helpers from pgx-repo_test.go.
func createLedgerEntry(t *testing.T, pool *pgxpool.Pool, amount float64) *LedgerEntry {
	t.Helper()
	userID := createTestUser(t, pool, "adj-"+uuid.New().String()[:8])
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	volID := createTestVolunteer(t, pool)
	resultID := createTestResult(t, pool, wuID, volID, randChecksum())

	entry := &LedgerEntry{
		VolunteerID:  volID,
		LeafID:       leafID,
		WorkUnitID:   wuID,
		ResultID:     resultID,
		CreditAmount: amount,
	}
	if err := NewPgxRepository(pool).Create(context.Background(), entry); err != nil {
		t.Fatalf("create ledger entry: %v", err)
	}
	return entry
}

// --- Clawback ---------------------------------------------------------------------------

func TestAdjustmentClawbackHappyPath(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	entry := createLedgerEntry(t, pool, 5.0)
	repo := NewPgxAdjustmentsRepository(pool)

	// Partial clawback of 2.0 leaves 3.0 remaining.
	a1, err := repo.Clawback(ctx, entry.ID, floatPtr(2.0), "OPERATOR_CLAWBACK", "partial", AdjustmentByOperator)
	if err != nil {
		t.Fatalf("partial clawback: %v", err)
	}
	if a1.Amount != -2.0 {
		t.Errorf("a1.Amount = %v, want -2.0", a1.Amount)
	}
	// volunteer_id/leaf_id must come from the locked ledger row, not the caller.
	if a1.VolunteerID != entry.VolunteerID || a1.LeafID != entry.LeafID {
		t.Errorf("a1 volunteer/leaf = %v/%v, want %v/%v (from the locked entry)",
			a1.VolunteerID, a1.LeafID, entry.VolunteerID, entry.LeafID)
	}
	if a1.Note != "partial" || a1.CreatedBy != AdjustmentByOperator {
		t.Errorf("a1 note/createdBy = %q/%q, unexpected", a1.Note, a1.CreatedBy)
	}

	// Full remaining (nil magnitude) claws back the residual 3.0.
	a2, err := repo.Clawback(ctx, entry.ID, nil, "OPERATOR_CLAWBACK", "", AdjustmentByOperator)
	if err != nil {
		t.Fatalf("full-remaining clawback: %v", err)
	}
	if a2.Amount != -3.0 {
		t.Errorf("a2.Amount = %v, want -3.0", a2.Amount)
	}

	// A third clawback against a now-exhausted entry is ErrAdjustmentExhausted.
	if _, err := repo.Clawback(ctx, entry.ID, nil, "OPERATOR_CLAWBACK", "", AdjustmentByOperator); !errors.Is(err, ErrAdjustmentExhausted) {
		t.Fatalf("third clawback err = %v, want ErrAdjustmentExhausted", err)
	}

	sum, err := repo.SumForEntry(ctx, entry.ID)
	if err != nil {
		t.Fatalf("SumForEntry: %v", err)
	}
	if sum != -5.0 {
		t.Errorf("SumForEntry = %v, want -5.0 (entry fully cancelled, net 0)", sum)
	}
}

func TestAdjustmentClawbackFullRemainingDefault(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	entry := createLedgerEntry(t, pool, 4.0)
	repo := NewPgxAdjustmentsRepository(pool)

	a, err := repo.Clawback(ctx, entry.ID, nil, "OPERATOR_CLAWBACK", "", AdjustmentByOperator)
	if err != nil {
		t.Fatalf("full-remaining clawback: %v", err)
	}
	if a.Amount != -4.0 {
		t.Errorf("a.Amount = %v, want -4.0 (full remaining of a fresh entry)", a.Amount)
	}
	if sum, _ := repo.SumForEntry(ctx, entry.ID); sum != -4.0 {
		t.Errorf("SumForEntry = %v, want -4.0", sum)
	}
}

func TestAdjustmentClawbackOvershootRejected(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	entry := createLedgerEntry(t, pool, 5.0)
	repo := NewPgxAdjustmentsRepository(pool)

	if _, err := repo.Clawback(ctx, entry.ID, floatPtr(6.0), "OPERATOR_CLAWBACK", "", AdjustmentByOperator); !errors.Is(err, ErrAdjustmentOvershoot) {
		t.Fatalf("overshoot err = %v, want ErrAdjustmentOvershoot", err)
	}
	// No row should have been written.
	if sum, _ := repo.SumForEntry(ctx, entry.ID); sum != 0 {
		t.Errorf("SumForEntry = %v, want 0 (overshoot must not write)", sum)
	}
}

func TestAdjustmentClawbackEntryNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	repo := NewPgxAdjustmentsRepository(pool)
	_, err := repo.Clawback(ctx, uuid.New(), floatPtr(1.0), "OPERATOR_CLAWBACK", "", AdjustmentByOperator)
	var apiErr interface{ Error() string }
	if err == nil || !errors.As(err, &apiErr) {
		t.Fatalf("clawback of absent entry err = %v, want a not-found error", err)
	}
}

// TestAdjustmentClawbackConcurrent is the audit-F1 forcing function: N goroutines
// simultaneously full-cancel ONE entry. The FOR UPDATE lock in the repo's transaction must
// serialize them so exactly one succeeds and the final net is exactly 0 — never negative. A
// naive single-statement guarded INSERT (each concurrent txn snapshotting the pre-commit
// net) would let several full-cancels commit and drive the net negative; this test MUST fail
// against that implementation.
func TestAdjustmentClawbackConcurrent(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const credit = 5.0
	entry := createLedgerEntry(t, pool, credit)
	repo := NewPgxAdjustmentsRepository(pool)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	adjs := make([]*Adjustment, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			adjs[i], errs[i] = repo.Clawback(ctx, entry.ID, nil, "OPERATOR_CLAWBACK", "", AdjustmentByOperator)
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			successes++
			if adjs[i].Amount != -credit {
				t.Errorf("winning adjustment amount = %v, want %v", adjs[i].Amount, -credit)
			}
		} else if !errors.Is(errs[i], ErrAdjustmentExhausted) {
			t.Errorf("goroutine %d err = %v, want nil or ErrAdjustmentExhausted", i, errs[i])
		}
	}
	if successes != 1 {
		t.Fatalf("successful full-cancels = %d, want exactly 1", successes)
	}

	// Net must be exactly 0 (sum of adjustments == -credit), never negative.
	sum, err := repo.SumForEntry(ctx, entry.ID)
	if err != nil {
		t.Fatalf("SumForEntry: %v", err)
	}
	if sum != -credit {
		t.Fatalf("adjustment sum = %v, want %v (net exactly 0, never below)", sum, -credit)
	}
}

func TestAdjustmentListByVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	entry := createLedgerEntry(t, pool, 6.0)
	repo := NewPgxAdjustmentsRepository(pool)

	if _, err := repo.Clawback(ctx, entry.ID, floatPtr(1.0), "OPERATOR_CLAWBACK", "", AdjustmentByOperator); err != nil {
		t.Fatalf("clawback 1: %v", err)
	}
	if _, err := repo.Clawback(ctx, entry.ID, floatPtr(2.0), "OPERATOR_CLAWBACK", "", AdjustmentByOperator); err != nil {
		t.Fatalf("clawback 2: %v", err)
	}

	list, err := repo.ListByVolunteer(ctx, entry.VolunteerID, 100, 0)
	if err != nil {
		t.Fatalf("ListByVolunteer: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list length = %d, want 2", len(list))
	}
	// Newest first, and all bound to the entry's volunteer.
	for _, a := range list {
		if a.VolunteerID != entry.VolunteerID {
			t.Errorf("adjustment volunteer = %v, want %v", a.VolunteerID, entry.VolunteerID)
		}
	}
}

// --- CreateCapped -----------------------------------------------------------------------

func TestCreateCapped(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxRepository(pool)

	userID := createTestUser(t, pool, "cap-"+uuid.New().String()[:8])
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	newEntry := func(amount float64) *LedgerEntry {
		wuID := createTestWorkUnit(t, pool, leafID)
		resultID := createTestResult(t, pool, wuID, volID, randChecksum())
		return &LedgerEntry{
			VolunteerID: volID, LeafID: leafID, WorkUnitID: wuID,
			ResultID: resultID, CreditAmount: amount,
		}
	}

	const cap = 10.0

	// Under cap: sum(0)+4 <= 10 → inserts.
	e1 := newEntry(4.0)
	ins, err := repo.CreateCapped(ctx, e1, cap)
	if err != nil {
		t.Fatalf("CreateCapped e1: %v", err)
	}
	if !ins {
		t.Fatal("e1 should insert under cap")
	}
	if e1.ID == uuid.Nil {
		t.Error("e1.ID should be populated on insert")
	}

	// Boundary exactly at cap: sum(4)+6 == 10 <= 10 → inserts.
	e2 := newEntry(6.0)
	ins, err = repo.CreateCapped(ctx, e2, cap)
	if err != nil {
		t.Fatalf("CreateCapped e2: %v", err)
	}
	if !ins {
		t.Fatal("e2 should insert exactly at cap")
	}

	// Over cap: sum(10)+0.5 > 10 → suppressed (false, nil, no row).
	e3 := newEntry(0.5)
	ins, err = repo.CreateCapped(ctx, e3, cap)
	if err != nil {
		t.Fatalf("CreateCapped e3 returned an error, want suppression: %v", err)
	}
	if ins {
		t.Fatal("e3 should be suppressed over cap")
	}
	if _, err := repo.GetByResultID(ctx, e3.ResultID); err == nil {
		t.Error("suppressed grant must not have written a ledger row")
	}
}

func TestCreateCappedWindow(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxRepository(pool)

	userID := createTestUser(t, pool, "capwin-"+uuid.New().String()[:8])
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	newEntry := func(amount float64) *LedgerEntry {
		wuID := createTestWorkUnit(t, pool, leafID)
		resultID := createTestResult(t, pool, wuID, volID, randChecksum())
		return &LedgerEntry{
			VolunteerID: volID, LeafID: leafID, WorkUnitID: wuID,
			ResultID: resultID, CreditAmount: amount,
		}
	}

	// A large grant, then aged beyond the 24h window by direct SQL.
	old := newEntry(9.0)
	if err := repo.Create(ctx, old); err != nil {
		t.Fatalf("create old entry: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE credit_ledger SET granted_at = now() - interval '25 hours' WHERE id = $1", old.ID); err != nil {
		t.Fatalf("age old entry: %v", err)
	}

	// The aged 9.0 is outside the rolling 24h window, so a fresh 9.0 with cap 10 inserts
	// (window sum is 0, not 9).
	fresh := newEntry(9.0)
	ins, err := repo.CreateCapped(ctx, fresh, 10.0)
	if err != nil {
		t.Fatalf("CreateCapped fresh: %v", err)
	}
	if !ins {
		t.Fatal("fresh grant should insert; the aged entry must not count toward the 24h window")
	}
}

// A duplicate result_id must STILL raise the idempotency Conflict, not be swallowed as cap
// suppression: 23505 is a real conflict, not an over-cap outcome.
func TestCreateCappedDuplicateResult(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxRepository(pool)

	userID := createTestUser(t, pool, "capdup-"+uuid.New().String()[:8])
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)
	wuID := createTestWorkUnit(t, pool, leafID)
	resultID := createTestResult(t, pool, wuID, volID, randChecksum())

	entry := &LedgerEntry{VolunteerID: volID, LeafID: leafID, WorkUnitID: wuID, ResultID: resultID, CreditAmount: 1.0}
	if err := repo.Create(ctx, entry); err != nil {
		t.Fatalf("seed ledger row: %v", err)
	}

	// A high cap keeps the sum-guard satisfied so the INSERT actually attempts and hits the
	// unique-result constraint — proving 23505 is not suppressed.
	dup := &LedgerEntry{VolunteerID: volID, LeafID: leafID, WorkUnitID: wuID, ResultID: resultID, CreditAmount: 1.0}
	ins, err := repo.CreateCapped(ctx, dup, 1000.0)
	if err == nil {
		t.Fatal("duplicate result_id should return a Conflict error, got nil")
	}
	if ins {
		t.Error("duplicate result_id must not report inserted=true")
	}
}
