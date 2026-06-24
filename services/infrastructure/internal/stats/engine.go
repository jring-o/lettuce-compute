package stats

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Engine computes and stores leaf statistics snapshots.
type Engine struct {
	pool *pgxpool.Pool
}

// NewEngine creates a new stats Engine.
func NewEngine(pool *pgxpool.Pool) *Engine {
	return &Engine{pool: pool}
}

// ComputeSnapshot aggregates work unit state counts for a leaf,
// inserts a new snapshot row, and returns it.
func (e *Engine) ComputeSnapshot(ctx context.Context, leafID types.ID) (*LeafStatsSnapshot, error) {
	var snap LeafStatsSnapshot
	snap.LeafID = leafID

	// Single aggregation query over work_units.
	err := e.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) AS total_work_units,
			-- Per-copy dispatch: a unit stays QUEUED while its copies run, so "assigned"
			-- and "running" are derived from its live copies, not the unit state. queued =
			-- a QUEUED unit with NO live copy; assigned = a live RESERVED-only copy
			-- (buffered, not started); running = at least one RUNNING copy.
			COUNT(*) FILTER (WHERE state = 'QUEUED' AND NOT EXISTS (
				SELECT 1 FROM work_unit_assignment_history h
				WHERE h.work_unit_id = work_units.id AND h.outcome IS NULL)) AS work_units_queued,
			COUNT(*) FILTER (WHERE EXISTS (
				SELECT 1 FROM work_unit_assignment_history h
				WHERE h.work_unit_id = work_units.id AND h.outcome IS NULL AND h.started_at IS NULL
			) AND NOT EXISTS (
				SELECT 1 FROM work_unit_assignment_history h2
				WHERE h2.work_unit_id = work_units.id AND h2.outcome IS NULL AND h2.started_at IS NOT NULL
			)) AS work_units_assigned,
			COUNT(*) FILTER (WHERE EXISTS (
				SELECT 1 FROM work_unit_assignment_history h
				WHERE h.work_unit_id = work_units.id AND h.outcome IS NULL AND h.started_at IS NOT NULL
			)) AS work_units_running,
			COUNT(*) FILTER (WHERE state = 'COMPLETED') AS work_units_completed,
			COUNT(*) FILTER (WHERE state = 'VALIDATED') AS work_units_validated,
			COUNT(*) FILTER (WHERE state IN ('REJECTED', 'EXPIRED', 'FAILED')) AS work_units_failed,
			COUNT(*) FILTER (WHERE spot_check = true) AS spot_checks_total,
			COUNT(*) FILTER (WHERE spot_check = true AND state = 'VALIDATED') AS spot_checks_passed,
			COUNT(*) FILTER (WHERE spot_check = true AND state IN ('REJECTED', 'FAILED')) AS spot_checks_failed
		FROM work_units
		WHERE leaf_id = $1
	`, leafID).Scan(
		&snap.TotalWorkUnits,
		&snap.WorkUnitsQueued,
		&snap.WorkUnitsAssigned,
		&snap.WorkUnitsRunning,
		&snap.WorkUnitsCompleted,
		&snap.WorkUnitsValidated,
		&snap.WorkUnitsFailed,
		&snap.SpotChecksTotal,
		&snap.SpotChecksPassed,
		&snap.SpotChecksFailed,
	)
	if err != nil {
		return nil, apierror.Internal("failed to compute stats snapshot", err)
	}
	snap.SpotCheckPassRate = spotCheckPassRate(snap.SpotChecksPassed, snap.SpotChecksTotal)

	// ActiveVolunteers is the same rolling-window count the head endpoints report
	// (live copies plus volunteers active within the window), so every surface
	// agrees on "active volunteers".
	activeVolunteers, err := leaf.CountActiveVolunteersForLeaf(ctx, e.pool, leafID)
	if err != nil {
		return nil, apierror.Internal("failed to count active volunteers", err)
	}
	snap.ActiveVolunteers = activeVolunteers

	// TotalCreditGranted is the sum of every credit_ledger row for this leaf (one
	// row is written per agreed, validated result). The ledger is the authoritative
	// source of granted credit, so the leaf-detail page and the per-leaf snapshot
	// report the same total the operator credit-analysis endpoints do — instead of
	// the hardcoded 0 that previously made the leaf page read "0 total credit".
	// idx_credit_ledger_leaf_time (leaf_id, granted_at) covers this scan.
	if err := e.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(credit_amount), 0)
		FROM credit_ledger
		WHERE leaf_id = $1
	`, leafID).Scan(&snap.TotalCreditGranted); err != nil {
		return nil, apierror.Internal("failed to sum credit granted", err)
	}

	// The nullable metrics (AvgCompletionSeconds, AgreementRate, ThroughputPerHour)
	// remain at zero values until throughput tracking is implemented.

	// Insert snapshot and get back the generated id, snapshot_at, created_at.
	err = e.pool.QueryRow(ctx, `
		INSERT INTO leaf_stats_snapshots (
			leaf_id,
			total_work_units, work_units_queued, work_units_assigned,
			work_units_running, work_units_completed, work_units_validated,
			work_units_failed, active_volunteers, total_credit_granted,
			avg_completion_seconds, agreement_rate, throughput_per_hour,
			spot_checks_total, spot_checks_passed, spot_checks_failed
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING id, snapshot_at, created_at
	`,
		snap.LeafID,
		snap.TotalWorkUnits, snap.WorkUnitsQueued, snap.WorkUnitsAssigned,
		snap.WorkUnitsRunning, snap.WorkUnitsCompleted, snap.WorkUnitsValidated,
		snap.WorkUnitsFailed, snap.ActiveVolunteers, snap.TotalCreditGranted,
		snap.AvgCompletionSeconds, snap.AgreementRate, snap.ThroughputPerHour,
		snap.SpotChecksTotal, snap.SpotChecksPassed, snap.SpotChecksFailed,
	).Scan(&snap.ID, &snap.SnapshotAt, &snap.CreatedAt)
	if err != nil {
		return nil, apierror.Internal("failed to insert stats snapshot", err)
	}

	return &snap, nil
}

