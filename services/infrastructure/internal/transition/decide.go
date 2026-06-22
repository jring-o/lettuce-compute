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
// results: the largest agreeing group and the agreement ratio. The transitioner computes it
// from the validation engine when a snapshot has at least MinQuorum pending results; Decide
// treats a nil verdict as "no agreement attempt is possible yet".
type ComparisonVerdict struct {
	MajorityCount      int
	Total              int
	Ratio              float64
	AgreedResultIDs    []types.ID
	DisagreedResultIDs []types.ID
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
	// Comparison is the comparator verdict over the PENDING results. The caller computes it
	// iff PendingCount >= Policy.MinQuorum; nil otherwise.
	Comparison *ComparisonVerdict
}

// Decision is Decide's output: the action plus whether the executor must transition the unit
// through COMPLETED first (the legal-edge requirement for VALIDATED/REJECTED, and the
// observable "COMPLETED while corroborating" state today's code parks a fully-collected unit
// in while it waits for stragglers).
type Decision struct {
	Action        Action
	CompleteFirst bool
	Reason        string
}

// Dispatchable reports whether the dispatcher should hand out ANOTHER copy of this unit
// (ignoring per-volunteer distinctness, which the cache/reserve layer adds). It is the
// redundancy-headroom half of the eligibility predicate, defined from the SAME
// (live+pending) vs target comparison Decide uses — so dispatch and the transitioner cannot
// disagree about whether a unit still needs copies. This mirrors the SQL dispatch predicate
// (EffectiveTargetCopiesSQL); the property tests assert the two never diverge.
func Dispatchable(s UnitSnapshot) bool {
	return s.State == workunit.WorkUnitStateQueued &&
		(s.LiveCopies+s.PendingCount) < s.Policy.TargetCopies
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
	return (s.LiveCopies + s.PendingCount) >= s.Policy.TargetCopies
}

// capsExhausted reports whether the unit's copy budget is spent: the total-copy ceiling is
// reached, or (when set) the error-copy ceiling. A unit with capsExhausted that also has no
// live copy and unmet quorum is dead-lettered. Note: capsExhausted alone never dead-letters a
// unit with a live copy — that copy is allowed to finish (matches DeadLetterIfExhausted's
// "no live copy" guard).
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
	//    agree within threshold, WITHOUT waiting for the remaining target copies (the
	//    validate-at-quorum win). The caller computes Comparison iff PendingCount >= MinQuorum,
	//    so a non-nil verdict already means the quorum-many results are present; the agreement
	//    decision is then purely the ratio-vs-threshold gate, identical to today's
	//    applyThreshold (min_quorum plays the old redundancy_factor's role as the attempt gate,
	//    NOT an extra agreeing-group-size floor — so threshold < 1.0 leaves are unchanged).
	if v := s.Comparison; v != nil && v.Ratio >= p.AgreementThreshold {
		return Decision{Action: ActionValidate, CompleteFirst: true, Reason: "quorum agreement"}
	}

	// "More copies may still arrive" = a live copy is running, OR a fresh copy can still be
	// dispatched (headroom under target) and the copy budget is not exhausted. While true, a
	// non-agreeing unit waits rather than rejecting or dead-lettering.
	dispatchHeadroom := (s.LiveCopies + pending) < p.TargetCopies
	moreCopiesPossible := s.LiveCopies > 0 || (dispatchHeadroom && !capsExhausted(s))

	// 2. A quorum's worth of results is in, but they do not agree (verdict present, didn't
	//    pass §1).
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
