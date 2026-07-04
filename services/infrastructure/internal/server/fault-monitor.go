package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// trustStarvationProber is the OPTIONAL repository capability behind the trust-starved
// WARN sweep: counting QUEUED units whose remaining headroom the trusted-corroborator
// reservation is withholding for trusted subjects none have taken. It is a type-asserted
// side interface rather than a WorkUnitRepository method so the many repository fakes need
// not implement it — a repo without it (or with the trust gate off, which short-circuits
// inside the pgx implementation) simply skips the sweep.
type trustStarvationProber interface {
	CountTrustStarvedUnits(ctx context.Context, sampleLimit int) (int, []types.ID, error)
}

// trustStarvedWarnEvery throttles the trust-starved WARN: the sweep runs every scan (it is
// one cheap query, and zero queries with the gate off), but the log line repeats at most
// this often — a starved population changes on operator timescales (seeding trusted
// subjects, trusted volunteers arriving), so re-warning every 30s scan would only be noise.
const trustStarvedWarnEvery = 10 * time.Minute

// standingPopulationWarnEvery throttles the auto-benched/probation WARN (see
// warnStandingPopulation): the sweep is one small in-memory read (and skipped
// entirely when the backpressure machine is off), but the standing population
// changes on operator timescales, so re-warning every scan would only be noise.
const standingPopulationWarnEvery = 10 * time.Minute

// FaultMonitor periodically scans for expired and abandoned work units.
type FaultMonitor struct {
	workUnitRepo   workunit.WorkUnitRepository
	assignRepo     assignment.Repository
	checkpointRepo checkpoint.Repository
	leafRepo       leaf.Repository
	// reliabilityRepo feeds the per-host measured-reliability signal (TODO #54): a timed-out
	// (EXPIRED) or abandoned (ABANDONED) copy is wasted work for the machine that held it.
	// May be nil (tests / pre-#54) -> the signal is simply not recorded (best-effort).
	reliabilityRepo reliability.Repository
	// transitioner is the SINGLE owner of the post-copy-close redundancy decision (TODO #50):
	// after a timed-out copy is closed, the fault monitor delegates "requeue vs dead-letter
	// vs (now) validate-at-quorum" to it. May be nil (tests) -> the legacy direct
	// DeadLetterIfExhausted path is used, which is behavior-identical for the dead-letter case.
	transitioner *transition.Transitioner
	logger       *slog.Logger
	scanInterval time.Duration
	batchSize    int
	// lastTrustStarvedWarn is when the trust-starved WARN last fired (zero = never),
	// the trustStarvedWarnEvery throttle clock. Touched only by the single Start loop
	// (ScanOnce is not concurrent with itself), so it needs no lock.
	lastTrustStarvedWarn time.Time
	// standingPopulationRepo is the OPTIONAL account-standing read behind the
	// auto-benched/probation WARN. nil (the default) = the standing-backpressure
	// machine is off, so the sweep is skipped at zero cost; the orchestrator wires it
	// via WithStandingPopulation only when the machine is enabled.
	standingPopulationRepo standing.Repository
	// lastStandingPopulationWarn is when the standing-population WARN last fired
	// (zero = never), the standingPopulationWarnEvery throttle clock. Touched only by
	// the single Start loop (ScanOnce is not concurrent with itself), so no lock —
	// same justification as lastTrustStarvedWarn above.
	lastStandingPopulationWarn time.Time
}

// NewFaultMonitor creates a new FaultMonitor with default settings. transitioner may be nil
// (tests / no validation engine) -> the monitor falls back to direct DeadLetterIfExhausted.
func NewFaultMonitor(
	workUnitRepo workunit.WorkUnitRepository,
	assignRepo assignment.Repository,
	checkpointRepo checkpoint.Repository,
	leafRepo leaf.Repository,
	reliabilityRepo reliability.Repository,
	transitioner *transition.Transitioner,
	logger *slog.Logger,
) *FaultMonitor {
	return &FaultMonitor{
		workUnitRepo:    workUnitRepo,
		assignRepo:      assignRepo,
		checkpointRepo:  checkpointRepo,
		leafRepo:        leafRepo,
		reliabilityRepo: reliabilityRepo,
		transitioner:    transitioner,
		logger:          logger,
		scanInterval:    30 * time.Second,
		batchSize:       100,
	}
}

