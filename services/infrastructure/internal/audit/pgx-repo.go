package audit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// defaultAuditListLimit bounds the admin read surface when the caller passes limit <= 0.
const defaultAuditListLimit = 100

// ErrDuplicateOpenAudit is returned by Enqueue when the partial unique index
// (uq_result_audits_open_unit — one OPEN audit per unit) rejects the insert. The sampling
// hook logs it and drops the sample: enqueue is best-effort and never fails validation.
var ErrDuplicateOpenAudit = errors.New("an open audit already exists for this work unit")

// DBTX is the common interface satisfied by *pgxpool.Pool and pgx.Tx (mirrors
// credit.DBTX / trust.DBTX). The audit stores run on the pool: every lifecycle mutation is
// a single race-safe statement (SKIP LOCKED claim, guarded completion, guarded sweeps), so
// no enclosing transaction is needed.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// --- trusted_runners registry -----------------------------------------------------------

// runnerColumns is the standard SELECT/RETURNING list for a Runner (note NULL-coalesced to
// the empty string so a nil note column scans into the non-pointer Runner.Note).
const runnerColumns = `id, volunteer_id, label, COALESCE(note, '') AS note, active, created_at, updated_at`

func scanRunner(row pgx.Row) (*Runner, error) {
	var rn Runner
	err := row.Scan(
		&rn.ID,
		&rn.VolunteerID,
		&rn.Label,
		&rn.Note,
		&rn.Active,
		&rn.CreatedAt,
		&rn.UpdatedAt,
	)
	return &rn, err
}

// PgxRunnersRepository implements RunnersRepository using pgx.
type PgxRunnersRepository struct {
	db DBTX
}

// NewPgxRunnersRepository creates a new PgxRunnersRepository.
func NewPgxRunnersRepository(db DBTX) *PgxRunnersRepository {
	return &PgxRunnersRepository{db: db}
}

// Register creates the registry row for a volunteer, or reactivates + relabels an existing
// one (upsert on the UNIQUE volunteer_id). A volunteer id that does not exist surfaces the
// FK violation as ErrUnknownVolunteer.
func (r *PgxRunnersRepository) Register(ctx context.Context, volunteerID types.ID, label, note string) (*Runner, error) {
	row := r.db.QueryRow(ctx, `
		INSERT INTO trusted_runners (volunteer_id, label, note, active)
		VALUES ($1, $2, $3, true)
		ON CONFLICT (volunteer_id) DO UPDATE SET
			label = EXCLUDED.label,
			note = EXCLUDED.note,
			active = true,
			updated_at = now()
		RETURNING `+runnerColumns,
		volunteerID, label, note,
	)
	rn, err := scanRunner(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return nil, ErrUnknownVolunteer
		}
		return nil, apierror.Internal("failed to register trusted runner", err)
	}
	return rn, nil
}

