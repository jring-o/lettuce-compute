package server

import (
	"math"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// The reason on an empty work reply.
//
// An empty RequestWorkUnitResponse used to be one reasonless shape, so the client's only
// explanation was "the heads have no units matching this machine" — wrong when the machine
// was at this head's in-flight cap, when its account already had a result on every unit,
// when a recent failed copy had benched it from a unit, when the account itself was
// benched, or when the head judged it too slow for a unit's deadline. The reply now names
// those causes, so the client can say the right thing and stop counting them as an empty
// head.
//
// Everything here describes the REQUESTER's own state and nothing else: no queue depths,
// nothing about other volunteers' copies or caps, and nothing about a leaf the requester
// could not see. The head works the reason out only over the candidates in the request's
// scope — the leaves it named, or, when it named none, the leaves the visibility gate
// would serve it (not UNLISTED or PRIVATE, with a fresh snapshot). A request naming a leaf
// that is not known to be PUBLIC from a fresh snapshot gets no unit-derived reason at all,
// so the reply cannot confirm that a hidden leaf exists or has units. Every other cause of
// an empty answer is UNSPECIFIED.

// noWorkReply is the reason an empty hand-out carries, with the figures its wording needs.
// The zero value is UNSPECIFIED. The machine's in-flight cap and count, which the
// INFLIGHT_CAP wording also needs, are on every reply (handOutReply).
type noWorkReply struct {
	reason lettucev1.NoWorkReason
	// INFEASIBLE_DEADLINE: the first refused unit's deadline and estimated runtime.
	deadlineSeconds  int64
	estimatedSeconds int64
	// ACCOUNT_BENCHED: when the bench ends; nil when it has no end time.
	benchedUntil *time.Time
}

// apply stamps the reason and its figures on an empty reply.
func (r noWorkReply) apply(resp *lettucev1.RequestWorkUnitResponse) {
	resp.NoWorkReason = r.reason
	switch r.reason {
	case lettucev1.NoWorkReason_NO_WORK_REASON_INFEASIBLE_DEADLINE:
		resp.DeadlineSeconds = r.deadlineSeconds
		resp.EstimatedSeconds = r.estimatedSeconds
	case lettucev1.NoWorkReason_NO_WORK_REASON_ACCOUNT_BENCHED:
		if r.benchedUntil != nil {
			resp.BenchedUntilUnix = r.benchedUntil.Unix()
		}
	}
}

// noWorkScope is which candidates of a hand-out may produce a reason.
type noWorkScope int

const (
	// noWorkScopeNone: the request named a leaf not known to be PUBLIC from a fresh
	// snapshot, so no unit-derived reason is given.
	noWorkScopeNone noWorkScope = iota
	// noWorkScopeNamed: every leaf the request named is PUBLIC with a fresh snapshot;
	// every candidate that passes the leaf filters is in scope.
	noWorkScopeNamed
	// noWorkScopeAnyLeaf: the request named no leaf; a candidate is in scope when its
	// leaf passes the visibility gate.
	noWorkScopeAnyLeaf
)

// noWorkScopeLocked resolves the scope for a request. Caller holds mu (the leaf snapshot
// lock nests inside it, as in eligibleLocked).
func (c *dispatchCache) noWorkScopeLocked(opts workunit.AssignmentOptions) noWorkScope {
	if len(opts.LeafIDs) == 0 {
		return noWorkScopeAnyLeaf
	}
	for _, id := range opts.LeafIDs {
		if !c.leafVisibleFresh(id) {
			return noWorkScopeNone
		}
	}
	return noWorkScopeNamed
}

// leafVisibleFresh reports whether a leaf's snapshot vouches that it is visible to any
// requester: cached, fresh, and neither UNLISTED nor PRIVATE. It matches the
// visibility gate in eligibleLocked, which is matched on the two hidden values so a
// sparsely built snapshot with no visibility stays eligible.
func (c *dispatchCache) leafVisibleFresh(id types.ID) bool {
	lf, fresh := c.peekLeafFresh(id)
	if lf == nil || !fresh {
		return false
	}
	return lf.Visibility != leaf.VisibilityUnlisted && lf.Visibility != leaf.VisibilityPrivate
}

// noWorkTally counts why the in-scope candidates of a hand-out were refused, in the
// classes a reply can name.
type noWorkTally struct {
	// capBinding: at least one in-scope unit was refused only by this machine's in-flight
	// cap (it passes every other check), so the cap is what stood between the machine and
	// work. A unit the cap refused that would also fail a later check is counted under
	// that check instead.
	capBinding bool
	// contributed: refused because this account's trust subject already has a result on
	// the unit or holds a copy of it (already_contributed, self_held).
	contributed int
	// cooldown: refused because this account's recent copy of the unit failed or lapsed.
	cooldown int
	// infeasible: refused because the account's benchmark is too slow for the deadline;
	// deadlineSeconds and estimatedSeconds are the first such unit's figures.
	infeasible       int
	deadlineSeconds  int64
	estimatedSeconds int64
	// other: any other in-scope refusal, including every one caused by other volunteers'
	// copies. It keeps ALREADY_CONTRIBUTED from claiming "every unit".
	other int
}

// noteLocked records one refused candidate. noCap is the request's options with the
// in-flight cap switched off, used to ask whether a unit the cap refused would otherwise
// have been handed out. Caller holds mu.
func (t *noWorkTally) noteLocked(c *dispatchCache, volunteerID, hostKey types.ID, noCap workunit.AssignmentOptions, scope noWorkScope, cand candidate, reason rejectReason) {
	if scope == noWorkScopeNone || t.capBinding {
		// No reason can come from units, or the cap already outranks everything the rest
		// of the scan could find.
		return
	}
	switch reason {
	case rejectLeafFilter, rejectBlockedLeaf:
		return // outside the request
	}
	if scope == noWorkScopeAnyLeaf && !c.leafVisibleFresh(cand.unit.LeafID) {
		return // a leaf the requester did not name and may not see
	}
	if reason == rejectInflightCap {
		ok, next := c.eligibleLocked(volunteerID, hostKey, noCap, cand)
		if ok {
			t.capBinding = true
			return
		}
		reason = next
	}
	switch reason {
	case rejectAlreadyContributed, rejectSelfHeld:
		t.contributed++
	case rejectBenched:
		t.cooldown++
	case rejectInfeasibleDeadline:
		if t.infeasible == 0 {
			t.deadlineSeconds = int64(cand.unit.DeadlineSeconds)
			if lf := c.peekLeaf(cand.unit.LeafID); lf != nil && noCap.BenchmarkFPOPS > 0 {
				t.estimatedSeconds = int64(math.Ceil(lf.ExecutionConfig.RscFpopsEst / noCap.BenchmarkFPOPS))
			}
		}
		t.infeasible++
	default:
		t.other++
	}
}

// noWorkReplyLocked chooses the reason for an empty hand-out. Precedence, first match
// wins: ACCOUNT_BENCHED, INFLIGHT_CAP, INFEASIBLE_DEADLINE, BENCH_COOLDOWN,
// ALREADY_CONTRIBUTED.
//
// ACCOUNT_BENCHED is read from the account's standing directly rather than from the
// tally: a benched account can take nothing from this head whatever the pool holds, and
// the fact is its own, so it is reported even when the requested leaf is empty or hidden.
// ALREADY_CONTRIBUTED, whose wording says "every", is reported only when every in-scope
// refusal was this account's own result or copy. The others need one in-scope unit.
// Caller holds mu.
func (c *dispatchCache) noWorkReplyLocked(volunteerID types.ID, t noWorkTally) noWorkReply {
	if e, ok := c.standingSnapshot[volunteerID]; ok &&
		volunteer.EffectiveStanding(e.Standing, e.BenchedUntil, c.now()) == volunteer.StandingBenched {
		return noWorkReply{reason: lettucev1.NoWorkReason_NO_WORK_REASON_ACCOUNT_BENCHED, benchedUntil: e.BenchedUntil}
	}
	switch {
	case t.capBinding:
		return noWorkReply{reason: lettucev1.NoWorkReason_NO_WORK_REASON_INFLIGHT_CAP}
	case t.infeasible > 0:
		return noWorkReply{
			reason:           lettucev1.NoWorkReason_NO_WORK_REASON_INFEASIBLE_DEADLINE,
			deadlineSeconds:  t.deadlineSeconds,
			estimatedSeconds: t.estimatedSeconds,
		}
	case t.cooldown > 0:
		return noWorkReply{reason: lettucev1.NoWorkReason_NO_WORK_REASON_BENCH_COOLDOWN}
	case t.contributed > 0 && t.other == 0:
		return noWorkReply{reason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED}
	}
	return noWorkReply{}
}
