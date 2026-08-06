package workunit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// The operator revive (TB-39). Dead-letter parks a unit FAILED + flagged-for-review "for
// review", but review had no verb: the requeue endpoint refuses FAILED, the state chart
// gave FAILED no exit, and — correctly, since TB-35 — dispatch refuses any unit whose copy
// budget reads exhausted, which a dead-lettered unit's does by construction. So the TB-35
// remediation of 42 healthy units had to be hand-written UPDATEs against both production
// databases. ReviveDeadLettered productizes exactly that surgery, atomically.

// givebackReasonPredicateSQL selects the closed copies a revive may re-judge RETURNED:
// ABANDONED rows that verifiably never run-started AND whose recorded reason
// (outcome_reason, migration 00028) is one of the client's give-back strings — the same
// judgment the head has made on the wire flag since v0.10.8 ("work buffer full…" from the
// buffer give-back, "volunteer shutdown" from a graceful stop), and the exact predicate
// the TB-35 remediation ran. Un-started rows with OTHER reasons (prepare failures,
// runtime-unavailable, blank legacy rows) stay billed: they may be real information about
// the unit, and re-judging them is what the explicit refund override is for.
const givebackReasonPredicateSQL = `outcome = 'ABANDONED' AND started_at IS NULL
	AND (outcome_reason LIKE 'work buffer full%' OR outcome_reason = 'volunteer shutdown')`

// ReviveOutcome reports what one revive actually did, for the handler's response and the
// audit log line — every operand of the decision, not just the verdict (the TB-38 rule).
type ReviveOutcome struct {
	ReclassifiedGivebacks int
	ResurrectedResults    int
	// The post-reclassification budget reading the dispatchability verdict was made on:
	// budget-counting copies vs the effective dead-letter ceiling, error copies vs the
	// effective error cap (0 = unlimited).
	BudgetSpent   int
	BudgetCeiling int
	ErrorSpent    int
	ErrorCeiling  int
	Refunded      bool
}