// Deactivate sets active = false. Rows are never deleted (claimed_by provenance on past
// audits must survive). An unknown volunteer touches zero rows → ErrNotRegistered.
func (r *PgxRunnersRepository) Deactivate(ctx context.Context, volunteerID types.ID) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE trusted_runners SET active = false, updated_at = now() WHERE volunteer_id = $1`,
		volunteerID,
	)
	if err != nil {
		return apierror.Internal("failed to deactivate trusted runner", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotRegistered
	}
	return nil
}

// List returns every registry row, active and inactive, newest first.
func (r *PgxRunnersRepository) List(ctx context.Context) ([]*Runner, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+runnerColumns+` FROM trusted_runners ORDER BY created_at DESC`)
	if err != nil {
		return nil, apierror.Internal("failed to list trusted runners", err)
	}
	defer rows.Close()

	var out []*Runner
	for rows.Next() {
		rn, scanErr := scanRunner(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan trusted runner", scanErr)
		}
		out = append(out, rn)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate trusted runners", err)
	}
	return out, nil
}

// GetActiveByVolunteerID resolves the ACTIVE registry row for a volunteer (the AuditService
// authorization step). ErrNotRegistered when absent or inactive.
func (r *PgxRunnersRepository) GetActiveByVolunteerID(ctx context.Context, volunteerID types.ID) (*Runner, error) {
	row := r.db.QueryRow(ctx,
		`SELECT `+runnerColumns+` FROM trusted_runners WHERE volunteer_id = $1 AND active = true`,
		volunteerID,
	)
	rn, err := scanRunner(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotRegistered
		}
		return nil, apierror.Internal("failed to get active trusted runner", err)
	}
	return rn, nil
}

// ActiveRunnerSubjects returns the CURRENT trust subjects of all ACTIVE runners, computed
// through the canonical trust.SubjectForVolunteer over the volunteers join — never a
// denormalized subject column, because DID binding changes subjects. Empty slice = no active
// runners (the accrual rule then stays legacy).
func (r *PgxRunnersRepository) ActiveRunnerSubjects(ctx context.Context) ([]string, error) {
	rows, err := r.db.Query(ctx, `
		SELECT v.id, v.did, v.did_binding_status
		FROM trusted_runners tr
		JOIN volunteers v ON v.id = tr.volunteer_id
		WHERE tr.active`)
	if err != nil {
		return nil, apierror.Internal("failed to query active runner subjects", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var (
			id            types.ID
			did           *string
			bindingStatus *string
		)
		if scanErr := rows.Scan(&id, &did, &bindingStatus); scanErr != nil {
			return nil, apierror.Internal("failed to scan active runner subject", scanErr)
		}
		// Populate exactly the fields SubjectForVolunteer reads (id + the live-binding
		// gate); the golden-pinned subject expression owns the rest.
		v := volunteer.Volunteer{ID: id, DID: did, DIDBindingStatus: bindingStatus}
		out = append(out, trust.SubjectForVolunteer(&v))
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate active runner subjects", err)
	}
	return out, nil
}

// --- result_audits job store ------------------------------------------------------------

// auditColumns is the standard SELECT/RETURNING list for an Audit. It DELIBERATELY excludes
// runner_output (a potentially large bytea): no repo read surface returns those bytes — a
// claimed row has none yet, and List/GetByID must stay cheap.
const auditColumns = `id, work_unit_id, leaf_id, accepted_result_id, accepted_comparison_key,
	comparison_snapshot, required_hr_class, artifact_version_id, execution_snapshot,
	status, verdict, verdict_detail, attempts, claimed_by, lease_expires_at,
	runner_output_checksum, created_at, claimed_at, completed_at`

func scanAudit(row pgx.Row) (*Audit, error) {
	var a Audit
	// status is never NULL; verdict is NULL until COMPLETED. Scanning both through string
	// locals sidesteps any pgx named-string-type edge and keeps NULL handling explicit.
	var status string
	var verdict *string
	err := row.Scan(
		&a.ID,
		&a.WorkUnitID,
		&a.LeafID,
		&a.AcceptedResultID,
		&a.AcceptedComparisonKey,
		&a.ComparisonSnapshot,
		&a.RequiredHRClass,
		&a.ArtifactVersionID,
		&a.ExecutionSnapshot,
		&status,
		&verdict,
		&a.VerdictDetail,
		&a.Attempts,
		&a.ClaimedBy,
		&a.LeaseExpiresAt,
		&a.RunnerOutputChecksum,
		&a.CreatedAt,
		&a.ClaimedAt,
		&a.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	a.Status = Status(status)
	if verdict != nil {
		v := Verdict(*verdict)
		a.Verdict = &v
	}
	return &a, nil
}

// PgxAuditsRepository implements AuditsRepository (and thus Enqueuer) using pgx.
type PgxAuditsRepository struct {
	db DBTX
}

// NewPgxAuditsRepository creates a new PgxAuditsRepository.
func NewPgxAuditsRepository(db DBTX) *PgxAuditsRepository {
	return &PgxAuditsRepository{db: db}
}

// Enqueue inserts a QUEUED audit row. A unique-violation on uq_result_audits_open_unit (one
// OPEN audit per unit) returns ErrDuplicateOpenAudit, which the best-effort sampling hook
// logs and drops.
func (r *PgxAuditsRepository) Enqueue(ctx context.Context, a *Audit) error {
	row := r.db.QueryRow(ctx, `
		INSERT INTO result_audits (
			work_unit_id, leaf_id, accepted_result_id, accepted_comparison_key,
			comparison_snapshot, required_hr_class, artifact_version_id, execution_snapshot
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+auditColumns,
		a.WorkUnitID,
		a.LeafID,
		a.AcceptedResultID,
		a.AcceptedComparisonKey,
		a.ComparisonSnapshot,
		a.RequiredHRClass,
		a.ArtifactVersionID,
		a.ExecutionSnapshot,
	)
	created, err := scanAudit(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicateOpenAudit
		}
		return apierror.Internal("failed to enqueue result audit", err)
	}
	*a = *created
	return nil
}

