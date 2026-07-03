package workunit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// DBTX is the common interface satisfied by *pgxpool.Pool and pgx.Tx,
// allowing repository methods to work within explicit transactions.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
}

// defaultClaimLeaseSeconds is the fallback dispatch-claim lease (seconds) used by
// the repo when a caller passes a non-positive lease. It mirrors the config-package
// default; the config Validate floors keep the configured value sane.
const defaultClaimLeaseSeconds = 120

// workUnitColumns is the standard column list for SELECT queries on work_units.
// The per-unit reserved_until/reserved_volunteer_id columns were retired in
// migration 00006 (live dispatch state is now per-copy in work_unit_assignment_history);
// max_total_copies (the dead-letter ceiling) takes their place.
const workUnitColumns = `id, leaf_id, batch_id, state, priority,
	input_data, input_data_ref, code_artifact_ref, parameters,
	estimated_duration_seconds, deadline_seconds, output_spec,
	assigned_volunteer_id, assigned_at, started_at, completed_at, validated_at,
	reassignment_count, max_reassignments, max_total_copies, last_heartbeat_at,
	flagged_for_review, spot_check, last_checkpoint_at, last_checkpoint_sequence,
	created_at, updated_at, hr_class,
	target_copies, min_quorum, max_error_copies, max_success_copies`

// scanWorkUnit scans a work unit row into a WorkUnit struct.
// The column order must match workUnitColumns.
func scanWorkUnit(row pgx.Row) (*WorkUnit, error) {
	var wu WorkUnit
	err := row.Scan(
		&wu.ID,
		&wu.LeafID,
		&wu.BatchID,
		&wu.State,
		&wu.Priority,
		&wu.InputData,
		&wu.InputDataRef,
		&wu.CodeArtifactRef,
		&wu.Parameters,
		&wu.EstimatedDurationSeconds,
		&wu.DeadlineSeconds,
		&wu.OutputSpec,
		&wu.AssignedVolunteerID,
		&wu.AssignedAt,
		&wu.StartedAt,
		&wu.CompletedAt,
		&wu.ValidatedAt,
		&wu.ReassignmentCount,
		&wu.MaxReassignments,
		&wu.MaxTotalCopies,
		&wu.LastHeartbeatAt,
		&wu.FlaggedForReview,
		&wu.SpotCheck,
		&wu.LastCheckpointAt,
		&wu.LastCheckpointSequence,
		&wu.CreatedAt,
		&wu.UpdatedAt,
		&wu.HRClass,
		&wu.TargetCopies,
		&wu.MinQuorum,
		&wu.MaxErrorCopies,
		&wu.MaxSuccessCopies,
	)
	return &wu, err
}

// batchColumns is the standard column list for SELECT queries on batches.
const batchColumns = `id, leaf_id, sequence_number, total_work_units,
	completed_work_units, created_at, updated_at`

// scanBatch scans a batch row into a Batch struct.
func scanBatch(row pgx.Row) (*Batch, error) {
	var b Batch
	err := row.Scan(
		&b.ID,
		&b.LeafID,
		&b.SequenceNumber,
		&b.TotalWorkUnits,
		&b.CompletedWorkUnits,
		&b.CreatedAt,
		&b.UpdatedAt,
	)
	return &b, err
}

// --- PgxWorkUnitRepository ---

// PgxWorkUnitRepository implements WorkUnitRepository using pgx.
type PgxWorkUnitRepository struct {
	db DBTX
}

// NewPgxWorkUnitRepository creates a new PgxWorkUnitRepository.
// Accepts *pgxpool.Pool for normal use or pgx.Tx for transactional use.
func NewPgxWorkUnitRepository(db DBTX) *PgxWorkUnitRepository {
	return &PgxWorkUnitRepository{db: db}
}

// Create inserts a new work unit. On return, wu is populated with DB-generated
// id and timestamps.
func (r *PgxWorkUnitRepository) Create(ctx context.Context, wu *WorkUnit) error {
	row := r.db.QueryRow(ctx, `
		INSERT INTO work_units (
			leaf_id, batch_id, state, priority,
			input_data, input_data_ref, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds, output_spec,
			assigned_volunteer_id, assigned_at, started_at, completed_at, validated_at,
			reassignment_count, max_reassignments, max_total_copies, last_heartbeat_at,
			flagged_for_review, spot_check,
			target_copies, min_quorum, max_error_copies, max_success_copies
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11,
			$12, $13, $14, $15, $16,
			$17, $18, $19, $20,
			$21, $22,
			$23, $24, $25, $26
		) RETURNING `+workUnitColumns,
		wu.LeafID, wu.BatchID, wu.State, wu.Priority,
		wu.InputData, wu.InputDataRef, wu.CodeArtifactRef, wu.Parameters,
		wu.EstimatedDurationSeconds, wu.DeadlineSeconds, wu.OutputSpec,
		wu.AssignedVolunteerID, wu.AssignedAt, wu.StartedAt, wu.CompletedAt, wu.ValidatedAt,
		wu.ReassignmentCount, wu.MaxReassignments, wu.MaxTotalCopies, wu.LastHeartbeatAt,
		wu.FlaggedForReview, wu.SpotCheck,
		wu.TargetCopies, wu.MinQuorum, wu.MaxErrorCopies, wu.MaxSuccessCopies,
	)

	result, err := scanWorkUnit(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == "23503" {
				return apierror.Conflict(
					"referenced entity does not exist",
					map[string]string{"constraint": pgErr.ConstraintName},
				)
			}
		}
		return apierror.Internal("failed to create work unit", err)
	}
	*wu = *result
	return nil
}

// GetByID retrieves a work unit by its UUID.
func (r *PgxWorkUnitRepository) GetByID(ctx context.Context, id types.ID) (*WorkUnit, error) {
	row := r.db.QueryRow(ctx,
		"SELECT "+workUnitColumns+" FROM work_units WHERE id = $1", id)

	wu, err := scanWorkUnit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("work_unit", id.String())
		}
		return nil, apierror.Internal("failed to get work unit", err)
	}
	return wu, nil
}

// List retrieves work units with optional filters and cursor-based pagination.
// When filtering by state = QUEUED, uses priority ordering (CRITICAL > HIGH > NORMAL,
// then FIFO by created_at) to leverage the idx_work_units_queue partial index.
func (r *PgxWorkUnitRepository) List(ctx context.Context, filters WorkUnitListFilters, page types.PaginationRequest) ([]*WorkUnit, types.PaginationResponse, error) {
	pageSize := page.ClampPageSize()

	var conditions []string
	var args []any
	argIdx := 1

	if filters.LeafID != nil {
		conditions = append(conditions, fmt.Sprintf("leaf_id = $%d", argIdx))
		args = append(args, *filters.LeafID)
		argIdx++
	}
	if filters.BatchID != nil {
		conditions = append(conditions, fmt.Sprintf("batch_id = $%d", argIdx))
		args = append(args, *filters.BatchID)
		argIdx++
	}
	if filters.State != nil {
		conditions = append(conditions, fmt.Sprintf("state = $%d", argIdx))
		args = append(args, *filters.State)
		argIdx++
	}
	if filters.Priority != nil {
		conditions = append(conditions, fmt.Sprintf("priority = $%d", argIdx))
		args = append(args, *filters.Priority)
		argIdx++
	}
	if filters.AssignedVolunteerID != nil {
		conditions = append(conditions, fmt.Sprintf("assigned_volunteer_id = $%d", argIdx))
		args = append(args, *filters.AssignedVolunteerID)
		argIdx++
	}
	if filters.FlaggedForReview != nil {
		conditions = append(conditions, fmt.Sprintf("flagged_for_review = $%d", argIdx))
		args = append(args, *filters.FlaggedForReview)
		argIdx++
	}

	// Cursor-based pagination.
	if page.Cursor != "" {
		cursorTime, cursorID, err := types.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid cursor", nil)
		}
		conditions = append(conditions, fmt.Sprintf("(created_at, id) < ($%d, $%d)", argIdx, argIdx+1))
		args = append(args, cursorTime, cursorID)
		argIdx += 2
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	// When filtering by QUEUED state, use priority ordering to leverage the
	// partial index idx_work_units_queue. Priority enum ordering:
	// CRITICAL (highest) → HIGH → NORMAL (lowest), then FIFO by created_at.
	var orderClause string
	if filters.State != nil && *filters.State == WorkUnitStateQueued {
		orderClause = "ORDER BY priority DESC, created_at ASC, id ASC"
	} else {
		orderClause = "ORDER BY created_at DESC, id DESC"
	}

	query := fmt.Sprintf("SELECT %s FROM work_units %s %s LIMIT $%d",
		workUnitColumns, where, orderClause, argIdx)
	args = append(args, pageSize+1)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to list work units", err)
	}
	defer rows.Close()

	var workUnits []*WorkUnit
	for rows.Next() {
		wu, err := scanWorkUnit(rows)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.Internal("failed to scan work unit", err)
		}
		workUnits = append(workUnits, wu)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to iterate work units", err)
	}

	pagination := types.PaginationResponse{}
	if len(workUnits) > pageSize {
		workUnits = workUnits[:pageSize]
		last := workUnits[pageSize-1]
		pagination.HasMore = true
		pagination.NextCursor = types.EncodeCursor(last.CreatedAt, last.ID)
	}

	return workUnits, pagination, nil
}

