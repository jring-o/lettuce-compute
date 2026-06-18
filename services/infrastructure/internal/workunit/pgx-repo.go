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
const workUnitColumns = `id, leaf_id, batch_id, state, priority,
	input_data, input_data_ref, code_artifact_ref, parameters,
	estimated_duration_seconds, deadline_seconds, output_spec,
	assigned_volunteer_id, assigned_at, started_at, completed_at, validated_at,
	reassignment_count, max_reassignments, last_heartbeat_at,
	flagged_for_review, spot_check, last_checkpoint_at, last_checkpoint_sequence,
	reserved_until, reserved_volunteer_id,
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
		&wu.ReservedUntil,
		&wu.ReservedVolunteerID,
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
			flagged_for_review = $11,
			-- Layer 3: clear any dispatch claim on EVERY state transition (defensive /
			-- self-healing). EXPIRED/REJECTED -> QUEUED reassignment routes through here
			-- (Reassign -> UpdateState), so the requeue path is claim-clean independent
			-- of Assign ordering: a re-QUEUED unit is always immediately re-claimable.
			-- A pure no-op in the common case (a unit that reached ASSIGNED already had
			-- its claim NULLed by Assign).
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
	wu.reserved_until, wu.reserved_volunteer_id,
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
		  -- Redundancy: a NORMAL buffered (reserved) unit is leased purely via the
		  -- reservation columns and writes NO assignment_history row (so a crashed
		  -- holder leaves no stale active row to leak — a lapsed lease is re-reservable
		  -- with zero cleanup). The active-redundancy count is therefore the active
		  -- history rows PLUS one for a live NORMAL reservation held by any OTHER
		  -- volunteer. Spot-check units are excluded from the reservation term: they
		  -- DO write a history row alongside their reservation, so they are already
		  -- counted by the history-row subquery (adding the reservation too would
		  -- double-count and wrongly block the second corroborating volunteer). A live
		  -- reservation by THIS volunteer is excluded by the self-exclusion guard
		  -- below; once a unit flips to ASSIGNED at run-start, Assign clears the
		  -- reservation columns, so there is no overlap between the two terms.
		  AND (
		    (
		      SELECT COUNT(*) FROM work_unit_assignment_history wuah
		      WHERE wuah.work_unit_id = wu.id AND wuah.outcome IS NULL
		    )
		    -- Results already submitted for this unit count toward the redundancy
		    -- need: a corroborator that finished holds one of the N slots even though
		    -- its assignment row is now closed and the unit has been returned to the
		    -- queue for the next distinct volunteer.
		    + (
		      SELECT COUNT(*) FROM results res
		      WHERE res.work_unit_id = wu.id AND res.validation_status = 'PENDING'
		    )
		    + CASE
		        WHEN NOT wu.spot_check
		             AND wu.reserved_until IS NOT NULL AND wu.reserved_until > NOW()
		             AND wu.reserved_volunteer_id IS DISTINCT FROM $9
		        THEN 1 ELSE 0
		      END
		  ) < CASE WHEN wu.spot_check THEN 2
		       ELSE COALESCE((l.validation_config->>'redundancy_factor')::int, 2)
		      END
		  -- Reservation guard: a live reservation held by ANOTHER volunteer hides the
		  -- unit; a lapsed lease (reserved_until < NOW()) or this volunteer's own
		  -- reservation does not (the latter is then handled by the self-exclusion
		  -- guard below so the holder is never handed its own held unit twice).
		  -- Spot-check units are exempt: they must stay visible to a SECOND volunteer
		  -- for corroboration despite the first volunteer's reservation — their
		  -- dedup/redundancy is enforced by the history-row subqueries (the
		  -- "not already assigned" guard excludes the first volunteer; the
		  -- redundancy < 2 admits exactly one more).
		  AND (
		    wu.spot_check
		    OR wu.reserved_until IS NULL
		    OR wu.reserved_until < NOW()
		    OR wu.reserved_volunteer_id = $9
		  )
		  -- Self-exclusion: never hand this volunteer a unit it already holds — either
		  -- via an active history row (assigned, or a spot-checked unit it touched) or
		  -- via a live reservation of its own.
		  AND NOT EXISTS (
		    SELECT 1 FROM work_unit_assignment_history wuah2
		    WHERE wuah2.work_unit_id = wu.id
		      AND wuah2.volunteer_id = $9
		      AND (wuah2.outcome IS NULL OR wu.spot_check)
		  )
		  AND NOT (
		    wu.reserved_volunteer_id = $9
		    AND wu.reserved_until IS NOT NULL
		    AND wu.reserved_until > NOW()
		  )
		  -- Never hand this volunteer a unit it has already produced a result for, so
		  -- each of the N redundant results comes from a DISTINCT volunteer (the unit
		  -- is returned to the queue after each partial submit, which would otherwise
		  -- let the same volunteer pick it up again).
		  AND NOT EXISTS (
		    SELECT 1 FROM results res3
		    WHERE res3.work_unit_id = wu.id
		      AND res3.volunteer_id = $9
		      AND res3.validation_status = 'PENDING'
		  )
		  -- Per-volunteer inflight cap counts BOTH active assignments (active history
		  -- rows) AND this volunteer's live reservations, so one volunteer cannot
		  -- reserve the whole queue. The two terms never overlap: a reserved QUEUED
		  -- unit has no history row, and Assign clears the reservation when it writes
		  -- the history row at run-start.
		  AND (
		    $12::int <= 0
		    OR (
		      (SELECT COUNT(*) FROM work_unit_assignment_history wuah3
		       WHERE wuah3.volunteer_id = $9 AND wuah3.outcome IS NULL)
		      + (SELECT COUNT(*) FROM work_units wur
		         WHERE wur.reserved_volunteer_id = $9 AND wur.reserved_until > NOW())
		    ) < $12
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
			CASE WHEN wu.spot_check THEN 2
			     ELSE COALESCE((l.validation_config->>'redundancy_factor')::int, 2)
			END AS effective_redundancy,
			(
				(SELECT COUNT(*) FROM work_unit_assignment_history wuah
				 WHERE wuah.work_unit_id = wu.id AND wuah.outcome IS NULL)
				+ (SELECT COUNT(*) FROM results res2
				   WHERE res2.work_unit_id = wu.id AND res2.validation_status = 'PENDING')
			) AS active_assignments,
			COALESCE(l.execution_config->>'runtime', 'NATIVE') AS runtime
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
		  -- Redundancy: active history rows + one for any live NORMAL reservation
		  -- (held by anyone, since there is no specific requester at refill time).
		  -- Spot-check units are excluded from the reservation term (they carry their
		  -- own history row and are counted by the history subquery).
		  AND (
		    (
		      SELECT COUNT(*) FROM work_unit_assignment_history wuah
		      WHERE wuah.work_unit_id = wu.id AND wuah.outcome IS NULL
		    )
		    -- Already-submitted results hold a redundancy slot even after their
		    -- assignment row closes and the unit returns to the queue.
		    + (
		      SELECT COUNT(*) FROM results res
		      WHERE res.work_unit_id = wu.id AND res.validation_status = 'PENDING'
		    )
		    + CASE
		        WHEN NOT wu.spot_check
		             AND wu.reserved_until IS NOT NULL AND wu.reserved_until > NOW()
		        THEN 1 ELSE 0
		      END
		  ) < CASE WHEN wu.spot_check THEN 2
		       ELSE COALESCE((l.validation_config->>'redundancy_factor')::int, 2)
		      END
		  -- Reservation guard: a live NORMAL reservation hides the unit; a lapsed
		  -- lease or a spot-check unit (must stay visible for corroboration) does not.
		  AND (
		    wu.spot_check
		    OR wu.reserved_until IS NULL
		    OR wu.reserved_until < NOW()
		  )
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
		if err := rows.Scan(
			&wu.ID, &wu.LeafID, &wu.BatchID, &wu.State, &wu.Priority,
			&wu.InputData, &wu.InputDataRef, &wu.CodeArtifactRef, &wu.Parameters,
			&wu.EstimatedDurationSeconds, &wu.DeadlineSeconds, &wu.OutputSpec,
			&wu.AssignedVolunteerID, &wu.AssignedAt, &wu.StartedAt, &wu.CompletedAt, &wu.ValidatedAt,
			&wu.ReassignmentCount, &wu.MaxReassignments, &wu.LastHeartbeatAt,
			&wu.FlaggedForReview, &wu.SpotCheck, &wu.LastCheckpointAt, &wu.LastCheckpointSequence,
			&wu.ReservedUntil, &wu.ReservedVolunteerID, &wu.CreatedAt, &wu.UpdatedAt,
			&redundancy, &active, &runtime,
		); err != nil {
			return nil, apierror.Internal("failed to scan dispatchable work unit", err)
		}
		cand := wu
		out = append(out, DispatchCandidate{
			WorkUnit:          &cand,
			LeafID:            wu.LeafID,
			RedundancyFactor:  redundancy,
			ActiveAssignments: active,
			Runtime:           runtime,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate dispatchable work units", err)
	}
	return out, nil
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
			  -- Redundancy: active history rows + one for any live NORMAL reservation
			  -- (held by anyone). Spot-check units carry their own history row and are
			  -- counted by the history subquery, so they are excluded from the term.
			  AND (
			    (
			      SELECT COUNT(*) FROM work_unit_assignment_history wuah
			      WHERE wuah.work_unit_id = wu2.id AND wuah.outcome IS NULL
			    )
			    + CASE
			        WHEN NOT wu2.spot_check
			             AND wu2.reserved_until IS NOT NULL AND wu2.reserved_until > NOW()
			        THEN 1 ELSE 0
			      END
			  ) < CASE WHEN wu2.spot_check THEN 2
			       ELSE COALESCE((l2.validation_config->>'redundancy_factor')::int, 2)
			      END
			  -- Reservation guard: a live NORMAL reservation hides the unit; a lapsed
			  -- lease or a spot-check unit (must stay visible for corroboration) does not.
			  AND (
			    wu2.spot_check
			    OR wu2.reserved_until IS NULL
			    OR wu2.reserved_until < NOW()
			  )
			ORDER BY wu2.priority DESC, wu2.created_at ASC
			LIMIT $1
			FOR UPDATE OF wu2 SKIP LOCKED
		)
		  AND l.id = wu.leaf_id
		RETURNING `+prefixedWorkUnitColumns+`,
			CASE WHEN wu.spot_check THEN 2
			     ELSE COALESCE((l.validation_config->>'redundancy_factor')::int, 2)
			END AS effective_redundancy,
			(
				(SELECT COUNT(*) FROM work_unit_assignment_history wuah
				 WHERE wuah.work_unit_id = wu.id AND wuah.outcome IS NULL)
				+ (SELECT COUNT(*) FROM results res2
				   WHERE res2.work_unit_id = wu.id AND res2.validation_status = 'PENDING')
			) AS active_assignments,
			COALESCE(l.execution_config->>'runtime', 'NATIVE') AS runtime`,
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
		if err := rows.Scan(
			&wu.ID, &wu.LeafID, &wu.BatchID, &wu.State, &wu.Priority,
			&wu.InputData, &wu.InputDataRef, &wu.CodeArtifactRef, &wu.Parameters,
			&wu.EstimatedDurationSeconds, &wu.DeadlineSeconds, &wu.OutputSpec,
			&wu.AssignedVolunteerID, &wu.AssignedAt, &wu.StartedAt, &wu.CompletedAt, &wu.ValidatedAt,
			&wu.ReassignmentCount, &wu.MaxReassignments, &wu.LastHeartbeatAt,
			&wu.FlaggedForReview, &wu.SpotCheck, &wu.LastCheckpointAt, &wu.LastCheckpointSequence,
			&wu.ReservedUntil, &wu.ReservedVolunteerID, &wu.CreatedAt, &wu.UpdatedAt,
			&redundancy, &active, &runtime,
		); err != nil {
			return nil, apierror.Internal("failed to scan claimed work unit", err)
		}
		cand := wu
		out = append(out, DispatchCandidate{
			WorkUnit:          &cand,
			LeafID:            wu.LeafID,
			RedundancyFactor:  redundancy,
			ActiveAssignments: active,
			Runtime:           runtime,
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

// FlushReservation is one async reservation write produced by the dispatch cache.
type FlushReservation struct {
	WorkUnitID    types.ID
	VolunteerID   types.ID
	ReservedUntil time.Time
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
func (r *PgxWorkUnitRepository) FlushReservations(ctx context.Context, recs []FlushReservation, headID types.ID, claimLease time.Duration) ([]types.ID, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	ids := make([]types.ID, len(recs))
	vols := make([]types.ID, len(recs))
	untils := make([]time.Time, len(recs))
	for i, rec := range recs {
		ids[i] = rec.WorkUnitID
		vols[i] = rec.VolunteerID
		untils[i] = rec.ReservedUntil
	}
	leaseSecs := claimLease.Seconds()
	if leaseSecs <= 0 {
		leaseSecs = float64(defaultClaimLeaseSeconds)
	}
	rows, err := r.db.Query(ctx, `
		UPDATE work_units AS wu SET
			reserved_until = v.reserved_until,
			reserved_volunteer_id = v.vol,
			-- Renew THIS head's claim (only ours: the equality guard prevents touching
			-- another replica's claim). When the unit is not claimed by us the COALESCE
			-- leaves the existing expiry untouched.
			dispatch_claim_expires_at = CASE
				WHEN wu.dispatch_claimed_by = $4
				THEN NOW() + make_interval(secs => $5)
				ELSE wu.dispatch_claim_expires_at
			END
		FROM (
			SELECT unnest($1::uuid[]) AS id,
			       unnest($2::uuid[]) AS vol,
			       unnest($3::timestamptz[]) AS reserved_until
		) AS v
		WHERE wu.id = v.id
		  AND wu.state = 'QUEUED'
		  AND (wu.reserved_until IS NULL
		       OR wu.reserved_until < NOW()
		       OR wu.reserved_volunteer_id = v.vol)
		RETURNING wu.id`,
		ids, vols, untils, headID, leaseSecs,
	)
	if err != nil {
		return nil, apierror.Internal("failed to flush reservations", err)
	}
	defer rows.Close()

	var landed []types.ID
	for rows.Next() {
		var id types.ID
		if err := rows.Scan(&id); err != nil {
			return nil, apierror.Internal("failed to scan flushed reservation id", err)
		}
		landed = append(landed, id)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate flushed reservations", err)
	}
	return landed, nil
}

// CountActiveByVolunteer returns the authoritative per-volunteer inflight count
// (active history rows + live reservations) for every volunteer that currently
// holds any, keyed by volunteer id. The dispatch cache reconciles its in-memory
// inflight counters against this so crash/drift can never cause permanent
// over-admission.
func (r *PgxWorkUnitRepository) CountActiveByVolunteer(ctx context.Context) (map[types.ID]int, error) {
	rows, err := r.db.Query(ctx, `
		SELECT vol, SUM(cnt)::bigint FROM (
			SELECT volunteer_id AS vol, COUNT(*) AS cnt
			FROM work_unit_assignment_history
			WHERE outcome IS NULL
			GROUP BY volunteer_id
			UNION ALL
			SELECT reserved_volunteer_id AS vol, COUNT(*) AS cnt
			FROM work_units
			WHERE reserved_volunteer_id IS NOT NULL AND reserved_until > NOW()
			GROUP BY reserved_volunteer_id
		) t
		GROUP BY vol`)
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

	reservedUntil := time.Now().UTC().Add(lease)
	row := r.db.QueryRow(ctx, `
		UPDATE work_units SET
			reserved_until = $2,
			reserved_volunteer_id = $3
		WHERE id = $1 AND state = 'QUEUED'
		RETURNING `+workUnitColumns,
		wu.ID, reservedUntil, opts.VolunteerID,
	)

	reserved, err := scanWorkUnit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.Conflict(
				"work unit is no longer in QUEUED state",
				map[string]string{"code": "RESERVATION_CONFLICT"},
			)
		}
		return nil, apierror.Internal("failed to reserve work unit", err)
	}
	return reserved, nil
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
			last_heartbeat_at = $3,
			reserved_until = NULL,
			reserved_volunteer_id = NULL,
			-- Layer 3: run-start releases the dispatch claim in the SAME atomic UPDATE
			-- that clears the reservation, so a unit leaving the dispatchable universe
			-- never strands its claim.
			dispatch_claimed_by = NULL,
			dispatch_claim_expires_at = NULL
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
// deadline_seconds = 0 means "no deadline" and is never expired here. NoDeadline
// leafs no longer stamp 0: ResolveDeadlineSeconds gives them a large synthetic
// reclaim ceiling (deadline_seconds > 0) so they remain covered by this sweep —
// a unit on a vanished volunteer is always reclaimed, at most after the ceiling.
// A unit still QUEUED+reserved (never run-started) is reclaimed separately by the
// lapsed-reservation sweep (FindLapsedReservations) once its lease lapses.
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

// FindLapsedReservations returns still-QUEUED work units whose buffer lease has
// lapsed (reserved_until IS NOT NULL AND reserved_until < NOW()). These are
// buffered (reserved) units whose holder vanished before run-start — either an
// ordinary client that buffered work then died, or a head crash that flushed a
// reservation whose in-memory owner is gone (the #22 lapsed-lease reclaim gap).
//
// Neither FindExpiredWorkUnits nor the (removed) heartbeat sweep covered these:
// both filter state IN ('ASSIGNED','RUNNING'), so a QUEUED-reserved unit whose
// reservation lapsed was never actively reclaimed. With per-task heartbeats gone
// and lease-renewal retired, this sweep is the load-bearing dead-holder reclaim
// for units that never StartWork'd. The caller clears each returned unit's
// reservation (ClearReservation), leaving it QUEUED and immediately re-stageable
// by the dispatch cache — no TransitionToExpired/Reassign is needed (the unit
// never left QUEUED).
func (r *PgxWorkUnitRepository) FindLapsedReservations(ctx context.Context, limit int) ([]*WorkUnit, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+workUnitColumns+` FROM work_units
		WHERE state = 'QUEUED'
		  AND reserved_until IS NOT NULL
		  AND reserved_until < NOW()
		ORDER BY reserved_until ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, apierror.Internal("failed to find lapsed reservations", err)
	}
	defer rows.Close()

	var workUnits []*WorkUnit
	for rows.Next() {
		wu, err := scanWorkUnit(rows)
		if err != nil {
			return nil, apierror.Internal("failed to scan lapsed reservation", err)
		}
		workUnits = append(workUnits, wu)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate lapsed reservations", err)
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

// StampReservation sets reserved_until / reserved_volunteer_id on a still-QUEUED
// work unit without re-running the assignment predicate. Used in the batch
// spot-check branch (belt-and-suspenders): the "not already assigned" subquery
// already excludes a spot-checked volunteer on the next loop iteration, and the
// reservation guard reinforces it. Returns the updated WorkUnit (so the response
// can echo reserved_until_unix).
func (r *PgxWorkUnitRepository) StampReservation(ctx context.Context, id, volunteerID types.ID, lease time.Duration) (*WorkUnit, error) {
	reservedUntil := time.Now().UTC().Add(lease)
	row := r.db.QueryRow(ctx, `
		UPDATE work_units SET
			reserved_until = $2,
			reserved_volunteer_id = $3
		WHERE id = $1 AND state = 'QUEUED'
		RETURNING `+workUnitColumns,
		id, reservedUntil, volunteerID,
	)
	wu, err := scanWorkUnit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.Conflict(
				"work unit is no longer in QUEUED state",
				map[string]string{"code": "RESERVATION_CONFLICT"},
			)
		}
		return nil, apierror.Internal("failed to stamp reservation", err)
	}
	return wu, nil
}

// ClearReservation drops the reservation columns (reserved_until /
// reserved_volunteer_id) on a still-QUEUED unit currently reserved to
// volunteerID, leaving it QUEUED so it is immediately re-reservable by any
// volunteer. Used when a volunteer abandons a buffered (reserved, un-started)
// unit — e.g. a prepare failure or queue-full drop before the unit ever ran. It
// is a no-op-safe guard: it only matches a unit still reserved to this
// volunteer, so a unit that has since flipped to ASSIGNED (different volunteer,
// or run-started) is not touched. Returns the updated WorkUnit, or
// apierror.Conflict if no matching reserved QUEUED unit exists.
func (r *PgxWorkUnitRepository) ClearReservation(ctx context.Context, id, volunteerID types.ID) (*WorkUnit, error) {
	row := r.db.QueryRow(ctx, `
		UPDATE work_units SET
			reserved_until = NULL,
			reserved_volunteer_id = NULL,
			-- Layer 3: an abandon / flush-conflict void of a buffered (reserved) unit
			-- also releases this head's dispatch claim so the unit is immediately
			-- re-claimable by any replica (not left QUEUED with a stranded live claim).
			dispatch_claimed_by = NULL,
			dispatch_claim_expires_at = NULL
		WHERE id = $1 AND state = 'QUEUED' AND reserved_volunteer_id = $2
		RETURNING `+workUnitColumns,
		id, volunteerID,
	)
	wu, err := scanWorkUnit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.Conflict(
				"work unit is not reserved to this volunteer in QUEUED state",
				map[string]string{"code": "RESERVATION_CONFLICT"},
			)
		}
		return nil, apierror.Internal("failed to clear reservation", err)
	}
	return wu, nil
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
