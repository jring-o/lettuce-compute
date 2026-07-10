package attestation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- test doubles -----------------------------------------------------------------------

// revTestSigner builds a Signer over a freshly generated key (self-contained, no fixtures).
func revTestSigner(t *testing.T) *Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return NewSigner(priv)
}

func revTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubRow is a pgx.Row whose Scan copies a fixed value slice into the destinations. It backs
// the single-row pendingRevocationQuery in EmitForAdjustment without a live database.
type stubRow struct {
	vals []any
	err  error
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("stubRow: %d destinations, %d values", len(dest), len(r.vals))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *types.ID:
			*p = r.vals[i].(types.ID)
		case *string:
			*p = r.vals[i].(string)
		default:
			return fmt.Errorf("stubRow: unsupported destination type %T", d)
		}
	}
	return nil
}

// stubDBTX is a DBTX whose QueryRow returns a preset row. Query/Exec are unused by the
// EmitForAdjustment unit tests (Reconcile's multi-row SQL is integration-tested).
type stubDBTX struct {
	row pgx.Row
}

func (s *stubDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (s *stubDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (s *stubDBTX) QueryRow(context.Context, string, ...any) pgx.Row       { return s.row }

// fakeGrantRepo is a grantLookupCreator double recording created revocation rows.
type fakeGrantRepo struct {
	grant     *Attestation
	getErr    error
	createErr error
	created   []*Attestation
}

func (f *fakeGrantRepo) GetGrantByResultID(_ context.Context, _ types.ID) (*Attestation, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.grant, nil
}

func (f *fakeGrantRepo) Create(_ context.Context, att *Attestation) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, att)
	return nil
}

// grantFixture builds an AGREED v2 grant attestation for a result.
func grantFixture(resultID types.ID) *Attestation {
	return &Attestation{
		ID:                 types.NewID(),
		SchemaVersion:      SchemaVersionV2,
		LeafID:             types.NewID(),
		WorkUnitID:         types.NewID(),
		VolunteerPublicKey: []byte("volunteer-public-key-bytes"),
		ResultID:           &resultID,
		ValidationOutcome:  OutcomeAgreed,
	}
}

// --- EmitForAdjustment ------------------------------------------------------------------

func TestEmitForAdjustment_HappyPath(t *testing.T) {
	signer := revTestSigner(t)
	resultID := types.NewID()
	grant := grantFixture(resultID)
	adjID := types.NewID()

	db := &stubDBTX{row: stubRow{vals: []any{adjID, "OPERATOR_CLAWBACK", "2.500000", resultID}}}
	repo := &fakeGrantRepo{grant: grant}
	e := NewRevocationEmitter(db, repo, signer, revTestLogger())

	if err := e.EmitForAdjustment(context.Background(), adjID); err != nil {
		t.Fatalf("EmitForAdjustment: %v", err)
	}
	if len(repo.created) != 1 {
		t.Fatalf("created rows = %d, want 1", len(repo.created))
	}
	got := repo.created[0]

	if got.SchemaVersion != SchemaVersionV2 {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, SchemaVersionV2)
	}
	if got.ValidationOutcome != OutcomeRevoked {
		t.Errorf("validation_outcome = %q, want %q", got.ValidationOutcome, OutcomeRevoked)
	}
	// Canonical credit string is passed through verbatim, never re-derived from the float.
	if got.CreditAmountCanonical != "2.500000" {
		t.Errorf("credit_amount canonical = %q, want %q", got.CreditAmountCanonical, "2.500000")
	}
	if got.CreditAmount != 2.5 {
		t.Errorf("credit_amount (display) = %v, want 2.5", got.CreditAmount)
	}
	if got.RevokesAttestationID == nil || *got.RevokesAttestationID != grant.ID {
		t.Errorf("revokes_attestation_id = %v, want %v", got.RevokesAttestationID, grant.ID)
	}
	if got.AdjustmentID == nil || *got.AdjustmentID != adjID {
		t.Errorf("adjustment_id = %v, want %v", got.AdjustmentID, adjID)
	}
	if got.Reason == nil || *got.Reason != "OPERATOR_CLAWBACK" {
		t.Errorf("reason = %v, want OPERATOR_CLAWBACK", got.Reason)
	}
	if got.ResultID == nil || *got.ResultID != resultID {
		t.Errorf("result_id = %v, want %v", got.ResultID, resultID)
	}
	// Leaf / work unit / volunteer key are copied from the grant row.
	if got.LeafID != grant.LeafID || got.WorkUnitID != grant.WorkUnitID {
		t.Errorf("leaf/work_unit = %v/%v, want %v/%v", got.LeafID, got.WorkUnitID, grant.LeafID, grant.WorkUnitID)
	}
	if string(got.VolunteerPublicKey) != string(grant.VolunteerPublicKey) {
		t.Errorf("volunteer_public_key not copied from grant")
	}
	if got.RawMetrics == nil || len(got.RawMetrics) != 0 {
		t.Errorf("raw_metrics = %v, want empty non-nil map", got.RawMetrics)
	}
	if !VerifyAttestation(signer.PublicKey(), got) {
		t.Error("revocation signature does not verify under the signing key")
	}
}

