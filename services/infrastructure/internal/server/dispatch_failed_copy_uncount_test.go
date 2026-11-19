package server

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// A copy that ends without a result (timed out, abandoned, given back) stops covering its
// unit: the SQL landing then refuses its holder only for the post-failure cooldown, and
// admits it again after the cooldown, or after the pool-exhausted grace when nobody else
// holds the unit. The staged candidate must agree. These tests pin that it does, for a
// copy run-started after the unit was staged and for one that was already live when it
// was staged, and that a PENDING result's coverage is never undone by a copy close.

const failedCopyKillReason = "non-zero exit code 137 (killed, usually out of memory at its 7000 MB limit)"

// candidateRefusal returns eligibleLocked's reason for the staged unit, or "not staged".
func candidateRefusal(c *dispatchCache, vol types.ID, opts workunit.AssignmentOptions, unitID types.ID) string {
	hostKey := meterID(vol, opts.HostID)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cd := range c.ready {
		if cd.unit.ID == unitID {
			_, r := c.eligibleLocked(vol, hostKey, opts, cd)
			return r.String()
		}
	}
	return "not staged"
}

// stagedCandidate returns a copy of the staged candidate for unitID (ok false when the
// unit is not staged).
func stagedCandidate(c *dispatchCache, unitID types.ID) (candidate, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cd := range c.ready {
		if cd.unit.ID == unitID {
			return cd, true
		}
	}
	return candidate{}, false
}

// stageThroughRefill stages the given refill rows through the cache's own refill path
// (fetchAndStage), so the candidate is built exactly as production builds it.
func stageThroughRefill(t *testing.T, c *dispatchCache, wuRepo *fakeWURepo, rows ...workunit.DispatchCandidate) {
	t.Helper()
	wuRepo.mu.Lock()
	wuRepo.dispatchFn = func(int, []types.ID, []types.ID) ([]workunit.DispatchCandidate, error) {
		return rows, nil
	}
	wuRepo.mu.Unlock()
	c.fetchAndStage(context.Background(), len(rows), nil, nil)
	wuRepo.mu.Lock()
	wuRepo.dispatchFn = nil
	wuRepo.mu.Unlock()
	for _, r := range rows {
		if _, ok := stagedCandidate(c, r.WorkUnit.ID); !ok {
			t.Fatalf("setup: unit %s was not staged by the refill", r.WorkUnit.ID)
		}
	}
}

// refillRow is one FindDispatchableBatch row for a QUEUED unit with a 12 h deadline.
func refillRow(unitID, leafID types.ID, redundancy, active int) workunit.DispatchCandidate {
	return workunit.DispatchCandidate{
		WorkUnit: &workunit.WorkUnit{
			ID: unitID, LeafID: leafID, State: workunit.WorkUnitStateQueued, DeadlineSeconds: 43200,
		},
		LeafID:            leafID,
		RedundancyFactor:  redundancy,
		ActiveAssignments: active,
		Runtime:           "NATIVE",
	}
}