// Claim atomically claims the oldest QUEUED job the runner is eligible for (required_hr_class
// NULL or equal to runnerHRClass), respecting MaxConcurrentClaims per runner. The lease is
// computed in-SQL as now() + max(unit deadline_seconds, LeaseFloor). The SKIP LOCKED subquery
// is the claim-queue idiom: two concurrent runners get two different jobs. Returns (nil, nil)
// when nothing is claimable (no eligible job, or the runner is at its concurrency cap).
func (r *PgxAuditsRepository) Claim(ctx context.Context, runnerID types.ID, runnerHRClass string) (*Audit, error) {
	leaseFloorSecs := int(LeaseFloor.Seconds())
	// The lease derives from the claimed row's own unit deadline, read via a correlated
	// scalar subquery rather than an UPDATE ... FROM join — a join would put work_units in
	// RETURNING scope and make bare column names (id/leaf_id/created_at exist on both tables)
	// ambiguous. FOR UPDATE OF c SKIP LOCKED is the concurrent-claim queue idiom.
	row := r.db.QueryRow(ctx, `
		UPDATE result_audits
		SET status = 'CLAIMED',
			claimed_by = $1,
			claimed_at = now(),
			attempts = attempts + 1,
			lease_expires_at = now() + make_interval(secs => GREATEST(
				(SELECT wu.deadline_seconds FROM work_units wu WHERE wu.id = result_audits.work_unit_id),
				$3))
		WHERE id = (
			SELECT c.id FROM result_audits c
			WHERE c.status = 'QUEUED'
			  AND (c.required_hr_class IS NULL OR c.required_hr_class = $2)
			ORDER BY c.created_at
			LIMIT 1
			FOR UPDATE OF c SKIP LOCKED
		  )
		  AND (
			SELECT COUNT(*) FROM result_audits h
			WHERE h.claimed_by = $1 AND h.status = 'CLAIMED'
		  ) < $4
		RETURNING `+auditColumns,
		runnerID, runnerHRClass, leaseFloorSecs, MaxConcurrentClaims,
	)
	claimed, err := scanAudit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, apierror.Internal("failed to claim result audit", err)
	}
	return claimed, nil
}

// GetByID loads one audit row (runner_output excluded — see auditColumns).
func (r *PgxAuditsRepository) GetByID(ctx context.Context, id types.ID) (*Audit, error) {
	row := r.db.QueryRow(ctx,
		`SELECT `+auditColumns+` FROM result_audits WHERE id = $1`, id)
	a, err := scanAudit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("result_audit", id.String())
		}
		return nil, apierror.Internal("failed to get result audit", err)
	}
	return a, nil
}

