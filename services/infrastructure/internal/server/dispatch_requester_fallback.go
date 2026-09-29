package server

import (
	"context"
	"sort"
	"strings"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// The per-requester database fallback.
//
// The ready pool is stocked without regard to who asks, oldest first. A redundancy-2 unit that
// already carries one account's result stays staged until a SECOND account takes it, so an
// account that outruns the rest of the fleet on a leaf can fill the pool with units that all
// refuse it ("already contributed") while the database still holds units of that leaf it has
// never touched. The refill cannot help: it stages the same oldest units again, and it must not
// evict the demanded leaf's candidates to make room, because those are exactly the units the
// other volunteers need in order to validate this account's results.
//
// So when a hand-out yields nothing and the requester was refused on its own account (its own
// result or copy, or a post-failure cooldown), the cache asks the database directly with the
// per-requester query (FindAssignableBatch, which shares FindNextAssignable's predicate), lands
// the copies through the landing write every hand-out uses (FlushReservations), and hands them
// to the requester without staging them. The pool, and every other volunteer's view of it, is
// unchanged.
//
// Cost: the fallback never runs for a request the pool serves. It runs under the maintenance
// admission budget (and skips, rather than waits, when that is full), with the dispatch
// database timeout, and asks for no more than the request's n and the machine's remaining
// in-flight room. It is throttled per machine and request scope: an empty answer stands for
// fallbackEmptyTTL, and after an answer that found work the next run waits at least
// fallbackFoundSpacing (the per-machine send interval spaces real hand-outs further).
//
// The reason: ALREADY_CONTRIBUTED says the account has done "every task" the head has for the
// scope, so it is sent only while the fallback's latest answer for that machine and scope is
// that the database has nothing either — never on the pool's word alone.

const (
	// fallbackEmptyTTL is how long an empty per-requester answer stands for its machine and
	// request scope: the query is not re-run, and the empty reply may say ALREADY_CONTRIBUTED.
	// Units created meanwhile reach such a machine within this window.
	fallbackEmptyTTL = 5 * time.Minute
	// fallbackFoundSpacing is the minimum gap, after a fallback that found work, before the next
	// one for the same machine and scope.
	fallbackFoundSpacing = 10 * time.Second
)

// fallbackKey is one machine's request scope, the unit the fallback throttle and its standing
// answer are kept for.
type fallbackKey struct {
	host  types.ID
	scope string
}

// fallbackRun is the latest fallback for a fallbackKey: when it ran and whether it found work.
type fallbackRun struct {
	at    time.Time
	found bool
}

type fallbackState int

const (
	// fallbackNotApplicable: nothing refused the requester on its own account, or its account
	// is benched, so the database would answer no differently.
	fallbackNotApplicable fallbackState = iota
	// fallbackDue: run the per-requester query now.
	fallbackDue
	// fallbackAnsweredEmpty: a run for this machine and scope found nothing within
	// fallbackEmptyTTL, and that answer stands.
	fallbackAnsweredEmpty
	// fallbackUnverified: the query cannot run now (the machine has no in-flight room, or a run
	// found work moments ago), so nothing is known about the database.
	fallbackUnverified
)

// fallbackPlan is what planFallbackLocked decided, with what a due run needs.
type fallbackPlan struct {
	state    fallbackState
	key      fallbackKey
	limit    int
	excluded []types.ID
}

// fallbackScopeKey identifies a request's scope for the fallback throttle: its leaf filter and
// its blocked leaves, in any order.
func fallbackScopeKey(opts workunit.AssignmentOptions) string {
	join := func(in []types.ID) string {
		s := make([]string, len(in))
		for i, id := range in {
			s[i] = id.String()
		}
		sort.Strings(s)
		return strings.Join(s, ",")
	}
	return join(opts.LeafIDs) + "|" + join(opts.BlockedLeafIDs)
}

// planFallbackLocked decides whether a hand-out that took nothing may try the database. rejects
// is the hand-out's per-reason refusal tally, opts the request with the effective in-flight cap
// the hand-out applied, and noWork the reason the pool's answer produced. Caller holds mu.
func (c *dispatchCache) planFallbackLocked(hostKey types.ID, opts workunit.AssignmentOptions, n int, rejects *[numRejectReasons]int, noWork noWorkReply) fallbackPlan {
	if rejects[rejectAlreadyContributed]+rejects[rejectSelfHeld]+rejects[rejectBenched] == 0 {
		return fallbackPlan{state: fallbackNotApplicable}
	}
	if noWork.reason == lettucev1.NoWorkReason_NO_WORK_REASON_ACCOUNT_BENCHED || c.deps.wuRepo == nil {
		return fallbackPlan{state: fallbackNotApplicable}
	}
	limit := n
	if opts.MaxInflightPerVolunteer > 0 {
		room := opts.MaxInflightPerVolunteer - c.inflight[hostKey]
		if room <= 0 {
			return fallbackPlan{state: fallbackUnverified}
		}
		if room < limit {
			limit = room
		}
	}
	key := fallbackKey{host: hostKey, scope: fallbackScopeKey(opts)}
	if last, ok := c.fallbackRuns[key]; ok {
		age := c.now().Sub(last.at)
		if !last.found && age < fallbackEmptyTTL {
			return fallbackPlan{state: fallbackAnsweredEmpty}
		}
		if last.found && age < fallbackFoundSpacing {
			return fallbackPlan{state: fallbackUnverified}
		}
	}
	return fallbackPlan{state: fallbackDue, key: key, limit: limit, excluded: c.excludedIDsLocked()}
}

// unverified drops a reason that claims the whole scope ("every task") when the database's
// answer is not known.
func (r noWorkReply) unverified() noWorkReply {
	if r.reason == lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED {
		return noWorkReply{}
	}
	return r
}

// handOutFromDB runs a due fallback: the per-requester query, the landing write, and the
// in-memory bookkeeping of a hand-out for every copy that landed (the hold and the machine's
// in-flight count), without staging anything. ran is false when the database could not be
// asked (the maintenance budget was full, or a query failed); the caller must then not send an
// unverified ALREADY_CONTRIBUTED.
func (c *dispatchCache) handOutFromDB(volunteerID, hostKey types.ID, reqSubject string, opts workunit.AssignmentOptions, plan fallbackPlan) (results []handOutResult, ran bool) {
	release, ok := c.tryAcquireMaintenance()
	if !ok {
		return nil, false
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), dispatchDBTimeout)
	defer cancel()

	units, err := c.deps.wuRepo.FindAssignableBatch(ctx, opts, plan.limit, plan.excluded, c.cfg.headID, c.cfg.claimLease)
	if err != nil {
		c.logger.Warn("dispatch cache: per-requester fallback query failed",
			"volunteer_id", volunteerID, "host_id", hostKey, "error", err)
		return nil, false
	}
	recs := make([]workunit.FlushReservation, len(units))
	landed := make(map[types.ID]bool, len(units))
	if len(units) > 0 {
		now := c.now().UTC()
		for i, u := range units {
			// Held until the unit's deadline, like a pool hand-out; the configured lease only
			// for a unit without one.
			until := now.Add(time.Duration(c.cfg.leaseSeconds) * time.Second)
			if u.DeadlineSeconds > 0 {
				until = now.Add(time.Duration(u.DeadlineSeconds) * time.Second)
			}
			recs[i] = workunit.FlushReservation{
				WorkUnitID:      u.ID,
				VolunteerID:     volunteerID,
				HostID:          opts.HostID,
				ReservedUntil:   until,
				DeadlineSeconds: u.DeadlineSeconds,
			}
		}
		flushed, ferr := c.deps.wuRepo.FlushReservations(ctx, recs, c.cfg.headID, c.cfg.claimLease)
		if ferr != nil {
			c.logger.Warn("dispatch cache: per-requester fallback landing failed",
				"volunteer_id", volunteerID, "host_id", hostKey, "count", len(recs), "error", ferr)
			return nil, false
		}
		for _, fc := range flushed {
			if fc.VolunteerID == volunteerID {
				landed[fc.WorkUnitID] = true
			}
		}
	}

	c.mu.Lock()
	for i, u := range units {
		if !landed[u.ID] {
			continue
		}
		until := recs[i].ReservedUntil
		holders := c.reservedInMem[u.ID]
		if holders == nil {
			holders = make(map[types.ID]heldCopy)
			c.reservedInMem[u.ID] = holders
		}
		holders[volunteerID] = heldCopy{reservedUntil: until, hostID: hostKey, subject: reqSubject}
		c.inflight[hostKey]++
		unitCopy := *u
		unitCopy.ReservedUntil = &until
		vid := volunteerID
		unitCopy.ReservedVolunteerID = &vid
		results = append(results, handOutResult{unit: &unitCopy})
	}
	c.fallbackRuns[plan.key] = fallbackRun{at: c.now(), found: len(results) > 0}
	if len(results) > 0 && c.cfg.minSendInterval > 0 {
		c.lastHandOut[hostKey] = c.now()
	}
	c.mu.Unlock()

	// The spot-check decision a pool hand-out makes at a unit's first copy (see HandOut): a
	// redundancy-1 unit this requester is the first to take may be chosen for a corroborating
	// second copy.
	for _, r := range results {
		c.markSpotCheckIfChosen(ctx, r.unit)
	}
	if len(results) > 0 {
		c.logger.Info("dispatch cache: per-requester fallback handed out units the ready pool could not offer",
			"volunteer_id", volunteerID, "host_id", hostKey, "count", len(results),
			"leaf_ids", opts.LeafIDs, "found", len(units))
	}
	return results, true
}