// TestFailedCopy_PoolExhaustedFallbackReadmits: redundancy 2, nobody else holds the unit,
// 12 h deadline. A copy abandoned before it started and one abandoned after it started are
// both refused as benched inside the grace, and both re-offered 3 minutes later, when the
// unit has had no live copy for longer than the pool-exhausted grace (120 s).
func TestFailedCopy_PoolExhaustedFallbackReadmits(t *testing.T) {
	for _, started := range []bool{false, true} {
		name := "not-started"
		if started {
			name = "started"
		}
		t.Run(name, func(t *testing.T) {
			svc, c, ctx, volID, unitID, advance := tb40Service(t, 43200,
				workunit.ClosedCopy{Outcome: "ABANDONED"})
			opts := capableOpts(volID, 0)
			if res, _ := c.HandOut(volID, opts, 1); len(res) != 1 {
				t.Fatalf("setup: initial hand-out = %d, want 1", len(res))
			}
			if started {
				c.onRunStart(unitID, volID)
			}
			if _, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
				WorkUnitId: unitID.String(), VolunteerId: volID.String(), Reason: failedCopyKillReason,
			}); err != nil {
				t.Fatalf("AbandonWorkUnit: %v", err)
			}
			res, _, reply := c.HandOutWithReason(volID, opts, 1)
			if len(res) != 0 {
				t.Fatalf("re-offered inside the bench")
			}
			if got := candidateRefusal(c, volID, opts, unitID); got != rejectBenched.String() {
				t.Fatalf("inside the bench the refusal is %s, want %s", got, rejectBenched)
			}
			if reply.reason != lettucev1.NoWorkReason_NO_WORK_REASON_BENCH_COOLDOWN {
				t.Fatalf("inside the bench the empty reply says %v, want NO_WORK_REASON_BENCH_COOLDOWN", reply.reason)
			}
			advance(3 * time.Minute)
			c.refreshStaleLeafSnapshots(context.Background())
			res, _, reply = c.HandOutWithReason(volID, opts, 1)
			if len(res) != 1 {
				t.Fatalf("started=%v: still refused 3 min after the abandon with the unit uncovered "+
					"(pool-exhausted grace 120 s); refusal reason %s, reply reason %v",
					started, candidateRefusal(c, volID, opts, unitID), reply.reason)
			}
		})
	}
}

// TestFailedCopy_StartedCopyBenchedThenReofferedAfterWindow: redundancy 3, another
// account holds a copy, 12 h deadline. A copy run-started after staging and then
// abandoned is refused as benched inside the window (the other account's hold keeps the
// pool-exhausted fallback shut) and re-offered after it. The candidate no longer counts
// the closed copy.
func TestFailedCopy_StartedCopyBenchedThenReofferedAfterWindow(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	clock := time.Now()
	c.now = func() time.Time { return clock }
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 3, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageUnit(unitID, leafID, 3, 0)
	c.mu.Lock()
	c.ready[0].unit.DeadlineSeconds = 43200
	c.mu.Unlock()
	other := types.NewID()
	if res, _ := c.HandOut(other, pinnedOpts(other, leafID, nil), 1); len(res) != 1 {
		t.Fatalf("setup: other account not handed the unit")
	}
	vol, host := types.NewID(), types.NewID()
	opts := pinnedOpts(vol, leafID, &host)
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 1 {
		t.Fatalf("setup: vol not handed the unit")
	}
	c.onRunStart(unitID, vol)
	c.onCopyClosed(unitID, vol, false) // ABANDONED after start: the abandon RPC's call
	closedAt := clock

	cand, ok := stagedCandidate(c, unitID)
	if !ok {
		t.Fatalf("unit no longer staged after the close")
	}
	if _, counted := cand.contributors[trust.SubjectForVolunteerID(vol)]; counted || cand.dbActiveCount != 0 {
		t.Fatalf("after the close the candidate still counts the closed copy: contributors %d, dbActiveCount %d (want 0, 0)",
			len(cand.contributors), cand.dbActiveCount)
	}

	clock = closedAt.Add(time.Minute)
	c.refreshStaleLeafSnapshots(context.Background()) // the refiller's tick: a reason needs a fresh leaf snapshot
	res, _, reply := c.HandOutWithReason(vol, opts, 1)
	if len(res) != 0 {
		t.Fatalf("re-offered inside the bench")
	}
	if got := candidateRefusal(c, vol, opts, unitID); got != rejectBenched.String() {
		t.Fatalf("1 min after the close the refusal is %s, want %s", got, rejectBenched)
	}
	if reply.reason != lettucev1.NoWorkReason_NO_WORK_REASON_BENCH_COOLDOWN {
		t.Fatalf("1 min after the close the empty reply says %v, want NO_WORK_REASON_BENCH_COOLDOWN", reply.reason)
	}

	clock = closedAt.Add(13 * time.Hour)
	c.refreshStaleLeafSnapshots(context.Background())
	res, _, reply = c.HandOutWithReason(vol, opts, 1)
	if len(res) != 1 {
		t.Fatalf("13 h after its started copy was abandoned (bench 12 h) the account is still refused: "+
			"refusal reason %s, reply reason %v", candidateRefusal(c, vol, opts, unitID), reply.reason)
	}
}