func TestEmitForAdjustment_V1EraGrantNotFound(t *testing.T) {
	resultID := types.NewID()
	adjID := types.NewID()

	db := &stubDBTX{row: stubRow{vals: []any{adjID, "OPERATOR_CLAWBACK", "1.000000", resultID}}}
	repo := &fakeGrantRepo{getErr: apierror.NotFound("grant_attestation_for_result", resultID.String())}
	e := NewRevocationEmitter(db, repo, revTestSigner(t), revTestLogger())

	if err := e.EmitForAdjustment(context.Background(), adjID); err != nil {
		t.Fatalf("v1-era grant (NotFound) should yield nil error, got %v", err)
	}
	if len(repo.created) != 0 {
		t.Fatalf("no revocation row should be created for a v1-era grant, got %d", len(repo.created))
	}
}

func TestEmitForAdjustment_DuplicateConflictIdempotent(t *testing.T) {
	resultID := types.NewID()
	adjID := types.NewID()

	db := &stubDBTX{row: stubRow{vals: []any{adjID, "OPERATOR_CLAWBACK", "1.000000", resultID}}}
	repo := &fakeGrantRepo{
		grant: grantFixture(resultID),
		createErr: apierror.Conflict("attestation already exists",
			map[string]string{"constraint": "uq_attestations_adjustment"}),
	}
	e := NewRevocationEmitter(db, repo, revTestSigner(t), revTestLogger())

	if err := e.EmitForAdjustment(context.Background(), adjID); err != nil {
		t.Fatalf("duplicate-adjustment conflict should be swallowed as idempotent, got %v", err)
	}
}

// A conflict on any OTHER constraint (here an FK violation) is a real error and must propagate
// — only the uq_attestations_adjustment conflict is the idempotent already-emitted case.
func TestEmitForAdjustment_OtherConflictPropagates(t *testing.T) {
	resultID := types.NewID()
	adjID := types.NewID()

	db := &stubDBTX{row: stubRow{vals: []any{adjID, "OPERATOR_CLAWBACK", "1.000000", resultID}}}
	repo := &fakeGrantRepo{
		grant: grantFixture(resultID),
		createErr: apierror.Conflict("referenced entity does not exist",
			map[string]string{"constraint": "credit_attestations_result_id_fkey"}),
	}
	e := NewRevocationEmitter(db, repo, revTestSigner(t), revTestLogger())

	if err := e.EmitForAdjustment(context.Background(), adjID); err == nil {
		t.Fatal("a non-idempotent Create conflict must propagate, got nil")
	}
}

// The row load failing (e.g. the adjustment vanished) is surfaced, not silently ignored.
func TestEmitForAdjustment_RowLoadErrorPropagates(t *testing.T) {
	db := &stubDBTX{row: stubRow{err: pgx.ErrNoRows}}
	repo := &fakeGrantRepo{}
	e := NewRevocationEmitter(db, repo, revTestSigner(t), revTestLogger())

	if err := e.EmitForAdjustment(context.Background(), types.NewID()); err == nil {
		t.Fatal("a row-load error must propagate, got nil")
	}
	if len(repo.created) != 0 {
		t.Fatalf("no revocation row should be created when the adjustment cannot be loaded")
	}
}
