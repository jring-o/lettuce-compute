package server

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- account-standing dispatch enforcement (BG-24b), in-memory half --------------
//
// These exercise the cache half of standing enforcement with no DB: the standing snapshot is
// installed directly (mirroring how refreshStanding would refresh it from the store), and
// candidates are staged with the countable-coverage inputs the SQL refill would populate.

// fakeStandingRepo is a stand-in account-standing store: AllNonOK returns a canned non-OK
// population (or an error), for the refresh / nil-tolerance tests.
type fakeStandingRepo struct {
	entries map[types.ID]standing.Entry
	err     error
	calls   int
}

func (f *fakeStandingRepo) AllNonOK(_ context.Context) (map[types.ID]standing.Entry, error) {
	f.calls++
	return f.entries, f.err
}

// setStandingSnapshot installs the in-memory non-OK standing snapshot directly (the unit
// tests drive the gates without the DB-backed refill that normally refreshes it).
func (c *dispatchCache) setStandingSnapshot(entries map[types.ID]standing.Entry) {
	c.mu.Lock()
	c.standingSnapshot = entries
	c.standingSnapshotAt = c.now()
	c.mu.Unlock()
}

func benchedEntry(until *time.Time) standing.Entry {
	return standing.Entry{Standing: volunteer.StandingBenched, BenchedUntil: until}
}

func probationEntry() standing.Entry {
	return standing.Entry{Standing: volunteer.StandingProbation}
}

// TestEffectiveStandingLocked resolves the snapshot through volunteer.EffectiveStanding:
// absent -> OK, a live bench -> BENCHED, an expired bench -> PROBATION, stored PROBATION ->
// PROBATION. A nil snapshot classifies everyone OK.
func TestEffectiveStandingLocked(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	past := c.now().Add(-time.Hour)
	future := c.now().Add(time.Hour)

	absent := types.NewID()
	benchLive := types.NewID()
	benchIndef := types.NewID()
	benchExpired := types.NewID()
	prob := types.NewID()
	c.setStandingSnapshot(map[types.ID]standing.Entry{
		benchLive:    benchedEntry(&future),
		benchIndef:   benchedEntry(nil),
		benchExpired: benchedEntry(&past),
		prob:         probationEntry(),
	})

	c.mu.Lock()
	defer c.mu.Unlock()
	cases := []struct {
		id   types.ID
		want string
	}{
		{absent, volunteer.StandingOK},
		{benchLive, volunteer.StandingBenched},
		{benchIndef, volunteer.StandingBenched},
		{benchExpired, volunteer.StandingProbation}, // expired bench neutralizes, not blocks
		{prob, volunteer.StandingProbation},
	}
	for _, tc := range cases {
		if got := c.effectiveStandingLocked(tc.id); got != tc.want {
			t.Errorf("effectiveStandingLocked(%s) = %q, want %q", tc.id, got, tc.want)
		}
	}

	// Nil snapshot: everyone OK.
	c.standingSnapshot = nil
	if got := c.effectiveStandingLocked(types.NewID()); got != volunteer.StandingOK {
		t.Errorf("nil snapshot effectiveStandingLocked = %q, want OK", got)
	}
}

// TestEligibleLocked_BenchedRequesterRefused: a BENCHED account is refused with
// rejectStandingBenched; an OK account and an expired-bench (PROBATION) account are both
// eligible for the same redundancy-2 candidate (coverage never the reason).
func TestEligibleLocked_BenchedRequesterRefused(t *testing.T) {
	wuRepo, leafRepo := &fakeWURepo{}, &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	cand := candidate{
		unit:                &workunit.WorkUnit{ID: types.NewID(), LeafID: leafID, State: workunit.WorkUnitStateQueued},
		effectiveRedundancy: 2,
	}

	benched := types.NewID()
	expired := types.NewID()
	past := c.now().Add(-time.Hour)
	c.setStandingSnapshot(map[types.ID]standing.Entry{
		benched: benchedEntry(nil),
		expired: benchedEntry(&past),
	})

	c.mu.Lock()
	defer c.mu.Unlock()

	if ok, reason := c.eligibleLocked(benched, benched, capableOpts(benched, 0), cand); ok || reason != rejectStandingBenched {
		t.Fatalf("benched requester eligibleLocked = (%v, %v), want (false, rejectStandingBenched)", ok, reason)
	}
	ok := types.NewID() // absent from snapshot => OK
	if eligible, reason := c.eligibleLocked(ok, ok, capableOpts(ok, 0), cand); !eligible {
		t.Fatalf("OK requester eligibleLocked = (false, %v), want eligible", reason)
	}
	if eligible, reason := c.eligibleLocked(expired, expired, capableOpts(expired, 0), cand); !eligible {
		t.Fatalf("expired-bench (PROBATION) requester eligibleLocked = (false, %v), want eligible (PROBATION still dispatched)", reason)
	}
}