// TestFailedCopy_LiveAtStagingCopyReofferedAfterWindow: the account's copy was already
// live (running) when the unit was staged, so the refill counted it. After the copy is
// abandoned and its 12 h bench has passed, the account is offered the unit again.
func TestFailedCopy_LiveAtStagingCopyReofferedAfterWindow(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	clock := time.Now()
	c.now = func() time.Time { return clock }
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	vol, host := types.NewID(), types.NewID()
	row := refillRow(unitID, leafID, 2, 1) // vol's running copy is the one live row
	row.LiveContributors = []workunit.LiveContributor{
		{VolunteerID: vol, Subject: trust.SubjectForVolunteerID(vol), Countable: true},
	}
	stageThroughRefill(t, c, wuRepo, row)

	c.onCopyClosed(unitID, vol, false)
	closedAt := clock
	opts := pinnedOpts(vol, leafID, &host)
	if got := candidateRefusal(c, vol, opts, unitID); got != rejectBenched.String() {
		t.Fatalf("right after the close the refusal is %s, want %s", got, rejectBenched)
	}
	clock = closedAt.Add(13 * time.Hour)
	c.refreshStaleLeafSnapshots(context.Background())
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 1 {
		t.Fatalf("13 h after its live-at-staging copy was abandoned the account is still refused: %s",
			candidateRefusal(c, vol, opts, unitID))
	}
}

// TestFailedCopy_LiveAtStagingCopyStartedThenFailed: the account's copy was reserved when
// the unit was staged, under the account's live DID, and starts afterwards on this head
// with no in-memory hold here. The start must not count the copy a second time or record
// it under another subject, so that when the copy fails the candidate holds exactly what a
// fresh refill would (nothing), and the account is re-offered the unit after its bench.
func TestFailedCopy_LiveAtStagingCopyStartedThenFailed(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	clock := time.Now()
	c.now = func() time.Time { return clock }
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 3, false, 0), leafRepo)
	unitID := types.NewID()
	vol, host := types.NewID(), types.NewID()
	did := "did:plc:" + types.NewID().String()
	row := refillRow(unitID, leafID, 3, 1) // vol's reserved copy is the one live row
	row.LiveContributors = []workunit.LiveContributor{{VolunteerID: vol, Subject: did, Countable: true}}
	stageThroughRefill(t, c, wuRepo, row)

	c.onRunStart(unitID, vol)
	cand, _ := stagedCandidate(c, unitID)
	if cand.dbActiveCount != 1 {
		t.Fatalf("after the start of a copy the refill already counted, dbActiveCount = %d, want 1", cand.dbActiveCount)
	}
	c.onCopyClosed(unitID, vol, false)
	cand, _ = stagedCandidate(c, unitID)
	if cand.dbActiveCount != 0 || len(cand.contributors) != 0 || len(cand.liveCopies) != 0 {
		t.Fatalf("after the close the candidate holds dbActiveCount %d, contributors %v, live copies %d; "+
			"a fresh refill would hold 0, none, 0", cand.dbActiveCount, cand.contributors, len(cand.liveCopies))
	}
	clock = clock.Add(13 * time.Hour)
	c.refreshStaleLeafSnapshots(context.Background())
	opts := pinnedOpts(vol, leafID, &host)
	opts.TrustSubject = did
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 1 {
		t.Fatalf("13 h after the close the account is still refused: %s", candidateRefusal(c, vol, opts, unitID))
	}
}

