package contentverify

// dispose.go is the pure per-row state machine (design doc §10.6 steps 2-5). It is
// factored out of the SQL plumbing in sweep.go so it is exhaustively table-testable
// without a database: decide() reads only a rowSnapshot, the two knobs, an injected
// clock, and a fetch outcome, and returns the disposition the serial apply pass writes.
//
// The load-bearing invariant is audit F2 (§10.7): a successful fetch ALWAYS promotes
// on the SERVED hash. The volunteer's claimed checksum is never a gate — a served/
// claimed divergence is a diagnostic INFO, never a sanction — because a stably
// transforming honest origin (CDN minify, charset/EOL normalization, re-serialize)
// cannot be distinguished from fraud, and any false-MISMATCH→slash is intolerable.

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// rowSnapshot is one claimed AWAITING_CONTENT_VERIFICATION row plus the unit state and
// leaf config read in the same claim JOIN (§10.6 step 1). It is the ONLY thing decide()
// reads about a row. NOTE unitState is CLAIM-TIME state: the fetch window (up to the
// per-row deadline) runs between the claim and the apply pass, so decide()'s
// finalized-unit check is a fast path only — the promote write re-checks the unit state
// under the work_units row lock (★BG-21h, sweep.go apply), because a unit that finalizes
// mid-fetch would otherwise take a PENDING row it can never adjudicate.
type rowSnapshot struct {
	resultID        types.ID
	workUnitID      types.ID
	leafID          types.ID
	volunteerID     types.ID
	hostID          *types.ID
	outputDataRef   string
	claimedChecksum string
	attempts        int
	createdAt       time.Time
	unitState       workunit.WorkUnitState
	valCfg          leaf.ValidationConfig
	dataCfg         leaf.DataConfig
}

// fetchOutcome carries one fetch attempt's result into decide(). On the pre-fetch pass
// fetched is false (decide then returns actionFetch if a fetch is due); after a fetch it
// is true with exactly one of hash / err set (err is nil on success).
type fetchOutcome struct {
	fetched bool
	hash    string
	err     *FetchError
}

// dispositionAction is the write the apply pass performs for a row.
type dispositionAction int

const (
	// actionNone leaves the row untouched: knob off and the row is younger than the
	// holding lifetime, so no network I/O and no write — the operator may re-enable and
	// the row is rescanned next tick.
	actionNone dispositionAction = iota
	// actionFetch means the pre-fetch checks passed and the caller must fetch with
	// fetchCap, then re-invoke decide() with the outcome.
	actionFetch
	// actionPromote flips the row PENDING on servedHash (the §10.6 success lane).
	actionPromote
	// actionRetry bumps the transient-failure budget and reschedules the next attempt.
	actionRetry
	// actionTerminal fails the row CONTENT_VERIFICATION_FAILED (reason-coded).
	actionTerminal
)

// disposition is the pure decision for one row.
type disposition struct {
	action     dispositionAction
	fetchCap   int64  // actionFetch: min(leaf cap>0, global cap)
	servedHash string // actionPromote: the head-computed hash the row votes on
	// claimMismatch marks a promotion whose served hash differs from the volunteer's
	// claim — logged as a diagnostic INFO only, NEVER a sanction (§10.7, audit F2).
	claimMismatch bool
	attempts      int    // actionRetry: the new content_fetch_attempts value
	reasonCode    string // actionRetry/actionTerminal: the machine reason code (for the WARN)
	lastError     string // actionRetry/actionTerminal: the content_fetch_last_error value
}

// terminal builds an actionTerminal disposition. lastError is the bare code, or
// "code: detail" when a detail is supplied (the validator message, the underlying
// fetch error) — the exact string stamped into content_fetch_last_error.
func terminal(code, detail string) disposition {
	d := disposition{action: actionTerminal, reasonCode: code, lastError: code}
	if detail != "" {
		d.lastError = code + ": " + detail
	}
	return d
}