// TestEligibleLocked_ProbationHolderNotCountedInCoverage isolates the in-memory holder
// standing filter (countableHoldersLocked): a redundancy-2 candidate with ONE countable DB
// copy plus ONE in-memory holder. When that holder is OK the countable coverage reaches 2 and
// the requester is refused rejectRedundancyFull; when it is PROBATION the holder does not
// count, coverage is 1, and the requester is still eligible — while the RAW holder cap (1 < 2)
// never binds, so the difference is attributable to the coverage filter alone.
func TestEligibleLocked_ProbationHolderNotCountedInCoverage(t *testing.T) {
	wuRepo, leafRepo := &fakeWURepo{}, &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	cand := candidate{
		unit:                &workunit.WorkUnit{ID: unitID, LeafID: leafID, State: workunit.WorkUnitStateQueued},
		effectiveRedundancy: 2,
		dbActiveCount:       1, // one countable DB copy already covers one slot
	}

	holder := types.NewID()
	requester := types.NewID()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.reservedInMem[unitID] = map[types.ID]heldCopy{
		holder: {reservedUntil: c.now().Add(time.Minute), hostID: holder, subject: "vol:holder"},
	}

	// Holder OK: countable coverage = 1 (db) + 1 (holder) = 2 = redundancy, requester refused.
	c.standingSnapshot = nil // holder absent => OK
	if ok, reason := c.eligibleLocked(requester, requester, capableOpts(requester, 0), cand); ok || reason != rejectRedundancyFull {
		t.Fatalf("requester eligibleLocked with OK holder = (%v, %v), want (false, rejectRedundancyFull)", ok, reason)
	}

	// Holder PROBATION: countable coverage = 1 (db) + 0 = 1 < 2, requester still eligible
	// (the holder cap 1 < 2 does not bind, so this is the coverage filter's doing).
	c.standingSnapshot = map[types.ID]standing.Entry{holder: probationEntry()}
	c.standingSnapshotAt = c.now()
	if ok, reason := c.eligibleLocked(requester, requester, capableOpts(requester, 0), cand); !ok {
		t.Fatalf("OK requester refused (%v) while the extra holder is PROBATION; forced replication should keep the slot open", reason)
	}
}

// TestEligibleLocked_ProbationCoverageSeedForcesReplication: the refill-time probationCoverage
// seed (non-countable DB rows) is subtracted from dbActiveCount, so a redundancy-1 candidate
// whose only seeded coverage is non-countable stays open for a fresh OK requester.
func TestEligibleLocked_ProbationCoverageSeedForcesReplication(t *testing.T) {
	wuRepo, leafRepo := &fakeWURepo{}, &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)

	// dbActiveCount 1 but probationCoverage 1 => countable seed 0.
	openCand := candidate{
		unit:                &workunit.WorkUnit{ID: types.NewID(), LeafID: leafID, State: workunit.WorkUnitStateQueued},
		effectiveRedundancy: 1,
		dbActiveCount:       1,
		probationCoverage:   1,
	}
	// Same seed but countable (probationCoverage 0) => coverage full.
	fullCand := candidate{
		unit:                &workunit.WorkUnit{ID: types.NewID(), LeafID: leafID, State: workunit.WorkUnitStateQueued},
		effectiveRedundancy: 1,
		dbActiveCount:       1,
		probationCoverage:   0,
	}

	req := types.NewID()
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok, reason := c.eligibleLocked(req, req, capableOpts(req, 0), openCand); !ok {
		t.Fatalf("requester refused (%v) despite the only seeded coverage being non-countable", reason)
	}
	if ok, reason := c.eligibleLocked(req, req, capableOpts(req, 0), fullCand); ok || reason != rejectRedundancyFull {
		t.Fatalf("requester eligibleLocked with countable seed = (%v, %v), want (false, rejectRedundancyFull)", ok, reason)
	}
}

// TestEligibleLocked_ConcurrencyCapStaysRaw: the in-memory holder cap bounds SIMULTANEOUS
// holders regardless of standing — a redundancy-2 candidate fully held by two PROBATION
// accounts (countable coverage 0) is still refused by the RAW holder cap, not offered a third
// concurrent copy. (Forced replication happens over time as those copies drop out, not by
// stacking more than N concurrent copies.)
func TestEligibleLocked_ConcurrencyCapStaysRaw(t *testing.T) {
	wuRepo, leafRepo := &fakeWURepo{}, &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	cand := candidate{
		unit:                &workunit.WorkUnit{ID: unitID, LeafID: leafID, State: workunit.WorkUnitStateQueued},
		effectiveRedundancy: 2,
	}

	h1, h2 := types.NewID(), types.NewID()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reservedInMem[unitID] = map[types.ID]heldCopy{
		h1: {reservedUntil: c.now().Add(time.Minute), hostID: h1, subject: "vol:h1"},
		h2: {reservedUntil: c.now().Add(time.Minute), hostID: h2, subject: "vol:h2"},
	}
	c.standingSnapshot = map[types.ID]standing.Entry{h1: probationEntry(), h2: probationEntry()}
	c.standingSnapshotAt = c.now()

	req := types.NewID()
	ok, reason := c.eligibleLocked(req, req, capableOpts(req, 0), cand)
	if ok || reason != rejectHolderCap {
		t.Fatalf("eligibleLocked with two PROBATION holders = (%v, %v), want (false, rejectHolderCap) — the concurrency cap is standing-agnostic", ok, reason)
	}
}