// TestFailedCopy_ReturnedLiveAtStagingCopyReofferedAfterThrottle: a give-back (RETURNED)
// of a copy that was reserved when the unit was staged, as the client's give-back or the
// held-copy reconcile writes it. The account is refused for the 10-minute re-offer
// throttle and offered the unit again after it.
func TestFailedCopy_ReturnedLiveAtStagingCopyReofferedAfterThrottle(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	clock := time.Now()
	c.now = func() time.Time { return clock }
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 3, false, 0), leafRepo)
	unitID := types.NewID()
	vol, host := types.NewID(), types.NewID()
	other := types.NewID()
	row := refillRow(unitID, leafID, 3, 2) // vol's reserved copy + another account's running copy
	row.LiveContributors = []workunit.LiveContributor{
		{VolunteerID: vol, Subject: trust.SubjectForVolunteerID(vol), Countable: true},
		{VolunteerID: other, Subject: trust.SubjectForVolunteerID(other), Countable: true},
	}
	stageThroughRefill(t, c, wuRepo, row)

	c.onCopyClosed(unitID, vol, true)
	opts := pinnedOpts(vol, leafID, &host)
	if got := candidateRefusal(c, vol, opts, unitID); got != rejectBenched.String() {
		t.Fatalf("inside the re-offer throttle the refusal is %s, want %s", got, rejectBenched)
	}
	clock = clock.Add(11 * time.Minute)
	c.refreshStaleLeafSnapshots(context.Background())
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 1 {
		t.Fatalf("11 min after its give-back the account is still refused: %s", candidateRefusal(c, vol, opts, unitID))
	}
	// The other account's copy is untouched: it still counts and its holder is still refused.
	otherOpts := pinnedOpts(other, leafID, nil)
	if got := candidateRefusal(c, other, otherOpts, unitID); got != rejectAlreadyContributed.String() {
		t.Fatalf("the other account's live copy no longer excludes it: refusal %s, want %s", got, rejectAlreadyContributed)
	}
}

// TestFailedCopy_PendingAuthorStaysExcluded: a PENDING result's coverage stands while the
// unit is staged. Closing another account's copy, or a copy the cache never counted,
// never re-admits the author.
func TestFailedCopy_PendingAuthorStaysExcluded(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	clock := time.Now()
	c.now = func() time.Time { return clock }
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 3, false, 0), leafRepo)
	unitID := types.NewID()
	author, holder := types.NewID(), types.NewID()
	row := refillRow(unitID, leafID, 3, 2)
	row.PendingContributorSubjects = []string{trust.SubjectForVolunteerID(author)}
	row.LiveContributors = []workunit.LiveContributor{
		{VolunteerID: holder, Subject: trust.SubjectForVolunteerID(holder), Countable: true},
	}
	stageThroughRefill(t, c, wuRepo, row)

	c.onCopyClosed(unitID, holder, false)
	c.onCopyClosed(unitID, author, false) // no live copy of the author's is counted
	clock = clock.Add(13 * time.Hour)
	c.refreshStaleLeafSnapshots(context.Background())

	authorOpts := pinnedOpts(author, leafID, nil)
	if got := candidateRefusal(c, author, authorOpts, unitID); got != rejectAlreadyContributed.String() {
		t.Fatalf("the PENDING author is no longer excluded: refusal %s, want %s", got, rejectAlreadyContributed)
	}
	cand, _ := stagedCandidate(c, unitID)
	if cand.dbActiveCount != 1 {
		t.Fatalf("dbActiveCount = %d after the holder's copy closed, want 1 (the author's result)", cand.dbActiveCount)
	}
	if res, _ := c.HandOut(holder, pinnedOpts(holder, leafID, nil), 1); len(res) != 1 {
		t.Fatalf("the holder whose copy failed 13 h ago is still refused: %s",
			candidateRefusal(c, holder, pinnedOpts(holder, leafID, nil), unitID))
	}
}

