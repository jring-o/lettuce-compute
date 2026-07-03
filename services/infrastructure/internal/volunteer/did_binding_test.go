//go:build integration

package volunteer

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// createBoundVolunteer creates a volunteer and binds it to a DID, returning the
// row. boundAt sets both did_bound_at and did_binding_checked_at.
func createBoundVolunteer(t *testing.T, repo *PgxRepository, did, uri, cid string, boundAt time.Time) *Volunteer {
	t.Helper()
	ctx := context.Background()
	v := newTestVolunteer()
	if err := repo.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.SetDIDBinding(ctx, v.ID, did, uri, cid, boundAt); err != nil {
		t.Fatalf("SetDIDBinding: %v", err)
	}
	return v
}

func TestSetDIDBindingAndGet(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	boundAt := time.Now().UTC().Truncate(time.Microsecond)
	v := createBoundVolunteer(t, repo,
		"did:plc:examplealice", "at://did:plc:examplealice/tech.scios.lettuce.keyAuthorization/self", "bafyreiexamplecid1",
		boundAt)

	got, err := repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.DID == nil || *got.DID != "did:plc:examplealice" {
		t.Errorf("DID = %v, want did:plc:examplealice", got.DID)
	}
	if got.DIDBindingURI == nil || *got.DIDBindingURI != "at://did:plc:examplealice/tech.scios.lettuce.keyAuthorization/self" {
		t.Errorf("DIDBindingURI = %v, unexpected", got.DIDBindingURI)
	}
	if got.DIDBindingCID == nil || *got.DIDBindingCID != "bafyreiexamplecid1" {
		t.Errorf("DIDBindingCID = %v, unexpected", got.DIDBindingCID)
	}
	if got.DIDBindingStatus == nil || *got.DIDBindingStatus != DIDBindingStatusOK {
		t.Errorf("DIDBindingStatus = %v, want OK", got.DIDBindingStatus)
	}
	if got.DIDBindingCheckFailures != 0 {
		t.Errorf("DIDBindingCheckFailures = %d, want 0", got.DIDBindingCheckFailures)
	}
	if got.DIDBoundAt == nil || !got.DIDBoundAt.Equal(boundAt) {
		t.Errorf("DIDBoundAt = %v, want %v", got.DIDBoundAt, boundAt)
	}
	if got.DIDBindingCheckedAt == nil || !got.DIDBindingCheckedAt.Equal(boundAt) {
		t.Errorf("DIDBindingCheckedAt = %v, want %v", got.DIDBindingCheckedAt, boundAt)
	}
	if got.DIDFrozenUntil != nil {
		t.Errorf("DIDFrozenUntil = %v, want nil", got.DIDFrozenUntil)
	}
}

func TestSetDIDBindingNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	err := repo.SetDIDBinding(ctx, types.NewID(), "did:plc:ghost", "at://ghost", "cid", time.Now().UTC())
	if err == nil {
		t.Fatal("expected not-found error for unknown volunteer")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestListDIDBindingsForRecheck(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Microsecond)

	// v1: checked 3h ago (most overdue).
	v1 := createBoundVolunteer(t, repo, "did:plc:one", "at://one", "cid1", base.Add(-3*time.Hour))
	// v2: checked 2h ago.
	v2 := createBoundVolunteer(t, repo, "did:plc:two", "at://two", "cid2", base.Add(-2*time.Hour))
	// v3: bound 3h ago then hard-revoked — must be excluded regardless of check time.
	v3 := createBoundVolunteer(t, repo, "did:plc:three", "at://three", "cid3", base.Add(-3*time.Hour))
	if err := repo.RevokeDIDBinding(ctx, v3.ID, base.Add(-2*time.Hour)); err != nil {
		t.Fatalf("RevokeDIDBinding v3: %v", err)
	}

	// Cutoff after every active check: v1 and v2 are due, v3 (REVOKED) is not.
	// Order must be oldest-checked first: v1 then v2.
	due, err := repo.ListDIDBindingsForRecheck(ctx, base.Add(-1*time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDIDBindingsForRecheck: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("got %d due bindings, want 2 (v3 REVOKED must be excluded)", len(due))
	}
	if due[0].ID != v1.ID || due[1].ID != v2.ID {
		t.Errorf("order = [%v, %v], want oldest-first [%v, %v]", due[0].ID, due[1].ID, v1.ID, v2.ID)
	}

	// limit honored: only the single most-overdue binding.
	limited, err := repo.ListDIDBindingsForRecheck(ctx, base.Add(-1*time.Hour), 1)
	if err != nil {
		t.Fatalf("ListDIDBindingsForRecheck(limit=1): %v", err)
	}
	if len(limited) != 1 || limited[0].ID != v1.ID {
		t.Fatalf("limit=1 got %d rows (first %v), want 1 (v1)", len(limited), func() any {
			if len(limited) > 0 {
				return limited[0].ID
			}
			return nil
		}())
	}

	// Cutoff between v1 and v2's checks: only v1 (checked 3h ago) predates it.
	tight, err := repo.ListDIDBindingsForRecheck(ctx, base.Add(-150*time.Minute), 10)
	if err != nil {
		t.Fatalf("ListDIDBindingsForRecheck(tight cutoff): %v", err)
	}
	if len(tight) != 1 || tight[0].ID != v1.ID {
		t.Fatalf("tight cutoff got %d rows, want 1 (v1 only)", len(tight))
	}
}

func TestMarkDIDBindingCheckFailedEscalatesAndResets(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Microsecond)
	v := createBoundVolunteer(t, repo, "did:plc:flaky", "at://flaky", "cid0", base.Add(-time.Hour))

	const staleAfter = 3

	// First two failures increment but stay OK (below threshold).
	for i := 1; i <= 2; i++ {
		checkedAt := base.Add(time.Duration(i) * time.Minute)
		if err := repo.MarkDIDBindingCheckFailed(ctx, v.ID, checkedAt, staleAfter); err != nil {
			t.Fatalf("MarkDIDBindingCheckFailed #%d: %v", i, err)
		}
		got, err := repo.GetByID(ctx, v.ID)
		if err != nil {
			t.Fatalf("GetByID after failure #%d: %v", i, err)
		}
		if got.DIDBindingCheckFailures != i {
			t.Errorf("after failure #%d: failures = %d, want %d", i, got.DIDBindingCheckFailures, i)
		}
		if got.DIDBindingStatus == nil || *got.DIDBindingStatus != DIDBindingStatusOK {
			t.Errorf("after failure #%d: status = %v, want OK (below threshold)", i, got.DIDBindingStatus)
		}
	}

	// Third failure reaches the threshold and escalates to STALE.
	thirdCheckedAt := base.Add(3 * time.Minute)
	if err := repo.MarkDIDBindingCheckFailed(ctx, v.ID, thirdCheckedAt, staleAfter); err != nil {
		t.Fatalf("MarkDIDBindingCheckFailed #3: %v", err)
	}
	got, err := repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID after failure #3: %v", err)
	}
	if got.DIDBindingCheckFailures != 3 {
		t.Errorf("failures = %d, want 3", got.DIDBindingCheckFailures)
	}
	if got.DIDBindingStatus == nil || *got.DIDBindingStatus != DIDBindingStatusStale {
		t.Errorf("status = %v, want STALE at threshold", got.DIDBindingStatus)
	}
	if got.DIDBindingCheckedAt == nil || !got.DIDBindingCheckedAt.Equal(thirdCheckedAt) {
		t.Errorf("DIDBindingCheckedAt = %v, want %v", got.DIDBindingCheckedAt, thirdCheckedAt)
	}

	// A successful recheck clears STALE, refreshes the CID, and resets the counter.
	recheckAt := base.Add(10 * time.Minute)
	if err := repo.MarkDIDBindingChecked(ctx, v.ID, "cid-refreshed", recheckAt); err != nil {
		t.Fatalf("MarkDIDBindingChecked: %v", err)
	}
	got, err = repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID after recheck: %v", err)
	}
	if got.DIDBindingStatus == nil || *got.DIDBindingStatus != DIDBindingStatusOK {
		t.Errorf("status = %v, want OK after successful recheck", got.DIDBindingStatus)
	}
	if got.DIDBindingCheckFailures != 0 {
		t.Errorf("failures = %d, want 0 after successful recheck", got.DIDBindingCheckFailures)
	}
	if got.DIDBindingCID == nil || *got.DIDBindingCID != "cid-refreshed" {
		t.Errorf("DIDBindingCID = %v, want cid-refreshed", got.DIDBindingCID)
	}
	if got.DIDBindingCheckedAt == nil || !got.DIDBindingCheckedAt.Equal(recheckAt) {
		t.Errorf("DIDBindingCheckedAt = %v, want %v", got.DIDBindingCheckedAt, recheckAt)
	}
}

func TestRevokeDIDBinding(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Microsecond)
	v := createBoundVolunteer(t, repo, "did:plc:revoke", "at://revoke", "cidR", base.Add(-time.Hour))

	revokedAt := base.Truncate(time.Microsecond)
	if err := repo.RevokeDIDBinding(ctx, v.ID, revokedAt); err != nil {
		t.Fatalf("RevokeDIDBinding: %v", err)
	}

	got, err := repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.DIDBindingStatus == nil || *got.DIDBindingStatus != DIDBindingStatusRevoked {
		t.Errorf("status = %v, want REVOKED", got.DIDBindingStatus)
	}
	if got.DIDBindingCheckedAt == nil || !got.DIDBindingCheckedAt.Equal(revokedAt) {
		t.Errorf("DIDBindingCheckedAt = %v, want %v", got.DIDBindingCheckedAt, revokedAt)
	}
	// did/uri/cid are retained for audit after revocation.
	if got.DID == nil || *got.DID != "did:plc:revoke" {
		t.Errorf("DID = %v, want retained did:plc:revoke", got.DID)
	}
	if got.DIDBindingCID == nil || *got.DIDBindingCID != "cidR" {
		t.Errorf("DIDBindingCID = %v, want retained cidR", got.DIDBindingCID)
	}

	// A REVOKED binding is terminal — the recheck scan must never return it.
	due, err := repo.ListDIDBindingsForRecheck(ctx, base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDIDBindingsForRecheck: %v", err)
	}
	for _, d := range due {
		if d.ID == v.ID {
			t.Errorf("revoked volunteer %v should not appear in recheck list", v.ID)
		}
	}
}

func TestSetDIDFrozenUntil(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	if err := repo.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}

	until := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Microsecond)
	if err := repo.SetDIDFrozenUntil(ctx, v.ID, until); err != nil {
		t.Fatalf("SetDIDFrozenUntil: %v", err)
	}

	got, err := repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.DIDFrozenUntil == nil || !got.DIDFrozenUntil.Equal(until) {
		t.Errorf("DIDFrozenUntil = %v, want %v", got.DIDFrozenUntil, until)
	}
}
