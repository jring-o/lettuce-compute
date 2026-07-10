package attestation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// grantLookupCreator is the narrow repository surface the revocation emitter needs: find THE
// AGREED v2 grant attestation for a result, and append the signed revocation row. Defined
// consumer-side so the emitter depends on exactly these two methods (*PgxRepository satisfies
// it).
type grantLookupCreator interface {
	GetGrantByResultID(ctx context.Context, resultID types.ID) (*Attestation, error)
	Create(ctx context.Context, att *Attestation) error
}

// pendingRevocationQuery loads the single adjustment a revocation is built from, joined to its
// ledger entry for the result id. (-a.amount)::text renders the clawed-back magnitude at the
// column's numeric(18,6) scale — exactly six fractional digits, which IS the canonical credit
// string that the signed payload and the stored value share (never a re-rounded float).
const pendingRevocationQuery = `SELECT a.id, a.reason, (-a.amount)::text, l.result_id
	FROM credit_adjustments a
	JOIN credit_ledger l ON l.id = a.ledger_entry_id
	WHERE a.id = $1`

// unreconciledAdjustmentsQuery selects committed adjustments that still lack a revocation
// attestation but whose ledger entry's result carries an AGREED v2 grant. The grant-row join
// is what restricts the sweep to post-cutover adjustments: v2 rows exist only from the slice
// that introduced this emitter, so v1-era adjustments (no v2 grant) are excluded by
// construction and no epoch tracking is needed.
const unreconciledAdjustmentsQuery = `SELECT a.id
	FROM credit_adjustments a
	JOIN credit_ledger l ON l.id = a.ledger_entry_id
	JOIN credit_attestations g ON g.result_id = l.result_id
		AND g.validation_outcome = 'AGREED' AND g.schema_version = 2
	WHERE NOT EXISTS (
		SELECT 1 FROM credit_attestations r WHERE r.adjustment_id = a.id
	)
	ORDER BY a.created_at
	LIMIT 100`

// pendingRevocation is the adjustment-side data a revocation attestation is built from.
type pendingRevocation struct {
	AdjustmentID       types.ID
	Reason             string
	MagnitudeCanonical string
	ResultID           types.ID
}

// RevocationEmitter builds, signs, and appends the revocation attestation that records a
// credit clawback, and reconciles adjustments whose in-handler emission was lost. It is the
// single seam every revocation producer (the manual clawback endpoint; the automated slasher
// in a later slice) routes through.
type RevocationEmitter struct {
	db     DBTX
	repo   grantLookupCreator
	signer *Signer
	logger *slog.Logger
}

// NewRevocationEmitter builds a RevocationEmitter over a database handle (for the
// adjustment-side reads), the attestation repository, and the head's signer.
func NewRevocationEmitter(db DBTX, repo grantLookupCreator, signer *Signer, logger *slog.Logger) *RevocationEmitter {
	return &RevocationEmitter{db: db, repo: repo, signer: signer, logger: logger}
}