// ComputeLeafStatsBatch aggregates work unit state counts for multiple leafs
// in a single query. Returns a map keyed by project ID. Projects with no work units
// are included with zero-value stats.
// Note: spot-check stats (SpotChecksTotal/Passed/Failed) are intentionally omitted
// from batch queries — use ComputeSnapshot for per-leaf spot-check detail.
func (e *Engine) ComputeLeafStatsBatch(ctx context.Context, leafIDs []types.ID) (map[types.ID]*LeafStatsSnapshot, error) {
	result := make(map[types.ID]*LeafStatsSnapshot, len(leafIDs))
	// Pre-fill with zero-value snapshots so missing leafs return zeros.
	for _, id := range leafIDs {
		result[id] = &LeafStatsSnapshot{LeafID: id}
	}

	rows, err := e.pool.Query(ctx, `
		SELECT
			leaf_id,
			COUNT(*) AS total_work_units,
			COUNT(*) FILTER (WHERE state = 'QUEUED' AND NOT EXISTS (
				SELECT 1 FROM work_unit_assignment_history h
				WHERE h.work_unit_id = work_units.id AND h.outcome IS NULL)) AS work_units_queued,
			COUNT(*) FILTER (WHERE EXISTS (
				SELECT 1 FROM work_unit_assignment_history h
				WHERE h.work_unit_id = work_units.id AND h.outcome IS NULL AND h.started_at IS NULL
			) AND NOT EXISTS (
				SELECT 1 FROM work_unit_assignment_history h2
				WHERE h2.work_unit_id = work_units.id AND h2.outcome IS NULL AND h2.started_at IS NOT NULL
			)) AS work_units_assigned,
			COUNT(*) FILTER (WHERE EXISTS (
				SELECT 1 FROM work_unit_assignment_history h
				WHERE h.work_unit_id = work_units.id AND h.outcome IS NULL AND h.started_at IS NOT NULL
			)) AS work_units_running,
			COUNT(*) FILTER (WHERE state = 'COMPLETED') AS work_units_completed,
			COUNT(*) FILTER (WHERE state = 'VALIDATED') AS work_units_validated,
			COUNT(*) FILTER (WHERE state IN ('REJECTED', 'EXPIRED', 'FAILED')) AS work_units_failed
		FROM work_units
		WHERE leaf_id = ANY($1)
		GROUP BY leaf_id
	`, leafIDs)
	if err != nil {
		return nil, apierror.Internal("failed to compute batch stats", err)
	}
	defer rows.Close()

	for rows.Next() {
		var snap LeafStatsSnapshot
		if err := rows.Scan(
			&snap.LeafID,
			&snap.TotalWorkUnits,
			&snap.WorkUnitsQueued,
			&snap.WorkUnitsAssigned,
			&snap.WorkUnitsRunning,
			&snap.WorkUnitsCompleted,
			&snap.WorkUnitsValidated,
			&snap.WorkUnitsFailed,
		); err != nil {
			return nil, apierror.Internal("failed to scan batch stats row", err)
		}
		result[snap.LeafID] = &snap
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate batch stats", err)
	}

	// TotalCreditGranted per leaf from the credit ledger, in one grouped scan, so
	// the leaf-list endpoint reports the same granted-credit total as the leaf
	// detail page (previously hardcoded 0). credit_ledger only references leaves
	// that have work units, so every leaf with credit already has a (work-unit or
	// pre-filled zero) entry in result; we just overlay the credit sum.
	creditRows, err := e.pool.Query(ctx, `
		SELECT leaf_id, COALESCE(SUM(credit_amount), 0)
		FROM credit_ledger
		WHERE leaf_id = ANY($1)
		GROUP BY leaf_id
	`, leafIDs)
	if err != nil {
		return nil, apierror.Internal("failed to compute batch credit", err)
	}
	defer creditRows.Close()

	for creditRows.Next() {
		var leafID types.ID
		var credit float64
		if err := creditRows.Scan(&leafID, &credit); err != nil {
			return nil, apierror.Internal("failed to scan batch credit row", err)
		}
		if snap, ok := result[leafID]; ok {
			snap.TotalCreditGranted = credit
		}
	}
	if err := creditRows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate batch credit", err)
	}

	return result, nil
}

