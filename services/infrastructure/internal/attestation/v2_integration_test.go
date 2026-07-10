//go:build integration

package attestation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// DB-backed regression tests for the attestation v2 cutover (design §8.9 items b–d, i, j):
// stored rows re-verify after the real numeric/jsonb round-trip, v1 fixture rows keep
// verifying under the frozen rule, and the revocation emitter + reconciliation sweep behave
// against live SQL and the partial unique indexes.

func integrationLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func createTestVolunteer(t *testing.T, pool *pgxpool.Pool, pubKey []byte) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO volunteers (id, public_key) VALUES ($1, $2)`, id, pubKey)
	if err != nil {
		t.Fatalf("create test volunteer: %v", err)
	}
	return id
}

func createTestLedgerEntry(t *testing.T, pool *pgxpool.Pool, volID, leafID, wuID, resultID types.ID, amount float64) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO credit_ledger (id, volunteer_id, leaf_id, work_unit_id, result_id, credit_amount)
		 VALUES ($1, $2, $3, $4, $5, $6)`, id, volID, leafID, wuID, resultID, amount)
	if err != nil {
		t.Fatalf("create test ledger entry: %v", err)
	}
	return id
}

func createTestAdjustment(t *testing.T, pool *pgxpool.Pool, entryID, volID, leafID types.ID, amount float64, reason string) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO credit_adjustments (id, ledger_entry_id, volunteer_id, leaf_id, amount, reason, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, 'OPERATOR')`, id, entryID, volID, leafID, amount, reason)
	if err != nil {
		t.Fatalf("create test adjustment: %v", err)
	}
	return id
}

// TestV2RoundTrip_SignStoreScanVerify (design §8.9(b) core + the F-H1 live regression): a v2
// grant signed with a TIE-ADJACENT credit value still verifies after the real
// sign→INSERT→scan round-trip, because the repository stores the exact signed decimal string
// rather than letting the driver and Postgres re-round a raw float64. The descriptor also
// survives the jsonb round-trip value-exactly.
func TestV2RoundTrip_SignStoreScanVerify(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := createTestUser(t, pool, "att-v2-rt")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	resultID := createTestResult(t, pool, wuID)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	repo := NewPgxRepository(pool)

	// 0.1000005 is a 7th-decimal tie: Go (half-even on the binary value) and Postgres
	// (half-away on the decimal) can legitimately disagree about its 6dp rounding — the
	// store-exact-signed-string rule makes the disagreement unreachable.
	att := makeV2TestAttestation(leafID, wuID, resultID, signer.PublicKey(), 0.1000005, types.Now())
	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	att.Signature = sig
	if err := repo.Create(ctx, att); err != nil {
		t.Fatalf("Create: %v", err)
	}

	stored, err := repo.GetByID(ctx, att.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.SchemaVersion != SchemaVersionV2 {
		t.Errorf("schema_version = %d, want 2", stored.SchemaVersion)
	}
	if stored.CreditAmountCanonical != CanonicalCreditString(0.1000005) {
		t.Errorf("stored canonical credit = %q, want %q (store must equal sign, F-H1)",
			stored.CreditAmountCanonical, CanonicalCreditString(0.1000005))
	}
	if stored.QuorumDescriptor == nil || *stored.QuorumDescriptor != *att.QuorumDescriptor {
		t.Errorf("descriptor after jsonb round-trip = %+v, want %+v", stored.QuorumDescriptor, att.QuorumDescriptor)
	}
	if !VerifyAttestation(signer.PublicKey(), stored) {
		t.Error("v2 attestation failed to verify after the DB round-trip")
	}
}

// TestV1FixtureRow_VerifiesUnderV1Rule (design §8.9(c), the D8 contract): a row shaped like a
// pre-cutover attestation — schema_version 1, none of the v2 columns — still verifies under
// the frozen v1 canonical form after storage.
func TestV1FixtureRow_VerifiesUnderV1Rule(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := createTestUser(t, pool, "att-v1-fixture")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	repo := NewPgxRepository(pool)

	att := &Attestation{
		SchemaVersion:        SchemaVersionV1,
		LeafID:               leafID,
		VolunteerPublicKey:   signer.PublicKey(),
		WorkUnitID:           wuID,
		RawMetrics:           map[string]any{"cpu_seconds_user": float64(90)},
		ValidationOutcome:    OutcomeAgreed,
		CreditAmount:         1.0,
		AttestationTimestamp: types.Now(),
	}
	sig, err := signer.Sign(att)
	if err != nil {
		t.Fatalf("Sign (v1 form): %v", err)
	}
	att.Signature = sig
	if err := repo.Create(ctx, att); err != nil {
		t.Fatalf("Create v1-shaped row: %v", err)
	}

	stored, err := repo.GetByID(ctx, att.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.SchemaVersion != SchemaVersionV1 {
		t.Fatalf("schema_version = %d, want 1", stored.SchemaVersion)
	}
	if !VerifyAttestation(signer.PublicKey(), stored) {
		t.Error("v1 fixture row failed to verify under the frozen v1 rule")
	}
}

// TestUniqueAgreedGrantPerResult (design §8.9(j), audit F-M5): the partial unique index
// refuses a second AGREED v2 grant attestation for one result.
func TestUniqueAgreedGrantPerResult(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := createTestUser(t, pool, "att-uq-agreed")
	leafID := createTestLeaf(t, pool, &userID)
	wuID := createTestWorkUnit(t, pool, leafID)
	resultID := createTestResult(t, pool, wuID)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	repo := NewPgxRepository(pool)

	for i := 0; i < 2; i++ {
		att := makeV2TestAttestation(leafID, wuID, resultID, signer.PublicKey(), 1.0, types.Now())
		sig, _ := signer.Sign(att)
		att.Signature = sig
		err := repo.Create(ctx, att)
		if i == 0 && err != nil {
			t.Fatalf("first Create: %v", err)
		}
		if i == 1 && err == nil {
			t.Fatal("second AGREED grant attestation for the same result was accepted; want unique-index refusal")
		}
	}
}

// TestClawbackRevocationFlow (design §8.9(d) + (i)): the emitter writes a verifiable
// revocation for a clawed-back v2 grant; partial clawbacks produce one revocation per
// adjustment; a v1-era grant yields no revocation and no error; re-emission is idempotent;
// and the reconciliation sweep recovers a lost emission exactly once.
func TestClawbackRevocationFlow(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := createTestUser(t, pool, "att-revoke-flow")
	leafID := createTestLeaf(t, pool, &userID)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := NewSigner(priv)
	repo := NewPgxRepository(pool)
	emitter := NewRevocationEmitter(pool, repo, signer, integrationLogger())
	volID := createTestVolunteer(t, pool, signer.PublicKey())

	newGrantedResult := func(credit float64) (wuID, resultID, entryID types.ID, grant *Attestation) {
		wuID = createTestWorkUnit(t, pool, leafID)
		resultID = createTestResult(t, pool, wuID)
		entryID = createTestLedgerEntry(t, pool, volID, leafID, wuID, resultID, credit)
		grant = makeV2TestAttestation(leafID, wuID, resultID, signer.PublicKey(), credit, types.Now())
		sig, _ := signer.Sign(grant)
		grant.Signature = sig
		if err := repo.Create(ctx, grant); err != nil {
			t.Fatalf("create grant attestation: %v", err)
		}
		return wuID, resultID, entryID, grant
	}

	// (d) happy path + partial x2: two partial adjustments on one entry produce two
	// verifiable revocations, both referencing the same original.
	_, _, entryID, grant := newGrantedResult(2.0)
	adj1 := createTestAdjustment(t, pool, entryID, volID, leafID, -0.5, "OPERATOR_CLAWBACK")
	adj2 := createTestAdjustment(t, pool, entryID, volID, leafID, -1.5, "FRAUD_CONFIRMED")
	if err := emitter.EmitForAdjustment(ctx, adj1); err != nil {
		t.Fatalf("EmitForAdjustment adj1: %v", err)
	}
	if err := emitter.EmitForAdjustment(ctx, adj2); err != nil {
		t.Fatalf("EmitForAdjustment adj2: %v", err)
	}
	revocations, err := repo.ListRevocationsOf(ctx, grant.ID)
	if err != nil {
		t.Fatalf("ListRevocationsOf: %v", err)
	}
	if len(revocations) != 2 {
		t.Fatalf("revocations = %d, want 2 (one per partial adjustment)", len(revocations))
	}
	wantMagnitudes := map[string]bool{"0.500000": false, "1.500000": false}
	for _, rev := range revocations {
		if rev.ValidationOutcome != OutcomeRevoked {
			t.Errorf("revocation outcome = %q, want REVOKED", rev.ValidationOutcome)
		}
		if rev.RevokesAttestationID == nil || *rev.RevokesAttestationID != grant.ID {
			t.Errorf("revocation references %v, want %v", rev.RevokesAttestationID, grant.ID)
		}
		if _, ok := wantMagnitudes[rev.CreditAmountCanonical]; !ok {
			t.Errorf("unexpected revocation magnitude %q", rev.CreditAmountCanonical)
		}
		wantMagnitudes[rev.CreditAmountCanonical] = true
		if !VerifyAttestation(signer.PublicKey(), rev) {
			t.Error("revocation failed to verify after the DB round-trip")
		}
	}
	for m, seen := range wantMagnitudes {
		if !seen {
			t.Errorf("no revocation with magnitude %q", m)
		}
	}

	// Idempotency: re-emitting an already-recorded adjustment adds nothing and no error.
	if err := emitter.EmitForAdjustment(ctx, adj1); err != nil {
		t.Fatalf("re-EmitForAdjustment: %v", err)
	}
	revocations, _ = repo.ListRevocationsOf(ctx, grant.ID)
	if len(revocations) != 2 {
		t.Fatalf("after re-emit: revocations = %d, want still 2", len(revocations))
	}

	// v1-era: a clawback whose grant only has a v1 attestation gets no revocation, no error.
	wuV1 := createTestWorkUnit(t, pool, leafID)
	resV1 := createTestResult(t, pool, wuV1)
	entryV1 := createTestLedgerEntry(t, pool, volID, leafID, wuV1, resV1, 1.0)
	v1att := &Attestation{
		SchemaVersion: SchemaVersionV1, LeafID: leafID, VolunteerPublicKey: signer.PublicKey(),
		WorkUnitID: wuV1, RawMetrics: map[string]any{}, ValidationOutcome: OutcomeAgreed,
		CreditAmount: 1.0, AttestationTimestamp: types.Now(),
	}
	v1sig, _ := signer.Sign(v1att)
	v1att.Signature = v1sig
	if err := repo.Create(ctx, v1att); err != nil {
		t.Fatalf("create v1 attestation: %v", err)
	}
	adjV1 := createTestAdjustment(t, pool, entryV1, volID, leafID, -1.0, "OPERATOR_CLAWBACK")
	if err := emitter.EmitForAdjustment(ctx, adjV1); err != nil {
		t.Fatalf("EmitForAdjustment v1-era: %v", err)
	}
	var v1Revocations int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM credit_attestations WHERE adjustment_id = $1`, adjV1).Scan(&v1Revocations); err != nil {
		t.Fatalf("count v1-era revocations: %v", err)
	}
	if v1Revocations != 0 {
		t.Errorf("v1-era clawback produced %d revocation rows, want 0 (DB record only)", v1Revocations)
	}

	// (i) reconciliation: an adjustment whose in-handler emission never happened is picked
	// up by Reconcile exactly once; the v1-era adjustment above is never picked up.
	_, _, entry2ID, grant2 := newGrantedResult(3.0)
	adjLost := createTestAdjustment(t, pool, entry2ID, volID, leafID, -3.0, "OPERATOR_CLAWBACK")
	emitted, err := emitter.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if emitted != 1 {
		t.Fatalf("Reconcile emitted %d, want exactly 1 (the lost adjustment; v1-era excluded)", emitted)
	}
	rev2, err := repo.ListRevocationsOf(ctx, grant2.ID)
	if err != nil || len(rev2) != 1 {
		t.Fatalf("revocations of grant2 = %d (err %v), want 1", len(rev2), err)
	}
	if rev2[0].AdjustmentID == nil || *rev2[0].AdjustmentID != adjLost {
		t.Errorf("reconciled revocation adjustment = %v, want %v", rev2[0].AdjustmentID, adjLost)
	}
	if emitted, err = emitter.Reconcile(ctx); err != nil || emitted != 0 {
		t.Fatalf("second Reconcile emitted %d (err %v), want 0", emitted, err)
	}
}
