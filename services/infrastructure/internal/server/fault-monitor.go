package server

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/audit"
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

// emissionAnomalyWarnEvery throttles the emission-anomaly WARN (see warnEmissionAnomaly):
// like the trust-starved sweep the probe runs every scan (and not at all when the anomaly
// halt is off), but the WARN line repeats at most this often — an emission burst is an
// operator-timescale event, so re-warning every 30s scan would only be noise.
const emissionAnomalyWarnEvery = 10 * time.Minute

// resultAuditWarnEvery throttles the result-audit WARNs (see warnResultAudits): the probe
// is one cheap aggregate query per scan (and not at all when audits are off), but new
// MISMATCH verdicts and a starving claim queue are operator-timescale events.
const resultAuditWarnEvery = 10 * time.Minute

// resultAuditQueueAgeWarn is the oldest-QUEUED age past which the claim queue counts as
// starved even if nothing has expired yet — e.g. every queued job requires a hardware
// class no registered runner presents. Well under the queue lifetime (72h) so the WARN
// fires before jobs start expiring silently.
const resultAuditQueueAgeWarn = 24 * time.Hour

// resultAuditIneligibleWarnEvery throttles the ineligible-lane WARN: a leaf sitting in a
// never-audited lane (network access, CUSTOM, unpinned NUMERIC) skips EVERY validated
// unit, so a tight throttle would re-log constantly for a single legitimate leaf. Six
// hours keeps the lane operator-visible without drowning the log.
const resultAuditIneligibleWarnEvery = 6 * time.Hour

// contentVerifyWarnEvery throttles the content-verification WARNs (see
// warnContentVerification): like the other optional probes the sweep runs every scan
// (and not at all when no probe is wired), but a stalled fetch lane, a run of terminal
// CONTENT_VERIFICATION_FAILED rows, and knob-off stragglers are all operator-timescale
// events, so each WARN line repeats at most this often.
const contentVerifyWarnEvery = 10 * time.Minute

// contentVerifyStalledAge is the oldest-held age past which the fetch-and-verify lane
// counts as stalled: a ref-only result should be fetched and hashed within seconds, so a
// row held past ten minutes means the lane is not draining (leadership flapping, the
// fetch worker is dead, or the origin is unreachable).
const contentVerifyStalledAge = 10 * time.Minute