// UpdateState transitions a work unit from one state to another.
// Uses ValidateTransition to check legality. For REJECTED/EXPIRED → QUEUED,
// applies TransitionToQueued business rules. For → FAILED, applies TransitionToFailed.
// Returns apierror.Conflict if the current DB state doesn't match `from`.
func (r *PgxWorkUnitRepository) UpdateState(ctx context.Context, id types.ID, from, to WorkUnitState) (*WorkUnit, error) {
	if err := ValidateTransition(from, to); err != nil {
		return nil, err
	}

	// Read current state from DB to verify it matches `from`.
	wu, err := r.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if wu.State != from {
		return nil, apierror.Conflict(
			fmt.Sprintf("work unit state is %s, expected %s", wu.State, from),
			map[string]string{
				"code":     "STATE_MISMATCH",
				"actual":   string(wu.State),
				"expected": string(from),
			},
		)
	}

	// Apply business rules for special transitions.
	if to == WorkUnitStateQueued && (from == WorkUnitStateRejected || from == WorkUnitStateExpired) {
		if err := TransitionToQueued(wu); err != nil {
			return nil, err
		}
	} else if to == WorkUnitStateFailed {
		TransitionToFailed(wu)
	} else if to == WorkUnitStateValidated {
		TransitionToValidated(wu)
	} else {
		wu.State = to
	}

	// Persist state and all business-rule-modified fields with optimistic concurrency
	// on the `from` state to prevent concurrent transitions.
	tag, err := r.db.Exec(ctx, `
		UPDATE work_units SET
			state = $2,
			priority = $3,
			assigned_volunteer_id = $4,
			assigned_at = $5,
			started_at = $6,
			completed_at = $7,
			validated_at = $8,
			last_heartbeat_at = $9,
			reassignment_count = $10,
			flagged_for_review = $11,
			-- Layer 3: clear any dispatch claim on EVERY state transition (defensive /
			-- self-healing). EXPIRED/REJECTED -> QUEUED reassignment routes through here
			-- (Reassign -> UpdateState), so the requeue path is claim-clean: a re-QUEUED
			-- unit is always immediately re-claimable. Per-copy run-start (Assign) keeps
			-- the unit QUEUED and does NOT touch the claim, so terminal transitions
			-- (COMPLETED/VALIDATED/FAILED) here are what release a finished unit's claim.
			dispatch_claimed_by = NULL,
			dispatch_claim_expires_at = NULL
		WHERE id = $1 AND state = $12`,
		id,
		wu.State,
		wu.Priority,
		wu.AssignedVolunteerID,
		wu.AssignedAt,
		wu.StartedAt,
		wu.CompletedAt,
		wu.ValidatedAt,
		wu.LastHeartbeatAt,
		wu.ReassignmentCount,
		wu.FlaggedForReview,
		from,
	)
	if err != nil {
		return nil, apierror.Internal("failed to update work unit state", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, apierror.Conflict(
			"work unit state changed concurrently",
			map[string]string{"code": "CONCURRENT_STATE_CHANGE"},
		)
	}

	// Re-read to get updated_at from trigger.
	return r.GetByID(ctx, id)
}

// BulkCreate inserts multiple work units efficiently using pgx.CopyFrom. Each input
// struct is stamped (in order) with a client-generated ID — pgx.CopyFrom returns no
// DB-generated values, so the IDs are produced here (the work_units.id column otherwise
// defaults to gen_random_uuid()) so callers can report the created work-unit IDs. Any
// ID already set on an input is preserved. DB-generated timestamps (created_at/updated_at)
// are NOT back-filled onto the structs — re-read with GetByID/List if you need them.
func (r *PgxWorkUnitRepository) BulkCreate(ctx context.Context, wus []*WorkUnit) error {
	if len(wus) == 0 {
		return nil
	}

	columns := []string{
		"id",
		"leaf_id", "batch_id", "state", "priority",
		"input_data", "input_data_ref", "code_artifact_ref", "parameters",
		"estimated_duration_seconds", "deadline_seconds", "output_spec",
		"assigned_volunteer_id", "assigned_at", "started_at", "completed_at", "validated_at",
		"reassignment_count", "max_reassignments", "max_total_copies", "last_heartbeat_at",
		"flagged_for_review", "spot_check",
		"target_copies", "min_quorum", "max_error_copies", "max_success_copies",
	}

	rows := make([][]any, len(wus))
	for i, wu := range wus {
		if wu.ID == types.NilID() {
			wu.ID = types.NewID()
		}
		rows[i] = []any{
			wu.ID,
			wu.LeafID, wu.BatchID, wu.State, wu.Priority,
			wu.InputData, wu.InputDataRef, wu.CodeArtifactRef, wu.Parameters,
			wu.EstimatedDurationSeconds, wu.DeadlineSeconds, wu.OutputSpec,
			wu.AssignedVolunteerID, wu.AssignedAt, wu.StartedAt, wu.CompletedAt, wu.ValidatedAt,
			wu.ReassignmentCount, wu.MaxReassignments, wu.MaxTotalCopies, wu.LastHeartbeatAt,
			wu.FlaggedForReview, wu.SpotCheck,
			wu.TargetCopies, wu.MinQuorum, wu.MaxErrorCopies, wu.MaxSuccessCopies,
		}
	}

	copyCount, err := r.db.CopyFrom(
		ctx,
		pgx.Identifier{"work_units"},
		columns,
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return apierror.Conflict(
				"referenced entity does not exist",
				map[string]string{"constraint": pgErr.ConstraintName},
			)
		}
		return apierror.Internal("failed to bulk create work units", err)
	}

	if int(copyCount) != len(wus) {
		return apierror.Internal(
			fmt.Sprintf("bulk create: expected %d rows, inserted %d", len(wus), copyCount),
			nil,
		)
	}

	return nil
}

