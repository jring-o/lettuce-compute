package contentverify

// sweep.go is the SQL plumbing around the pure state machine in dispose.go: one tick
// claims due rows (FOR UPDATE OF results SKIP LOCKED so a two-leader failover window
// cannot double-process a row), fetches the batch CONCURRENTLY (audit F1 — a slow
// origin adds ≤ 120s to its OWN row only, never head-of-line-blocking honest rows),
// then applies every disposition SERIALLY on the claim tx (pgx.Tx is not safe for
// concurrent use) and commits. Promoted units are re-evaluated AFTER commit, so the
// transitioner sees the PENDING row.

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// sweep runs one tick of the §10.6 pipeline. Any tx/scan error aborts the tick (the
// next tick retries) and is never fatal; an empty batch is the common case and a
// single indexed no-row query.
func (w *Worker) sweep(ctx context.Context) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.logger.Error("content verification sweep: begin tx failed", "error", err)
		return
	}
	// A no-op after Commit; on any early return it releases the row locks.
	defer tx.Rollback(ctx)

	rows, err := w.claim(ctx, tx)
	if err != nil {
		w.logger.Error("content verification sweep: claim failed", "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}

	// A single clock for the tick: the expiry lane compares against it, and both decide
	// passes for a fetched row must agree.
	now := time.Now()

	// Pre-fetch decision per row: the expiry lane, the knob gate, and the re-check
	// resolve without I/O; a row that still needs a fetch is dispatched to its own
	// goroutine with its own per-row deadline (inside Fetch). All DB writes wait until
	// every fetch has finished.
	dispositions := make([]disposition, len(rows))
	var wg sync.WaitGroup
	for i := range rows {
		d := decide(rows[i], w.fetchEnabled, w.globalMaxBytes, now, fetchOutcome{})
		if d.action != actionFetch {
			dispositions[i] = d
			continue
		}
		wg.Add(1)
		go func(i int, fetchCap int64) {
			defer wg.Done()
			hash, ferr := Fetch(ctx, w.client, rows[i].outputDataRef, fetchCap)
			var fe *FetchError
			if ferr != nil && !errors.As(ferr, &fe) {
				// Fetch's contract is *FetchError; treat anything else as transient.
				fe = &FetchError{Code: CodeNetworkError, Transient: true, Err: ferr}
			}
			dispositions[i] = decide(rows[i], w.fetchEnabled, w.globalMaxBytes, now,
				fetchOutcome{fetched: true, hash: hash, err: fe})
		}(i, d.fetchCap)
	}
	wg.Wait()

	// Apply serially on the claim tx, then commit.
	var promoted []promotedUnit
	for i := range rows {
		if p, ok := w.apply(ctx, tx, rows[i], dispositions[i]); ok {
			promoted = append(promoted, p)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		w.logger.Error("content verification sweep: commit failed", "error", err)
		return
	}

	// Post-commit re-evaluation: the transitioner owns the COMPLETED mark and the quorum
	// decision (the browser-submit precedent). Evaluate failure is WARN-and-continue —
	// the unit is re-adjudicated on its next natural event, the same best-effort posture
	// as submit.
	for _, p := range promoted {
		if err := w.evaluate(ctx, p.workUnitID); err != nil {
			w.logger.Warn("content verification: evaluate after promotion failed",
				"error", err, "work_unit_id", p.workUnitID, "result_id", p.resultID)
		}
	}
}

// claim selects up to batchSize due rows FOR UPDATE OF results SKIP LOCKED, ordered by
// due time to ride idx_results_content_fetch_due, joining work_units and leafs so each
// row carries its CURRENT unit state and leaf config. A cascaded parent delete removes
// the results row too, so an INNER JOIN cannot strand a due row (§10.6 step 1). The
// validation_config / data_config JSONB columns scan straight into the leaf structs via
// pgx's jsonb codec (the scanLeaf precedent). All rows are drained into memory before
// this returns, so the tx is free for the apply-pass Execs.
func (w *Worker) claim(ctx context.Context, tx pgx.Tx) ([]rowSnapshot, error) {
	rows, err := tx.Query(ctx, `
		SELECT r.id, r.work_unit_id, r.volunteer_id, r.host_id,
		       r.output_data_ref, r.output_checksum, r.content_fetch_attempts, r.created_at,
		       wu.state, l.id, l.validation_config, l.data_config
		FROM results r
		JOIN work_units wu ON wu.id = r.work_unit_id
		JOIN leafs l ON l.id = wu.leaf_id
		WHERE r.validation_status = 'AWAITING_CONTENT_VERIFICATION'
		  AND r.content_fetch_next_attempt_at <= now()
		ORDER BY r.content_fetch_next_attempt_at
		LIMIT $1
		FOR UPDATE OF r SKIP LOCKED`,
		batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []rowSnapshot
	for rows.Next() {
		var snap rowSnapshot
		var ref *string
		if err := rows.Scan(
			&snap.resultID, &snap.workUnitID, &snap.volunteerID, &snap.hostID,
			&ref, &snap.claimedChecksum, &snap.attempts, &snap.createdAt,
			&snap.unitState, &snap.leafID, &snap.valCfg, &snap.dataCfg,
		); err != nil {
			return nil, err
		}
		if ref != nil {
			snap.outputDataRef = *ref
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// promotedUnit identifies a row whose promotion landed, for the post-commit Evaluate.
type promotedUnit struct {
	resultID   types.ID
	workUnitID types.ID
}

// apply writes one row's disposition on the claim tx. Every write is guarded on
// validation_status = 'AWAITING_CONTENT_VERIFICATION': 0 rows affected means another
// actor resolved the row first, so the disposition is dropped silently. Returns the
// promoted unit (and true) ONLY when a promotion actually landed, so a raced promotion
// does not trigger a spurious Evaluate.
func (w *Worker) apply(ctx context.Context, tx pgx.Tx, snap rowSnapshot, d disposition) (promotedUnit, bool) {
	switch d.action {
	case actionNone:
		return promotedUnit{}, false

	case actionPromote:
		// Terminal-unit door (★BG-21h): decide() checked the CLAIM-TIME unit state, but a
		// fetch adds up to its full per-row deadline between that read and this write, and
		// the claim locks only the results row — so the unit can finalize (VALIDATE or
		// dead-letter) in the window, and an unguarded promotion would land a PENDING row
		// under a terminal unit that nothing can ever adjudicate: post-commit Evaluate
		// no-ops on terminal states and every recovery-sweep shape excludes them. Mirror
		// the submit door's under-lock check: take the work_units row lock — the same
		// serializer the finalization tx and both submit surfaces use — and re-decide on
		// the state read UNDER it. If the finalization committed first, the locked read
		// sees the terminal state and the row terminates UNIT_FINALIZED (exactly decide()'s
		// own fetch-time rule for a claim-time-terminal unit); if this promotion wins the
		// lock instead, the finalization tx queues behind this tick's commit and its in-tx
		// PENDING recheck then sees the promoted row and retries with it included.
		var unitState workunit.WorkUnitState
		if err := tx.QueryRow(ctx,
			`SELECT state FROM work_units WHERE id = $1 FOR UPDATE`,
			snap.workUnitID).Scan(&unitState); err != nil {
			w.logger.Error("content verification: promote unit-state recheck failed",
				"error", err, "result_id", snap.resultID, "work_unit_id", snap.workUnitID)
			return promotedUnit{}, false
		}
		if unitState == workunit.WorkUnitStateValidated || unitState == workunit.WorkUnitStateFailed {
			return w.applyTerminal(ctx, tx, snap, terminal(CodeUnitFinalized, ""))
		}
		// Overwrite output_checksum with the head's own hash so EVERY downstream reader
		// — comparisonKey AND the attestation builder — votes on the verified value.
		tag, err := tx.Exec(ctx, `
			UPDATE results SET
				validation_status = 'PENDING',
				verified_output_checksum = $2,
				output_checksum = $2,
				content_fetch_next_attempt_at = NULL,
				content_fetch_last_error = NULL,
				updated_at = now()
			WHERE id = $1 AND validation_status = 'AWAITING_CONTENT_VERIFICATION'`,
			snap.resultID, d.servedHash)
		if err != nil {
			w.logger.Error("content verification: promote update failed",
				"error", err, "result_id", snap.resultID)
			return promotedUnit{}, false
		}
		if tag.RowsAffected() == 0 {
			return promotedUnit{}, false
		}
		w.logger.Info("external output verified; result promoted to pending",
			"result_id", snap.resultID, "work_unit_id", snap.workUnitID,
			"leaf_id", snap.leafID, "verified_checksum", d.servedHash)
		if d.claimMismatch {
			// Diagnostic only (§10.7, audit F2): the ref votes on the served hash; wrong
			// content surfaces later as an ordinary DISAGREED, never a slice-5 sanction.
			w.logger.Info("external output served checksum differs from claimed; voting on served",
				"result_id", snap.resultID, "work_unit_id", snap.workUnitID)
		}
		return promotedUnit{resultID: snap.resultID, workUnitID: snap.workUnitID}, true

	case actionRetry:
		if _, err := tx.Exec(ctx, `
			UPDATE results SET
				content_fetch_attempts = $2,
				content_fetch_next_attempt_at = now() + make_interval(secs => $3),
				content_fetch_last_error = $4,
				updated_at = now()
			WHERE id = $1 AND validation_status = 'AWAITING_CONTENT_VERIFICATION'`,
			snap.resultID, d.attempts, int(retryDelay.Seconds()), d.lastError); err != nil {
			w.logger.Error("content verification: retry update failed",
				"error", err, "result_id", snap.resultID)
		}
		return promotedUnit{}, false

	case actionTerminal:
		return w.applyTerminal(ctx, tx, snap, d)
	}
	return promotedUnit{}, false
}

// applyTerminal writes a terminal (CONTENT_VERIFICATION_FAILED) disposition on the claim tx.
// Shared by the actionTerminal case and the promote path's terminal-unit door (★BG-21h),
// which downgrades a promotion to terminal(CodeUnitFinalized) when the under-lock re-check
// finds the unit already finalized.
func (w *Worker) applyTerminal(ctx context.Context, tx pgx.Tx, snap rowSnapshot, d disposition) (promotedUnit, bool) {
	tag, err := tx.Exec(ctx, `
		UPDATE results SET
			validation_status = 'CONTENT_VERIFICATION_FAILED',
			content_fetch_next_attempt_at = NULL,
			content_fetch_last_error = $2,
			updated_at = now()
		WHERE id = $1 AND validation_status = 'AWAITING_CONTENT_VERIFICATION'`,
		snap.resultID, d.lastError)
	if err != nil {
		w.logger.Error("content verification: terminal update failed",
			"error", err, "result_id", snap.resultID)
		return promotedUnit{}, false
	}
	if tag.RowsAffected() == 0 {
		return promotedUnit{}, false
	}
	w.logger.Warn("external output verification failed",
		"reason", d.reasonCode, "result_id", snap.resultID,
		"volunteer_id", snap.volunteerID, "work_unit_id", snap.workUnitID,
		"leaf_id", snap.leafID, "host", hostOf(snap.outputDataRef))
	return promotedUnit{}, false
}

// hostOf returns the URL host for the observability WARN, or "" if the ref does not
// parse (a URL_DISALLOWED terminal — the reason code already carries the detail).
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