// CompleteVerdict finalizes a CLAIMED job with a head-computed verdict, storing the verbatim
// runner bytes + head-computed checksum. Guarded: the row must still be CLAIMED by runnerID,
// else ErrNotClaimant. enforcementEligible stamps the enforcement knob's verdict-write-time
// state, and the SAME statement moves an eligible MISMATCH ORIGINAL straight to
// AWAITING_CONFIRMATION — an actionable root is never observable in NONE, so no crash
// window between verdict and confirmation-enqueue can route around the second-runner
// requirement (design doc §9.2, audit H1).
func (r *PgxAuditsRepository) CompleteVerdict(ctx context.Context, id, runnerID types.ID, verdict Verdict, detail string, runnerOutput []byte, checksum string, enforcementEligible bool) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE result_audits SET
			status = 'COMPLETED',
			verdict = $3,
			verdict_detail = $4,
			runner_output = $5,
			runner_output_checksum = $6,
			completed_at = now(),
			lease_expires_at = NULL,
			enforcement_eligible = $7,
			enforcement_state = CASE
				WHEN $7 AND $3 = 'MISMATCH' AND confirms_audit_id IS NULL
					THEN 'AWAITING_CONFIRMATION'
				ELSE enforcement_state
			END
		WHERE id = $1 AND status = 'CLAIMED' AND claimed_by = $2`,
		id, runnerID, string(verdict), detail, runnerOutput, checksum, enforcementEligible,
	)
	if err != nil {
		return apierror.Internal("failed to complete audit verdict", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotClaimant
	}
	return nil
}

// CompleteInconclusive finalizes a job INCONCLUSIVE outside the submit path (the claim
// handler uses it when the sampled artifacts cannot be resolved). Guarded on the row still
// being CLAIMED by runnerID, else ErrNotClaimant.
func (r *PgxAuditsRepository) CompleteInconclusive(ctx context.Context, id, runnerID types.ID, detail string) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE result_audits SET
			status = 'COMPLETED',
			verdict = 'INCONCLUSIVE',
			verdict_detail = $3,
			completed_at = now(),
			lease_expires_at = NULL
		WHERE id = $1 AND status = 'CLAIMED' AND claimed_by = $2`,
		id, runnerID, detail,
	)
	if err != nil {
		return apierror.Internal("failed to complete audit inconclusive", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotClaimant
	}
	return nil
}

// ReleaseFailure handles a runner-reported execution failure: attempts < MaxAttempts → back
// to QUEUED (claim fields cleared); else EXPIRED (claim fields preserved for provenance,
// completed fields untouched — EXPIRED carries no verdict). The error message lands in
// verdict_detail either way. Guarded on CLAIMED-by-runnerID, else ErrNotClaimant.
func (r *PgxAuditsRepository) ReleaseFailure(ctx context.Context, id, runnerID types.ID, errMsg string) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE result_audits SET
			status           = CASE WHEN attempts < $3 THEN 'QUEUED' ELSE 'EXPIRED' END,
			claimed_by       = CASE WHEN attempts < $3 THEN NULL ELSE claimed_by END,
			claimed_at       = CASE WHEN attempts < $3 THEN NULL ELSE claimed_at END,
			lease_expires_at = CASE WHEN attempts < $3 THEN NULL ELSE lease_expires_at END,
			verdict_detail   = $4
		WHERE id = $1 AND status = 'CLAIMED' AND claimed_by = $2`,
		id, runnerID, MaxAttempts, errMsg,
	)
	if err != nil {
		return apierror.Internal("failed to release failed audit", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotClaimant
	}
	return nil
}

// SweepLapsedLeases requeues CLAIMED rows whose lease has lapsed (attempts < MaxAttempts) and
// expires the rest. Two guarded UPDATEs; returns (requeued, expired).
func (r *PgxAuditsRepository) SweepLapsedLeases(ctx context.Context) (int, int, error) {
	reqTag, err := r.db.Exec(ctx, `
		UPDATE result_audits SET
			status = 'QUEUED',
			claimed_by = NULL,
			claimed_at = NULL,
			lease_expires_at = NULL
		WHERE status = 'CLAIMED' AND lease_expires_at < now() AND attempts < $1`,
		MaxAttempts,
	)
	if err != nil {
		return 0, 0, apierror.Internal("failed to requeue lapsed audit leases", err)
	}
	expTag, err := r.db.Exec(ctx, `
		UPDATE result_audits SET status = 'EXPIRED'
		WHERE status = 'CLAIMED' AND lease_expires_at < now() AND attempts >= $1`,
		MaxAttempts,
	)
	if err != nil {
		return int(reqTag.RowsAffected()), 0, apierror.Internal("failed to expire lapsed audit leases", err)
	}
	return int(reqTag.RowsAffected()), int(expTag.RowsAffected()), nil
}

// SweepStaleQueued expires QUEUED rows older than QueuedLifetime.
func (r *PgxAuditsRepository) SweepStaleQueued(ctx context.Context) (int, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE result_audits SET status = 'EXPIRED'
		WHERE status = 'QUEUED' AND created_at < now() - make_interval(secs => $1)`,
		int(QueuedLifetime.Seconds()),
	)
	if err != nil {
		return 0, apierror.Internal("failed to expire stale queued audits", err)
	}
	return int(tag.RowsAffected()), nil
}