// BulkTransitionByBatch transitions all work units in a batch from one state to another
// in a single UPDATE. Returns the number of rows affected.
func (r *PgxWorkUnitRepository) BulkTransitionByBatch(ctx context.Context, batchID types.ID, from, to WorkUnitState) (int64, error) {
	if err := ValidateTransition(from, to); err != nil {
		return 0, err
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE work_units SET state = $1 WHERE batch_id = $2 AND state = $3`,
		to, batchID, from,
	)
	if err != nil {
		return 0, apierror.Internal("failed to bulk transition work units", err)
	}
	return tag.RowsAffected(), nil
}

// prefixedWorkUnitColumns is workUnitColumns with a wu. table alias prefix.
const prefixedWorkUnitColumns = `wu.id, wu.leaf_id, wu.batch_id, wu.state, wu.priority,
	wu.input_data, wu.input_data_ref, wu.code_artifact_ref, wu.parameters,
	wu.estimated_duration_seconds, wu.deadline_seconds, wu.output_spec,
	wu.assigned_volunteer_id, wu.assigned_at, wu.started_at, wu.completed_at, wu.validated_at,
	wu.reassignment_count, wu.max_reassignments, wu.max_total_copies, wu.last_heartbeat_at,
	wu.flagged_for_review, wu.spot_check, wu.last_checkpoint_at, wu.last_checkpoint_sequence,
	wu.created_at, wu.updated_at, wu.hr_class,
	wu.target_copies, wu.min_quorum, wu.max_error_copies, wu.max_success_copies`

// scanDispatchCandidate scans the prefixedWorkUnitColumns set (column order matching
// the const above) into a WorkUnit. Shared by FindDispatchableBatch /
// ClaimDispatchableBatch which append three extra trailing columns.
func scanDispatchWorkUnit(rows pgx.Rows, wu *WorkUnit, extra ...any) error {
	dst := []any{
		&wu.ID, &wu.LeafID, &wu.BatchID, &wu.State, &wu.Priority,
		&wu.InputData, &wu.InputDataRef, &wu.CodeArtifactRef, &wu.Parameters,
		&wu.EstimatedDurationSeconds, &wu.DeadlineSeconds, &wu.OutputSpec,
		&wu.AssignedVolunteerID, &wu.AssignedAt, &wu.StartedAt, &wu.CompletedAt, &wu.ValidatedAt,
		&wu.ReassignmentCount, &wu.MaxReassignments, &wu.MaxTotalCopies, &wu.LastHeartbeatAt,
		&wu.FlaggedForReview, &wu.SpotCheck, &wu.LastCheckpointAt, &wu.LastCheckpointSequence,
		&wu.CreatedAt, &wu.UpdatedAt, &wu.HRClass,
		&wu.TargetCopies, &wu.MinQuorum, &wu.MaxErrorCopies, &wu.MaxSuccessCopies,
	}
	dst = append(dst, extra...)
	return rows.Scan(dst...)
}

// FindNextAssignable finds the highest-priority QUEUED work unit from active projects
// that matches the volunteer's capabilities and has fewer active assignments than
// the project's redundancy_factor. Uses FOR UPDATE SKIP LOCKED to prevent
// concurrent assignment of the same work unit. Returns nil, nil if no work available.
func (r *PgxWorkUnitRepository) FindNextAssignable(ctx context.Context, opts AssignmentOptions) (*WorkUnit, error) {
	row := r.db.QueryRow(ctx, `
		SELECT `+prefixedWorkUnitColumns+`
		FROM work_units wu
		JOIN leafs l ON wu.leaf_id = l.id
		WHERE wu.state = 'QUEUED'
		  AND l.state = 'ACTIVE'
		  AND (array_length($1::uuid[], 1) IS NULL OR wu.leaf_id = ANY($1))
		  AND (array_length($2::uuid[], 1) IS NULL OR NOT wu.leaf_id = ANY($2))
		  AND COALESCE((l.resource_requirements->>'min_cpu_cores')::int, 0) <= $3
		  -- Memory matches on the container limit (execution_config.max_memory_mb),
		  -- the single source of truth: the volunteer's budget ($4) must cover the
		  -- cap, identical to the client's canAccommodateWU admission check. Matching
		  -- on a separate min_memory_mb let the two drift (matched-but-can't-run).
		  AND COALESCE((l.execution_config->>'max_memory_mb')::int, 0) <= $4
		  AND COALESCE((l.resource_requirements->>'min_disk_mb')::bigint, 0) <= $5
		  -- GPU presence: a leaf needs a GPU if EITHER gpu_required flag is set
		  -- (execution_config.gpu_required is the natural author-set flag;
		  -- resource_requirements.gpu_required is the parallel matching field). The two were
		  -- historically unsynced, so gating presence on resource_requirements alone let a
		  -- leaf that set only the execution_config flag reach GPU-less volunteers (#30).
		  AND (
		    (NOT COALESCE((l.resource_requirements->>'gpu_required')::boolean, false)
		     AND NOT COALESCE((l.execution_config->>'gpu_required')::boolean, false))
		    OR ($6::boolean AND COALESCE((l.resource_requirements->>'min_gpu_vram_mb')::int, 0) <= $7)
		  )
		  AND (l.execution_config->>'runtime') = ANY($8::text[])
		  AND (
		    NOT COALESCE((l.execution_config->>'gpu_required')::boolean, false)
		    OR COALESCE(l.execution_config->>'gpu_type', 'ANY') = 'ANY'
		    OR UPPER(COALESCE(l.execution_config->>'gpu_type', 'ANY')) = ANY($10::text[])
		  )
		  AND (
		    NOT COALESCE((l.resource_requirements->>'gpu_required')::boolean, false)
		    OR (l.resource_requirements->>'gpu_compute_capability') IS NULL
		    OR (l.resource_requirements->>'gpu_compute_capability') = ANY($11::text[])
		  )
		  -- Per-copy redundancy (migration 00006): a unit is dispatchable while the
		  -- copies already covering its redundancy need are below the leaf's effective
		  -- redundancy. Coverage = live copies (RESERVED/RUNNING history rows, outcome
		  -- IS NULL) + already-submitted PENDING results (a copy that finished closed
		  -- its history row and holds its slot via the result). Each live copy and each
		  -- result is a DISTINCT volunteer (uq_wuah_live_copy_per_volunteer +
		  -- uq_results_work_unit_volunteer), so up to N copies of one unit are dispatched
		  -- IN PARALLEL to N different volunteers. The two terms never overlap (a
		  -- completed copy is closed, no longer outcome IS NULL).
		  AND (
		    (
		      SELECT COUNT(*) FROM work_unit_assignment_history wuah
		      WHERE wuah.work_unit_id = wu.id AND wuah.outcome IS NULL
		    )
		    + (
		      SELECT COUNT(*) FROM results res
		      WHERE res.work_unit_id = wu.id AND res.validation_status = 'PENDING'
		    )
		  ) < `+effTargetWuL+`
		  -- Hard distinctness: never hand this volunteer a unit it already holds a LIVE
		  -- copy of (no two concurrent copies to one volunteer).
		  AND NOT EXISTS (
		    SELECT 1 FROM work_unit_assignment_history wuah2
		    WHERE wuah2.work_unit_id = wu.id
		      AND wuah2.volunteer_id = $9
		      AND wuah2.outcome IS NULL
		  )
		  -- Never hand this volunteer a unit it already produced a result for, so each
		  -- of the N redundant results comes from a DISTINCT volunteer.
		  AND NOT EXISTS (
		    SELECT 1 FROM results res3
		    WHERE res3.work_unit_id = wu.id
		      AND res3.volunteer_id = $9
		      AND res3.validation_status = 'PENDING'
		  )
		  -- Prefer-distinct on requeue (property 6): a volunteer whose recent copy of
		  -- this unit TIMED OUT or was abandoned mid-run is benched for roughly one more
		  -- deadline so a fresh volunteer gets first refusal; after that cooldown it is
		  -- eligible again, so a small volunteer pool never strands the work. The cooldown
		  -- is a RELIABILITY signal, so it benches only a copy the volunteer actually
		  -- STARTED (started_at set): a graceful return of UN-STARTED buffered work
		  -- (AbandonWorkUnit on a never-run copy, or the buffer reconciler reaping a dropped
		  -- prefetch — both close it ABANDONED with started_at NULL) is not unreliable and
		  -- must not bench, else a dominated pool strands the work it returned (#59).
		  AND NOT EXISTS (
		    SELECT 1 FROM work_unit_assignment_history wuah4
		    WHERE wuah4.work_unit_id = wu.id
		      AND wuah4.volunteer_id = $9
		      AND (wuah4.outcome = 'EXPIRED'
		           OR (wuah4.outcome = 'ABANDONED' AND wuah4.started_at IS NOT NULL))
		      AND wuah4.outcome_at IS NOT NULL
		      AND wuah4.outcome_at > NOW() - GREATEST(wu.deadline_seconds, 1) * INTERVAL '1 second'
		  )
		  -- Per-MACHINE inflight cap (TODO #19): this HOST's live copies across all units.
		  -- Keyed on COALESCE(host_id, volunteer_id) = the requester's effective host id
		  -- ($14, the account id when no host was reported) so a user's rig and laptop have
		  -- independent budgets. This is DELIBERATELY separate from the per-account
		  -- distinctness/redundancy predicates above (which stay keyed on volunteer_id $9):
		  -- distinctness is per-account, the in-flight cap is per-machine.
		  AND (
		    $12::int <= 0
		    OR (SELECT COUNT(*) FROM work_unit_assignment_history wuah5
		        WHERE COALESCE(wuah5.host_id, wuah5.volunteer_id) = COALESCE($14::uuid, $9)
		          AND wuah5.outcome IS NULL) < $12
		  )
		  -- Homogeneous Redundancy: a unit already pinned to a hardware class only goes to
		  -- volunteers of that SAME class — including a requester that reports NO class
		  -- (an unknown-class machine is not the pinned class, and its results would not
		  -- be bit-comparable with the pinned cohort's). Unpinned units (hr_class IS NULL
		  -- or '', incl. all non-HR leafs) are unconstrained. This clause must mirror
		  -- eligibleLocked's rejectHRClassMismatch (dispatch_cache.go) exactly; the
		  -- dispatch-predicate parity suite (internal/dispatchparity) pins the agreement.
		  AND (wu.hr_class IS NULL OR wu.hr_class = '' OR wu.hr_class = $13)
		  -- Feasibility-at-dispatch: exclude a unit this host's measured benchmark ($15,
		  -- FP-ops/sec) says it cannot finish before its deadline. Skipped (no exclusion)
		  -- when any input is unknown -- benchmark, leaf rsc_fpops_est, or deadline <= 0 --
		  -- so an un-benchmarked host or un-estimated leaf is never refused on a guess.
		  -- Mirrors workunit.FeasibleByDeadline and the FlushReservations/ReserveCopy gates.
		  AND (
		    $15::float8 <= 0
		    OR COALESCE((l.execution_config->>'rsc_fpops_est')::float8, 0) <= 0
		    OR wu.deadline_seconds <= 0
		    OR (COALESCE((l.execution_config->>'rsc_fpops_est')::float8, 0)
		        / NULLIF($15::float8, 0)) <= wu.deadline_seconds
		  )
		ORDER BY wu.priority DESC, wu.created_at ASC
		LIMIT 1
		FOR UPDATE OF wu SKIP LOCKED`,
		opts.LeafIDs,
		opts.BlockedLeafIDs,
		opts.MaxCPUCores,
		opts.MaxMemoryMB,
		opts.MaxDiskMB,
		opts.HasGPU,
		opts.MaxGPUVRAMMB,
		opts.AvailableRuntimes,
		opts.VolunteerID,
		opts.GPUVendors,
		opts.GPUComputeCapabilities,
		opts.MaxInflightPerVolunteer,
		opts.HRClass,
		opts.HostID,
		opts.BenchmarkFPOPS,
	)

	wu, err := scanWorkUnit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, apierror.Internal("failed to find assignable work unit", err)
	}
	return wu, nil
}

// DispatchCandidate is one stageable QUEUED unit returned by
// FindDispatchableBatch, carrying everything the in-memory dispatch cache needs
// to hand it out and build the proto assignment without any further DB read.
type DispatchCandidate struct {
	WorkUnit *WorkUnit
	// LeafID is wu.LeafID, duplicated for the cache's per-leaf metadata lookup.
	LeafID types.ID
	// RedundancyFactor is the leaf's effective redundancy (validation_config), the
	// cap the cache enforces on in-memory hand-outs of this unit.
	RedundancyFactor int
	// ActiveAssignments is the unit's CURRENT active-history-row count (outcome IS
	// NULL) at refill time. The cache seeds its per-unit redundancy headroom from
	// this authoritative DB count so a unit already partially dispatched (e.g. a
	// redundancy>1 unit with one running holder) is staged with the correct
	// remaining headroom.
	ActiveAssignments int
	// Runtime is the leaf's execution_config.runtime (used to assert the WASM
	// partition and for capability matching at hand-out).
	Runtime string
	// ContributorVolunteerIDs is the set of volunteers that ALREADY count toward
	// this unit's redundancy at refill time: every holder of a live copy (history
	// row, outcome IS NULL) plus every author of an already-submitted PENDING
	// result. Each of the unit's redundant results must come from a DISTINCT
	// volunteer, so the hand-out path excludes any volunteer in this set — the
	// in-memory mirror of the volunteer-specific distinctness FindNextAssignable
	// enforces in SQL but FindDispatchableBatch (volunteer-agnostic) cannot.
	ContributorVolunteerIDs []types.ID
	// BenchedVolunteerIDs is the set of volunteers whose recent copy of this unit
	// TIMED OUT or was ABANDONED within roughly one deadline window. They are
	// benched (given last refusal) so a fresh volunteer gets first crack on a
	// requeue; after the cooldown elapses they are eligible again, so a small
	// volunteer pool never strands the work.
	BenchedVolunteerIDs []types.ID
}

// FindDispatchableBatch bulk-selects up to `limit` QUEUED, dispatch-eligible work
// units for the in-memory dispatch cache to refill from. It is the LIMIT-N,
// volunteer-AGNOSTIC counterpart of FindNextAssignable: the global no-double-hand
// and redundancy/reservation guards are kept in SQL, but the per-requester
// predicates (capability fit, blocked-leaf, self-exclusion, the per-volunteer
// inflight cap) are dropped — those are re-checked in memory at hand-out against
// each requester.
//
// Differences from FindNextAssignable, all deliberate:
//   - LIMIT $1 (the refill batch), not LIMIT 1.
//   - No specific requester: the redundancy term counts active history rows + any
//     live NORMAL reservation (held by anyone); the reservation guard hides any
//     live-reserved NORMAL unit. A redundancy>1 unit with one live reservation but
//     unmet redundancy is still staged (its remaining headroom is carried out in
//     DispatchCandidate.ActiveAssignments + the cache's reservation accounting).
//   - excludeIDs (the cache's in-memory-reserved id set) are excluded via
//     NOT wu.id = ANY($2): a DB-level backstop so two refill ticks cannot re-stage
//     a unit the cache already handed out but has not yet flushed.
//   - WASM-runtime leafs are excluded: those are dispatched by the separate
//     immediate-assign browser path, partitioned from the cache by runtime so there
//     is exactly one writer per unit.
//   - FOR UPDATE OF wu SKIP LOCKED is KEPT (short-lived for a bulk read), the proven
//     no-double-hand primitive; the refill writes nothing, so the lock is released
//     at the end of this SELECT's transaction.
func (r *PgxWorkUnitRepository) FindDispatchableBatch(ctx context.Context, limit int, excludeIDs []types.ID, leafIDs []types.ID) ([]DispatchCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+prefixedWorkUnitColumns+`,
			`+effTargetWuL+` AS effective_redundancy,
			(
				(SELECT COUNT(*) FROM work_unit_assignment_history wuah
				 WHERE wuah.work_unit_id = wu.id AND wuah.outcome IS NULL)
				+ (SELECT COUNT(*) FROM results res2
				   WHERE res2.work_unit_id = wu.id AND res2.validation_status = 'PENDING')
			) AS active_assignments,
			COALESCE(l.execution_config->>'runtime', 'NATIVE') AS runtime,
			-- Volunteers already counting toward redundancy (distinct): live-copy holders
			-- + PENDING-result authors. The hand-out excludes these so each redundant
			-- result comes from a DISTINCT volunteer (the volunteer-specific guard the
			-- volunteer-agnostic refill cannot express, re-checked in memory).
			ARRAY(
				SELECT DISTINCT v::text FROM (
					SELECT wuah_c.volunteer_id AS v FROM work_unit_assignment_history wuah_c
					 WHERE wuah_c.work_unit_id = wu.id AND wuah_c.outcome IS NULL AND wuah_c.volunteer_id IS NOT NULL
					UNION
					SELECT res_c.volunteer_id AS v FROM results res_c
					 WHERE res_c.work_unit_id = wu.id AND res_c.validation_status = 'PENDING'
				) contribs
			) AS contributor_ids,
			-- Volunteers benched by a recent timeout / mid-run abandon of this unit
			-- (~one deadline cooldown). Only a STARTED copy benches; a graceful return of
			-- UN-STARTED buffered work (ABANDONED, started_at NULL) is not unreliable (#59).
			ARRAY(
				SELECT DISTINCT wuah_b.volunteer_id::text FROM work_unit_assignment_history wuah_b
				 WHERE wuah_b.work_unit_id = wu.id AND wuah_b.volunteer_id IS NOT NULL
				   AND (wuah_b.outcome = 'EXPIRED'
				        OR (wuah_b.outcome = 'ABANDONED' AND wuah_b.started_at IS NOT NULL))
				   AND wuah_b.outcome_at IS NOT NULL
				   AND wuah_b.outcome_at > NOW() - GREATEST(wu.deadline_seconds, 1) * INTERVAL '1 second'
			) AS benched_ids
		FROM work_units wu
		JOIN leafs l ON wu.leaf_id = l.id
		WHERE wu.state = 'QUEUED'
		  AND l.state = 'ACTIVE'
		  -- WASM is dispatched by the immediate-assign browser path, not the cache.
		  AND COALESCE(l.execution_config->>'runtime', 'NATIVE') <> 'WASM'
		  -- DB-level backstop: never re-stage a unit the cache already holds in memory.
		  -- Guard the NULL/empty exclude set explicitly: id = ANY(NULL::uuid[]) is NULL
		  -- (not FALSE), so a bare NOT (id = ANY($2)) would filter out EVERY row whenever
		  -- excludeIDs is nil (e.g. a cold-cache refill) — the array-length guard makes an
		  -- empty/absent exclude set a no-op instead.
		  AND (array_length($2::uuid[], 1) IS NULL OR NOT (wu.id = ANY($2::uuid[])))
		  -- Optional leaf scope (the on-demand leaf-scoped refill): when $3 is empty the
		  -- select spans all ACTIVE non-WASM leafs; otherwise it is confined to those
		  -- leafs so a leaf-filtered requester can be served even when the ready pool is
		  -- monopolized by a higher-priority/older leaf.
		  AND (array_length($3::uuid[], 1) IS NULL OR wu.leaf_id = ANY($3::uuid[]))
		  -- Per-copy redundancy (volunteer-agnostic at refill): live copies (history
		  -- rows with outcome IS NULL) + already-submitted PENDING results must be below
		  -- the leaf's effective redundancy. A unit with one live copy but unmet
		  -- redundancy stays stageable so a SECOND distinct volunteer gets a parallel
		  -- copy; the per-requester distinctness is re-checked in memory at hand-out.
		  AND (
		    (
		      SELECT COUNT(*) FROM work_unit_assignment_history wuah
		      WHERE wuah.work_unit_id = wu.id AND wuah.outcome IS NULL
		    )
		    + (
		      SELECT COUNT(*) FROM results res
		      WHERE res.work_unit_id = wu.id AND res.validation_status = 'PENDING'
		    )
		  ) < `+effTargetWuL+`
		ORDER BY wu.priority DESC, wu.created_at ASC
		LIMIT $1
		FOR UPDATE OF wu SKIP LOCKED`,
		limit, excludeIDs, leafIDs,
	)
	if err != nil {
		return nil, apierror.Internal("failed to find dispatchable batch", err)
	}
	defer rows.Close()

	var out []DispatchCandidate
	for rows.Next() {
		var wu WorkUnit
		var redundancy, active int
		var runtime string
		var contributorTexts, benchedTexts []string
		if err := scanDispatchWorkUnit(rows, &wu, &redundancy, &active, &runtime, &contributorTexts, &benchedTexts); err != nil {
			return nil, apierror.Internal("failed to scan dispatchable work unit", err)
		}
		cand := wu
		out = append(out, DispatchCandidate{
			WorkUnit:                &cand,
			LeafID:                  wu.LeafID,
			RedundancyFactor:        redundancy,
			ActiveAssignments:       active,
			Runtime:                 runtime,
			ContributorVolunteerIDs: parseIDTexts(contributorTexts),
			BenchedVolunteerIDs:     parseIDTexts(benchedTexts),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate dispatchable work units", err)
	}
	return out, nil
}

// parseIDTexts converts a slice of uuid text values (as scanned from a Postgres
// text[] column) into types.ID, skipping any that fail to parse. Defensive: a
// malformed id is dropped rather than failing the whole refill.
func parseIDTexts(texts []string) []types.ID {
	if len(texts) == 0 {
		return nil
	}
	ids := make([]types.ID, 0, len(texts))
	for _, t := range texts {
		if id, err := types.ParseID(t); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// ClaimDispatchableBatch is the horizontal-scale-out (Layer 3) claim-on-refill
// counterpart of FindDispatchableBatch: it selects the same LIMIT-N batch of
// QUEUED, dispatch-eligible units but ATOMICALLY stamps a per-head dispatch claim
// on each one (dispatch_claimed_by = headID, dispatch_claim_expires_at = NOW() +
// lease) in a single UPDATE...RETURNING. The unit stays state='QUEUED' — the claim
// is a SECOND, head-owned lease distinct from the volunteer reservation (00002) —
// but it is now DB-owned by headID, so every OTHER replica's refill excludes it
// (its claim is live and owned by another head) and cannot stage the same unit. A
// unit staged in this head's in-memory ready pool is therefore invisible to every
// other replica, closing the cross-replica double-hand.
//
// The claim cost is amortized at bulk-refill (one UPDATE per LIMIT-N refill), so
// the per-request HandOut hot path stays 100% in memory — the Layer-2 win is
// preserved. A held unit's claim is renewed off the hot path by the async
// reservation flush (FlushReservations bumps dispatch_claim_expires_at). A crashed
// replica's claims simply expire and become re-claimable by any survivor.
//
// The claim WHERE-term added to the FindDispatchableBatch predicate set:
//   - dispatch_claimed_by IS NULL  → never claimed: claimable.
//   - dispatch_claim_expires_at < NOW()  → claim expired (e.g. a crashed owner that
//     stopped renewing): re-claimable.
//   - dispatch_claimed_by = headID  → this head's OWN claim (re-stage / renew on a
//     subsequent refill tick): re-claimable.
//   - otherwise (another head's LIVE claim): excluded.
//
// FOR UPDATE OF wu SKIP LOCKED is kept on the inner select (the proven
// no-double-hand primitive); the outer UPDATE makes the claim atomic with that
// select so two replicas cannot both stamp the same row.
func (r *PgxWorkUnitRepository) ClaimDispatchableBatch(ctx context.Context, headID types.ID, lease time.Duration, limit int, excludeIDs []types.ID, leafIDs []types.ID) ([]DispatchCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	leaseSecs := lease.Seconds()
	if leaseSecs <= 0 {
		leaseSecs = float64(defaultClaimLeaseSeconds)
	}
	rows, err := r.db.Query(ctx, `
		UPDATE work_units AS wu SET
			dispatch_claimed_by = $4,
			dispatch_claim_expires_at = NOW() + make_interval(secs => $5)
		FROM leafs l
		WHERE wu.id IN (
			SELECT wu2.id
			FROM work_units wu2
			JOIN leafs l2 ON wu2.leaf_id = l2.id
			WHERE wu2.state = 'QUEUED'
			  AND l2.state = 'ACTIVE'
			  -- WASM is dispatched by the immediate-assign browser path, not the cache.
			  AND COALESCE(l2.execution_config->>'runtime', 'NATIVE') <> 'WASM'
			  -- DB-level backstop: never re-stage a unit the cache already holds in memory.
			  AND (array_length($2::uuid[], 1) IS NULL OR NOT (wu2.id = ANY($2::uuid[])))
			  -- Optional leaf scope (the on-demand leaf-scoped refill).
			  AND (array_length($3::uuid[], 1) IS NULL OR wu2.leaf_id = ANY($3::uuid[]))
			  -- CLAIM EXCLUDE: another replica's LIVE claim hides the unit; a NULL claim,
			  -- an expired claim, or THIS head's own claim is re-claimable.
			  AND (wu2.dispatch_claimed_by IS NULL
			       OR wu2.dispatch_claim_expires_at < NOW()
			       OR wu2.dispatch_claimed_by = $4)
			  -- Per-copy redundancy: live copies (history rows, outcome IS NULL) +
			  -- already-submitted PENDING results below the leaf's effective redundancy.
			  AND (
			    (
			      SELECT COUNT(*) FROM work_unit_assignment_history wuah
			      WHERE wuah.work_unit_id = wu2.id AND wuah.outcome IS NULL
			    )
			    + (
			      SELECT COUNT(*) FROM results res
			      WHERE res.work_unit_id = wu2.id AND res.validation_status = 'PENDING'
			    )
			  ) < `+effTargetSQL("wu2", "l2")+`
			ORDER BY wu2.priority DESC, wu2.created_at ASC
			LIMIT $1
			FOR UPDATE OF wu2 SKIP LOCKED
		)
		  AND l.id = wu.leaf_id
		RETURNING `+prefixedWorkUnitColumns+`,
			`+effTargetWuL+` AS effective_redundancy,
			(
				(SELECT COUNT(*) FROM work_unit_assignment_history wuah
				 WHERE wuah.work_unit_id = wu.id AND wuah.outcome IS NULL)
				+ (SELECT COUNT(*) FROM results res2
				   WHERE res2.work_unit_id = wu.id AND res2.validation_status = 'PENDING')
			) AS active_assignments,
			COALESCE(l.execution_config->>'runtime', 'NATIVE') AS runtime,
			-- Distinct volunteers already covering redundancy (live copies + PENDING
			-- results): excluded at hand-out so each result comes from a distinct volunteer.
			ARRAY(
				SELECT DISTINCT v::text FROM (
					SELECT wuah_c.volunteer_id AS v FROM work_unit_assignment_history wuah_c
					 WHERE wuah_c.work_unit_id = wu.id AND wuah_c.outcome IS NULL AND wuah_c.volunteer_id IS NOT NULL
					UNION
					SELECT res_c.volunteer_id AS v FROM results res_c
					 WHERE res_c.work_unit_id = wu.id AND res_c.validation_status = 'PENDING'
				) contribs
			) AS contributor_ids,
			-- Volunteers benched by a recent timeout / mid-run abandon of this unit (cooldown
			-- ~1 deadline). Only a STARTED copy benches; a graceful return of un-started
			-- buffered work (ABANDONED, started_at NULL) is not unreliable (#59).
			ARRAY(
				SELECT DISTINCT wuah_b.volunteer_id::text FROM work_unit_assignment_history wuah_b
				 WHERE wuah_b.work_unit_id = wu.id AND wuah_b.volunteer_id IS NOT NULL
				   AND (wuah_b.outcome = 'EXPIRED'
				        OR (wuah_b.outcome = 'ABANDONED' AND wuah_b.started_at IS NOT NULL))
				   AND wuah_b.outcome_at IS NOT NULL
				   AND wuah_b.outcome_at > NOW() - GREATEST(wu.deadline_seconds, 1) * INTERVAL '1 second'
			) AS benched_ids`,
		limit, excludeIDs, leafIDs, headID, leaseSecs,
	)
	if err != nil {
		return nil, apierror.Internal("failed to claim dispatchable batch", err)
	}
	defer rows.Close()

	var out []DispatchCandidate
	for rows.Next() {
		var wu WorkUnit
		var redundancy, active int
		var runtime string
		var contributorTexts, benchedTexts []string
		if err := scanDispatchWorkUnit(rows, &wu, &redundancy, &active, &runtime, &contributorTexts, &benchedTexts); err != nil {
			return nil, apierror.Internal("failed to scan claimed work unit", err)
		}
		cand := wu
		out = append(out, DispatchCandidate{
			WorkUnit:                &cand,
			LeafID:                  wu.LeafID,
			RedundancyFactor:        redundancy,
			ActiveAssignments:       active,
			Runtime:                 runtime,
			ContributorVolunteerIDs: parseIDTexts(contributorTexts),
			BenchedVolunteerIDs:     parseIDTexts(benchedTexts),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate claimed work units", err)
	}
	return out, nil
}

// ClearExpiredDispatchClaims NULLs the dispatch-claim columns on every unit whose
// claim has expired (dispatch_claim_expires_at < NOW()). This is HYGIENE ONLY: a
// unit with an expired claim is ALREADY re-claimable by any replica's refill (the
// claim WHERE-term treats an expired claim as claimable), so reclaim does not depend
// on this sweep — it keeps the table tidy and observability clean. Run from the
// leader-gated fault monitor. Returns the number of rows cleared.
func (r *PgxWorkUnitRepository) ClearExpiredDispatchClaims(ctx context.Context) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE work_units SET
			dispatch_claimed_by = NULL,
			dispatch_claim_expires_at = NULL
		WHERE dispatch_claimed_by IS NOT NULL
		  AND dispatch_claim_expires_at < NOW()`)
	if err != nil {
		return 0, apierror.Internal("failed to clear expired dispatch claims", err)
	}
	return tag.RowsAffected(), nil
}

// FlushReservation is one async copy-reservation write produced by the dispatch
// cache: it materializes an in-memory hold as a RESERVED copy row (a
// work_unit_assignment_history row with reserved_until set, started_at NULL,
// outcome NULL).
type FlushReservation struct {
	WorkUnitID  types.ID
	VolunteerID types.ID
	// HostID attributes the copy to the requesting machine (TODO #19); nil = no host
	// reported (the copy row's host_id is left NULL → it counts under the account).
	HostID          *types.ID
	ReservedUntil   time.Time
	DeadlineSeconds int
}

// FlushedCopy identifies a copy reservation that actually landed in the DB, so the
// cache can void exactly the (unit, volunteer) holds whose copy did NOT persist.
type FlushedCopy struct {
	WorkUnitID  types.ID
	VolunteerID types.ID
}

// FlushReservations writes a batch of dispatch-cache reservations in ONE multi-row
// UPDATE, using the SAME optimistic guard ReserveNextAssignable uses per-row: a
// reservation lands only if the unit is still QUEUED and either unreserved, lease-
// lapsed, or already reserved to this same volunteer. The set of work_unit_ids that
// actually landed is returned (via RETURNING); any id NOT in the returned set is a
// conflict — the cache voids that in-memory hand-out (decrementing its headroom /
// inflight counters) so a reservation it could not persist is never counted.
//
// This is the head's single batched reservation-write path that takes Postgres off
// the RequestWorkUnit hot path: hand-out is authoritative in memory immediately and
// the flush lags by at most flushInterval / flushBatchSize.
//
// Layer 3 (claim renewal): the SAME statement that lands the reservation also
// RENEWS this head's dispatch claim on the unit (dispatch_claim_expires_at = NOW()
// + claimLease) whenever the unit is still claimed by headID. This is the
// deterministic close of the unflushed-reservation race: a unit that lives in this
// replica's ready pool or in-flight has its claim continuously renewed off the hot
// path (piggybacked on the existing flush batch, ZERO new per-request DB write), so
// a held-but-unflushed unit's claim never expires under it. The renewal is gated on
// dispatch_claimed_by = headID so it can NEVER void or hijack another replica's
// legitimate claim/flush (gotcha 1). headID == uuid.Nil disables renewal
// (single-replica / pre-Layer-3 callers): the COALESCE keeps the existing claim.
func (r *PgxWorkUnitRepository) FlushReservations(ctx context.Context, recs []FlushReservation, headID types.ID, claimLease time.Duration) ([]FlushedCopy, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	ids := make([]types.ID, len(recs))
	vols := make([]types.ID, len(recs))
	hosts := make([]*types.ID, len(recs))
	untils := make([]time.Time, len(recs))
	deadlines := make([]int, len(recs))
	for i, rec := range recs {
		ids[i] = rec.WorkUnitID
		vols[i] = rec.VolunteerID
		hosts[i] = rec.HostID
		untils[i] = rec.ReservedUntil
		deadlines[i] = rec.DeadlineSeconds
	}
	// Land each in-memory hold as a RESERVED copy row. This is the authoritative
	// per-volunteer eligibility gate for the normal (non-spot-check) hand-out path —
	// the single place those reservations come into existence — so every distinctness
	// rule is enforced here regardless of what the in-memory hand-out decided to offer.
	// Guards:
	//   * the unit is still QUEUED (the aggregate accepts copies),
	//   * redundancy headroom remains (live copies + PENDING results < redundancy),
	//   * the volunteer has NOT already authored a PENDING result for the unit (so each
	//     of the N redundant results comes from a DISTINCT volunteer) — the guard whose
	//     absence let a re-queued unit be re-handed to its own prior submitter,
	//   * the volunteer is NOT in post-failure cooldown (a recent copy of this unit it
	//     STARTED but did not finish — EXPIRED, or ABANDONED mid-run — benches it for ~one
	//     deadline so a fresh volunteer gets first crack; a graceful return of un-started
	//     buffered work, ABANDONED with started_at NULL, is not unreliable and does not bench),
	//   * ON CONFLICT on the live-copy partial unique enforces "one live copy per
	//     volunteer" (a duplicate hold for the same volunteer is silently dropped and
	//     thus voided by the caller).
	// A reservation the guard rejects is simply absent from RETURNING, so the caller
	// voids that in-memory hold and the volunteer never run-starts it — no wasted compute.
	// Two rows for the SAME unit but DISTINCT volunteers both land when redundancy
	// allows — that is exactly the parallel-copy case (property 7).
	rows, err := r.db.Query(ctx, `
		INSERT INTO work_unit_assignment_history
			(work_unit_id, volunteer_id, host_id, assigned_at, reserved_until, deadline_seconds)
		SELECT v.id, v.vol, v.host_id, NOW(), v.reserved_until, v.deadline_seconds
		FROM (
			SELECT unnest($1::uuid[]) AS id,
			       unnest($2::uuid[]) AS vol,
			       unnest($5::uuid[]) AS host_id,
			       unnest($3::timestamptz[]) AS reserved_until,
			       unnest($4::int[]) AS deadline_seconds
		) AS v
		JOIN work_units wu ON wu.id = v.id AND wu.state = 'QUEUED'
		JOIN leafs l ON l.id = wu.leaf_id
		JOIN volunteers vv ON vv.id = v.vol
		WHERE (
		        (SELECT COUNT(*) FROM work_unit_assignment_history h
		         WHERE h.work_unit_id = v.id AND h.outcome IS NULL)
		        + (SELECT COUNT(*) FROM results res
		           WHERE res.work_unit_id = v.id AND res.validation_status = 'PENDING')
		      ) < `+effTargetWuL+`
		  AND NOT EXISTS (
		    SELECT 1 FROM results res2
		    WHERE res2.work_unit_id = v.id AND res2.volunteer_id = v.vol
		      AND res2.validation_status = 'PENDING'
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM work_unit_assignment_history hb
		    WHERE hb.work_unit_id = v.id AND hb.volunteer_id = v.vol
		      -- Only a STARTED copy benches (reliability signal); a graceful return of
		      -- un-started buffered work (ABANDONED, started_at NULL) does not (#59).
		      AND (hb.outcome = 'EXPIRED'
		           OR (hb.outcome = 'ABANDONED' AND hb.started_at IS NOT NULL))
		      AND hb.outcome_at IS NOT NULL
		      AND hb.outcome_at > NOW() - GREATEST(wu.deadline_seconds, 1) * INTERVAL '1 second'
		  )
		  -- Feasibility-at-dispatch (authoritative landing): refuse to create a copy this
		  -- volunteer's stored benchmark says it cannot finish before the unit's deadline,
		  -- so even a stale/raced in-memory hand-out never run-starts an over-deadline copy.
		  -- Skipped when benchmark / rsc_fpops_est / deadline is unknown. Mirrors
		  -- workunit.FeasibleByDeadline and the FindNextAssignable / ReserveCopy gates.
		  AND (
		    COALESCE((vv.hardware_capabilities->>'benchmark_fpops')::float8, 0) <= 0
		    OR COALESCE((l.execution_config->>'rsc_fpops_est')::float8, 0) <= 0
		    OR wu.deadline_seconds <= 0
		    OR (COALESCE((l.execution_config->>'rsc_fpops_est')::float8, 0)
		        / NULLIF((vv.hardware_capabilities->>'benchmark_fpops')::float8, 0)) <= wu.deadline_seconds
		  )
		ON CONFLICT (work_unit_id, volunteer_id) WHERE outcome IS NULL DO NOTHING
		RETURNING work_unit_id, volunteer_id`,
		ids, vols, untils, deadlines, hosts,
	)
	if err != nil {
		return nil, apierror.Internal("failed to flush reservations", err)
	}
	defer rows.Close()

	var landed []FlushedCopy
	for rows.Next() {
		var fc FlushedCopy
		if err := rows.Scan(&fc.WorkUnitID, &fc.VolunteerID); err != nil {
			return nil, apierror.Internal("failed to scan flushed copy", err)
		}
		landed = append(landed, fc)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate flushed copies", err)
	}

	// Layer 3: renew this head's dispatch claim on the batch's units (off the hot
	// path), so a held-but-unflushed unit's claim never expires under it. Gated on
	// dispatch_claimed_by = headID so it can never touch another replica's claim;
	// headID == zero (single-replica) disables renewal.
	if headID != (types.ID{}) {
		leaseSecs := claimLease.Seconds()
		if leaseSecs <= 0 {
			leaseSecs = float64(defaultClaimLeaseSeconds)
		}
		if _, err := r.db.Exec(ctx, `
			UPDATE work_units SET dispatch_claim_expires_at = NOW() + make_interval(secs => $2)
			WHERE id = ANY($1::uuid[]) AND dispatch_claimed_by = $3`,
			ids, leaseSecs, headID,
		); err != nil {
			// Non-fatal: the claim simply lapses and the unit becomes re-claimable.
			return landed, nil
		}
	}
	return landed, nil
}

// CountActiveByVolunteer returns the authoritative per-volunteer inflight count
// (active history rows + live reservations) for every volunteer that currently
// holds any, keyed by volunteer id. The dispatch cache reconciles its in-memory
// inflight counters against this so crash/drift can never cause permanent
// over-admission.
func (r *PgxWorkUnitRepository) CountActiveByVolunteer(ctx context.Context) (map[types.ID]int, error) {
	// A volunteer's inflight count is its live copies (RESERVED + RUNNING history
	// rows). With per-copy dispatch this single source covers both buffered and
	// run-started work — there are no separate per-unit reservations to add.
	rows, err := r.db.Query(ctx, `
		SELECT volunteer_id, COUNT(*)::bigint
		FROM work_unit_assignment_history
		WHERE outcome IS NULL AND volunteer_id IS NOT NULL
		GROUP BY volunteer_id`)
	if err != nil {
		return nil, apierror.Internal("failed to count active by volunteer", err)
	}
	defer rows.Close()

	out := make(map[types.ID]int)
	for rows.Next() {
		var vol types.ID
		var cnt int64
		if err := rows.Scan(&vol, &cnt); err != nil {
			return nil, apierror.Internal("failed to scan active-by-volunteer row", err)
		}
		out[vol] = int(cnt)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate active-by-volunteer rows", err)
	}
	return out, nil
}

// CountActiveByHost returns the authoritative per-MACHINE inflight count (live copies)
// keyed by effective host id (TODO #19). It groups on COALESCE(host_id, volunteer_id)
// so a copy from a volunteer that reported no host (NULL host_id) counts under its
// account id — which is exactly effectiveHostID(volunteer, "") — so the keys match the
// dispatch cache's per-host inflight map with no special-casing. Pre-migration copies
// (NULL host_id) therefore fold into the account's key, identical to the fallback.
func (r *PgxWorkUnitRepository) CountActiveByHost(ctx context.Context) (map[types.ID]int, error) {
	rows, err := r.db.Query(ctx, `
		SELECT COALESCE(host_id, volunteer_id) AS host, COUNT(*)::bigint
		FROM work_unit_assignment_history
		WHERE outcome IS NULL AND volunteer_id IS NOT NULL
		GROUP BY COALESCE(host_id, volunteer_id)`)
	if err != nil {
		return nil, apierror.Internal("failed to count active by host", err)
	}
	defer rows.Close()

	out := make(map[types.ID]int)
	for rows.Next() {
		var host types.ID
		var cnt int64
		if err := rows.Scan(&host, &cnt); err != nil {
			return nil, apierror.Internal("failed to scan active-by-host row", err)
		}
		out[host] = int(cnt)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate active-by-host rows", err)
	}
	return out, nil
}

// ReleaseStaleBufferedCopies closes a volunteer's buffered (RESERVED, not-yet-run-
// started) live copies that it no longer holds in its client buffer, per the held
// set the volunteer reports on each request. See the WorkUnitRepository interface
// for the full contract. The work unit stays QUEUED so it redispatches at once;
// RUNNING copies (started_at set) are never touched here — they ride their deadline.
// The created_at < olderThan guard is the grace window that protects a copy handed
// out moments ago from being reaped before the volunteer's next report includes it.
func (r *PgxWorkUnitRepository) ReleaseStaleBufferedCopies(ctx context.Context, hostID types.ID, heldWorkUnitIDs []types.ID, olderThan time.Time) ([]types.ID, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE work_unit_assignment_history
		SET outcome = 'ABANDONED', outcome_at = NOW()
		-- Match by MACHINE (TODO #19): COALESCE(host_id, volunteer_id) = the reporting
		-- host's effective id, so only THIS machine's buffered copies are reaped and host
		-- A's report never releases host B's buffer. Equals volunteer_id for a no-host copy.
		WHERE COALESCE(host_id, volunteer_id) = $1
		  AND outcome IS NULL
		  AND started_at IS NULL
		  AND created_at < $2
		  -- Held-set guard: an empty/absent set (the machine holds nothing) releases every
		  -- grace-aged buffered copy; otherwise release only those NOT in the held set.
		  AND (array_length($3::uuid[], 1) IS NULL OR NOT (work_unit_id = ANY($3::uuid[])))
		RETURNING work_unit_id`,
		hostID, olderThan, heldWorkUnitIDs,
	)
	if err != nil {
		return nil, apierror.Internal("failed to release stale buffered copies", err)
	}
	defer rows.Close()

	var released []types.ID
	for rows.Next() {
		var wuID types.ID
		if err := rows.Scan(&wuID); err != nil {
			return nil, apierror.Internal("failed to scan released buffered copy", err)
		}
		released = append(released, wuID)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate released buffered copies", err)
	}
	return released, nil
}

// ReserveNextAssignable is the batch-fill counterpart to FindNextAssignable: it
// finds the next assignable QUEUED unit for the volunteer (honoring every
// capability/redundancy/reservation/inflight predicate) and, instead of
// transitioning it to ASSIGNED, stamps a lease (reserved_until = NOW() + lease,
// reserved_volunteer_id = vol) while keeping state='QUEUED'. The row is held by
// the FOR UPDATE lock taken in FindNextAssignable for the life of the enclosing
// transaction, so the follow-up UPDATE cannot race another reserver. Returns
// (nil, nil) when no work is available.
//
// Because the reservation is written within the caller's transaction, a
// subsequent ReserveNextAssignable in the same tx sees the bumped live
// reservation count and the reservation guard, so the same unit is never
// reserved twice and the per-volunteer inflight cap is respected across the
// whole batch.
func (r *PgxWorkUnitRepository) ReserveNextAssignable(ctx context.Context, opts AssignmentOptions, lease time.Duration) (*WorkUnit, error) {
	wu, err := r.FindNextAssignable(ctx, opts)
	if err != nil {
		return nil, err
	}
	if wu == nil {
		return nil, nil
	}

	// Hold a buffered copy until its head-owned deadline, not a short liveness lease:
	// a volunteer keeps buffered work until the deadline lapses, and only a
	// deadline-miss reclaims the copy. The passed lease is a fallback for a unit with
	// no positive deadline.
	hold := lease
	if wu.DeadlineSeconds > 0 {
		hold = time.Duration(wu.DeadlineSeconds) * time.Second
	}
	reservedUntil := time.Now().UTC().Add(hold)
	if _, err := r.ReserveCopy(ctx, wu.ID, opts.VolunteerID, opts.HostID, reservedUntil, wu.DeadlineSeconds); err != nil {
		return nil, err
	}
	// Echo the reservation window on the returned unit (transient, for the proto
	// reserved_until_unix). The unit row itself stays QUEUED.
	ru := reservedUntil
	vid := opts.VolunteerID
	wu.ReservedUntil = &ru
	wu.ReservedVolunteerID = &vid
	return wu, nil
}

// ReserveCopy inserts a RESERVED copy (a buffered work_unit_assignment_history row,
// outcome NULL / started_at NULL) for (workUnitID, volunteerID), held until
// reservedUntil. deadlineSeconds is snapshotted onto the copy so the expiry sweep
// needs no join.
//
// This is the AUTHORITATIVE per-volunteer eligibility gate: it is the single point
// where a copy comes into existence, so it enforces every distinctness rule
// regardless of any optimization that decided to offer the unit. It returns
// apierror.Conflict (no copy created) when the unit is not QUEUED, OR the volunteer
// already holds a live copy (the live-copy partial unique), OR the volunteer already
// authored a PENDING result for this unit (each redundant result must come from a
// DISTINCT volunteer), OR the volunteer's recent copy of this unit timed out / was
// abandoned within the cooldown window (benched until a fresh volunteer gets first
// refusal). Enforcing these here means a hand-out raced by a concurrent submit is
// rejected BEFORE the volunteer ever run-starts, so no compute is wasted on a copy
// that could never be accepted.
func (r *PgxWorkUnitRepository) ReserveCopy(ctx context.Context, workUnitID, volunteerID types.ID, hostID *types.ID, reservedUntil time.Time, deadlineSeconds int) (*Copy, error) {
	row := r.db.QueryRow(ctx, `
		INSERT INTO work_unit_assignment_history
			(work_unit_id, volunteer_id, host_id, assigned_at, reserved_until, deadline_seconds)
		SELECT $1, $2, $5, NOW(), $3, $4
		FROM work_units wu
		JOIN leafs l ON l.id = wu.leaf_id
		JOIN volunteers vv ON vv.id = $2
		WHERE wu.id = $1 AND wu.state = 'QUEUED'
		  -- Distinctness: never reserve a unit this volunteer already produced a result
		  -- for (so each of the N redundant results comes from a distinct volunteer).
		  AND NOT EXISTS (
		    SELECT 1 FROM results res
		    WHERE res.work_unit_id = $1 AND res.volunteer_id = $2 AND res.validation_status = 'PENDING'
		  )
		  -- Cooldown: a volunteer whose recent copy of this unit it STARTED timed out / was
		  -- abandoned mid-run is benched for ~one deadline so a fresh volunteer gets first
		  -- crack on the requeue. A graceful return of un-started buffered work (ABANDONED,
		  -- started_at NULL) is not a reliability signal and does not bench (#59).
		  AND NOT EXISTS (
		    SELECT 1 FROM work_unit_assignment_history h
		    WHERE h.work_unit_id = $1 AND h.volunteer_id = $2
		      AND (h.outcome = 'EXPIRED'
		           OR (h.outcome = 'ABANDONED' AND h.started_at IS NOT NULL))
		      AND h.outcome_at IS NOT NULL
		      AND h.outcome_at > NOW() - GREATEST(wu.deadline_seconds, 1) * INTERVAL '1 second'
		  )
		  -- Feasibility-at-dispatch (spot-check landing): refuse a copy this volunteer's
		  -- stored benchmark says it cannot finish before the unit's deadline. Skipped when
		  -- benchmark / rsc_fpops_est / deadline is unknown. Mirrors workunit.FeasibleByDeadline.
		  AND (
		    COALESCE((vv.hardware_capabilities->>'benchmark_fpops')::float8, 0) <= 0
		    OR COALESCE((l.execution_config->>'rsc_fpops_est')::float8, 0) <= 0
		    OR wu.deadline_seconds <= 0
		    OR (COALESCE((l.execution_config->>'rsc_fpops_est')::float8, 0)
		        / NULLIF((vv.hardware_capabilities->>'benchmark_fpops')::float8, 0)) <= wu.deadline_seconds
		  )
		ON CONFLICT (work_unit_id, volunteer_id) WHERE outcome IS NULL DO NOTHING
		RETURNING `+copyColumns,
		workUnitID, volunteerID, reservedUntil, deadlineSeconds, hostID,
	)
	cp, err := scanCopy(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.Conflict(
				"work unit not dispatchable for this volunteer (not QUEUED, live copy held, result already submitted, or in post-failure cooldown)",
				map[string]string{"code": "RESERVATION_CONFLICT"},
			)
		}
		return nil, apierror.Internal("failed to reserve copy", err)
	}
	return cp, nil
}

// Assign is the run-start of a volunteer's copy: it flips that volunteer's live
// RESERVED copy to RUNNING (started_at = NOW), which starts the per-copy deadline
// clock. The WORK UNIT stays QUEUED (a pure aggregate) so its other redundancy copies
// keep dispatching in parallel. If the volunteer has no live un-started copy (the
// reservation lapsed or was never flushed), it returns apierror.Conflict so StartWork
// reports Ok=false and the client drops the unit. The denormalized
// work_units.assigned_volunteer_id/assigned_at are updated best-effort to the most
// recent run-start for observability only.
func (r *PgxWorkUnitRepository) Assign(ctx context.Context, workUnitID types.ID, volunteerID types.ID) (*WorkUnit, error) {
	now := time.Now().UTC()
	// Idempotent: COALESCE keeps an already-started copy's started_at, and matches any
	// LIVE copy (reserved or running) so a retried StartWork after a lost response is a
	// no-op success. 0 rows affected means this volunteer holds no live copy.
	tag, err := r.db.Exec(ctx, `
		UPDATE work_unit_assignment_history
		SET started_at = COALESCE(started_at, $3)
		WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL`,
		workUnitID, volunteerID, now,
	)
	if err != nil {
		return nil, apierror.Internal("failed to start work unit copy", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, apierror.Conflict(
			"no reserved copy to start for this volunteer",
			map[string]string{"code": "ASSIGNMENT_CONFLICT"},
		)
	}
	// Best-effort denormalized "most recent copy" pointer for observability.
	_, _ = r.db.Exec(ctx, `
		UPDATE work_units
		SET assigned_volunteer_id = $2, assigned_at = $3, last_heartbeat_at = $3,
		    started_at = COALESCE(started_at, $3)
		WHERE id = $1`,
		workUnitID, volunteerID, now,
	)
	return r.GetByID(ctx, workUnitID)
}

// CountByLeafAndState returns the count of work units for a project in a given state.
func (r *PgxWorkUnitRepository) CountByLeafAndState(ctx context.Context, projectID types.ID, state WorkUnitState) (int64, error) {
	var count int64
	err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_units WHERE leaf_id = $1 AND state = $2`,
		projectID, state,
	).Scan(&count)
	if err != nil {
		return 0, apierror.Internal("count work units by state", err)
	}
	return count, nil
}

// scanCopy scans a copy row (copyColumns order) into a Copy.
func scanCopy(row pgx.Row) (*Copy, error) {
	var c Copy
	err := row.Scan(
		&c.ID, &c.WorkUnitID, &c.VolunteerID, &c.HostID, &c.AssignedAt,
		&c.ReservedUntil, &c.StartedAt, &c.DeadlineSeconds, &c.Outcome, &c.OutcomeAt, &c.ResultID,
	)
	return &c, err
}

// FindExpiredCopies returns LIVE copies (outcome IS NULL) that have timed out — the
// per-copy replacement for the old unit-level deadline sweep. Two cases, both keyed
// on the deadline (property 5: the deadline is the only early-reclaim clock):
//   - RUNNING copy (started_at set) past started_at + deadline_seconds, or
//   - RESERVED copy (started_at NULL, buffered) past reserved_until — a holder that
//     vanished before run-start.
// deadline_seconds = 0 means "no deadline" and a RUNNING copy is never expired here.
func (r *PgxWorkUnitRepository) FindExpiredCopies(ctx context.Context, limit int) ([]*Copy, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+copyColumns+` FROM work_unit_assignment_history
		WHERE outcome IS NULL
		  AND (
		    (started_at IS NOT NULL AND deadline_seconds > 0
		       AND NOW() - started_at > deadline_seconds * INTERVAL '1 second')
		    OR (started_at IS NULL AND reserved_until IS NOT NULL AND reserved_until < NOW())
		  )
		ORDER BY assigned_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, apierror.Internal("failed to find expired copies", err)
	}
	defer rows.Close()

	var copies []*Copy
	for rows.Next() {
		cp, err := scanCopy(rows)
		if err != nil {
			return nil, apierror.Internal("failed to scan expired copy", err)
		}
		copies = append(copies, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate expired copies", err)
	}
	return copies, nil
}

// FindStuckSpotCheckUnits returns QUEUED spot-check units that have sat for over an
// hour without a second corroborator. The caller clears spot_check so the single
// result validates (the spot-check-never-got-a-partner reclaim, unchanged in intent
// from the old unit sweep's QUEUED+spot_check arm).
func (r *PgxWorkUnitRepository) FindStuckSpotCheckUnits(ctx context.Context, limit int) ([]*WorkUnit, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+workUnitColumns+` FROM work_units
		WHERE state = 'QUEUED' AND spot_check = true
		  AND created_at < NOW() - INTERVAL '1 hour'
		ORDER BY created_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, apierror.Internal("failed to find stuck spot-check units", err)
	}
	defer rows.Close()

	var workUnits []*WorkUnit
	for rows.Next() {
		wu, err := scanWorkUnit(rows)
		if err != nil {
			return nil, apierror.Internal("failed to scan stuck spot-check unit", err)
		}
		workUnits = append(workUnits, wu)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate stuck spot-check units", err)
	}
	return workUnits, nil
}

// CloseCopy closes a copy by id with the given outcome (e.g. EXPIRED, ABANDONED),
// stamping outcome_at = NOW(). Idempotent: only a still-live copy is closed.
func (r *PgxWorkUnitRepository) CloseCopy(ctx context.Context, copyID types.ID, outcome string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE work_unit_assignment_history
		SET outcome = $2, outcome_at = NOW()
		WHERE id = $1 AND outcome IS NULL`,
		copyID, outcome,
	)
	if err != nil {
		return apierror.Internal("failed to close copy", err)
	}
	return nil
}

// CloseCopyByVolunteer closes a volunteer's live copy of a unit with the given
// outcome (used by submit/abandon). resultID may be nil. Returns apierror.Conflict
// if the volunteer has no live copy of the unit.
func (r *PgxWorkUnitRepository) CloseCopyByVolunteer(ctx context.Context, workUnitID, volunteerID types.ID, outcome string, resultID *types.ID) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE work_unit_assignment_history
		SET outcome = $3, outcome_at = NOW(), result_id = COALESCE($4, result_id)
		WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL`,
		workUnitID, volunteerID, outcome, resultID,
	)
	if err != nil {
		return apierror.Internal("failed to close copy by volunteer", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.Conflict(
			"no live copy for this volunteer to close",
			map[string]string{"code": "COPY_CONFLICT"},
		)
	}
	return nil
}

// ExpireLiveCopies closes ALL live copies of a unit with the given outcome (used by
// the operator manual-requeue: abandon every in-flight copy so fresh ones dispatch).
// Returns how many copies were closed.
func (r *PgxWorkUnitRepository) ExpireLiveCopies(ctx context.Context, workUnitID types.ID, outcome string) (int, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE work_unit_assignment_history
		SET outcome = $2, outcome_at = NOW()
		WHERE work_unit_id = $1 AND outcome IS NULL`,
		workUnitID, outcome,
	)
	if err != nil {
		return 0, apierror.Internal("failed to expire live copies", err)
	}
	return int(tag.RowsAffected()), nil
}

// CountLiveCopies returns the number of live (RESERVED + RUNNING) copies of a unit.
func (r *PgxWorkUnitRepository) CountLiveCopies(ctx context.Context, workUnitID types.ID) (int, error) {
	var n int
	if err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NULL`,
		workUnitID,
	).Scan(&n); err != nil {
		return 0, apierror.Internal("failed to count live copies", err)
	}
	return n, nil
}

// CountTotalCopies returns the total number of copies (history rows) ever created for
// a unit — the dead-letter ceiling probe (property 6).
func (r *PgxWorkUnitRepository) CountTotalCopies(ctx context.Context, workUnitID types.ID) (int, error) {
	var n int
	if err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1`,
		workUnitID,
	).Scan(&n); err != nil {
		return 0, apierror.Internal("failed to count total copies", err)
	}
	return n, nil
}

// MarkCompleted transitions a unit QUEUED/ASSIGNED/RUNNING -> COMPLETED — the pre-validation
// state once a quorum's worth of results is in. Idempotent: an already-COMPLETED or terminal
// unit is untouched (0 rows). This is the inline UPDATE SubmitResult used before the
// transitioner, extracted so the transitioner is the sole caller of the COMPLETED mark.
func (r *PgxWorkUnitRepository) MarkCompleted(ctx context.Context, id types.ID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE work_units SET
			state = 'COMPLETED',
			started_at = COALESCE(started_at, NOW()),
			completed_at = NOW()
		WHERE id = $1 AND state IN ('QUEUED', 'ASSIGNED', 'RUNNING')`, id)
	if err != nil {
		return apierror.Internal("failed to mark work unit completed", err)
	}
	return nil
}

// CountErrorCopies returns the unit's wasted-work tally: copies that ended EXPIRED or ABANDONED
// plus DISAGREED results — the max_error_copies cap probe (TODO #50).
func (r *PgxWorkUnitRepository) CountErrorCopies(ctx context.Context, workUnitID types.ID) (int, error) {
	var n int
	if err := r.db.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM work_unit_assignment_history
		   WHERE work_unit_id = $1 AND outcome IN ('EXPIRED', 'ABANDONED'))
		  + (SELECT COUNT(*) FROM results
		     WHERE work_unit_id = $1 AND validation_status = 'DISAGREED')`,
		workUnitID,
	).Scan(&n); err != nil {
		return 0, apierror.Internal("failed to count error copies", err)
	}
	return n, nil
}

// DeadLetterIfExhausted parks a unit FAILED + flagged-for-review iff it is QUEUED,
// has NO live copy outstanding, its redundancy is still unmet (PENDING results <
// redundancy), AND the total copies ever created has reached its dead-letter ceiling
// (max_total_copies, defaulting to redundancy + a margin). This is the ONLY cap on
// requeue (property 6): honest timeouts redispatch with no per-attempt limit, but a
// hopeless (poison) unit eventually stops burning the volunteer pool. Returns whether
// the unit was failed.
func (r *PgxWorkUnitRepository) DeadLetterIfExhausted(ctx context.Context, workUnitID types.ID) (bool, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE work_units wu SET state = 'FAILED', flagged_for_review = true
		FROM leafs l
		WHERE wu.id = $1 AND l.id = wu.leaf_id AND wu.state = 'QUEUED'
		  AND NOT EXISTS (
		    SELECT 1 FROM work_unit_assignment_history h
		    WHERE h.work_unit_id = wu.id AND h.outcome IS NULL
		  )
		  AND (
		    SELECT COUNT(*) FROM results res
		    WHERE res.work_unit_id = wu.id AND res.validation_status = 'PENDING'
		  ) < `+effQuorumWuL+`
		  AND (
		    SELECT COUNT(*) FROM work_unit_assignment_history h2
		    WHERE h2.work_unit_id = wu.id
		  ) >= `+effMaxTotalWuL+``,
		workUnitID,
	)
	if err != nil {
		return false, apierror.Internal("failed to dead-letter work unit", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Reassign returns an EXPIRED or REJECTED work unit to QUEUED for further
// corroboration. Property 6: there is NO per-reassignment cap — a unit is requeued as
// many times as needed; the dead-letter ceiling (DeadLetterIfExhausted) is the only
// terminal stop. Always returns requeued=true on success.
func (r *PgxWorkUnitRepository) Reassign(ctx context.Context, id types.ID) (*WorkUnit, bool, error) {
	wu, err := r.GetByID(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if wu.State != WorkUnitStateExpired && wu.State != WorkUnitStateRejected {
		return nil, false, apierror.Conflict(
			fmt.Sprintf("cannot reassign work unit in state %s: must be EXPIRED or REJECTED", wu.State),
			map[string]string{"code": "INVALID_REASSIGNMENT_STATE"},
		)
	}
	updated, err := r.UpdateState(ctx, id, wu.State, WorkUnitStateQueued)
	if err != nil {
		return nil, false, fmt.Errorf("reassign to QUEUED: %w", err)
	}
	return updated, true, nil
}

// MarkSpotCheck sets spot_check = true for a work unit.
func (r *PgxWorkUnitRepository) MarkSpotCheck(ctx context.Context, id types.ID) error {
	tag, err := r.db.Exec(ctx,
		"UPDATE work_units SET spot_check = true WHERE id = $1", id)
	if err != nil {
		return apierror.Internal("failed to mark work unit for spot-check", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("work_unit", id.String())
	}
	return nil
}

// EnsureWorkUnitHRClass stamps the homogeneous-redundancy hardware class first-writer-wins
// and returns the effective (post-COALESCE) class. The UPDATE is a no-op once a class is
// set, so concurrent first hand-outs converge on whichever landed first. Mirrors the
// artifact-version pin (EnsureWorkUnitPin) but for the hardware class.
func (r *PgxWorkUnitRepository) EnsureWorkUnitHRClass(ctx context.Context, workUnitID types.ID, class string) (string, error) {
	var effective string
	err := r.db.QueryRow(ctx,
		`UPDATE work_units SET hr_class = COALESCE(hr_class, $2) WHERE id = $1 RETURNING hr_class`,
		workUnitID, class,
	).Scan(&effective)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", apierror.NotFound("work_unit", workUnitID.String())
		}
		return "", apierror.Internal("failed to ensure work unit hr_class", err)
	}
	return effective, nil
}

// ClearSpotCheck sets spot_check = false for a work unit, allowing it to complete
// with single-result validation. Used when a spot-check times out (no second volunteer).
func (r *PgxWorkUnitRepository) ClearSpotCheck(ctx context.Context, id types.ID) error {
	tag, err := r.db.Exec(ctx,
		"UPDATE work_units SET spot_check = false WHERE id = $1 AND spot_check = true", id)
	if err != nil {
		return apierror.Internal("failed to clear spot-check", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("work_unit", id.String())
	}
	return nil
}

// FindRunningWithStaleCheckpoints returns running work units with checkpointing enabled
// whose last checkpoint is older than 2× the configured checkpoint interval.
func (r *PgxWorkUnitRepository) FindRunningWithStaleCheckpoints(ctx context.Context, limit int) ([]StaleCheckpointInfo, error) {
	rows, err := r.db.Query(ctx, `
		SELECT wu.id, wu.last_checkpoint_at,
			COALESCE((l.fault_tolerance_config->>'checkpoint_interval_seconds')::int, 300) AS interval_seconds,
			EXTRACT(EPOCH FROM NOW() - wu.last_checkpoint_at)::bigint AS age_seconds
		FROM work_units wu
		JOIN leafs l ON wu.leaf_id = l.id
		WHERE EXISTS (
		    SELECT 1 FROM work_unit_assignment_history h
		    WHERE h.work_unit_id = wu.id AND h.outcome IS NULL AND h.started_at IS NOT NULL
		  )
		  AND wu.last_checkpoint_at IS NOT NULL
		  AND COALESCE((l.fault_tolerance_config->>'checkpointing_enabled')::boolean, false) = true
		  AND NOW() - wu.last_checkpoint_at >
		      2 * COALESCE((l.fault_tolerance_config->>'checkpoint_interval_seconds')::int, 300) * INTERVAL '1 second'
		LIMIT $1`, limit)
	if err != nil {
		return nil, apierror.Internal("failed to find stale checkpoints", err)
	}
	defer rows.Close()

	var results []StaleCheckpointInfo
	for rows.Next() {
		var info StaleCheckpointInfo
		if err := rows.Scan(&info.WorkUnitID, &info.LastCheckpointAt, &info.CheckpointIntervalSeconds, &info.AgeSeconds); err != nil {
			return nil, apierror.Internal("failed to scan stale checkpoint info", err)
		}
		results = append(results, info)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate stale checkpoints", err)
	}
	return results, nil
}

// --- PgxBatchRepository ---

// PgxBatchRepository implements BatchRepository using pgx.
type PgxBatchRepository struct {
	pool *pgxpool.Pool
}

// NewPgxBatchRepository creates a new PgxBatchRepository.
func NewPgxBatchRepository(pool *pgxpool.Pool) *PgxBatchRepository {
	return &PgxBatchRepository{pool: pool}
}

// Create inserts a new batch. On return, b is populated with DB-generated
// id and timestamps.
func (r *PgxBatchRepository) Create(ctx context.Context, b *Batch) error {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO batches (
			leaf_id, sequence_number, total_work_units, completed_work_units
		) VALUES ($1, $2, $3, $4)
		RETURNING `+batchColumns,
		b.LeafID, b.SequenceNumber, b.TotalWorkUnits, b.CompletedWorkUnits,
	)

	result, err := scanBatch(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == "23505" {
				return apierror.Conflict(
					"batch sequence number already exists for this project",
					map[string]string{"constraint": pgErr.ConstraintName},
				)
			}
			if pgErr.Code == "23503" {
				return apierror.Conflict(
					"referenced project does not exist",
					map[string]string{"constraint": pgErr.ConstraintName},
				)
			}
		}
		return apierror.Internal("failed to create batch", err)
	}
	*b = *result
	return nil
}

// GetByID retrieves a batch by its UUID.
func (r *PgxBatchRepository) GetByID(ctx context.Context, id types.ID) (*Batch, error) {
	row := r.pool.QueryRow(ctx,
		"SELECT "+batchColumns+" FROM batches WHERE id = $1", id)

	b, err := scanBatch(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("batch", id.String())
		}
		return nil, apierror.Internal("failed to get batch", err)
	}
	return b, nil
}

// ListByLeaf retrieves batches for a project ordered by sequence_number,
// with cursor-based pagination. The cursor encodes (created_at, id) and the
// ordering uses (created_at ASC, id ASC) to match, which is equivalent to
// sequence_number ordering since batches are created in sequence order.
func (r *PgxBatchRepository) ListByLeaf(ctx context.Context, projectID types.ID, page types.PaginationRequest) ([]*Batch, types.PaginationResponse, error) {
	pageSize := page.ClampPageSize()

	var conditions []string
	var args []any
	argIdx := 1

	conditions = append(conditions, fmt.Sprintf("leaf_id = $%d", argIdx))
	args = append(args, projectID)
	argIdx++

	if page.Cursor != "" {
		cursorTime, cursorID, err := types.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.ValidationError("invalid cursor", nil)
		}
		conditions = append(conditions, fmt.Sprintf("(created_at, id) > ($%d, $%d)", argIdx, argIdx+1))
		args = append(args, cursorTime, cursorID)
		argIdx += 2
	}

	where := "WHERE " + strings.Join(conditions, " AND ")

	query := fmt.Sprintf("SELECT %s FROM batches %s ORDER BY created_at ASC, id ASC LIMIT $%d",
		batchColumns, where, argIdx)
	args = append(args, pageSize+1)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to list batches", err)
	}
	defer rows.Close()

	var batches []*Batch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, types.PaginationResponse{}, apierror.Internal("failed to scan batch", err)
		}
		batches = append(batches, b)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PaginationResponse{}, apierror.Internal("failed to iterate batches", err)
	}

	pagination := types.PaginationResponse{}
	if len(batches) > pageSize {
		batches = batches[:pageSize]
		last := batches[pageSize-1]
		pagination.HasMore = true
		pagination.NextCursor = types.EncodeCursor(last.CreatedAt, last.ID)
	}

	return batches, pagination, nil
}

// IncrementCompleted atomically increments a batch's completed_work_units by 1.
func (r *PgxBatchRepository) IncrementCompleted(ctx context.Context, batchID types.ID) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE batches SET completed_work_units = completed_work_units + 1 WHERE id = $1",
		batchID,
	)
	if err != nil {
		return apierror.Internal("failed to increment batch completed count", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("batch", batchID.String())
	}
	return nil
}
