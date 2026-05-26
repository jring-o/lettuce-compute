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

// workUnitColumns is the standard column list for SELECT queries on work_units.
const workUnitColumns = `id, leaf_id, batch_id, state, priority,
	input_data, input_data_ref, code_artifact_ref, parameters,
	estimated_duration_seconds, deadline_seconds, output_spec,
	assigned_volunteer_id, assigned_at, started_at, completed_at, validated_at,
	reassignment_count, max_reassignments, last_heartbeat_at,
	flagged_for_review, spot_check, last_checkpoint_at, last_checkpoint_sequence,
	created_at, updated_at`

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
		&wu.LastHeartbeatAt,
		&wu.FlaggedForReview,
		&wu.SpotCheck,
		&wu.LastCheckpointAt,
		&wu.LastCheckpointSequence,
		&wu.CreatedAt,
		&wu.UpdatedAt,
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
			reassignment_count, max_reassignments, last_heartbeat_at,
			flagged_for_review, spot_check
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11,
			$12, $13, $14, $15, $16,
			$17, $18, $19,
			$20, $21
		) RETURNING `+workUnitColumns,
		wu.LeafID, wu.BatchID, wu.State, wu.Priority,
		wu.InputData, wu.InputDataRef, wu.CodeArtifactRef, wu.Parameters,
		wu.EstimatedDurationSeconds, wu.DeadlineSeconds, wu.OutputSpec,
		wu.AssignedVolunteerID, wu.AssignedAt, wu.StartedAt, wu.CompletedAt, wu.ValidatedAt,
		wu.ReassignmentCount, wu.MaxReassignments, wu.LastHeartbeatAt,
		wu.FlaggedForReview, wu.SpotCheck,
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
			flagged_for_review = $11
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

// BulkCreate inserts multiple work units efficiently using pgx.CopyFrom.
// Unlike Create, the input structs are NOT populated with DB-generated IDs or
// timestamps after insertion. Use a follow-up List query if you need the IDs.
func (r *PgxWorkUnitRepository) BulkCreate(ctx context.Context, wus []*WorkUnit) error {
	if len(wus) == 0 {
		return nil
	}

	columns := []string{
		"leaf_id", "batch_id", "state", "priority",
		"input_data", "input_data_ref", "code_artifact_ref", "parameters",
		"estimated_duration_seconds", "deadline_seconds", "output_spec",
		"assigned_volunteer_id", "assigned_at", "started_at", "completed_at", "validated_at",
		"reassignment_count", "max_reassignments", "last_heartbeat_at",
		"flagged_for_review", "spot_check",
	}

	rows := make([][]any, len(wus))
	for i, wu := range wus {
		rows[i] = []any{
			wu.LeafID, wu.BatchID, wu.State, wu.Priority,
			wu.InputData, wu.InputDataRef, wu.CodeArtifactRef, wu.Parameters,
			wu.EstimatedDurationSeconds, wu.DeadlineSeconds, wu.OutputSpec,
			wu.AssignedVolunteerID, wu.AssignedAt, wu.StartedAt, wu.CompletedAt, wu.ValidatedAt,
			wu.ReassignmentCount, wu.MaxReassignments, wu.LastHeartbeatAt,
			wu.FlaggedForReview, wu.SpotCheck,
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
	wu.reassignment_count, wu.max_reassignments, wu.last_heartbeat_at,
	wu.flagged_for_review, wu.spot_check, wu.last_checkpoint_at, wu.last_checkpoint_sequence,
	wu.created_at, wu.updated_at`

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
		  AND (
		    NOT COALESCE((l.resource_requirements->>'gpu_required')::boolean, false)
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
		  AND (
		    SELECT COUNT(*) FROM work_unit_assignment_history wuah
		    WHERE wuah.work_unit_id = wu.id AND wuah.outcome IS NULL
		  ) < CASE WHEN wu.spot_check THEN 2
		       ELSE COALESCE((l.validation_config->>'redundancy_factor')::int, 2)
		      END
		  AND NOT EXISTS (
		    SELECT 1 FROM work_unit_assignment_history wuah2
		    WHERE wuah2.work_unit_id = wu.id
		      AND wuah2.volunteer_id = $9
		      AND (wuah2.outcome IS NULL OR wu.spot_check)
		  )
		  AND (
		    $12::int <= 0
		    OR (SELECT COUNT(*) FROM work_unit_assignment_history wuah3
		        WHERE wuah3.volunteer_id = $9 AND wuah3.outcome IS NULL) < $12
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

// Assign transitions a work unit from QUEUED to ASSIGNED and sets assignment metadata.
// Uses optimistic concurrency: the update succeeds only if the work unit is currently QUEUED.
func (r *PgxWorkUnitRepository) Assign(ctx context.Context, workUnitID types.ID, volunteerID types.ID) (*WorkUnit, error) {
	now := time.Now().UTC()
	row := r.db.QueryRow(ctx, `
		UPDATE work_units SET
			state = 'ASSIGNED',
			assigned_volunteer_id = $2,
			assigned_at = $3,
			last_heartbeat_at = $3
		WHERE id = $1 AND state = 'QUEUED'
		RETURNING `+workUnitColumns,
		workUnitID, volunteerID, now,
	)

	wu, err := scanWorkUnit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.Conflict(
				"work unit is no longer in QUEUED state",
				map[string]string{"code": "ASSIGNMENT_CONFLICT"},
			)
		}
		return nil, apierror.Internal("failed to assign work unit", err)
	}
	return wu, nil
}

