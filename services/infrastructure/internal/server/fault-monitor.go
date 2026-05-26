package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// FaultMonitor periodically scans for expired and abandoned work units.
type FaultMonitor struct {
	workUnitRepo   workunit.WorkUnitRepository
	assignRepo     assignment.Repository
	checkpointRepo checkpoint.Repository
	leafRepo    leaf.Repository
	logger         *slog.Logger
	scanInterval   time.Duration
	batchSize      int
}

// NewFaultMonitor creates a new FaultMonitor with default settings.
func NewFaultMonitor(
	workUnitRepo workunit.WorkUnitRepository,
	assignRepo assignment.Repository,
	checkpointRepo checkpoint.Repository,
	leafRepo leaf.Repository,
	logger *slog.Logger,
) *FaultMonitor {
	return &FaultMonitor{
		workUnitRepo:   workUnitRepo,
		assignRepo:     assignRepo,
		checkpointRepo: checkpointRepo,
		leafRepo:    leafRepo,
		logger:         logger,
		scanInterval:   30 * time.Second,
		batchSize:      100,
	}
}

// Start begins the background monitoring loop. Returns when ctx is cancelled.
func (m *FaultMonitor) Start(ctx context.Context) {
	m.logger.Info("fault monitor starting", "scan_interval", m.scanInterval, "batch_size", m.batchSize)
	ticker := time.NewTicker(m.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("fault monitor stopping")
			return
		case <-ticker.C:
			if err := m.ScanOnce(ctx); err != nil {
				m.logger.Error("fault monitor scan failed", "error", err)
			}
		}
	}
}

// ScanOnce performs a single scan for expired and abandoned work units.
func (m *FaultMonitor) ScanOnce(ctx context.Context) error {
	// Find and process expired work units (past deadline).
	expired, err := m.workUnitRepo.FindExpiredWorkUnits(ctx, m.batchSize)
	if err != nil {
		return err
	}
	for _, wu := range expired {
		// Spot-check WUs stuck in QUEUED state: clear the spot-check flag so the
		// first volunteer's result is accepted with single-result validation.
		if wu.State == workunit.WorkUnitStateQueued && wu.SpotCheck {
			if err := m.workUnitRepo.ClearSpotCheck(ctx, wu.ID); err != nil {
				m.logger.Error("failed to clear spot-check on timed-out work unit", "work_unit_id", wu.ID, "error", err)
			} else {
				m.logger.Info("spot-check timed out, accepting single result",
					"work_unit_id", wu.ID,
				)
			}
			continue
		}

		if _, err := m.workUnitRepo.TransitionToExpired(ctx, wu.ID); err != nil {
			m.logger.Error("failed to expire work unit", "work_unit_id", wu.ID, "error", err)
			continue
		}

		// Update assignment history.
		if wu.AssignedVolunteerID != nil {
			m.updateAssignmentOutcome(ctx, wu, assignment.OutcomeExpired)
		}

		m.logger.Warn("work unit expired",
			"work_unit_id", wu.ID,
			"volunteer_id", wu.AssignedVolunteerID,
			"deadline_seconds", wu.DeadlineSeconds,
		)

		// Reassign or fail the expired work unit.
		updated, requeued, err := m.workUnitRepo.Reassign(ctx, wu.ID)
		if err != nil {
			m.logger.Error("failed to reassign expired work unit", "work_unit_id", wu.ID, "error", err)
			continue
		}
		if requeued {
			// Log checkpoint preservation on reassignment.
			if wu.LastCheckpointSequence > 0 {
				m.logger.Info("checkpoint preserved for reassignment",
					"work_unit_id", wu.ID,
					"checkpoint_sequence", wu.LastCheckpointSequence,
					"last_checkpoint_at", wu.LastCheckpointAt,
				)
			}
			m.logger.Info("work unit reassigned", "work_unit_id", wu.ID, "reassignment_count", updated.ReassignmentCount)
		} else {
			m.logger.Warn("work unit failed after max reassignments", "work_unit_id", wu.ID, "reassignment_count", updated.ReassignmentCount)
			// Clean up checkpoints for failed work units.
			m.cleanupCheckpoint(ctx, wu)
		}
	}

	// Find and process abandoned work units (missed heartbeats).
	abandoned, err := m.workUnitRepo.FindAbandonedWorkUnits(ctx, m.batchSize)
	if err != nil {
		return err
	}
	for _, wu := range abandoned {
		if _, err := m.workUnitRepo.TransitionToExpired(ctx, wu.ID); err != nil {
			m.logger.Error("failed to expire abandoned work unit", "work_unit_id", wu.ID, "error", err)
			continue
		}

		// Update assignment history.
		if wu.AssignedVolunteerID != nil {
			m.updateAssignmentOutcome(ctx, wu, assignment.OutcomeAbandoned)
		}

		m.logger.Warn("work unit abandoned",
			"work_unit_id", wu.ID,
			"volunteer_id", wu.AssignedVolunteerID,
			"last_heartbeat_at", wu.LastHeartbeatAt,
		)

		// Reassign or fail the abandoned work unit.
		updated, requeued, err := m.workUnitRepo.Reassign(ctx, wu.ID)
		if err != nil {
			m.logger.Error("failed to reassign abandoned work unit", "work_unit_id", wu.ID, "error", err)
			continue
		}
		if requeued {
			// Log checkpoint preservation on reassignment.
			if wu.LastCheckpointSequence > 0 {
				m.logger.Info("checkpoint preserved for reassignment",
					"work_unit_id", wu.ID,
					"checkpoint_sequence", wu.LastCheckpointSequence,
					"last_checkpoint_at", wu.LastCheckpointAt,
				)
			}
			m.logger.Info("work unit reassigned", "work_unit_id", wu.ID, "reassignment_count", updated.ReassignmentCount)
		} else {
			m.logger.Warn("work unit failed after max reassignments", "work_unit_id", wu.ID, "reassignment_count", updated.ReassignmentCount)
			// Clean up checkpoints for failed work units.
			m.cleanupCheckpoint(ctx, wu)
		}
	}

	// Check for stale checkpoints across all running work units with checkpointing enabled.
	m.checkStaleCheckpoints(ctx)

	return nil
}