// GetLatestSnapshot returns the most recent snapshot for a leaf.
// Returns nil, nil if no snapshot exists.
func (e *Engine) GetLatestSnapshot(ctx context.Context, leafID types.ID) (*LeafStatsSnapshot, error) {
	snap, err := scanSnapshot(e.pool.QueryRow(ctx, `
		SELECT `+snapshotColumns+`
		FROM leaf_stats_snapshots
		WHERE leaf_id = $1
		ORDER BY snapshot_at DESC
		LIMIT 1
	`, leafID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, apierror.Internal("failed to get latest snapshot", err)
	}
	return snap, nil
}

// GetOrComputeSnapshot returns the latest snapshot if it's within maxAge,
// otherwise computes and stores a fresh one.
func (e *Engine) GetOrComputeSnapshot(ctx context.Context, leafID types.ID, maxAge time.Duration) (*LeafStatsSnapshot, error) {
	latest, err := e.GetLatestSnapshot(ctx, leafID)
	if err != nil {
		return nil, err
	}

	if latest != nil && time.Since(latest.SnapshotAt) <= maxAge {
		return latest, nil
	}

	return e.ComputeSnapshot(ctx, leafID)
}

// ListSnapshots returns snapshots in a time range with optional downsampling.
func (e *Engine) ListSnapshots(ctx context.Context, leafID types.ID, filters StatsHistoryFilters) ([]*LeafStatsSnapshot, error) {
	var rows pgx.Rows
	var err error

	switch filters.Interval {
	case "hourly":
		rows, err = e.pool.Query(ctx, `
			SELECT DISTINCT ON (date_trunc('hour', snapshot_at))
				`+snapshotColumns+`
			FROM leaf_stats_snapshots
			WHERE leaf_id = $1 AND snapshot_at >= $2 AND snapshot_at <= $3
			ORDER BY date_trunc('hour', snapshot_at), snapshot_at
		`, leafID, filters.From, filters.To)
	case "daily":
		rows, err = e.pool.Query(ctx, `
			SELECT DISTINCT ON (date_trunc('day', snapshot_at))
				`+snapshotColumns+`
			FROM leaf_stats_snapshots
			WHERE leaf_id = $1 AND snapshot_at >= $2 AND snapshot_at <= $3
			ORDER BY date_trunc('day', snapshot_at), snapshot_at
		`, leafID, filters.From, filters.To)
	default: // "raw"
		rows, err = e.pool.Query(ctx, `
			SELECT `+snapshotColumns+`
			FROM leaf_stats_snapshots
			WHERE leaf_id = $1 AND snapshot_at >= $2 AND snapshot_at <= $3
			ORDER BY snapshot_at ASC
		`, leafID, filters.From, filters.To)
	}
	if err != nil {
		return nil, apierror.Internal("failed to list stats snapshots", err)
	}
	defer rows.Close()

	var snapshots []*LeafStatsSnapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, apierror.Internal("failed to scan stats snapshot", err)
		}
		snapshots = append(snapshots, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate stats snapshots", err)
	}

	return snapshots, nil
}

const snapshotColumns = `id, leaf_id, snapshot_at,
	total_work_units, work_units_queued, work_units_assigned,
	work_units_running, work_units_completed, work_units_validated,
	work_units_failed, active_volunteers, total_credit_granted,
	avg_completion_seconds, agreement_rate, throughput_per_hour,
	spot_checks_total, spot_checks_passed, spot_checks_failed,
	created_at`

// scanSnapshot scans a single row into a LeafStatsSnapshot.
// Works with both pgx.Row and pgx.Rows (both implement the Scan interface).
func scanSnapshot(scanner interface{ Scan(dest ...any) error }) (*LeafStatsSnapshot, error) {
	var s LeafStatsSnapshot
	err := scanner.Scan(
		&s.ID, &s.LeafID, &s.SnapshotAt,
		&s.TotalWorkUnits, &s.WorkUnitsQueued, &s.WorkUnitsAssigned,
		&s.WorkUnitsRunning, &s.WorkUnitsCompleted, &s.WorkUnitsValidated,
		&s.WorkUnitsFailed, &s.ActiveVolunteers, &s.TotalCreditGranted,
		&s.AvgCompletionSeconds, &s.AgreementRate, &s.ThroughputPerHour,
		&s.SpotChecksTotal, &s.SpotChecksPassed, &s.SpotChecksFailed,
		&s.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	s.SpotCheckPassRate = spotCheckPassRate(s.SpotChecksPassed, s.SpotChecksTotal)
	return &s, nil
}

// spotCheckPassRate computes the pass rate as a percentage, or nil if no spot-checks exist.
func spotCheckPassRate(passed, total int) *float64 {
	if total <= 0 {
		return nil
	}
	rate := float64(passed) / float64(total)
	return &rate
}