// EmitForAdjustment builds and stores the revocation attestation for one committed
// credit_adjustments row. A result with no AGREED v2 grant (a v1-era clawback, or a missing
// grant) produces no row and no error — the adjustment stays the sole record, and inventing a
// heuristic match risks revoking the wrong attestation. A unique conflict from a concurrent
// emission is idempotently swallowed. Any other failure is returned so the caller (or the
// reconciliation sweep) can retry.
func (e *RevocationEmitter) EmitForAdjustment(ctx context.Context, adjustmentID types.ID) error {
	var p pendingRevocation
	if err := e.db.QueryRow(ctx, pendingRevocationQuery, adjustmentID).Scan(
		&p.AdjustmentID, &p.Reason, &p.MagnitudeCanonical, &p.ResultID,
	); err != nil {
		return fmt.Errorf("load adjustment %s for revocation: %w", adjustmentID, err)
	}

	grant, err := e.repo.GetGrantByResultID(ctx, p.ResultID)
	if err != nil {
		if isNotFoundError(err) {
			e.logger.Warn("no v2 grant attestation for clawed-back result; adjustment remains the only record",
				"adjustment_id", adjustmentID.String(), "result_id", p.ResultID.String())
			return nil
		}
		return err
	}

	// CreditAmount is display-only (the signed/stored value is the canonical string); parse it
	// for the JSON view. A parse failure here means malformed magnitude bytes that would also
	// fail signing, so surface it.
	magnitude, err := strconv.ParseFloat(p.MagnitudeCanonical, 64)
	if err != nil {
		return fmt.Errorf("parse revocation magnitude %q: %w", p.MagnitudeCanonical, err)
	}

	att := &Attestation{
		SchemaVersion:         SchemaVersionV2,
		LeafID:                grant.LeafID,
		VolunteerPublicKey:    grant.VolunteerPublicKey,
		WorkUnitID:            grant.WorkUnitID,
		ResultID:              grant.ResultID,
		RevokesAttestationID:  &grant.ID,
		AdjustmentID:          &adjustmentID,
		Reason:                &p.Reason,
		RawMetrics:            map[string]any{},
		ValidationOutcome:     OutcomeRevoked,
		CreditAmount:          magnitude,
		CreditAmountCanonical: p.MagnitudeCanonical,
		AttestationTimestamp:  types.Now(),
	}

	sig, err := e.signer.Sign(att)
	if err != nil {
		return fmt.Errorf("sign revocation for adjustment %s: %w", adjustmentID, err)
	}
	att.Signature = sig

	if err := e.repo.Create(ctx, att); err != nil {
		if isDuplicateAdjustmentConflict(err) {
			e.logger.Debug("revocation attestation already exists for adjustment; treating emission as done",
				"adjustment_id", adjustmentID.String())
			return nil
		}
		return err
	}
	return nil
}

// Reconcile emits the revocation attestation for every committed adjustment that is missing
// one but whose result carries an AGREED v2 grant, up to a bounded batch, and returns how many
// it wrote. It exists because in-handler emission is best-effort: a write that failed at
// clawback time would never be retried (re-POSTing the endpoint 409s before re-reaching
// emission), so this leader-gated sweep recovers it. Idempotency is structural — the
// uq_attestations_adjustment unique index turns a handler/sweep race into one insert plus one
// conflict-treated-as-done. Per-row failures are logged and skipped; the next sweep retries.
func (e *RevocationEmitter) Reconcile(ctx context.Context) (int, error) {
	rows, err := e.db.Query(ctx, unreconciledAdjustmentsQuery)
	if err != nil {
		return 0, fmt.Errorf("list unreconciled adjustments: %w", err)
	}
	// Drain the cursor before emitting: EmitForAdjustment issues its own queries, which must
	// not race an open rows handle on the same pooled connection.
	var ids []types.ID
	for rows.Next() {
		var id types.ID
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			return 0, fmt.Errorf("scan unreconciled adjustment id: %w", scanErr)
		}
		ids = append(ids, id)
	}
	iterErr := rows.Err()
	rows.Close()
	if iterErr != nil {
		return 0, fmt.Errorf("iterate unreconciled adjustments: %w", iterErr)
	}

	emitted := 0
	for _, id := range ids {
		if err := e.EmitForAdjustment(ctx, id); err != nil {
			e.logger.Warn("revocation reconcile: failed to emit for adjustment; will retry next sweep",
				"adjustment_id", id.String(), "error", err)
			continue
		}
		emitted++
	}
	return emitted, nil
}

// isNotFoundError reports whether err is an apierror 404 — the GetGrantByResultID "no AGREED
// v2 grant for this result" signal.
func isNotFoundError(err error) bool {
	var apiErr *apierror.APIError
	return errors.As(err, &apiErr) && apiErr.HTTPStatus == http.StatusNotFound
}

// isDuplicateAdjustmentConflict reports whether err is the unique-index conflict on
// uq_attestations_adjustment: a revocation for this adjustment already exists. Only that
// specific conflict is idempotently swallowed; other conflicts (e.g. an FK violation) are real
// errors the caller must see.
func isDuplicateAdjustmentConflict(err error) bool {
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusConflict {
		return false
	}
	detail, ok := apiErr.Details.(map[string]string)
	return ok && detail["constraint"] == "uq_attestations_adjustment"
}