// updateAssignmentOutcome finds the active assignment for a work unit and sets its outcome.
func (m *FaultMonitor) updateAssignmentOutcome(ctx context.Context, wu *workunit.WorkUnit, outcome assignment.AssignmentOutcome) {
	active, err := m.assignRepo.FindActiveByWorkUnitAndVolunteer(ctx, wu.ID, *wu.AssignedVolunteerID)
	if err != nil {
		m.logger.Error("failed to find active assignment",
			"work_unit_id", wu.ID,
			"volunteer_id", wu.AssignedVolunteerID,
			"error", err,
		)
		return
	}
	if err := m.assignRepo.UpdateOutcome(ctx, active.ID, outcome, nil); err != nil {
		m.logger.Error("failed to update assignment outcome",
			"assignment_id", active.ID,
			"outcome", outcome,
			"error", err,
		)
	}
}

// cleanupCheckpoint deletes checkpoint data for a work unit (best effort).
func (m *FaultMonitor) cleanupCheckpoint(ctx context.Context, wu *workunit.WorkUnit) {
	if wu.LastCheckpointSequence == 0 {
		return
	}
	if err := m.checkpointRepo.Delete(ctx, wu.ID); err != nil {
		m.logger.Error("failed to clean up checkpoint",
			"work_unit_id", wu.ID,
			"error", err,
		)
	}
}

// checkStaleCheckpoints logs warnings for running work units with checkpointing enabled
// whose last checkpoint is older than 2× the configured interval.
func (m *FaultMonitor) checkStaleCheckpoints(ctx context.Context) {
	// Query running work units with checkpoints that might be stale.
	// This is a lightweight monitoring query — not critical path.
	rows, err := m.workUnitRepo.FindRunningWithStaleCheckpoints(ctx, m.batchSize)
	if err != nil {
		m.logger.Error("stale checkpoint check failed", "error", err)
		return
	}
	for _, info := range rows {
		m.logger.Warn("stale checkpoint detected",
			"work_unit_id", info.WorkUnitID,
			"last_checkpoint_at", info.LastCheckpointAt,
			"checkpoint_interval_seconds", info.CheckpointIntervalSeconds,
			"age_seconds", info.AgeSeconds,
		)
	}
}