// TestHandOut_ProbationInflightFloor: with the reliability quota on and a warmed budget above
// the floor, an OK requester keeps its full adaptive budget while a PROBATION requester is
// pinned to the cold-start floor — a neutralized account cannot hog capacity. The two paths
// use separate caches so the first requester draining its (redundancy-1) units cannot affect
// the other's ready pool.
func TestHandOut_ProbationInflightFloor(t *testing.T) {
	const floor, flatCap, budget, nUnits = 1, 10, 5, 3

	// stageThree stages nUnits redundancy-1 units on a warmed leaf and warms host's budget.
	stageThree := func(c *dispatchCache, leafRepo *fakeLeafRepo, host types.ID) {
		leafID := types.NewID()
		c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
		for i := 0; i < nUnits; i++ {
			c.stageUnit(types.NewID(), leafID, 1, 0)
		}
		c.budgetMu.Lock()
		c.hostBudgetCache[host] = budget // above the floor, so the floor (not a miss) differentiates
		c.budgetMu.Unlock()
	}

	// OK requester: budget 5 >= 3, takes all three units it asks for.
	okVol := types.NewID()
	cOK, _, leafRepoOK := newQuotaCache(true, floor, flatCap, nil)
	stageThree(cOK, leafRepoOK, okVol)
	if res, _ := cOK.HandOut(okVol, capableOpts(okVol, flatCap), nUnits); len(res) != nUnits {
		t.Fatalf("OK requester hand-out = %d, want %d (full adaptive budget)", len(res), nUnits)
	}

	// PROBATION requester: pinned to floor 1, so it takes only one of the three.
	probVol := types.NewID()
	cProb, _, leafRepoProb := newQuotaCache(true, floor, flatCap, nil)
	stageThree(cProb, leafRepoProb, probVol)
	cProb.setStandingSnapshot(map[types.ID]standing.Entry{probVol: probationEntry()})
	if res, _ := cProb.HandOut(probVol, capableOpts(probVol, flatCap), nUnits); len(res) != floor {
		t.Fatalf("PROBATION requester hand-out = %d, want %d (pinned to the cold-start floor)", len(res), floor)
	}
}

// TestRefreshStanding_NilRepoTolerated: a nil standing repo leaves the snapshot nil (everyone
// OK) and refreshStanding is a no-op that never panics.
func TestRefreshStanding_NilRepoTolerated(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{}) // deps has no standingRepo -> nil
	c.refreshStanding(context.Background())
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.standingSnapshot != nil {
		t.Fatal("nil standing repo: snapshot should stay nil")
	}
	if got := c.effectiveStandingLocked(types.NewID()); got != volunteer.StandingOK {
		t.Fatalf("nil snapshot effectiveStandingLocked = %q, want OK", got)
	}
}

// TestRefreshStanding_PopulatesAndCaches: with a wired standing repo, refreshStanding loads
// the non-OK population once, and a second call within the TTL does not re-read the store.
func TestRefreshStanding_PopulatesAndCaches(t *testing.T) {
	benched := types.NewID()
	repo := &fakeStandingRepo{entries: map[types.ID]standing.Entry{benched: benchedEntry(nil)}}
	c := newDispatchCache(dispatchCacheConfig{
		readyPoolSize: 100, admissionCap: 4, flushInterval: time.Hour,
	}, dispatchDeps{
		wuRepo: &fakeWURepo{}, leafRepo: &fakeLeafRepo{}, assignRepo: &fakeAssignRepo{}, standingRepo: repo,
	}, testLogger())

	c.refreshStanding(context.Background())
	c.mu.Lock()
	if _, ok := c.standingSnapshot[benched]; !ok {
		c.mu.Unlock()
		t.Fatalf("refreshStanding did not load the benched account into the snapshot")
	}
	if got := c.effectiveStandingLocked(benched); got != volunteer.StandingBenched {
		c.mu.Unlock()
		t.Fatalf("effectiveStandingLocked(benched) = %q, want BENCHED", got)
	}
	c.mu.Unlock()

	// A second refresh within the TTL is served from the cache (no extra store read).
	c.refreshStanding(context.Background())
	if repo.calls != 1 {
		t.Fatalf("standing store read %d times, want 1 (second refresh within TTL should be cached)", repo.calls)
	}
}