// UpdateHeartbeat updates last_heartbeat_at to NOW() for a work unit.
func (r *PgxWorkUnitRepository) UpdateHeartbeat(ctx context.Context, id types.ID) error {
	tag, err := r.db.Exec(ctx,
		"UPDATE work_units SET last_heartbeat_at = NOW() WHERE id = $1", id)
	if err != nil {
		return apierror.Internal("failed to update heartbeat", err)
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound("work_unit", id.String())
	}
	return nil
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

// FindExpiredWorkUnits returns work units past their deadline. This includes:
// - ASSIGNED or RUNNING work units past their deadline (based on assigned_at)
// - Spot-check work units stuck in QUEUED state for over 1 hour (never picked up by a second volunteer)
//
// deadline_seconds = 0 means "no deadline" (leaf opted out via NoDeadline); such
// units are never expired here and rely on heartbeat-based abandonment instead.
func (r *PgxWorkUnitRepository) FindExpiredWorkUnits(ctx context.Context, limit int) ([]*WorkUnit, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+workUnitColumns+` FROM work_units
		WHERE (state IN ('ASSIGNED', 'RUNNING')
		  AND assigned_at IS NOT NULL
		  AND deadline_seconds > 0
		  AND NOW() - assigned_at > deadline_seconds * INTERVAL '1 second')
		   OR (state = 'QUEUED' AND spot_check = true
		  AND created_at < NOW() - INTERVAL '1 hour')
		ORDER BY created_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, apierror.Internal("failed to find expired work units", err)
	}
	defer rows.Close()

	var workUnits []*WorkUnit
	for rows.Next() {
		wu, err := scanWorkUnit(rows)
		if err != nil {
			return nil, apierror.Internal("failed to scan expired work unit", err)
		}
		workUnits = append(workUnits, wu)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate expired work units", err)
	}
	return workUnits, nil
}

// FindAbandonedWorkUnits returns ASSIGNED or RUNNING work units with stale
// heartbeats. ASSIGNED is included so a volunteer that vanishes before its first
// RUNNING heartbeat (e.g. mid image-pull, or while the unit waits in its
// prefetch queue) is still reclaimed — critical for no_deadline leafs, which
// FindExpiredWorkUnits never touches. last_heartbeat_at is set at assignment
// time and refreshed by PREPARING heartbeats while the unit is held, so live
// pulls/queued units are not falsely reclaimed.
func (r *PgxWorkUnitRepository) FindAbandonedWorkUnits(ctx context.Context, limit int) ([]*WorkUnit, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+prefixedWorkUnitColumns+`
		FROM work_units wu
		JOIN leafs l ON wu.leaf_id = l.id
		WHERE wu.state IN ('ASSIGNED', 'RUNNING')
		  AND wu.last_heartbeat_at IS NOT NULL
		  AND NOW() - wu.last_heartbeat_at >
		      (l.fault_tolerance_config->>'heartbeat_interval_seconds')::int
		      * (l.fault_tolerance_config->>'missed_heartbeats_threshold')::int
		      * INTERVAL '1 second'
		ORDER BY wu.last_heartbeat_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, apierror.Internal("failed to find abandoned work units", err)
	}
	defer rows.Close()

	var workUnits []*WorkUnit
	for rows.Next() {
		wu, err := scanWorkUnit(rows)
		if err != nil {
			return nil, apierror.Internal("failed to scan abandoned work unit", err)
		}
		workUnits = append(workUnits, wu)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate abandoned work units", err)
	}
	return workUnits, nil
}

// TransitionToExpired moves a work unit to EXPIRED state.
// Uses optimistic concurrency on the current state (ASSIGNED or RUNNING).
func (r *PgxWorkUnitRepository) TransitionToExpired(ctx context.Context, id types.ID) (*WorkUnit, error) {
	row := r.db.QueryRow(ctx, `
		UPDATE work_units SET state = 'EXPIRED'
		WHERE id = $1 AND state IN ('ASSIGNED', 'RUNNING')
		RETURNING `+workUnitColumns, id)

	wu, err := scanWorkUnit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.Conflict(
				"work unit is not in ASSIGNED or RUNNING state",
				map[string]string{"code": "TRANSITION_CONFLICT"},
			)
		}
		return nil, apierror.Internal("failed to transition work unit to expired", err)
	}
	return wu, nil
}

// Reassign transitions an EXPIRED or REJECTED work unit back to QUEUED with HIGH
// priority and incremented reassignment_count. If max reassignments is reached,
// transitions to FAILED instead and sets flagged_for_review.
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

	if wu.ReassignmentCount >= wu.MaxReassignments {
		updated, err := r.UpdateState(ctx, id, wu.State, WorkUnitStateFailed)
		if err != nil {
			return nil, false, fmt.Errorf("reassign to FAILED: %w", err)
		}
		return updated, false, nil
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
		WHERE wu.state = 'RUNNING'
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
