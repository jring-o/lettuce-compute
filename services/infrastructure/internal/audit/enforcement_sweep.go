package audit

import (
	"context"
	"errors"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// sweep runs one enforcement pass: select actionable roots (eligible MISMATCH originals in
// NONE/AWAITING_CONFIRMATION, oldest completed first, EnforcementBatchLimit) and run each
// through enforceRoot. A per-root failure is logged and left for the next sweep (every step
// is idempotent or bookkeeping-guarded, so a crashed/partial pass converges on re-run).
func (w *EnforcementWorker) sweep(ctx context.Context) {
	roots, err := w.deps.Audits.ListActionableRoots(ctx, EnforcementBatchLimit)
	if err != nil {
		w.deps.Logger.Error("enforcement sweep: failed to list actionable roots", "error", err)
		return
	}
	for _, root := range roots {
		if ctx.Err() != nil {
			return
		}
		if err := w.enforceRoot(ctx, root); err != nil {
			w.deps.Logger.Warn("enforcement pass failed; will retry next sweep",
				"audit_id", root.ID, "work_unit_id", root.WorkUnitID, "error", err)
		}
	}
}

// enforceRoot runs the ordered §9.3 pass for one actionable root, serialized against any
// transitioner activity on the same unit via the shared per-unit advisory lock (best-effort
// belt — correctness rests on the guarded/idempotent steps below).
func (w *EnforcementWorker) enforceRoot(ctx context.Context, root *Audit) error {
	return w.deps.Locker.WithUnitLock(ctx, root.WorkUnitID, func() error {
		return w.enforceRootLocked(ctx, root)
	})
}

func (w *EnforcementWorker) enforceRootLocked(ctx context.Context, root *Audit) error {
	// Step 0 (Gate). Defensive re-checks of the invariants the actionable-roots index
	// already enforces: a confirmation row is NEVER an enforcement root, and only an
	// eligible MISMATCH original is actionable. A row failing these must never reach
	// consequences (the F-M10 / H1 structural guards, asserted defensively here too).
	if root.ConfirmsAuditID != nil {
		w.deps.Logger.Warn("enforcement pass skipped: confirmation rows are never roots",
			"audit_id", root.ID, "confirms_audit_id", root.ConfirmsAuditID)
		return nil
	}
	if !root.EnforcementEligible || root.Verdict == nil || *root.Verdict != VerdictMismatch {
		w.deps.Logger.Warn("enforcement pass skipped: root is not an eligible MISMATCH",
			"audit_id", root.ID, "eligible", root.EnforcementEligible, "verdict", root.Verdict)
		return nil
	}

	// Step 1 (Confirmation resolution, Q1-B). Runs for EVERY root regardless of
	// enforcement_state (audit H1: the state column is bookkeeping, never the safety gate).
	// Returns proceed=true only on a COMPLETED-MISMATCH confirmation that mutually agrees
	// with the root's ground truth; otherwise it has waited / enqueued / CONTRADICTED /
	// STALLED this root and the pass is done (proceed=false, err=nil), or a step errored
	// (err!=nil → retry next sweep, state unchanged).
	proceed, groundTruths, confirm, err := w.resolveConfirmation(ctx, root)
	if err != nil || !proceed {
		return err
	}

	// --- Consequences (steps 2-10). Reached only with two independently-refuting runners
	// whose ground truths agree with each other. Any error before the ENFORCED close leaves
	// enforcement_state unchanged; the next sweep re-runs the whole pass (steps 3-8 are all
	// idempotent or bookkeeping-guarded, so re-execution converges). ---

	// Step 2 (Load the fraud set). MISMATCH refutes the WHOLE agreeing set (there is no
	// partial-quorum sanction case — §9.3 step 2).
	fraud, err := w.deps.Results.LoadFraudSet(ctx, root.WorkUnitID)
	if err != nil {
		return err
	}
	subjects := distinctSubjects(fraud)
	accounts := distinctAccounts(fraud)
	fraudIDs := make([]types.ID, 0, len(fraud))
	for _, fr := range fraud {
		fraudIDs = append(fraudIDs, fr.ResultID)
	}

	// Step 3 (Slash) every agreeing subject. Idempotent (re-slash re-zeroes). A slashed
	// subject immediately stops counting as trusted for every OTHER in-flight quorum —
	// hence slash first. The summary WARN names every slashed subject so an operator can
	// spot a slashed corroborator (this package cannot cheaply prove active-runner-ness).
	for _, subj := range subjects {
		if err := w.deps.Slasher.Slash(ctx, subj); err != nil {
			return err
		}
	}

	// Step 4 (Clawback). (a) the unit's own entries per fraud-set result; (b) all UNMATURED
	// entries of the agreeing accounts (§4.5's account-wide sweep). A nil,nil result means
	// no entry / already exhausted (cap-suppressed, legacy, or a prior pass) → skip quietly
	// (the F17 idempotent no-op).
	var (
		adjustments      []*EnforcementAdjustment
		unitAdjustments  int
		unmatAdjustments int
	)
	for _, fr := range fraud {
		adj, err := w.deps.Credit.ClawbackEntryForAudit(ctx, fr.ResultID, root.ID, ReasonAuditMismatch)
		if err != nil {
			return err
		}
		if adj != nil {
			adjustments = append(adjustments, adj)
			unitAdjustments++
		}
	}
	for _, volID := range accounts {
		adjs, err := w.deps.Credit.ClawbackUnmaturedForAudit(ctx, volID, root.ID, w.deps.MaturationDays, ReasonAuditMismatchUnmatured)
		if err != nil {
			return err
		}
		adjustments = append(adjustments, adjs...)
		unmatAdjustments += len(adjs)
	}

	// Steps 5 (Revocations) + 6 (RAC decrement), per adjustment. Revocation emission is
	// fire-and-forget (WARN + continue): the RevocationReconciler owns its recovery, so a
	// transient emit failure must not stall the whole money unwind. The RAC decrement is
	// exactly-once (rac_applied_at) but MUST succeed — a real error returns so the sweep
	// retries the pass (state still unchanged).
	revocationsEmitted := 0
	for _, adj := range adjustments {
		if err := w.deps.Revocations.EmitForAdjustment(ctx, adj.ID); err != nil {
			w.deps.Logger.Warn("revocation emission failed; reconciler will recover",
				"adjustment_id", adj.ID, "audit_id", root.ID, "error", err)
		} else {
			revocationsEmitted++
		}
		if _, err := w.deps.Credit.ApplyRACAdjustment(ctx, adj.ID); err != nil {
			return err
		}
	}

	// Step 7 (Fraud flips). Idempotent; proven safe on a terminal unit (§9.0 item 10). The
	// AGREED attestation rows stay in-table (append-only); the step-5 revocations are the
	// cryptographic record consumers net against.
	if len(fraudIDs) > 0 {
		if err := w.deps.Results.FlipToDisagreed(ctx, fraudIDs); err != nil {
			return err
		}
	}

	// Step 8 (Retroactive repair). Candidate selection AND adjudication happen inside
	// validation (this package cannot import the comparators); the worker passes the root's
	// snapshot + both runners' ground-truth bytes and reads back the report.
	report, err := w.deps.Repairer.RepairUnit(ctx, RepairRequest{
		RootAuditID:  root.ID,
		WorkUnitID:   root.WorkUnitID,
		Snapshot:     root.ComparisonSnapshot,
		GroundTruths: groundTruths,
	})
	if err != nil {
		return err
	}

	// Step 9 (Unit disposition, Q2-C). Only when NO result matched ground truth (the
	// post-repair AGREED set is empty): the head must stop serving an output it has proven
	// wrong — demote VALIDATED→REJECTED, refund the copy budget, requeue.
	disposition := "none"
	if report.AgreedAfter == 0 {
		if err := w.deps.Disposer.DemoteAndRequeue(ctx, root.WorkUnitID); err != nil {
			return err
		}
		disposition = "demote_requeue"
	}

	// Step 10 (Close). Guarded ENFORCED transition + one summary WARN (the operator page).
	// A missed guard means a concurrent pass already closed this root terminal — nothing
	// more to do.
	ok, err := w.deps.Audits.SetEnforcementState(ctx, root.ID, EnforcementEnforced)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	w.deps.Logger.Warn("audit enforcement ENFORCED",
		"audit_id", root.ID,
		"work_unit_id", root.WorkUnitID,
		"leaf_id", root.LeafID,
		"root_runner", ridStr(root.ClaimedBy),
		"confirmation_runner", ridStr(confirm.ClaimedBy),
		"subjects_slashed", subjects,
		"unit_adjustments", unitAdjustments,
		"unmatured_adjustments", unmatAdjustments,
		"revocations_emitted", revocationsEmitted,
		"results_flipped", len(fraudIDs),
		"repairs_granted", report.Repaired,
		"disposition", disposition,
	)
	return nil
}

// resolveConfirmation runs §9.3 step 1. proceed=true (with both runners' ground-truth bytes
// and the confirmation row) means the two runners independently refuted the accepted output
// AND agree with each other — consequences may run. proceed=false with err=nil means the
// pass reached a terminal-for-this-sweep decision (waiting / enqueued a fresh confirmation /
// CONTRADICTED / STALLED). err!=nil means a step failed and the pass must retry.
func (w *EnforcementWorker) resolveConfirmation(ctx context.Context, root *Audit) (bool, [][]byte, *Audit, error) {
	confirms, err := w.deps.Audits.ConfirmationsForRoot(ctx, root.ID)
	if err != nil {
		return false, nil, nil, err
	}

	// No confirmation rows (lost enqueue, or first pick-up of an AWAITING root whose
	// verdict-time enqueue was dropped): enqueue now, stay AWAITING_CONFIRMATION. A root
	// NEVER proceeds past here on zero completed confirmations (the H1 safety property).
	if len(confirms) == 0 {
		return false, nil, nil, w.enqueueAndAwait(ctx, root)
	}

	// The derived attempt count is the COUNT of confirmation rows (audit M4 — the per-row
	// attempts column is lease accounting and resets across re-enqueues); the row under
	// examination is the newest (ConfirmationsForRoot orders created_at DESC).
	attemptCount := len(confirms)
	latest := confirms[0]

	switch latest.Status {
	case StatusQueued, StatusClaimed:
		// Still in flight — wait for this sweep.
		return false, nil, nil, nil
	case StatusExpired:
		// No adjudicable verdict — escalate to a fresh runner, or STALL at the cap.
		return false, nil, nil, w.escalateOrStall(ctx, root, attemptCount)
	case StatusCompleted:
		if latest.Verdict == nil {
			// Defensive: COMPLETED implies a verdict by the CHECK constraint.
			return false, nil, nil, w.escalateOrStall(ctx, root, attemptCount)
		}
		switch *latest.Verdict {
		case VerdictMatch:
			// The second runner reproduced the ACCEPTED output — the two vetted runners
			// disagree about ground truth. No consequences; operator incident.
			return false, nil, nil, w.contradict(ctx, root, latest,
				"confirmation reproduced the accepted output")
		case VerdictInconclusive:
			return false, nil, nil, w.escalateOrStall(ctx, root, attemptCount)
		case VerdictMismatch:
			// Both runners refuted the accepted output. Consequences additionally require
			// they agree WITH EACH OTHER (both sides are RAW runner bytes — a symmetric
			// channel). An Agreement error is treated as NON-agreement: never fabricate
			// agreement into a slash.
			rootBytes, err := w.deps.Audits.GetRunnerOutput(ctx, root.ID)
			if err != nil {
				return false, nil, nil, err
			}
			confirmBytes, err := w.deps.Audits.GetRunnerOutput(ctx, latest.ID)
			if err != nil {
				return false, nil, nil, err
			}
			agree, agreeErr := w.deps.Agreement(root.ComparisonSnapshot, rootBytes, confirmBytes)
			if agreeErr != nil {
				return false, nil, nil, w.contradict(ctx, root, latest,
					"ground-truth agreement check errored: "+agreeErr.Error())
			}
			if !agree {
				return false, nil, nil, w.contradict(ctx, root, latest,
					"confirmation runner's ground truth disagrees with the root runner's (non-determinism)")
			}
			return true, [][]byte{rootBytes, confirmBytes}, latest, nil
		default:
			// Unknown verdict — treat as non-adjudicable.
			return false, nil, nil, w.escalateOrStall(ctx, root, attemptCount)
		}
	default:
		// Unknown status — wait rather than act.
		return false, nil, nil, nil
	}
}

// enqueueAndAwait enqueues a confirmation (already-enqueued is fine — the sweep backstops a
// lost enqueue and the tighter claim exclusions route it to a fresh runner) and leaves the
// root in AWAITING_CONFIRMATION.
func (w *EnforcementWorker) enqueueAndAwait(ctx context.Context, root *Audit) error {
	if _, err := w.deps.Audits.EnqueueConfirmation(ctx, root.ID); err != nil && !errors.Is(err, ErrDuplicateOpenAudit) {
		return err
	}
	if _, err := w.deps.Audits.SetEnforcementState(ctx, root.ID, EnforcementAwaitingConfirmation); err != nil {
		return err
	}
	return nil
}

// escalateOrStall enqueues a fresh confirmation while the derived attempt count is below the
// cap; at the cap it moves the root to STALLED (sticky, leaves the sweep index) with a WARN.
func (w *EnforcementWorker) escalateOrStall(ctx context.Context, root *Audit, attemptCount int) error {
	if attemptCount < MaxConfirmationAttempts {
		return w.enqueueAndAwait(ctx, root)
	}
	if _, err := w.deps.Audits.SetEnforcementState(ctx, root.ID, EnforcementStalled); err != nil {
		return err
	}
	w.deps.Logger.Warn("audit enforcement STALLED: confirmation attempts exhausted without an adjudicable second verdict",
		"audit_id", root.ID,
		"work_unit_id", root.WorkUnitID,
		"leaf_id", root.LeafID,
		"confirmation_attempts", attemptCount)
	return nil
}

// contradict moves the root to CONTRADICTED and WARNs, naming BOTH runners: two vetted
// runners disagree about ground truth (a compromised/broken runner, or a latently
// non-deterministic leaf). No consequences.
func (w *EnforcementWorker) contradict(ctx context.Context, root, confirm *Audit, reason string) error {
	if _, err := w.deps.Audits.SetEnforcementState(ctx, root.ID, EnforcementContradicted); err != nil {
		return err
	}
	w.deps.Logger.Warn("audit enforcement CONTRADICTED: two trusted runners disagree about ground truth; no consequences",
		"audit_id", root.ID,
		"work_unit_id", root.WorkUnitID,
		"leaf_id", root.LeafID,
		"root_runner", ridStr(root.ClaimedBy),
		"confirmation_runner", ridStr(confirm.ClaimedBy),
		"reason", reason)
	return nil
}

// distinctSubjects returns the DISTINCT trust subjects of the fraud set (the slash targets).
func distinctSubjects(fraud []FraudResult) []string {
	seen := make(map[string]struct{}, len(fraud))
	var out []string
	for _, fr := range fraud {
		if _, ok := seen[fr.Subject]; ok {
			continue
		}
		seen[fr.Subject] = struct{}{}
		out = append(out, fr.Subject)
	}
	return out
}

// distinctAccounts returns the DISTINCT volunteer ids of the fraud set (the unmatured-sweep
// targets).
func distinctAccounts(fraud []FraudResult) []types.ID {
	seen := make(map[types.ID]struct{}, len(fraud))
	var out []types.ID
	for _, fr := range fraud {
		if _, ok := seen[fr.VolunteerID]; ok {
			continue
		}
		seen[fr.VolunteerID] = struct{}{}
		out = append(out, fr.VolunteerID)
	}
	return out
}

// ridStr renders an optional runner id for a log field ("" when nil).
func ridStr(id *types.ID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