// ReviveDeadLettered restores a FAILED (dead-lettered) unit to the dispatchable queue —
// the productized TB-35 surgery, one transaction under the unit row lock:
//
//  1. Re-classify the unit's verified-unstarted give-back abandons to RETURNED so the
//     budget frees (givebackReasonPredicateSQL — the judgment the head now makes on the
//     wire flag, applied retroactively to rows billed by pre-v0.10.8 clients).
//  2. Refuse — rolling everything back — if the budget still reads exhausted: a bare
//     state flip would leave the unit QUEUED but refused by every dispatch read
//     (capsNotExhaustedSQL), parked undispatchable forever. A unit whose remaining billed
//     rows are real failures is a poison unit and stays dead, unless the operator passes
//     refundRealFailures: then the budget is rebased on top of everything still billed
//     (RefundCopyBudget's absolute-write shape, FAILED-guarded), with the fresh ceilings
//     the caller resolved.
//  3. Resurrect the unit's SUPERSEDED results to PENDING — the ★BG-21i disposal stamped
//     them only because the unit died, and they hold redundancy slots the moment the unit
//     is QUEUED again. (A unit that earlier finalized VALIDATED may also carry
//     version-residue SUPERSEDED rows from that disposal; resurrecting those too is
//     harmless — results are never compared across versions, so they either corroborate
//     honestly within their own version group or are disposed again at the next
//     finalization.)
//  4. Flip FAILED → QUEUED with the full requeue field semantics (TransitionToQueued's:
//     priority raised, reassignment counted, stale assignment fields and the dispatch
//     claim cleared) and the review flag lowered.
//
// The caller invalidates the in-memory dispatch state afterwards (the PB-9 hook), exactly
// as the requeue handler does.
func (r *PgxWorkUnitRepository) ReviveDeadLettered(ctx context.Context, id types.ID, refundRealFailures bool, freshTotal, freshError int) (*ReviveOutcome, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, apierror.Internal("failed to begin revive transaction", err)
	}
	// No-op after Commit; on any early return it releases the row lock AND undoes the
	// reclassification — a refused revive touches nothing.
	defer func() { _ = tx.Rollback(context.Background()) }()

	// The hard serializer first, then every read on the post-lock snapshot — the
	// DeadLetterIfExhausted discipline, so a revive racing a submit/sweep sees the truth.
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM work_units WHERE id = $1 FOR UPDATE`, id).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.NotFound("work_unit", id.String())
		}
		return nil, apierror.Internal("failed to lock work unit for revive", err)
	}
	if state != string(WorkUnitStateFailed) {
		return nil, apierror.Conflict(
			"only a dead-lettered (FAILED) work unit can be revived",
			map[string]string{"code": "INVALID_REVIVE_STATE", "current_state": state},
		)
	}

	out := &ReviveOutcome{}

	tag, err := tx.Exec(ctx, `
		UPDATE work_unit_assignment_history
		SET outcome = 'RETURNED'
		WHERE work_unit_id = $1 AND `+givebackReasonPredicateSQL,
		id,
	)
	if err != nil {
		return nil, apierror.Internal("failed to reclassify give-back copies", err)
	}
	out.ReclassifiedGivebacks = int(tag.RowsAffected())

	if refundRealFailures {
		// RefundCopyBudget's absolute-write shape with a FAILED guard: an absolute
		// ceiling on top of everything STILL billed after the reclassification above,
		// never a += on the default-0 column (which would materialize a ceiling below
		// the copies already consumed — the trap its doc comment records).
		if _, err := tx.Exec(ctx, `
			UPDATE work_units SET
				max_total_copies = `+budgetCopiesSQL("$1")+` + $2,
				max_error_copies = CASE WHEN $3 > 0 THEN `+errorCopiesSQL("$1")+` + $3 ELSE max_error_copies END,
				updated_at = now()
			WHERE id = $1 AND state = 'FAILED'`,
			id, freshTotal, freshError,
		); err != nil {
			return nil, apierror.Internal("failed to refund work unit copy budget for revive", err)
		}
		out.Refunded = true
	}

	// The dispatchability probe, on the post-reclassification (and post-refund) rows:
	// the same disjunct as transition.capsExhausted / the dead-letter ceiling, built from
	// the same shared fragments. Refusing here — not after the flip — is the whole point:
	// since TB-35 every dispatch read embeds capsNotExhaustedSQL, so a revived unit whose
	// budget still reads exhausted would sit QUEUED and never be staged, claimed, or
	// offered — dead in a different state, with the operator none the wiser.
	if err := tx.QueryRow(ctx, `
		SELECT `+budgetCopiesSQL("wu.id")+`, `+effMaxTotalWuL+`,
		       `+errorCopiesSQL("wu.id")+`, `+effMaxErrorWuL+`
		FROM work_units wu JOIN leafs l ON l.id = wu.leaf_id
		WHERE wu.id = $1`,
		id,
	).Scan(&out.BudgetSpent, &out.BudgetCeiling, &out.ErrorSpent, &out.ErrorCeiling); err != nil {
		return nil, apierror.Internal("failed to probe revive budget", err)
	}
	if out.BudgetSpent >= out.BudgetCeiling || (out.ErrorCeiling > 0 && out.ErrorSpent >= out.ErrorCeiling) {
		// Rollback (deferred) undoes the reclassification too: a refused revive is a
		// pure read. The message names every operand of its own arithmetic — a refusal
		// with partial numbers is what generates the next support round (TB-41's
		// prior-art lesson, one subsystem over).
		return nil, apierror.Conflict(
			fmt.Sprintf("work unit's copy budget still reads exhausted after re-judging %d give-back cop(ies): %d budget-counting copies >= ceiling %d (error copies %d, cap %d; 0 = unlimited) — the remaining billed copies are real failures, so this is a poison unit; pass refund_real_failures to revive it with a fresh budget anyway",
				out.ReclassifiedGivebacks, out.BudgetSpent, out.BudgetCeiling, out.ErrorSpent, out.ErrorCeiling),
			map[string]string{"code": "REVIVE_BUDGET_EXHAUSTED"},
		)
	}

	tag, err = tx.Exec(ctx, `
		UPDATE results SET validation_status = 'PENDING', updated_at = now()
		WHERE work_unit_id = $1 AND validation_status = 'SUPERSEDED'`,
		id,
	)
	if err != nil {
		return nil, apierror.Internal("failed to resurrect superseded results", err)
	}
	out.ResurrectedResults = int(tag.RowsAffected())

	tag, err = tx.Exec(ctx, `
		UPDATE work_units SET
			state = 'QUEUED',
			flagged_for_review = false,
			priority = $2,
			reassignment_count = reassignment_count + 1,
			assigned_volunteer_id = NULL,
			assigned_at = NULL,
			started_at = NULL,
			completed_at = NULL,
			validated_at = NULL,
			last_heartbeat_at = NULL,
			dispatch_claimed_by = NULL,
			dispatch_claim_expires_at = NULL,
			updated_at = now()
		WHERE id = $1 AND state = 'FAILED'`,
		id, string(WorkUnitPriorityHigh),
	)
	if err != nil {
		return nil, apierror.Internal("failed to requeue revived work unit", err)
	}
	if tag.RowsAffected() != 1 {
		// Unreachable while we hold the row lock; belt against a future restructure.
		return nil, apierror.Internal("revive flip matched no FAILED row", nil)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, apierror.Internal("failed to commit revive transaction", err)
	}
	return out, nil
}
