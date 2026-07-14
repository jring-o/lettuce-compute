package transition

import (
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Action is the next step the transitioner should take for a unit, decided purely from a
// UnitSnapshot. Exactly one action is returned for any snapshot.
type Action int

const (
	// ActionWait: do nothing — copies are still running, or not enough results have arrived
	// yet, or the majority is below threshold but corroborators are still in flight.
	ActionWait Action = iota
	// ActionValidate: a quorum-sized group agrees within threshold — accept + credit
	// (COMPLETED -> VALIDATED). The executor applies the comparator's majority/minority split.
	ActionValidate
	// ActionReject: a quorum's worth of results disagree and no more are coming — mark them
	// DISAGREED and requeue the unit for a fresh set (COMPLETED -> REJECTED -> QUEUED).
	ActionReject
	// ActionDeadLetter: the unit cannot reach quorum and its copy budget is exhausted with no
	// live copy left — park it FAILED + flagged-for-review (QUEUED -> FAILED).
	ActionDeadLetter
)

func (a Action) String() string {
	switch a {
	case ActionWait:
		return "WAIT"
	case ActionValidate:
		return "VALIDATE"
	case ActionReject:
		return "REJECT"
	case ActionDeadLetter:
		return "DEAD_LETTER"
	default:
		return "UNKNOWN"
	}
}

// ComparisonVerdict is the result of the (read-only) comparator over a unit's PENDING
// results, expressed in DISTINCT SUBJECTS rather than raw results. A subject is the
// account-level trust key stamped on each result (a bound volunteer's DID, else the
// per-keypair "vol:<uuid>" sentinel; see internal/trust): every result is attributed to
// exactly one subject, and two devices bound to one identity are ONE subject. Counting
// subjects — not results — is what makes validation Sybil-resistant: N copies from one
// principal corroborate as one, and a principal that contradicts itself corroborates as
// none. The transitioner computes the verdict via BuildComparisonVerdict when a snapshot
// has at least MinQuorum pending results; Decide treats a nil verdict as "no agreement
// attempt is possible yet".
//
// The three count/ratio fields are all in subject units:
//
//   - Total is the number of DISTINCT subjects across the compared pending set.
//   - MajorityCount is the number of distinct COHERENT AGREEING subjects: a subject whose
//     every result in the pending set falls in the comparator's majority (result-level)
//     group. A subject with results on both sides of the split contributes to Total but
//     NEVER to MajorityCount — incoherent testimony corroborates nothing. A tie or
//     otherwise-ambiguous comparison yields an empty majority group and thus
//     MajorityCount == 0, which can never validate.
//   - TrustedMajorityCount is the subset of those coherent agreeing subjects whose
//     submission-time snapshot score is at or above the resolved trust floor. This is the
//     number Decide's trust gate compares against MinTrustedCorroborators; it is always
//     <= MajorityCount.
//   - Ratio is MajorityCount/Total (0 when Total is 0), the subject-level agreement
//     fraction the configured AgreementThreshold is checked against.
//
// Decide gates on the integer counts directly (agreeing-group floor, strict majority, and
// the trusted-corroborator gate) rather than only on the derived Ratio, so those checks are
// exact.
type ComparisonVerdict struct {
	MajorityCount        int
	Total                int
	Ratio                float64
	TrustedMajorityCount int
	AgreedResultIDs      []types.ID
	DisagreedResultIDs   []types.ID
}

// UnitSnapshot is the immutable, side-effect-free view Decide operates on. Every field is a
// plain count or value read under the per-unit lock, so Decide is a pure function: the same
// snapshot always yields the same Decision. This is what the property tests exercise.
type UnitSnapshot struct {
	State  workunit.WorkUnitState
	Policy RedundancyPolicy
	// LiveCopies is the number of copies (work_unit_assignment_history rows) with no outcome
	// yet — RESERVED or RUNNING. A live copy may still produce a result, so the unit is never
	// dead-lettered while one exists.
	LiveCopies int
	// TotalCopies is the number of copies ever created for the unit (the dead-letter ceiling
	// probe).
	TotalCopies int
	// ErrorCopies is the number of copies that ended badly (EXPIRED/ABANDONED) plus DISAGREED
	// results — the max_error_copies probe.
	ErrorCopies int
	// PendingCount is the number of PENDING results (already version-homogeneous filtered).
	PendingCount int
	// ProbationLiveCopies and ProbationPendingCount are the subsets of LiveCopies / PendingCount
	// that DO NOT count toward redundancy coverage because the copy is held (live) or was
	// submitted (pending) by a non-OK-standing account (BG-24b): a live copy whose HOLDER's
	// CURRENT effective standing is not OK, and a pending result whose submit-time standing
	// STAMP was not OK. Redundancy coverage counts only the complement (live+pending MINUS
	// these), so a probation account forces full replication around itself instead of covering
	// a target slot. Both zero (the default) reproduce today's arithmetic byte-for-byte.
	ProbationLiveCopies   int
	ProbationPendingCount int
	// Comparison is the comparator verdict over the PENDING results. The caller computes it
	// iff PendingCount >= Policy.MinQuorum; nil otherwise.
	Comparison *ComparisonVerdict
}

// countableCopies is the redundancy-coverage count: live + pending copies MINUS the probation
// (non-OK-standing) ones, which cover nothing (BG-24b). Each subtraction is clamped at >= 0
// defensively so a transient miscount (a probation count momentarily above its live/pending
// total) can never push coverage negative. With zero probation counts this is exactly
// LiveCopies + PendingCount — the pre-standing coverage expression.
func countableCopies(s UnitSnapshot) int {
	live := s.LiveCopies - s.ProbationLiveCopies
	if live < 0 {
		live = 0
	}
	pending := s.PendingCount - s.ProbationPendingCount
	if pending < 0 {
		pending = 0
	}
	return live + pending
}

// Decision is Decide's output: the action plus whether the executor must transition the unit
// through COMPLETED first (the legal-edge requirement for VALIDATED/REJECTED, and the
// observable "COMPLETED while corroborating" state today's code parks a fully-collected unit
// in while it waits for stragglers).
type Decision struct {
	Action        Action
	CompleteFirst bool
	// Reopen marks a WAIT that rests on PHANTOM dispatch headroom: the unit is parked
	// COMPLETED (or is stranded REJECTED residue), no copy is live, the copy budget has
	// dispatch headroom left — but no dispatcher can ever use it, because dispatch requires
	// QUEUED. The executor demotes the unit back to QUEUED (COMPLETED: plain guarded flip
	// touching no results; REJECTED: the standard Reassign requeue), where the headroom is
	// real and dispatch supplies exactly the missing corroborators. Never set for QUEUED or
	// terminal states, and never while a live copy could still re-trigger Evaluate on close.
	Reopen bool
	Reason string
}

// Dispatchable reports whether the dispatcher should hand out ANOTHER copy of this unit
// (ignoring per-volunteer distinctness, which the cache/reserve layer adds). It is the
// redundancy-headroom half of the eligibility predicate, defined from the SAME
// (live+pending) vs target comparison Decide uses — so dispatch and the transitioner cannot
// disagree about whether a unit still needs copies. This mirrors the SQL dispatch predicate
// (EffectiveTargetCopiesSQL); the property tests assert the two never diverge.
func Dispatchable(s UnitSnapshot) bool {
	return s.State == workunit.WorkUnitStateQueued &&
		countableCopies(s) < s.Policy.TargetCopies
}

// RedundancyMet reports whether the unit's target is already covered (enough copies are live
// or have submitted) or it has already validated — i.e. it needs no further copies. It is the
// logical complement of "a QUEUED unit still has dispatch headroom", so by construction
// Dispatchable(s) implies !RedundancyMet(s). The agreement property test locks this: if a
// later change makes Dispatchable use a different bound than RedundancyMet, the test fails —
// the structural guard against another #49.
func RedundancyMet(s UnitSnapshot) bool {
	if s.State == workunit.WorkUnitStateValidated {
		return true
	}
	return countableCopies(s) >= s.Policy.TargetCopies
}

// capsExhausted reports whether the unit's copy budget is spent: the total-copy ceiling is
// reached, or (when set) the error-copy ceiling. A unit with capsExhausted that also has no
// live copy and unmet quorum is dead-lettered. Note: capsExhausted alone never dead-letters a
// unit with a live copy — that copy is allowed to finish (matches DeadLetterIfExhausted's
// "no live copy" guard).
//
// Both probes are DELIBERATELY RAW (TotalCopies / ErrorCopies, probation included): the copy
// budget is the resource valve that bounds a unit's total churn, so a unit that keeps drawing
// probation copies (which never cover redundancy) still eventually exhausts its budget and
// stops burning the pool — the forced-replication above cannot loop unboundedly.
func capsExhausted(s UnitSnapshot) bool {
	p := s.Policy
	if p.MaxTotalCopies > 0 && s.TotalCopies >= p.MaxTotalCopies {
		return true
	}
	if p.MaxErrorCopies > 0 && s.ErrorCopies >= p.MaxErrorCopies {
		return true
	}
	return false
}

// CapsHit reports whether the unit should dead-letter now: quorum unmet, no live copy left,
// and the copy budget exhausted. Exposed for observability + the property tests.
func CapsHit(s UnitSnapshot) bool {
	if s.State == workunit.WorkUnitStateValidated || s.State == workunit.WorkUnitStateFailed {
		return false
	}
	return s.PendingCount < s.Policy.MinQuorum && s.LiveCopies == 0 && capsExhausted(s)
}

// Decide is the single, pure redundancy decision for a unit. It is total (returns exactly one
// Action for any snapshot) and deterministic. The executor (Transitioner) applies the result
// via the proven copy/validation primitives; the dispatch-cache reads Dispatchable. Both draw
// every number from the same RedundancyPolicy, so they cannot drift.
//
// Behavior-preserving by default: with target == quorum == redundancy_factor (the resolution
// for any leaf that only sets redundancy_factor), Decide reproduces today's
// SubmitResult + TryValidate + DeadLetterIfExhausted outcomes exactly — asserted by the
// default-equivalence property test against a golden model of the old logic.
func Decide(s UnitSnapshot) Decision {
	// Terminal states never transition again.
	if s.State == workunit.WorkUnitStateValidated || s.State == workunit.WorkUnitStateFailed {
		return Decision{Action: ActionWait, Reason: "terminal"}
	}

	p := s.Policy
	pending := s.PendingCount

	// 1. Quorum agreement — validate as soon as a quorum's worth of results is in and they
	//    agree, WITHOUT waiting for the remaining target copies (the validate-at-quorum win).
	//    The caller computes Comparison iff PendingCount >= MinQuorum, so a non-nil verdict
	//    already means the quorum-many results are present. All counts are DISTINCT SUBJECTS,
	//    not raw results (see ComparisonVerdict): copies from one principal corroborate as
	//    one. FOUR independent gates must all hold; any failure flows to §2's wait-or-reject
	//    path (never an instant reject), exactly like "no agreement yet":
	//      (a) Ratio >= threshold          — the configured agreement fraction, and
	//      (b) MajorityCount >= MinQuorum   — the agreeing group is itself quorum-sized, so
	//                                         min_quorum is a floor on the WINNERS, not merely
	//                                         an attempt gate, and
	//      (c) 2*MajorityCount > Total      — the agreeing group is a STRICT majority of the
	//                                         compared pending set, so no config (e.g. a
	//                                         threshold <= 0.5 legacy row) can validate a
	//                                         minority or a plurality, and
	//      (d) TrustedMajorityCount >= MinTrustedCorroborators — the agreeing group contains
	//                                         enough DISTINCT, TRUSTED subjects (see
	//                                         internal/trust). This is the account-level Sybil
	//                                         gate: enough copies is not enough; enough copies
	//                                         from enough trusted principals is. MinTrusted-
	//                                         Corroborators is 0 whenever the head trust gate
	//                                         is disabled, so (d) is a vacuous auto-pass and
	//                                         behavior is byte-for-byte the pre-trust rule.
	//    A tie or non-finite comparison yields a zero-size agreeing group (MajorityCount == 0),
	//    which fails (b) and (c) and so never validates.
	//
	//    Crucially, when (a)-(c) hold but (d) fails — the results agree but too few trusted
	//    principals stand behind them — the unit does NOT reject. It flows into §2 exactly
	//    like "threshold unmet": more copies may bring a trusted corroborator, so it waits
	//    while any can still arrive and only rejects the round when none can. Blocked-by-trust
	//    is a "need more (trusted) corroboration" state, never a disagreement.
	if v := s.Comparison; v != nil &&
		v.Ratio >= p.AgreementThreshold &&
		v.MajorityCount >= p.MinQuorum &&
		2*v.MajorityCount > v.Total &&
		v.TrustedMajorityCount >= p.MinTrustedCorroborators {
		return Decision{Action: ActionValidate, CompleteFirst: true, Reason: "quorum agreement"}
	}

	// "More copies may still arrive" = a live copy is running, OR a fresh copy can still be
	// dispatched (headroom under target) and the copy budget is not exhausted. While true, a
	// non-agreeing unit waits rather than rejecting or dead-lettering.
	//
	// dispatchHeadroom uses the COUNTABLE coverage (probation copies excluded): a unit nominally
	// at target but held short by probation copies still has headroom, so it keeps dispatching
	// full replication around them instead of rejecting — the forced-replication case.
	dispatchHeadroom := countableCopies(s) < p.TargetCopies
	// moreCopiesPossible's first term is DELIBERATELY RAW: ANY live copy — probation included —
	// may still produce a result, so a unit is never dead-lettered or rejected while one exists.
	// A probation live copy does not COVER redundancy but it is not dead weight either; letting
	// it finish is strictly better than reaping the unit.
	moreCopiesPossible := s.LiveCopies > 0 || (dispatchHeadroom && !capsExhausted(s))

	// 2. A quorum's worth of results is in, but they do not agree (verdict present, didn't
	//    pass §1). This gate is DELIBERATELY RAW `pending` (probation results included): it asks
	//    only whether enough results have physically arrived to attempt a decision — a probation
	//    result is invisible to the verdict (§1) but its arrival still trips the reject/wait
	//    branch here, where moreCopiesPossible then keeps a probation-only unit waiting (never
	//    an instant reject) while the copy budget holds.
	if pending >= p.MinQuorum {
		if moreCopiesPossible {
			// Stragglers may still corroborate (matches applyThreshold's PENDING hold). The
			// unit is parked COMPLETED while it waits IFF the target is already covered (no
			// dispatch headroom) — the observable "COMPLETED while corroborating" state today.
			// With dispatch headroom (target > quorum) it stays QUEUED so more copies go out.
			return Decision{Action: ActionWait, CompleteFirst: !dispatchHeadroom,
				Reason: "threshold unmet; corroborators still possible"}
		}
		// No more copies coming and no agreement → reject this round and requeue for a fresh
		// set (matches rejectAll -> Reassign). Dead-lettering is reached on a later cycle once
		// the requeue drops pending to 0 and the copy budget is exhausted (the §3 path), so a
		// disagreeing unit is never dead-lettered directly here — identical to today.
		return Decision{Action: ActionReject, CompleteFirst: true, Reason: "no agreement; no more copies"}
	}

	// 3. Fewer than quorum results so far.
	if moreCopiesPossible {
		return Decision{Action: ActionWait, Reason: "awaiting more results"}
	}
	// No live copy, no dispatch headroom left (or budget exhausted), quorum unreachable →
	// dead-letter (matches DeadLetterIfExhausted: QUEUED, no live copy, pending < quorum,
	// total >= ceiling).
	return Decision{Action: ActionDeadLetter, Reason: "redundancy unmet; copy budget exhausted"}
}
