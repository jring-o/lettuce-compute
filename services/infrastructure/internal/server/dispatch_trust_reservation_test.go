package server

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// --- trusted-corroborator reservation tests -----------------------------------
//
// The reservation withholds a unit's last slots from UNTRUSTED requesters so a leaf
// requiring K trusted corroborators can still complete its quorum with trusted results.
// These exercise the in-memory half only (no DB): candidates are staged directly with the
// trust fields the SQL refill would populate, and the trust-score snapshot is installed
// in memory, mirroring how the refill path would refresh it.

// trustOpts returns capable AssignmentOptions carrying an explicit trust subject, so a
// test controls whether the requester is classified trusted (its subject's snapshot score
// vs. the leaf floor) independently of the volunteer id.
func trustOpts(vol types.ID, subject string) workunit.AssignmentOptions {
	o := capableOpts(vol, 0)
	o.TrustSubject = subject
	return o
}

// stageTrustUnit stages a ready candidate carrying a trusted-corroborator requirement.
// trustedContribs seeds the refill-time trusted-contributor snapshot (subjects already
// counting trusted toward the unit). Caller must have warmed the leaf.
func (c *dispatchCache) stageTrustUnit(unitID, leafID types.ID, redundancy, dbActive, trustK, trustFloor int, trustedContribs []string) {
	c.mu.Lock()
	c.ready = append(c.ready, candidate{
		unit:                &workunit.WorkUnit{ID: unitID, LeafID: leafID, State: workunit.WorkUnitStateQueued},
		effectiveRedundancy: redundancy,
		dbActiveCount:       dbActive,
		effectiveTrustK:     trustK,
		effectiveTrustFloor: trustFloor,
		trustedContributors: strSet(trustedContribs),
	})
	c.mu.Unlock()
}

// setTrustScores installs the in-memory trust-score snapshot directly (the unit tests
// drive the reservation without the DB-backed refill that normally refreshes it).
func (c *dispatchCache) setTrustScores(scores map[string]int) {
	c.mu.Lock()
	c.trustScores = scores
	c.trustScoresAt = c.now()
	c.mu.Unlock()
}

// TestHandOut_TrustReservation_GateOffUnchanged verifies the gate-off fast path: a
// candidate with effectiveTrustK == 0 is dispatched exactly as before — two distinct
// untrusted volunteers each take a copy of a redundancy-2 unit — with an empty trust-score
// snapshot, proving the reservation adds no behavior when the leaf requires no trust.
func TestHandOut_TrustReservation_GateOffUnchanged(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageTrustUnit(unitID, leafID, 2, 0, 0 /*K*/, 0 /*floor*/, nil)

	volA := types.NewID()
	if resA, _ := c.HandOut(volA, trustOpts(volA, "vol:a"), 1); len(resA) != 1 {
		t.Fatalf("volA (gate off) hand-out = %d, want 1", len(resA))
	}
	volB := types.NewID()
	if resB, _ := c.HandOut(volB, trustOpts(volB, "vol:b"), 1); len(resB) != 1 {
		t.Fatalf("volB (gate off) hand-out = %d, want 1 (both untrusted copies dispatch)", len(resB))
	}
}

// TestHandOut_TrustReservation_UntrustedRefusedAtReservedSlot verifies that with
// redundancy 2 and K 1, the FIRST untrusted volunteer takes a copy (a non-reserved slot)
// but the SECOND untrusted volunteer is refused — the unit's last slot is reserved for a
// trusted subject the quorum still needs.
func TestHandOut_TrustReservation_UntrustedRefusedAtReservedSlot(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageTrustUnit(unitID, leafID, 2, 0, 1 /*K*/, 10 /*floor*/, nil)
	c.setTrustScores(map[string]int{}) // nobody trusted

	volA := types.NewID()
	if resA, _ := c.HandOut(volA, trustOpts(volA, "vol:a"), 1); len(resA) != 1 {
		t.Fatalf("first untrusted hand-out = %d, want 1 (non-reserved slot)", len(resA))
	}
	volB := types.NewID()
	if resB, _ := c.HandOut(volB, trustOpts(volB, "vol:b"), 1); len(resB) != 0 {
		t.Fatalf("second untrusted hand-out = %d, want 0 (last slot reserved for trusted)", len(resB))
	}
}

