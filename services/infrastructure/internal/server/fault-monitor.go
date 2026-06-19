package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// FaultMonitor periodically scans for expired and abandoned work units.
type FaultMonitor struct {
	workUnitRepo   workunit.WorkUnitRepository
	assignRepo     assignment.Repository
	checkpointRepo checkpoint.Repository
	leafRepo       leaf.Repository
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
		leafRepo:       leafRepo,
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

// ScanOnce performs a single scan for timed-out copies and stuck spot-check units.
//
// Per-copy dispatch (migration 00006): timeouts are detected on COPIES (per-copy
// rows), not units. A timed-out copy is closed (EXPIRED for a run-started copy,
// ABANDONED for a buffered copy whose holder vanished before run-start); its work
// unit stays QUEUED and immediately redispatches a FRESH copy to a DISTINCT volunteer
// — with NO per-reassignment cap (property 6). Only when a unit has exhausted its
// dead-letter ceiling (max_total_copies) with redundancy unmet is it parked FAILED.
func (m *FaultMonitor) ScanOnce(ctx context.Context) error {
	// 1. Timed-out copies (the deadline is the only early-reclaim clock — property 5).
	expired, err := m.workUnitRepo.FindExpiredCopies(ctx, m.batchSize)
	if err != nil {
		return err
	}
	for _, cp := range expired {
		outcome := assignment.OutcomeExpired // run-started copy missed its deadline
		if cp.StartedAt == nil {
			outcome = assignment.OutcomeAbandoned // buffered copy: holder vanished pre-start
		}
		if err := m.workUnitRepo.CloseCopy(ctx, cp.ID, string(outcome)); err != nil {
			m.logger.Error("failed to close timed-out copy",
				"copy_id", cp.ID, "work_unit_id", cp.WorkUnitID, "error", err)
			continue
		}
		m.logger.Warn("work unit copy timed out",
			"copy_id", cp.ID, "work_unit_id", cp.WorkUnitID, "volunteer_id", cp.VolunteerID,
			"outcome", outcome, "deadline_seconds", cp.DeadlineSeconds)

		// Dead-letter only if the unit has exhausted its retry ceiling with redundancy
		// unmet and no live copy left; otherwise it stays QUEUED and redispatches.
		failed, err := m.workUnitRepo.DeadLetterIfExhausted(ctx, cp.WorkUnitID)
		if err != nil {
			m.logger.Error("dead-letter check failed", "work_unit_id", cp.WorkUnitID, "error", err)
			continue
		}
		if failed {
			m.logger.Warn("work unit dead-lettered after exhausting retry ceiling",
				"work_unit_id", cp.WorkUnitID)
			m.cleanupCheckpointByID(ctx, cp.WorkUnitID)
		}
	}

	// 2. Spot-check units stuck QUEUED with no second corroborator: clear spot_check
	//    so the single result validates.
	stuck, err := m.workUnitRepo.FindStuckSpotCheckUnits(ctx, m.batchSize)
	if err != nil {
		m.logger.Error("stuck spot-check sweep failed", "error", err)
	}
	for _, wu := range stuck {
		if err := m.workUnitRepo.ClearSpotCheck(ctx, wu.ID); err != nil {
			m.logger.Error("failed to clear spot-check on timed-out work unit", "work_unit_id", wu.ID, "error", err)
		} else {
			m.logger.Info("spot-check timed out, accepting single result", "work_unit_id", wu.ID)
		}
	}

	// Layer 3 dispatch-claim HYGIENE sweep. A crashed replica leaves its claimed
	// units QUEUED with a live claim it can no longer renew/release; once the
	// flusher stops renewing, the claim expires and the unit is ALREADY re-claimable
	// by any survivor's refill (the claim WHERE-term treats an expired claim as
	// claimable) — passive expiry is the reclaim guarantee, NOT this sweep. This
	// sweep only NULLs the now-meaningless expired-claim columns to keep the table
	// tidy and observable. It is leader-gated (this whole monitor runs on the leader
	// replica only), so during the bounded ≤15s leaderless window after a leader
	// crash no replica actively NULLs expired claims, but passive re-claim needs no
	// sweep, so dispatch is unaffected.
	if cleared, err := m.workUnitRepo.ClearExpiredDispatchClaims(ctx); err != nil {
		m.logger.Error("expired dispatch-claim hygiene sweep failed", "error", err)
	} else if cleared > 0 {
		m.logger.Debug("expired dispatch claims cleared", "count", cleared)
	}

	// Check for stale checkpoints across all running work units with checkpointing enabled.
	m.checkStaleCheckpoints(ctx)

	return nil
}

// cleanupCheckpointByID deletes checkpoint data for a work unit (best effort).
func (m *FaultMonitor) cleanupCheckpointByID(ctx context.Context, workUnitID types.ID) {
	if err := m.checkpointRepo.Delete(ctx, workUnitID); err != nil {
		m.logger.Debug("failed to clean up checkpoint", "work_unit_id", workUnitID, "error", err)
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
