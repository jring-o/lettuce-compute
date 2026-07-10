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
	runner_output_checksum, created_at, claimed_at, completed_at,
	enforcement_eligible, enforcement_state, enforced_at, confirms_audit_id, claimed_hr_class`

func scanAudit(row pgx.Row) (*Audit, error) {
	var a Audit
	// status/enforcement_state are never NULL; verdict is NULL until COMPLETED. Scanning
	// the named-string columns through string locals sidesteps any pgx named-string-type
	// edge and keeps NULL handling explicit.
	var status string
	var verdict *string
	var enforcementState string
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
		&a.EnforcementEligible,
		&enforcementState,
		&a.EnforcedAt,
		&a.ConfirmsAuditID,
		&a.ClaimedHRClass,
	)
	if err != nil {
		return nil, err
	}
	a.Status = Status(status)
	a.EnforcementState = EnforcementState(enforcementState)
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
	// The confirmation-exclusion predicates (design doc §9.2, audits M1/M2/L1) live INSIDE
	// the inner row-selection subquery, never on the outer UPDATE: on the outer UPDATE a
	// failed predicate voids the whole claim (returns nothing) and self-starves a runner
	// whose own confirmation heads the queue, instead of skipping the one ineligible row.
	// They gate ONLY confirmation rows (confirms_audit_id NOT NULL); for an original
	// (confirms_audit_id IS NULL) the guard is always-true, so originals behave
	// byte-identically to before slice 3. A confirmation is claimable by a runner that
	// (a) did NOT complete the parent ROOT, (b) has NOT already touched a PRIOR
	// confirmation of the same root that ended INCONCLUSIVE/EXPIRED (each re-enqueue must
	// reach a FRESH runner so a broken confirmer exhausts the pool into STALLED rather than
	// silently absorbing every confirmation), and (c) for an UNPINNED unit presents an
	// hr_class DIFFERENT from the root runner's recorded class (mutual agreement between
	// two same-class runners proves nothing about a hardware-biased EXACT leaf).
	row := r.db.QueryRow(ctx, `
		UPDATE result_audits
		SET status = 'CLAIMED',
			claimed_by = $1,
			claimed_at = now(),
			claimed_hr_class = $2,
			attempts = attempts + 1,
			lease_expires_at = now() + make_interval(secs => GREATEST(
				(SELECT wu.deadline_seconds FROM work_units wu WHERE wu.id = result_audits.work_unit_id),
				$3))
		WHERE id = (
			SELECT c.id FROM result_audits c
			WHERE c.status = 'QUEUED'
			  AND (c.required_hr_class IS NULL OR c.required_hr_class = $2)
			  AND (c.confirms_audit_id IS NULL OR (
				NOT EXISTS (
					SELECT 1 FROM result_audits root
					WHERE root.id = c.confirms_audit_id AND root.claimed_by = $1
				)
				AND NOT EXISTS (
					SELECT 1 FROM result_audits sib
					WHERE sib.confirms_audit_id = c.confirms_audit_id
					  AND sib.claimed_by = $1
					  AND ((sib.status = 'COMPLETED' AND sib.verdict = 'INCONCLUSIVE')
						   OR sib.status = 'EXPIRED')
				)
				AND (c.required_hr_class IS NOT NULL OR NOT EXISTS (
					SELECT 1 FROM result_audits root
					WHERE root.id = c.confirms_audit_id AND root.claimed_hr_class = $2
				))
			  ))
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
				WHEN $7 AND $8 AND confirms_audit_id IS NULL
					THEN 'AWAITING_CONFIRMATION'
				ELSE enforcement_state
			END
		WHERE id = $1 AND status = 'CLAIMED' AND claimed_by = $2`,
		id, runnerID, string(verdict), detail, runnerOutput, checksum, enforcementEligible,
		verdict == VerdictMismatch,
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

// SweepStaleQueued expires QUEUED rows older than their per-row queued lifetime: originals
// (confirms_audit_id IS NULL) get QueuedLifetime (72h); confirmations get the tighter
// ConfirmationQueuedLifetime (24h) — priority work, and a shorter lifetime keeps the
// worst-case enforcement horizon inside the maturation window Validate() demands (audit H2).
func (r *PgxAuditsRepository) SweepStaleQueued(ctx context.Context) (int, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE result_audits SET status = 'EXPIRED'
		WHERE status = 'QUEUED'
		  AND created_at < now() - make_interval(secs => CASE
			WHEN confirms_audit_id IS NULL THEN $1::int
			ELSE $2::int END)`,
		int(QueuedLifetime.Seconds()), int(ConfirmationQueuedLifetime.Seconds()),
	)
	if err != nil {
		return 0, apierror.Internal("failed to expire stale queued audits", err)
	}
	return int(tag.RowsAffected()), nil
}