// TestHandOut_TrustReservation_TrustedAdmittedToReservedSlot verifies a TRUSTED requester
// (snapshot score >= floor) is never blocked by the reservation: after an untrusted
// volunteer fills the first slot, a trusted volunteer fills the reserved last slot.
func TestHandOut_TrustReservation_TrustedAdmittedToReservedSlot(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageTrustUnit(unitID, leafID, 2, 0, 1 /*K*/, 10 /*floor*/, nil)
	c.setTrustScores(map[string]int{"did:trusted": 10})

	volA := types.NewID()
	if resA, _ := c.HandOut(volA, trustOpts(volA, "vol:a"), 1); len(resA) != 1 {
		t.Fatalf("untrusted hand-out = %d, want 1", len(resA))
	}
	volB := types.NewID()
	if resB, _ := c.HandOut(volB, trustOpts(volB, "did:trusted"), 1); len(resB) != 1 {
		t.Fatalf("trusted hand-out = %d, want 1 (trusted fills the reserved slot)", len(resB))
	}
}

// TestHandOut_TrustReservation_TrustedContributorFreesSlot verifies the refill-time
// trusted-contributor snapshot (TrustedContributorSubjects) frees a reserved slot: with a
// trusted subject already counting toward the unit, an untrusted requester is admitted;
// without it, the same requester is refused.
func TestHandOut_TrustReservation_TrustedContributorFreesSlot(t *testing.T) {
	leafID := types.NewID()

	// Case A: a trusted contributor is already present (one active copy, trusted). The
	// remaining slot is no longer reserved, so an untrusted requester is admitted.
	{
		wuRepo := &fakeWURepo{}
		leafRepo := &fakeLeafRepo{}
		c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
		c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
		unitID := types.NewID()
		c.stageTrustUnit(unitID, leafID, 2, 1 /*dbActive*/, 1 /*K*/, 10, []string{"did:trusted"})
		c.setTrustScores(map[string]int{"did:trusted": 10})

		vol := types.NewID()
		if res, _ := c.HandOut(vol, trustOpts(vol, "vol:untrusted"), 1); len(res) != 1 {
			t.Fatalf("untrusted hand-out with trusted contributor present = %d, want 1", len(res))
		}
	}

	// Case B: same unit but NO trusted contributor (the one active copy is untrusted). The
	// remaining slot stays reserved, so the untrusted requester is refused.
	{
		wuRepo := &fakeWURepo{}
		leafRepo := &fakeLeafRepo{}
		c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
		c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
		unitID := types.NewID()
		c.stageTrustUnit(unitID, leafID, 2, 1 /*dbActive*/, 1 /*K*/, 10, nil)
		c.setTrustScores(map[string]int{"did:trusted": 10})

		vol := types.NewID()
		if res, _ := c.HandOut(vol, trustOpts(vol, "vol:untrusted"), 1); len(res) != 0 {
			t.Fatalf("untrusted hand-out with no trusted contributor = %d, want 0 (slot reserved)", len(res))
		}
	}
}

// TestHandOut_TrustReservation_TrustedHoldFreesSlot verifies a TRUSTED in-memory hold
// counts toward the quorum: a trusted volunteer takes the first slot, and an untrusted
// volunteer is then admitted to the last slot (the reservation is satisfied by the trusted
// hold). Contrast TestHandOut_TrustReservation_UntrustedRefusedAtReservedSlot, where the
// first holder is untrusted and the second untrusted volunteer is refused.
func TestHandOut_TrustReservation_TrustedHoldFreesSlot(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageTrustUnit(unitID, leafID, 2, 0, 1 /*K*/, 10 /*floor*/, nil)
	c.setTrustScores(map[string]int{"did:trusted": 10})

	// A trusted volunteer takes the first slot; its hold carries the trusted subject.
	volT := types.NewID()
	if resT, _ := c.HandOut(volT, trustOpts(volT, "did:trusted"), 1); len(resT) != 1 {
		t.Fatalf("trusted hand-out = %d, want 1", len(resT))
	}
	// An untrusted volunteer is now admitted to the last slot: the trusted hold already
	// satisfies the K=1 reservation.
	volU := types.NewID()
	if resU, _ := c.HandOut(volU, trustOpts(volU, "vol:untrusted"), 1); len(resU) != 1 {
		t.Fatalf("untrusted hand-out after trusted hold = %d, want 1 (reservation satisfied)", len(resU))
	}
}