// WithStandingPopulation wires the OPTIONAL account-standing read that backs the
// auto-benched/probation operator WARN (warnStandingPopulation). Left unset the sweep
// is a no-op; the orchestrator calls this only when the standing-backpressure machine
// is enabled, so the feature stays zero-cost by default. Chainable; returns the
// monitor so it can be composed onto NewFaultMonitor without widening its signature.
func (m *FaultMonitor) WithStandingPopulation(repo standing.Repository) *FaultMonitor {
	m.standingPopulationRepo = repo
	return m
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

		// TODO #54: a timed-out (EXPIRED) or abandoned (ABANDONED) copy is wasted work — a
		// bad reliability signal for the machine that held it (host_id, folding onto the
		// account id when no host was reported). Best-effort: a failure is logged and the
		// scan continues (the signal is pure dispatch shaping, never correctness-bearing).
		if m.reliabilityRepo != nil {
			hostKey := cp.VolunteerID
			if cp.HostID != nil {
				hostKey = *cp.HostID
			}
			if err := m.reliabilityRepo.RecordOutcome(ctx, hostKey, false); err != nil {
				m.logger.Warn("failed to record host reliability for timed-out copy",
					"host_id", hostKey, "copy_id", cp.ID, "error", err)
			}
		}

		// Delegate the post-close decision to the single transitioner (TODO #50): with the
		// copy now closed, it decides requeue (stay QUEUED, redispatch) vs dead-letter (FAILED
		// when the copy budget is exhausted with redundancy unmet and no live copy) vs — for a
		// target>quorum leaf — validate/reject from the remaining results. Behavior-identical
		// to the old direct DeadLetterIfExhausted for the dead-letter case. Falls back to the
		// direct call when no transitioner is wired (tests).
		if m.transitioner != nil {
			outcome, terr := m.transitioner.Evaluate(ctx, cp.WorkUnitID)
			if terr != nil {
				m.logger.Error("transition evaluation failed after copy close", "work_unit_id", cp.WorkUnitID, "error", terr)
				continue
			}
			if outcome == transition.OutcomeDeadLettered {
				m.cleanupCheckpointByID(ctx, cp.WorkUnitID)
			}
		} else {
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

	// 3. Trust-starved units: the trusted-corroborator reservation is holding their
	//    remaining slots for trusted subjects none have taken. Waiting is the DESIGNED
	//    behavior (an untrusted copy there could never validate the unit), but a
	//    long-lived population of them is the operator signal that trusted capacity is
	//    missing — surface it.
	m.warnTrustStarved(ctx)

	// 4. Automatic standing backpressure population: how many volunteers the machine
	//    currently holds in PROBATION or BENCHED. A no-op when the machine is off (no
	//    standing repo wired); a signal that a chunk of the fleet is being neutralized
	//    or held back when it is on.
	m.warnStandingPopulation(ctx)

	// Check for stale checkpoints across all running work units with checkpointing enabled.
	m.checkStaleCheckpoints(ctx)

	return nil
}

// warnTrustStarved surfaces the trust-starved population (see trustStarvationProber): a
// WARN naming the count, a small id sample, and the operator remedies. Skipped entirely
// when the repository does not implement the probe; free when the trust gate is off (the
// pgx probe short-circuits before querying); throttled to trustStarvedWarnEvery so a
// stable starved population does not re-log every scan. Best-effort like every other
// sweep here — a probe error is logged and the scan continues.
func (m *FaultMonitor) warnTrustStarved(ctx context.Context) {
	prober, ok := m.workUnitRepo.(trustStarvationProber)
	if !ok {
		return
	}
	if !m.lastTrustStarvedWarn.IsZero() && time.Since(m.lastTrustStarvedWarn) < trustStarvedWarnEvery {
		return
	}
	count, sample, err := prober.CountTrustStarvedUnits(ctx, 5)
	if err != nil {
		m.logger.Error("trust-starved sweep failed", "error", err)
		return
	}
	if count == 0 {
		return
	}
	m.lastTrustStarvedWarn = time.Now()
	m.logger.Warn("work units trust-starved: their remaining copies are reserved for trusted subjects, and none have taken a slot for over an hour",
		"count", count,
		"sample_work_unit_ids", sample,
		"remedy", "seed or verify trusted subjects (POST /api/v1/admin/trust), add trusted volunteer capacity, or lower the leaf's min_trusted_corroborators")
}

// warnStandingPopulation surfaces the automatic-standing-backpressure population (see
// WithStandingPopulation): a WARN naming how many volunteers the machine currently
// holds in effective PROBATION vs BENCHED, a small id sample, and the operator
// remedies. Skipped entirely when no standing repo is wired (the machine is off);
// throttled to standingPopulationWarnEvery so a stable population does not re-log every
// scan. Best-effort like every other sweep here — a read error is logged and the scan
// continues. Each row is resolved through volunteer.EffectiveStanding, so an EXPIRED
// bench counts as PROBATION (its re-entry to OK goes through the backpressure exit
// threshold), never as BENCHED.
func (m *FaultMonitor) warnStandingPopulation(ctx context.Context) {
	if m.standingPopulationRepo == nil {
		return
	}
	if !m.lastStandingPopulationWarn.IsZero() && time.Since(m.lastStandingPopulationWarn) < standingPopulationWarnEvery {
		return
	}
	entries, err := m.standingPopulationRepo.AllNonOK(ctx)
	if err != nil {
		m.logger.Error("standing-population sweep failed", "error", err)
		return
	}
	now := time.Now()
	var benched, probation int
	sample := make([]types.ID, 0, 5)
	for id, e := range entries {
		switch volunteer.EffectiveStanding(e.Standing, e.BenchedUntil, now) {
		case volunteer.StandingBenched:
			benched++
		case volunteer.StandingProbation:
			probation++
		default:
			// An AllNonOK row's stored standing is PROBATION or BENCHED, so it always
			// resolves to one of the two cases above; guard defensively regardless.
			continue
		}
		if len(sample) < 5 {
			sample = append(sample, id)
		}
	}
	// Nothing effectively non-OK -> no WARN and, crucially, no throttle stamp, so the
	// next scan re-reads and the WARN fires the moment a population first appears.
	if benched == 0 && probation == 0 {
		return
	}
	m.lastStandingPopulationWarn = now
	m.logger.Warn("volunteers held by automatic standing backpressure: PROBATION accounts are still dispatched and credited but never count toward agreement or cover redundancy; BENCHED accounts get no new work until their bench expires",
		"benched", benched,
		"probation", probation,
		"sample_volunteer_ids", sample,
		"remedy", "inspect with GET /api/v1/admin/standing, release with POST /api/v1/admin/standing/clear; thresholds are the LETTUCE_HEAD_STANDING_* knobs")
}

// cleanupCheckpointByID deletes checkpoint data for a work unit (best effort).
func (m *FaultMonitor) cleanupCheckpointByID(ctx context.Context, workUnitID types.ID) {
	if err := m.checkpointRepo.Delete(ctx, workUnitID); err != nil {
		// H-8: a failed checkpoint cleanup is a real failure (leaked checkpoint storage),
		// not Debug noise — promote so it is visible.
		m.logger.Warn("failed to clean up checkpoint", "work_unit_id", workUnitID, "error", err)
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