// Stats returns the fault-monitor probe payload. Two round trips: the scalar lanes in one
// aggregate query, and the per-runner INCONCLUSIVE-confirmation share (a GROUP BY) in a
// second (design doc §9.8).
func (r *PgxAuditsRepository) Stats(ctx context.Context) (Stats, error) {
	var (
		s              Stats
		oldestQueued   float64
		oldestAwaiting float64
	)
	err := r.db.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM result_audits WHERE verdict = 'MISMATCH')::int,
			(SELECT COUNT(*) FROM result_audits WHERE status = 'EXPIRED')::int,
			(SELECT COUNT(*) FROM result_audits WHERE status = 'QUEUED')::int,
			COALESCE(EXTRACT(EPOCH FROM (
				now() - (SELECT MIN(created_at) FROM result_audits WHERE status = 'QUEUED')
			)), 0)::float8,
			(SELECT COUNT(*) FROM result_audits WHERE enforcement_state = 'ENFORCED')::int,
			(SELECT COUNT(*) FROM result_audits WHERE enforcement_state = 'CONTRADICTED')::int,
			(SELECT COUNT(*) FROM result_audits WHERE enforcement_state = 'STALLED')::int,
			COALESCE(EXTRACT(EPOCH FROM (
				now() - (SELECT MIN(completed_at) FROM result_audits
					WHERE enforcement_state = 'AWAITING_CONFIRMATION' AND confirms_audit_id IS NULL)
			)), 0)::float8`,
	).Scan(&s.MismatchTotal, &s.ExpiredTotal, &s.QueuedCount, &oldestQueued,
		&s.EnforcedTotal, &s.ContradictedTotal, &s.StalledCount, &oldestAwaiting)
	if err != nil {
		return Stats{}, apierror.Internal("failed to read audit stats", err)
	}
	s.OldestQueuedAge = time.Duration(oldestQueued * float64(time.Second))
	s.OldestAwaitingConfirmationAge = time.Duration(oldestAwaiting * float64(time.Second))

	// Per-runner INCONCLUSIVE share over CONFIRMATION rows only (a broken/suppressing
	// confirmer must be named, audit M2). Left nil when there are no such rows.
	rows, err := r.db.Query(ctx, `
		SELECT claimed_by, COUNT(*)::int
		FROM result_audits
		WHERE confirms_audit_id IS NOT NULL
		  AND status = 'COMPLETED' AND verdict = 'INCONCLUSIVE'
		  AND claimed_by IS NOT NULL
		GROUP BY claimed_by`)
	if err != nil {
		return Stats{}, apierror.Internal("failed to read confirmation inconclusive share", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runnerID types.ID
		var n int
		if scanErr := rows.Scan(&runnerID, &n); scanErr != nil {
			return Stats{}, apierror.Internal("failed to scan confirmation inconclusive share", scanErr)
		}
		if s.InconclusiveByRunner == nil {
			s.InconclusiveByRunner = make(map[string]int)
		}
		s.InconclusiveByRunner[runnerID.String()] = n
	}
	if err := rows.Err(); err != nil {
		return Stats{}, apierror.Internal("failed to iterate confirmation inconclusive share", err)
	}
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
	if f.EnforcementState != "" {
		args = append(args, string(f.EnforcementState))
		conds = append(conds, fmt.Sprintf("enforcement_state = $%d", len(args)))
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

// --- slice-3 enforcement surface (design doc §9) ---

// EnqueueConfirmation inserts a QUEUED second-runner confirmation for the root: one
// INSERT ... SELECT copying the root's unit/leaf/accepted/snapshot/pin/execution columns
// with confirms_audit_id = rootID (design doc §9.2). A unique-violation on
// uq_result_audits_open_unit (there is already an OPEN audit for the unit — the confirmation
// is already enqueued, or a fresh sample is open) surfaces as ErrDuplicateOpenAudit, which
// the enforcement pass treats as already-enqueued (WARN, never a retry storm). A rootID that
// no longer resolves (cascaded deletion) inserts zero rows → NotFound.
func (r *PgxAuditsRepository) EnqueueConfirmation(ctx context.Context, rootID types.ID) (*Audit, error) {
	row := r.db.QueryRow(ctx, `
		INSERT INTO result_audits (
			work_unit_id, leaf_id, accepted_result_id, accepted_comparison_key,
			comparison_snapshot, required_hr_class, artifact_version_id, execution_snapshot,
			confirms_audit_id, status
		)
		SELECT work_unit_id, leaf_id, accepted_result_id, accepted_comparison_key,
			comparison_snapshot, required_hr_class, artifact_version_id, execution_snapshot,
			$1, 'QUEUED'
		FROM result_audits WHERE id = $1
		RETURNING `+auditColumns,
		rootID,
	)
	created, err := scanAudit(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrDuplicateOpenAudit
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("result_audit", rootID.String())
		}
		return nil, apierror.Internal("failed to enqueue confirmation audit", err)
	}
	return created, nil
}

// GetRunnerOutput returns the persisted verbatim runner bytes of a COMPLETED audit (the
// enforcement pass's mutual ground-truth agreement check + repair, design doc §9.3/§9.6).
// runner_output is excluded from every other read; NotFound when the row is absent or not
// yet COMPLETED.
func (r *PgxAuditsRepository) GetRunnerOutput(ctx context.Context, id types.ID) ([]byte, error) {
	var out []byte
	err := r.db.QueryRow(ctx,
		`SELECT runner_output FROM result_audits WHERE id = $1 AND status = 'COMPLETED'`, id,
	).Scan(&out)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("result_audit runner output", id.String())
		}
		return nil, apierror.Internal("failed to read audit runner output", err)
	}
	return out, nil
}

// ListActionableRoots returns eligible MISMATCH ORIGINALS still in NONE/AWAITING_CONFIRMATION,
// oldest completed_at first — the exact predicate of idx_result_audits_enforcement (design
// doc §9.3). Observe-era rows (enforcement_eligible = false) are structurally excluded, as
// are confirmation rows and non-MISMATCH verdicts.
func (r *PgxAuditsRepository) ListActionableRoots(ctx context.Context, limit int) ([]*Audit, error) {
	if limit <= 0 {
		limit = defaultAuditListLimit
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+auditColumns+` FROM result_audits
		WHERE verdict = 'MISMATCH' AND enforcement_eligible
		  AND enforcement_state IN ('NONE', 'AWAITING_CONFIRMATION')
		  AND confirms_audit_id IS NULL
		ORDER BY completed_at ASC
		LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list actionable roots", err)
	}
	defer rows.Close()

	var out []*Audit
	for rows.Next() {
		a, scanErr := scanAudit(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan actionable root", scanErr)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate actionable roots", err)
	}
	return out, nil
}