// TestHandOut_TrustReservation_K2Arithmetic exercises K>1: a redundancy-3 unit requiring 2
// trusted corroborators admits exactly ONE untrusted copy (2 slots reserved) and then
// refuses further untrusted requesters until a trusted subject fills a reserved slot.
func TestHandOut_TrustReservation_K2Arithmetic(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 3, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageTrustUnit(unitID, leafID, 3, 0, 2 /*K*/, 10 /*floor*/, nil)
	c.setTrustScores(map[string]int{"did:trusted": 10})

	// First untrusted copy fits (3 - 2 reserved = 1 untrusted slot).
	volA := types.NewID()
	if resA, _ := c.HandOut(volA, trustOpts(volA, "vol:a"), 1); len(resA) != 1 {
		t.Fatalf("first untrusted (K=2) hand-out = %d, want 1", len(resA))
	}
	// Second untrusted copy is refused: both remaining slots are reserved for trusted.
	volB := types.NewID()
	if resB, _ := c.HandOut(volB, trustOpts(volB, "vol:b"), 1); len(resB) != 0 {
		t.Fatalf("second untrusted (K=2) hand-out = %d, want 0 (2 slots reserved)", len(resB))
	}
	// A trusted volunteer takes one reserved slot.
	volT := types.NewID()
	if resT, _ := c.HandOut(volT, trustOpts(volT, "did:trusted"), 1); len(resT) != 1 {
		t.Fatalf("trusted (K=2) hand-out = %d, want 1", len(resT))
	}
	// One trusted slot is still reserved, so another untrusted volunteer is still refused.
	volC := types.NewID()
	if resC, _ := c.HandOut(volC, trustOpts(volC, "vol:c"), 1); len(resC) != 0 {
		t.Fatalf("third untrusted (K=2, 1 trusted present) hand-out = %d, want 0 (1 slot still reserved)", len(resC))
	}
}

// TestHandOut_TrustReservation_FloorRespectedViaScoreMap verifies the floor is the
// classification threshold read from the score snapshot: with one untrusted holder already
// occupying a slot, a requester scoring BELOW the floor is refused (untrusted), while a
// requester scoring AT the floor is admitted (trusted) to the reserved slot.
func TestHandOut_TrustReservation_FloorRespectedViaScoreMap(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	c.stageTrustUnit(unitID, leafID, 2, 0, 1 /*K*/, 10 /*floor*/, nil)
	c.setTrustScores(map[string]int{"vol:below": 5, "vol:at": 10})

	// Untrusted holder fills the first slot.
	volH := types.NewID()
	if resH, _ := c.HandOut(volH, trustOpts(volH, "vol:holder"), 1); len(resH) != 1 {
		t.Fatalf("holder hand-out = %d, want 1", len(resH))
	}
	// Score 5 < floor 10: untrusted, refused at the reserved slot.
	volBelow := types.NewID()
	if res, _ := c.HandOut(volBelow, trustOpts(volBelow, "vol:below"), 1); len(res) != 0 {
		t.Fatalf("below-floor hand-out = %d, want 0 (score 5 < floor 10)", len(res))
	}
	// Score 10 >= floor 10: trusted, admitted to the reserved slot.
	volAt := types.NewID()
	if res, _ := c.HandOut(volAt, trustOpts(volAt, "vol:at"), 1); len(res) != 1 {
		t.Fatalf("at-floor hand-out = %d, want 1 (score 10 >= floor 10)", len(res))
	}
}