// markSpotCheckIfChosen makes the spot-check decision for a unit handed out by the fallback:
// the same rule and sampling as a pool hand-out's first copy, recorded synchronously because
// the copy already landed. Best-effort, like the flush's own marking: a failure leaves the unit
// unsampled.
func (c *dispatchCache) markSpotCheckIfChosen(ctx context.Context, u *workunit.WorkUnit) {
	if u.SpotCheck {
		return
	}
	lf := c.peekLeaf(u.LeafID)
	if lf == nil {
		var err error
		if lf, err = c.getLeaf(u.LeafID); err != nil || lf == nil {
			return
		}
	}
	if !lf.ValidationConfig.SpotCheckEnabled || lf.ValidationConfig.RedundancyFactor != 1 ||
		!workunit.ShouldSpotCheck(lf.ValidationConfig.SpotCheckPercentage) {
		return
	}
	if err := c.deps.wuRepo.MarkSpotCheck(ctx, u.ID); err != nil {
		c.logger.Warn("dispatch cache: per-requester fallback could not mark a spot-check",
			"work_unit_id", u.ID, "error", err)
		return
	}
	u.SpotCheck = true
}

// pruneFallbackRuns drops fallback records that can no longer throttle or stand as an answer,
// so the map is bounded by the machines seen within fallbackEmptyTTL.
func (c *dispatchCache) pruneFallbackRuns() {
	cutoff := c.now().Add(-fallbackEmptyTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, r := range c.fallbackRuns {
		if r.at.Before(cutoff) {
			delete(c.fallbackRuns, k)
		}
	}
}