// effectiveCap composes the fetch byte cap: min(leaf MaxOutputSizeBytes when > 0,
// global knob) — closing the ref bypass of the leaf output cap (§10.0 item 11). The
// global cap is already the resolved effective value (100 MB default), always > 0.
func effectiveCap(leafCap, globalCap int64) int64 {
	if leafCap > 0 && leafCap < globalCap {
		return leafCap
	}
	return globalCap
}

// decide is the pure per-row state machine. The expiry lane, the knob gate, and the
// fetch-time contract re-check resolve WITHOUT any fetch outcome; only when a fetch is
// due and its outcome is supplied does decide read oc. A fetched row is decided twice
// (once with oc.fetched=false to learn a fetch is due, once with the outcome): the
// checks above the fetch branch are config-only and stable across both calls, so the
// second call deterministically reaches the disposition. now is injected for
// determinism in tests; globalMaxBytes is the resolved global cap knob.
func decide(snap rowSnapshot, fetchEnabled bool, globalMaxBytes int64, now time.Time, oc fetchOutcome) disposition {
	// Expiry lane (runs knob-on or off): a row held past the lifetime terminates —
	// HOLDING_EXPIRED when the head was trying to fetch, FETCH_DISABLED when the knob
	// was off the whole time (the operator never enabled fetching).
	if now.Sub(snap.createdAt) > holdingLifetime {
		if fetchEnabled {
			return terminal(CodeHoldingExpired, "")
		}
		return terminal(CodeFetchDisabled, "")
	}
	// Knob off and still young: leave the row for a possible re-enable. No I/O, no write.
	if !fetchEnabled {
		return disposition{action: actionNone}
	}
	// Fetch-time contract re-check (D10) against the CURRENT leaf config: a mid-window
	// opt-out or allowlist shrink is honored here — the direction that respects the leaf
	// owner's latest intent and never widens access. ValidateExternalOutputURL is the
	// SAME rule the submit gate applies, so a URL cannot pass one seam and fail the other
	// except through a deliberate config change in between.
	if !snap.valCfg.AllowExternalOutput {
		return terminal(CodeLeafOptedOut, "")
	}
	if err := leaf.ValidateExternalOutputURL(snap.outputDataRef, snap.valCfg.ExternalOutputHosts); err != nil {
		return terminal(CodeURLDisallowed, err.Error())
	}
	// A fetch is due. On the pre-fetch pass, instruct the caller to fetch.
	if !oc.fetched {
		return disposition{action: actionFetch, fetchCap: effectiveCap(snap.dataCfg.MaxOutputSizeBytes, globalMaxBytes)}
	}

	// Dispose on the fetch outcome (§10.6 step 5).
	if oc.err == nil {
		// SUCCESS → PROMOTE on the SERVED hash. The claim is NEVER a gate (audit F2):
		// a divergence only sets claimMismatch for the diagnostic INFO.
		if snap.unitState == workunit.WorkUnitStateValidated || snap.unitState == workunit.WorkUnitStateFailed {
			// The seat is gone — the late-result mirror of submit's finalized-unit
			// refusal. COMPLETED/QUEUED/REJECTED units promote normally.
			return terminal(CodeUnitFinalized, "")
		}
		return disposition{
			action:        actionPromote,
			servedHash:    oc.hash,
			claimMismatch: oc.hash != snap.claimedChecksum,
		}
	}
	if oc.err.Transient {
		// attempts counts TRANSIENT failures ONLY (§10.6): success and permanent
		// failures never consume the budget, so a mismatch can never be pre-empted by
		// exhausting it.
		newAttempts := snap.attempts + 1
		if newAttempts >= maxAttempts {
			return terminal(CodeFetchFailed, oc.err.Error())
		}
		return disposition{
			action:     actionRetry,
			attempts:   newAttempts,
			reasonCode: oc.err.Code,
			lastError:  oc.err.Error(),
		}
	}
	// PERMANENT → terminal immediately, reason code from the FetchError.
	return terminal(oc.err.Code, oc.err.Err.Error())
}