// ConfirmationsForRoot returns every confirmation row of the root, newest first. Their COUNT
// is the derived confirmation-attempt counter (audit M4); the newest is the row the sweep
// examines (design doc §9.3 step 1).
func (r *PgxAuditsRepository) ConfirmationsForRoot(ctx context.Context, rootID types.ID) ([]*Audit, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+auditColumns+` FROM result_audits WHERE confirms_audit_id = $1 ORDER BY created_at DESC`,
		rootID,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list confirmations for root", err)
	}
	defer rows.Close()

	var out []*Audit
	for rows.Next() {
		a, scanErr := scanAudit(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan confirmation", scanErr)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate confirmations for root", err)
	}
	return out, nil
}

// SetEnforcementState transitions a root's enforcement bookkeeping. Guarded to the
// non-terminal states (NONE/AWAITING_CONFIRMATION) so a terminal row is never re-opened and
// a leadership-failover double-sweep is a no-op; ENFORCED also stamps enforced_at. Returns
// false when the guard missed (the row is already ENFORCED/CONTRADICTED/STALLED).
func (r *PgxAuditsRepository) SetEnforcementState(ctx context.Context, id types.ID, state EnforcementState) (bool, error) {
	// $3 carries the "is ENFORCED" flag as a bool rather than re-testing $2 in the CASE:
	// a bare `$2 = 'ENFORCED'` alongside `enforcement_state = $2` makes Postgres deduce two
	// types for $2 (varchar from the column, text from the literal) and reject the prepare.
	tag, err := r.db.Exec(ctx, `
		UPDATE result_audits SET
			enforcement_state = $2,
			enforced_at = CASE WHEN $3 THEN now() ELSE enforced_at END
		WHERE id = $1 AND enforcement_state IN ('NONE', 'AWAITING_CONFIRMATION')`,
		id, string(state), state == EnforcementEnforced,
	)
	if err != nil {
		return false, apierror.Internal("failed to set enforcement state", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimRepair inserts the audit_repairs idempotency claim for (auditID, resultID). The
// UNIQUE(result_id) constraint means only the FIRST claim of a result wins: claimed=false
// when the result was already repaired (by any audit), gating the non-idempotent repair
// effects (design doc §9.6).
func (r *PgxAuditsRepository) ClaimRepair(ctx context.Context, auditID, resultID types.ID) (bool, error) {
	tag, err := r.db.Exec(ctx,
		`INSERT INTO audit_repairs (audit_id, result_id) VALUES ($1, $2)
		 ON CONFLICT (result_id) DO NOTHING`,
		auditID, resultID,
	)
	if err != nil {
		return false, apierror.Internal("failed to claim audit repair", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FlaggedLeaves returns the admin flagged-leaves surface (design doc §9.8): one row per leaf
// with ≥ 1 ENFORCED/CONTRADICTED/STALLED ROOT audit (confirms_audit_id IS NULL), with
// per-state counts, the newest enforced_at, and the leaf's creator id. Newest-enforced first.
func (r *PgxAuditsRepository) FlaggedLeaves(ctx context.Context) ([]FlaggedLeaf, error) {
	rows, err := r.db.Query(ctx, `
		SELECT ra.leaf_id, lf.creator_id,
			COUNT(*) FILTER (WHERE ra.enforcement_state = 'ENFORCED')::int,
			COUNT(*) FILTER (WHERE ra.enforcement_state = 'CONTRADICTED')::int,
			COUNT(*) FILTER (WHERE ra.enforcement_state = 'STALLED')::int,
			MAX(ra.enforced_at)
		FROM result_audits ra
		JOIN leafs lf ON lf.id = ra.leaf_id
		WHERE ra.confirms_audit_id IS NULL
		  AND ra.enforcement_state IN ('ENFORCED', 'CONTRADICTED', 'STALLED')
		GROUP BY ra.leaf_id, lf.creator_id
		ORDER BY MAX(ra.enforced_at) DESC NULLS LAST`)
	if err != nil {
		return nil, apierror.Internal("failed to list flagged leaves", err)
	}
	defer rows.Close()

	var out []FlaggedLeaf
	for rows.Next() {
		var fl FlaggedLeaf
		if scanErr := rows.Scan(
			&fl.LeafID, &fl.OwnerID,
			&fl.EnforcedCount, &fl.ContradictedCount, &fl.StalledCount,
			&fl.LastEnforcedAt,
		); scanErr != nil {
			return nil, apierror.Internal("failed to scan flagged leaf", scanErr)
		}
		out = append(out, fl)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate flagged leaves", err)
	}
	return out, nil
}