// unitEvaluator re-drives a single work unit's state decision (validate / reject /
// requeue / dead-letter). *transition.Transitioner satisfies it; the fault monitor depends
// on this narrow interface rather than the concrete transitioner so its re-evaluate calls —
// the post-copy-close decision and the spot-check reclaim re-evaluate (design §4.4) — are
// unit-testable with a spy.
type unitEvaluator interface {
	Evaluate(ctx context.Context, workUnitID types.ID) (transition.Outcome, error)
}

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
	// Held as the narrow unitEvaluator interface (see above) so the re-evaluate calls are
	// unit-testable; *transition.Transitioner satisfies it.
	transitioner unitEvaluator
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
	// emissionAnomalyCheck is the OPTIONAL global emission-anomaly probe behind the
	// emission-anomaly operator WARN. nil (the default) = the sweep is skipped at zero
	// cost; the orchestrator wires it via WithEmissionAnomalyCheck only when the emission
	// anomaly halt is armed. The closure reports whether the export circuit breaker has
	// tripped plus the figures the WARN names.
	emissionAnomalyCheck func(ctx context.Context) (halted bool, today, baseline float64, err error)
	// lastEmissionAnomalyWarn is when the emission-anomaly WARN last fired (zero = never),
	// the emissionAnomalyWarnEvery throttle clock. Touched only by the single Start loop
	// (ScanOnce is not concurrent with itself), so no lock — same as lastTrustStarvedWarn.
	lastEmissionAnomalyWarn time.Time
	// auditStats is the OPTIONAL result-audit health probe behind the observe-only audit
	// WARNs (warnResultAudits). nil (the default) = audits are off, sweep skipped at zero
	// cost; the orchestrator wires it via WithResultAuditStats only when
	// LETTUCE_HEAD_RESULT_AUDIT_ENABLED is set.
	auditStats func(ctx context.Context) (audit.Stats, error)
	// Result-audit baselines: lifetime totals observed on the FIRST probe of this
	// process, so restart never re-pages historical verdicts; deltas above the baseline
	// are what WARN. Touched only by the single Start loop, so no lock.
	auditBaselineSet              bool
	auditMismatchBaseline         int
	auditExpiredBaseline         int
	auditIneligibleBaseline       int64
	lastResultAuditWarn           time.Time
	lastResultAuditQueueWarn      time.Time
	lastResultAuditIneligibleWarn time.Time
	// enforcementMaturationDays > 0 arms the slice-3 enforcement lanes inside
	// warnResultAudits (design §9.8): ENFORCED delta, CONTRADICTED delta (a
	// trusted-runner conflict is an incident), STALLED count, and the enforcement-
	// horizon aging guard (an actionable root older than half the maturation window
	// WARNs every scan — the live backstop for workloads whose lease-scaled horizon
	// outruns the static Validate() bound). Set via WithEnforcementWatch only when
	// LETTUCE_HEAD_AUDIT_ENFORCEMENT_ENABLED is on.
	enforcementMaturationDays  int
	auditEnforcedBaseline      int
	auditContradictedBaseline  int
	lastEnforcementWarn        time.Time
	lastEnforcementStalledWarn time.Time
	// contentVerifyStats is the OPTIONAL fetch-and-verify health probe behind the
	// observe-only content-verification WARNs (warnContentVerification): a stalled fetch
	// lane, new terminal CONTENT_VERIFICATION_FAILED rows, and knob-off stragglers. nil
	// (the default) = no probe wired, sweep skipped at zero cost; the orchestrator wires
	// it via WithContentVerificationStats. Unlike the result-audit probe it is wired even
	// when the content-fetch knob is OFF, so the knob-off-with-held lane can observe the
	// expiry drain (the closure reports the knob state in FetchEnabled).
	contentVerifyStats func(ctx context.Context) (ContentVerificationStats, error)
	// contentVerifyBaselineSet / contentVerifyFailedBaseline boot-baseline the terminal
	// CONTENT_VERIFICATION_FAILED total on the FIRST probe so pre-existing failed rows
	// never page at startup; only the delta above the baseline WARNs. Touched only by the
	// single Start loop, so no lock — same as the audit baselines above.
	contentVerifyBaselineSet    bool
	contentVerifyFailedBaseline int
	// Content-verification WARN throttle clocks (zero = never), one per lane so a stalled
	// page never suppresses a failed-delta or knob-off page. Same single-loop no-lock
	// justification as the other throttle clocks above.
	lastContentVerifyStalledWarn time.Time
	lastContentVerifyFailedWarn  time.Time
	lastContentVerifyKnobOffWarn time.Time
}