// TestFailedCopy_CoverageMatchesAFreshRefill: with the trusted-corroborator reservation on,
// each closed copy leaves the candidate's coverage as a fresh refill of the remaining rows
// would stage it: the raw count, its neutralized share, and the trusted and run-started
// subjects. A void (a landing that never wrote a copy row) un-counts nothing.
func TestFailedCopy_CoverageMatchesAFreshRefill(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	clock := time.Now()
	c.now = func() time.Time { return clock }
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 6, false, 0), leafRepo)
	unitID := types.NewID()

	trusted, neutralized, kept, starter := types.NewID(), types.NewID(), types.NewID(), types.NewID()
	sub := trust.SubjectForVolunteerID
	row := refillRow(unitID, leafID, 6, 3)
	row.ProbationCoverage = 1
	row.EffectiveTrustK = 1
	row.EffectiveTrustFloor = 50
	row.LiveContributors = []workunit.LiveContributor{
		{VolunteerID: trusted, Subject: sub(trusted), Countable: true},
		{VolunteerID: neutralized, Subject: sub(neutralized), Countable: false},
		{VolunteerID: kept, Subject: sub(kept), Countable: true},
	}
	row.TrustedContributorSubjects = []string{sub(trusted)}
	stageThroughRefill(t, c, wuRepo, row)

	// A copy handed out and run-started here after staging.
	if res, _ := c.HandOut(starter, pinnedOpts(starter, leafID, nil), 1); len(res) != 1 {
		t.Fatalf("setup: starter not handed the unit")
	}
	c.onRunStart(unitID, starter)

	type want struct {
		dbActive, probation int
		contributors        []string
		trusted, runStarted []string
	}
	check := func(step string, w want) {
		t.Helper()
		cand, ok := stagedCandidate(c, unitID)
		if !ok {
			t.Fatalf("%s: unit no longer staged", step)
		}
		if cand.dbActiveCount != w.dbActive || cand.probationCoverage != w.probation {
			t.Fatalf("%s: dbActiveCount %d, probationCoverage %d; want %d, %d",
				step, cand.dbActiveCount, cand.probationCoverage, w.dbActive, w.probation)
		}
		sameSet := func(name string, got map[string]struct{}, want []string) {
			t.Helper()
			if len(got) != len(want) {
				t.Fatalf("%s: %s = %v, want %v", step, name, got, want)
			}
			for _, s := range want {
				if _, ok := got[s]; !ok {
					t.Fatalf("%s: %s = %v, want %v", step, name, got, want)
				}
			}
		}
		sameSet("contributors", cand.contributors, w.contributors)
		sameSet("trustedContributors", cand.trustedContributors, w.trusted)
		sameSet("runStartedSubjects", cand.runStartedSubjects, w.runStarted)
	}

	check("staged, starter running", want{4, 1,
		[]string{sub(trusted), sub(neutralized), sub(kept), sub(starter)}, []string{sub(trusted)}, []string{sub(starter)}})

	c.voidNonLandedCopy(unitID, kept) // no copy row was written: nothing to un-count
	check("after a void", want{4, 1,
		[]string{sub(trusted), sub(neutralized), sub(kept), sub(starter)}, []string{sub(trusted)}, []string{sub(starter)}})

	c.onCopyClosed(unitID, trusted, false)
	check("trusted holder's copy expired", want{3, 1,
		[]string{sub(neutralized), sub(kept), sub(starter)}, nil, []string{sub(starter)}})

	c.onCopyClosed(unitID, neutralized, false)
	check("neutralized holder's copy abandoned", want{2, 0,
		[]string{sub(kept), sub(starter)}, nil, []string{sub(starter)}})

	c.onCopyClosed(unitID, starter, false)
	check("run-started copy abandoned", want{1, 0,
		[]string{sub(kept)}, nil, nil})
}