// Stats returns the fault-monitor probe payload in one round trip.
func (r *PgxAuditsRepository) Stats(ctx context.Context) (Stats, error) {
	var (
		s          Stats
		oldestSecs float64
	)
	err := r.db.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM result_audits WHERE verdict = 'MISMATCH')::int,
			(SELECT COUNT(*) FROM result_audits WHERE status = 'EXPIRED')::int,
			(SELECT COUNT(*) FROM result_audits WHERE status = 'QUEUED')::int,
			COALESCE(EXTRACT(EPOCH FROM (
				now() - (SELECT MIN(created_at) FROM result_audits WHERE status = 'QUEUED')
			)), 0)::float8`,
	).Scan(&s.MismatchTotal, &s.ExpiredTotal, &s.QueuedCount, &oldestSecs)
	if err != nil {
		return Stats{}, apierror.Internal("failed to read audit stats", err)
	}
	s.OldestQueuedAge = time.Duration(oldestSecs * float64(time.Second))
	return s, nil
}

// List returns audit rows for the admin read surface, newest first, optionally filtered.
// limit <= 0 applies defaultAuditListLimit. runner_output is excluded (see auditColumns).
func (r *PgxAuditsRepository) List(ctx context.Context, f ListFilter) ([]*Audit, error) {
	q := `SELECT ` + auditColumns + ` FROM result_audits`
	var conds []string
	var args []any
	if f.Status != "" {
		args = append(args, string(f.Status))
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.Verdict != "" {
		args = append(args, string(f.Verdict))
		conds = append(conds, fmt.Sprintf("verdict = $%d", len(args)))
	}
	if f.LeafID != nil {
		args = append(args, *f.LeafID)
		conds = append(conds, fmt.Sprintf("leaf_id = $%d", len(args)))
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultAuditListLimit
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, apierror.Internal("failed to list result audits", err)
	}
	defer rows.Close()

	var out []*Audit
	for rows.Next() {
		a, scanErr := scanAudit(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan result audit", scanErr)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate result audits", err)
	}
	return out, nil
}

// --- slice-3 enforcement surface (design doc §9) — keel stubs; the enforcement
// implementer replaces each with the real guarded SQL. ---

// EnqueueConfirmation inserts a QUEUED confirmation row for the root (§9.2).
func (r *PgxAuditsRepository) EnqueueConfirmation(ctx context.Context, rootID types.ID) (*Audit, error) {
	return nil, apierror.Internal("EnqueueConfirmation not implemented (slice-3 keel stub)", nil)
}

// GetRunnerOutput returns the persisted verbatim runner bytes of a COMPLETED audit.
func (r *PgxAuditsRepository) GetRunnerOutput(ctx context.Context, id types.ID) ([]byte, error) {
	return nil, apierror.Internal("GetRunnerOutput not implemented (slice-3 keel stub)", nil)
}

// ListActionableRoots returns eligible MISMATCH originals awaiting enforcement (§9.3).
func (r *PgxAuditsRepository) ListActionableRoots(ctx context.Context, limit int) ([]*Audit, error) {
	return nil, apierror.Internal("ListActionableRoots not implemented (slice-3 keel stub)", nil)
}

// ConfirmationsForRoot returns every confirmation row of the root, newest first.
func (r *PgxAuditsRepository) ConfirmationsForRoot(ctx context.Context, rootID types.ID) ([]*Audit, error) {
	return nil, apierror.Internal("ConfirmationsForRoot not implemented (slice-3 keel stub)", nil)
}

// SetEnforcementState transitions a root's enforcement bookkeeping (guarded).
func (r *PgxAuditsRepository) SetEnforcementState(ctx context.Context, id types.ID, state EnforcementState) (bool, error) {
	return false, apierror.Internal("SetEnforcementState not implemented (slice-3 keel stub)", nil)
}

// ClaimRepair inserts the audit_repairs idempotency claim (§9.6).
func (r *PgxAuditsRepository) ClaimRepair(ctx context.Context, auditID, resultID types.ID) (bool, error) {
	return false, apierror.Internal("ClaimRepair not implemented (slice-3 keel stub)", nil)
}