// TestEligibleLocked_TrustReservation_ReasonAndBypass asserts the exact reject reason for
// an untrusted requester at a reserved slot (rejectTrustReserved), and that a trusted
// requester bypasses the reservation entirely.
func TestEligibleLocked_TrustReservation_ReasonAndBypass(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	cand := candidate{
		unit:                &workunit.WorkUnit{ID: unitID, LeafID: leafID, State: workunit.WorkUnitStateQueued},
		effectiveRedundancy: 2,
		effectiveTrustK:     1,
		effectiveTrustFloor: 10,
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// One untrusted holder already occupies a slot; the last slot is reserved for trust.
	holderVol := types.NewID()
	c.reservedInMem[unitID] = map[types.ID]heldCopy{
		holderVol: {reservedUntil: c.now().Add(time.Minute), hostID: holderVol, subject: "vol:holder"},
	}
	c.trustScores = map[string]int{"did:trusted": 10}
	c.trustScoresAt = c.now()

	untrustedVol := types.NewID()
	ok, reason := c.eligibleLocked(untrustedVol, untrustedVol, trustOpts(untrustedVol, "vol:req"), cand)
	if ok {
		t.Fatal("untrusted requester should be refused at the reserved slot")
	}
	if reason != rejectTrustReserved {
		t.Fatalf("reject reason = %v, want rejectTrustReserved", reason)
	}

	trustedVol := types.NewID()
	if ok, _ := c.eligibleLocked(trustedVol, trustedVol, trustOpts(trustedVol, "did:trusted"), cand); !ok {
		t.Fatal("trusted requester should bypass the reservation and be eligible")
	}
}

// TestDispatchCache_NilTrustRepo_Tolerated verifies a nil trust repo (tests / no pool) is
// tolerated: refreshTrustScores is a no-op that leaves the snapshot nil, a K==0 candidate
// dispatches normally, and a K>0 candidate still hands out its non-reserved slots (a nil
// snapshot classifies nobody trusted, so the reservation is simply conservative).
func TestDispatchCache_NilTrustRepo_Tolerated(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{}) // dispatchDeps has no trustRepo -> nil

	// refreshTrustScores must not panic and must leave the snapshot nil.
	c.refreshTrustScores(context.Background())
	c.mu.Lock()
	if c.trustScores != nil {
		t.Fatal("nil trust repo: snapshot should stay nil")
	}
	c.mu.Unlock()

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)

	// K==0 candidate: dispatches normally to two untrusted volunteers.
	gateOff := types.NewID()
	c.stageTrustUnit(gateOff, leafID, 2, 0, 0 /*K*/, 0, nil)
	v1, v2 := types.NewID(), types.NewID()
	if res, _ := c.HandOut(v1, trustOpts(v1, "vol:1"), 1); len(res) != 1 {
		t.Fatalf("nil-repo K=0 first hand-out = %d, want 1", len(res))
	}
	if res, _ := c.HandOut(v2, trustOpts(v2, "vol:2"), 1); len(res) != 1 {
		t.Fatalf("nil-repo K=0 second hand-out = %d, want 1", len(res))
	}

	// K>0 candidate: the first (non-reserved) slot still hands out; the reserved slot is
	// withheld from the second untrusted volunteer (conservative with a nil snapshot).
	gated := types.NewID()
	c.stageTrustUnit(gated, leafID, 2, 0, 1 /*K*/, 10, nil)
	v3, v4 := types.NewID(), types.NewID()
	if res, _ := c.HandOut(v3, trustOpts(v3, "vol:3"), 1); len(res) != 1 {
		t.Fatalf("nil-repo K=1 first hand-out = %d, want 1 (non-reserved slot)", len(res))
	}
	if res, _ := c.HandOut(v4, trustOpts(v4, "vol:4"), 1); len(res) != 0 {
		t.Fatalf("nil-repo K=1 second hand-out = %d, want 0 (reserved, conservative)", len(res))
	}
}