// ContentVerificationStats is the health snapshot behind the fetch-and-verify observe-only
// WARNs (warnContentVerification), composed by the orchestrator's probe closure from a
// single aggregate query over the results table (design doc §10.10). It carries the held
// (AWAITING_CONTENT_VERIFICATION) population and its oldest age, the lifetime terminal
// CONTENT_VERIFICATION_FAILED total, and whether the content-fetch knob is currently on.
type ContentVerificationStats struct {
	// Held is the current AWAITING_CONTENT_VERIFICATION backlog size.
	Held int
	// OldestHeldAge is the age of the oldest held row (0 when none are held). A large
	// value means the fetch lane is not draining.
	OldestHeldAge time.Duration
	// FailedTotal is the lifetime count of CONTENT_VERIFICATION_FAILED rows (the terminal
	// did-not-become-votable state); the delta above the boot baseline is what WARNs.
	FailedTotal int
	// FetchEnabled mirrors LETTUCE_HEAD_CONTENT_FETCH_ENABLED: with fetching off, held
	// rows can only drain via the 24h holding-expiry lane, never through verification.
	FetchEnabled bool
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
	m := &FaultMonitor{
		workUnitRepo:    workUnitRepo,
		assignRepo:      assignRepo,
		checkpointRepo:  checkpointRepo,
		leafRepo:        leafRepo,
		reliabilityRepo: reliabilityRepo,
		logger:          logger,
		scanInterval:    30 * time.Second,
		batchSize:       100,
	}
	// Assign the transitioner only when non-nil so a typed-nil *transition.Transitioner does
	// not become a non-nil interface value — the nil-interface fallback to the direct
	// DeadLetterIfExhausted path (tests / no validation engine) must keep working.
	if transitioner != nil {
		m.transitioner = transitioner
	}
	return m
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

// WithEmissionAnomalyCheck wires the OPTIONAL global emission-anomaly probe that backs the
// emission-anomaly operator WARN (warnEmissionAnomaly). Left unset the sweep is a no-op;
// the orchestrator calls this only when the emission anomaly halt is enabled, so the
// feature stays zero-cost by default. Chainable; returns the monitor so it can be composed
// onto NewFaultMonitor without widening its signature.
func (m *FaultMonitor) WithEmissionAnomalyCheck(check func(ctx context.Context) (halted bool, today, baseline float64, err error)) *FaultMonitor {
	m.emissionAnomalyCheck = check
	return m
}

// WithResultAuditStats wires the OPTIONAL result-audit health probe that backs the
// observe-only audit WARNs (warnResultAudits): new MISMATCH verdicts (the whole point of
// the audit net — findings someone must look at) and a dying queue (EXPIRED growth or a
// claim-starved backlog, the failure mode where the net silently stops observing). Left
// unset the sweep is a no-op; the orchestrator wires it only when result audits are
// enabled. Chainable, same pattern as the other optional probes.
func (m *FaultMonitor) WithResultAuditStats(stats func(ctx context.Context) (audit.Stats, error)) *FaultMonitor {
	m.auditStats = stats
	return m
}

// WithEnforcementWatch arms the slice-3 enforcement lanes of the result-audit sweep
// (design §9.8). maturationDays is the head's credit-maturation window; the aging guard
// pages when an actionable root has waited past half of it. Wired only when audit
// enforcement is enabled; requires WithResultAuditStats to be wired too.
func (m *FaultMonitor) WithEnforcementWatch(maturationDays int) *FaultMonitor {
	m.enforcementMaturationDays = maturationDays
	return m
}

// WithContentVerificationStats wires the OPTIONAL fetch-and-verify health probe that backs
// the observe-only content-verification WARNs (warnContentVerification): a stalled fetch
// lane, new terminal CONTENT_VERIFICATION_FAILED rows, and knob-off stragglers waiting on
// the holding-expiry lane. Left unset the sweep is a no-op; the orchestrator wires it
// whenever the results table can hold ref rows (independent of the content-fetch knob, so
// the knob-off-with-held lane stays observable). Chainable, same pattern as the other
// optional probes.
func (m *FaultMonitor) WithContentVerificationStats(stats func(ctx context.Context) (ContentVerificationStats, error)) *FaultMonitor {
	m.contentVerifyStats = stats
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
			continue
		}
		m.logger.Info("spot-check timed out, accepting single result", "work_unit_id", wu.ID)

		// Re-evaluate now that the spot-check flag is cleared (design §4.4, BG-21d): clearing
		// drops the resolved quorum to 1, so the unit's single PENDING result can validate.
		// Without this the unit sat QUEUED forever with one complete, never-credited result.
		// Best-effort — the same posture the copy-close loop above uses; the recovery sweep's
		// QUEUED-at-quorum predicate independently backstops a lost re-evaluation.
		if m.transitioner != nil {
			if _, err := m.transitioner.Evaluate(ctx, wu.ID); err != nil {
				m.logger.Warn("failed to re-evaluate work unit after spot-check clear",
					"work_unit_id", wu.ID, "error", err)
			}
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

	// 3b. Emission anomaly circuit breaker: WARN when today's global grant total has
	//     tripped the anomaly halt and the public credit export has self-frozen (503). A
	//     no-op when the halt is off (no probe wired); the operator "page" for the freeze.
	m.warnEmissionAnomaly(ctx)

	// 3c. Result-audit health: WARN on new MISMATCH verdicts (observe-only findings) and
	//     on a dying audit queue (EXPIRED growth / claim starvation). A no-op when result
	//     audits are off (no probe wired).
	m.warnResultAudits(ctx)

	// 3d. Content-verification (fetch-and-verify) health: WARN on a stalled fetch lane
	//     (ref-only results held too long), new terminal CONTENT_VERIFICATION_FAILED rows,
	//     and knob-off stragglers draining on the holding-expiry lane. A no-op when no
	//     probe is wired.
	m.warnContentVerification(ctx)

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

// warnEmissionAnomaly surfaces a TRIPPED emission anomaly circuit breaker: a WARN naming
// today's total grant, the trailing baseline, and the operator remedies. Skipped entirely
// when no probe is wired (the anomaly halt is off); throttled to emissionAnomalyWarnEvery
// so a stable anomaly does not re-log every scan. Only warns when the breaker is actually
// tripped, so a healthy head is silent and — like the zero-count trust-starved sweep —
// never arms the throttle, meaning the WARN fires on the very scan the anomaly first
// appears. Best-effort like every other sweep here — a probe error is logged and the scan
// continues.
func (m *FaultMonitor) warnEmissionAnomaly(ctx context.Context) {
	if m.emissionAnomalyCheck == nil {
		return
	}
	if !m.lastEmissionAnomalyWarn.IsZero() && time.Since(m.lastEmissionAnomalyWarn) < emissionAnomalyWarnEvery {
		return
	}
	halted, today, baseline, err := m.emissionAnomalyCheck(ctx)
	if err != nil {
		m.logger.Error("emission-anomaly sweep failed", "error", err)
		return
	}
	if !halted {
		return
	}
	m.lastEmissionAnomalyWarn = time.Now()
	m.logger.Warn("emission anomaly: today's granted credit far exceeds the trailing baseline, and the public credit export has self-frozen (503) until the burst enters the baseline window or the operator intervenes",
		"today", today,
		"baseline", baseline,
		"remedy", "investigate the credit burst; if legitimate, raise LETTUCE_HEAD_EMISSION_ANOMALY_FACTOR or disable the halt (LETTUCE_HEAD_EMISSION_ANOMALY_HALT_ENABLED=false) and restart the head; the export stays frozen meanwhile")
}

// warnResultAudits surfaces result-audit health (see WithResultAuditStats). Two
// independent signals, each with its own throttle clock so a mismatch page never
// suppresses a starvation page: (1) MISMATCH verdicts recorded since this process's
// baseline — the audit net found something a human must look at (observe-only: nothing
// slashes, but silence here would make the whole mechanism decorative); (2) queue decay —
// EXPIRED growth since baseline, or an oldest-QUEUED age past resultAuditQueueAgeWarn
// (claim starvation: e.g. every queued job pins a hardware class no registered runner
// presents). Baselines are the lifetime totals at the FIRST probe, so a head restart
// never re-pages historical rows. Best-effort like every other sweep here.
func (m *FaultMonitor) warnResultAudits(ctx context.Context) {
	if m.auditStats == nil {
		return
	}
	mismatchThrottled := !m.lastResultAuditWarn.IsZero() && time.Since(m.lastResultAuditWarn) < resultAuditWarnEvery
	queueThrottled := !m.lastResultAuditQueueWarn.IsZero() && time.Since(m.lastResultAuditQueueWarn) < resultAuditWarnEvery
	ineligibleThrottled := !m.lastResultAuditIneligibleWarn.IsZero() && time.Since(m.lastResultAuditIneligibleWarn) < resultAuditIneligibleWarnEvery
	if mismatchThrottled && queueThrottled && ineligibleThrottled {
		return
	}
	stats, err := m.auditStats(ctx)
	if err != nil {
		m.logger.Error("result-audit sweep failed", "error", err)
		return
	}
	ineligibleTotal := int64(0)
	for _, n := range stats.IneligibleByLeaf {
		ineligibleTotal += n
	}
	if !m.auditBaselineSet {
		m.auditBaselineSet = true
		m.auditMismatchBaseline = stats.MismatchTotal
		m.auditExpiredBaseline = stats.ExpiredTotal
		m.auditIneligibleBaseline = ineligibleTotal
		m.auditEnforcedBaseline = stats.EnforcedTotal
		m.auditContradictedBaseline = stats.ContradictedTotal
		return
	}
	if !mismatchThrottled {
		if delta := stats.MismatchTotal - m.auditMismatchBaseline; delta > 0 {
			m.lastResultAuditWarn = time.Now()
			m.auditMismatchBaseline = stats.MismatchTotal
			m.logger.Warn("result audits recorded MISMATCH verdicts: a trusted re-execution contradicted an accepted quorum output (observe-only: nothing was slashed or clawed back)",
				"new_mismatches", delta,
				"remedy", "review GET /api/v1/admin/audit/results?verdict=MISMATCH; investigate the agreeing accounts and the leaf before enabling any enforcement")
		}
	}
	if !queueThrottled {
		expiredDelta := stats.ExpiredTotal - m.auditExpiredBaseline
		starved := stats.OldestQueuedAge > resultAuditQueueAgeWarn
		if expiredDelta > 0 || starved {
			m.lastResultAuditQueueWarn = time.Now()
			m.auditExpiredBaseline = stats.ExpiredTotal
			m.logger.Warn("result-audit queue is decaying: jobs are expiring unserviced or the oldest queued job has waited too long",
				"new_expired", expiredDelta,
				"queued", stats.QueuedCount,
				"oldest_queued_age", stats.OldestQueuedAge.Truncate(time.Minute).String(),
				"remedy", "check that at least one registered runner is running `lettuce-volunteer audit-runner`, covers the required hardware classes, and can reach the head")
		}
	}
	if !ineligibleThrottled {
		if delta := ineligibleTotal - m.auditIneligibleBaseline; delta > 0 {
			m.lastResultAuditIneligibleWarn = time.Now()
			m.auditIneligibleBaseline = ineligibleTotal
			m.logger.Warn("result audits: validated units skipped as audit-ineligible (owner-selectable never-audited lanes: network access, CUSTOM comparison, unpinned or ref-only NUMERIC)",
				"new_ineligible", delta,
				"by_leaf", topIneligibleLeaves(stats.IneligibleByLeaf, 3),
				"note", "these leaves receive no post-hoc re-execution coverage; closing the lanes is the deferred determinism_class work")
		}
	}
	m.warnEnforcement(stats)
}

// warnEnforcement is the slice-3 enforcement lane set (design §9.8), armed by
// WithEnforcementWatch. CONTRADICTED shares the enforcement throttle but is always
// included when present — two vetted runners disagreeing about ground truth is an
// incident (a compromised/broken runner OR latent leaf non-determinism). The aging
// guard is DELIBERATELY un-throttled: it is the live backstop for workloads whose
// lease-scaled enforcement horizon can outrun the static Validate() bound, and it
// pages until the root resolves or the operator intervenes.
func (m *FaultMonitor) warnEnforcement(stats audit.Stats) {
	if m.enforcementMaturationDays <= 0 {
		return
	}
	if !m.auditBaselineSet {
		return // first probe of warnResultAudits set the baselines below on its pass
	}
	throttled := !m.lastEnforcementWarn.IsZero() && time.Since(m.lastEnforcementWarn) < resultAuditWarnEvery
	if !throttled {
		enforcedDelta := stats.EnforcedTotal - m.auditEnforcedBaseline
		contradictedDelta := stats.ContradictedTotal - m.auditContradictedBaseline
		if enforcedDelta > 0 || contradictedDelta > 0 {
			m.lastEnforcementWarn = time.Now()
			m.auditEnforcedBaseline = stats.EnforcedTotal
			m.auditContradictedBaseline = stats.ContradictedTotal
			m.logger.Warn("audit enforcement activity: confirmed mismatches executed consequences and/or trusted runners contradicted each other",
				"new_enforced", enforcedDelta,
				"new_contradicted", contradictedDelta,
				"remedy", "review GET /api/v1/admin/audit/flagged-leaves and GET /api/v1/admin/audit/results?enforcement_state=ENFORCED (or CONTRADICTED — investigate BOTH runners of a contradicted root before trusting either)")
		}
	}
	stalledThrottled := !m.lastEnforcementStalledWarn.IsZero() && time.Since(m.lastEnforcementStalledWarn) < resultAuditWarnEvery
	if !stalledThrottled && stats.StalledCount > 0 {
		m.lastEnforcementStalledWarn = time.Now()
		m.logger.Warn("audit enforcement stalled: confirmed-mismatch roots exhausted their confirmation attempts without an adjudicable second verdict",
			"stalled", stats.StalledCount,
			"inconclusive_by_runner", topRunnerInconclusive(stats.InconclusiveByRunner, 3),
			"remedy", "enforcement liveness wants >= 3 independent registered runners spanning >= 2 hardware classes; the manual levers (admin trust slash + credit clawback) remain available")
	}
	// Aging guard (audit H2 backstop): un-throttled by design.
	if half := time.Duration(m.enforcementMaturationDays) * 24 * time.Hour / 2; stats.OldestAwaitingConfirmationAge > half {
		m.logger.Warn("audit enforcement horizon at risk: an actionable mismatch has waited past half the credit-maturation window without resolution",
			"oldest_awaiting_confirmation_age", stats.OldestAwaitingConfirmationAge.Truncate(time.Minute).String(),
			"maturation_days", m.enforcementMaturationDays,
			"inconclusive_by_runner", topRunnerInconclusive(stats.InconclusiveByRunner, 3),
			"remedy", "register an additional runner (different hardware class for unpinned units) or enforce manually before the fraud credit matures")
	}
}

// warnContentVerification surfaces fetch-and-verify health (see WithContentVerificationStats).
// Three independent signals, each with its own throttle clock so one page never suppresses
// another: (1) a stalled fetch lane — held (AWAITING_CONTENT_VERIFICATION) rows whose oldest
// has waited past contentVerifyStalledAge (leadership flapping, the fetch worker is dead, or
// an origin outage); (2) new terminal CONTENT_VERIFICATION_FAILED rows since this process's
// baseline (the delta a human should look at); (3) knob-off-with-held — the content-fetch
// knob is OFF yet ref rows are still held, so they can only drain on the 24h holding-expiry
// lane. Only the FAILED lane is boot-baselined (pre-existing terminal rows must not page at
// startup); the two absolute-state lanes fire on the very first probe by design, so a
// restart into an already-stalled or knob-off-with-held state pages promptly rather than
// waiting for a fresh delta. Best-effort like every other sweep here.
func (m *FaultMonitor) warnContentVerification(ctx context.Context) {
	if m.contentVerifyStats == nil {
		return
	}
	stalledThrottled := !m.lastContentVerifyStalledWarn.IsZero() && time.Since(m.lastContentVerifyStalledWarn) < contentVerifyWarnEvery
	failedThrottled := !m.lastContentVerifyFailedWarn.IsZero() && time.Since(m.lastContentVerifyFailedWarn) < contentVerifyWarnEvery
	knobOffThrottled := !m.lastContentVerifyKnobOffWarn.IsZero() && time.Since(m.lastContentVerifyKnobOffWarn) < contentVerifyWarnEvery
	if stalledThrottled && failedThrottled && knobOffThrottled {
		return
	}
	stats, err := m.contentVerifyStats(ctx)
	if err != nil {
		m.logger.Error("content-verification sweep failed", "error", err)
		return
	}
	// Boot-baseline the terminal FAILED total on the first probe so historical rows never
	// re-page after a restart; the delta below is measured from here. Unlike warnResultAudits
	// this does NOT return early, so the two absolute-state lanes still evaluate on the first
	// probe (on which the failed delta is 0 against the just-set baseline).
	if !m.contentVerifyBaselineSet {
		m.contentVerifyBaselineSet = true
		m.contentVerifyFailedBaseline = stats.FailedTotal
	}
	// Lane 1: stalled fetch lane. A held population whose oldest row has outlived the
	// stalled threshold means the lane is not draining.
	if !stalledThrottled && stats.Held > 0 && stats.OldestHeldAge > contentVerifyStalledAge {
		m.lastContentVerifyStalledWarn = time.Now()
		m.logger.Warn("content-verification fetch lane stalled: ref-only results have sat awaiting an external-output fetch past the stalled threshold (leadership flapping, the fetch worker is dead, or the origin serving output_data_ref is unreachable)",
			"held", stats.Held,
			"oldest_held_age", stats.OldestHeldAge.Truncate(time.Minute).String(),
			"remedy", "confirm this head holds leadership and the content-verification worker is running; check that the origin serving output_data_ref is reachable; held rows otherwise drain via the 24h holding-expiry lane")
	}
	// Lane 2: new terminal CONTENT_VERIFICATION_FAILED rows since baseline.
	if !failedThrottled {
		if delta := stats.FailedTotal - m.contentVerifyFailedBaseline; delta > 0 {
			m.lastContentVerifyFailedWarn = time.Now()
			m.contentVerifyFailedBaseline = stats.FailedTotal
			m.logger.Warn("content-verification results reached CONTENT_VERIFICATION_FAILED: ref-only results ended their fetch pipeline without becoming votable (fetch failure, size cap, disallowed URL, holding expiry, or unit finalized)",
				"new_failed", delta,
				"remedy", "review the leaf's results with ?validation_status=CONTENT_VERIFICATION_FAILED and each row's content_fetch_last_error reason code")
		}
	}
	// Lane 3: knob-off with held rows. With fetching disabled no verification runs, so held
	// stragglers can only leave via the holding-expiry lane.
	if !knobOffThrottled && !stats.FetchEnabled && stats.Held > 0 {
		m.lastContentVerifyKnobOffWarn = time.Now()
		m.logger.Warn("content-fetch knob is OFF but ref-only results are still held: with fetching disabled none can be verified, so these stragglers wait on the 24h holding-expiry lane to fail them out",
			"held", stats.Held,
			"remedy", "re-enable LETTUCE_HEAD_CONTENT_FETCH_ENABLED to verify them, or let the holding-expiry lane drain them after the 24h holding lifetime")
	}
}

// topRunnerInconclusive renders the N largest per-runner confirmation-INCONCLUSIVE
// counters as a compact "runner=count" list — a runner anomalously high here is either
// broken or suppressing enforcement (audit M2: liveness denial must be attributable).
func topRunnerInconclusive(byRunner map[string]int, n int) []string {
	converted := make(map[string]int64, len(byRunner))
	for k, v := range byRunner {
		converted[k] = int64(v)
	}
	return topIneligibleLeaves(converted, n)
}

// topIneligibleLeaves renders the N largest per-leaf ineligible counters as a compact
// "leaf=count" list for the ineligible-lane WARN.
func topIneligibleLeaves(byLeaf map[string]int64, n int) []string {
	type kv struct {
		leaf  string
		count int64
	}
	all := make([]kv, 0, len(byLeaf))
	for l, c := range byLeaf {
		all = append(all, kv{l, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].count != all[j].count {
			return all[i].count > all[j].count
		}
		return all[i].leaf < all[j].leaf
	})
	if len(all) > n {
		all = all[:n]
	}
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = fmt.Sprintf("%s=%d", e.leaf, e.count)
	}
	return out
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
