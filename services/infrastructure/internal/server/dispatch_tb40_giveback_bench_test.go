package server

// TB-40 regression tests: a give-back (or mid-run abandon) closed the volunteer's
// copy in SQL — starting the authoritative re-offer cooldown — but never recorded
// that close on the still-staged candidate's bench map, a refill-time snapshot that
// refreshes only on re-stage. eligibleLocked read the stale snapshot, admitted the
// closer's next poll, and handed out a reservation the SQL landing was guaranteed to
// refuse; the client was never told and buffered a phantom that died at run-start
// (~14 % of fleet slot starts on 2026-08-02, two idle-slot episodes per affected
// host).
//
// The close-time bench must mirror the SQL cooldown gate (cooldownGuardSQL) EXACTLY:
// RETURNED benches the short re-offer throttle, a STARTED abandon benches ~one
// deadline, and a graceful un-started ABANDONED return benches NOTHING (#59) — the
// first cut of this fix benched every abandon and stranded the one-volunteer pool
// the graceful-buffer-return e2e test pins.
//
// Red evidence (2026-08-03, pre-fix tree, tests in place): both bench tests failed
// with "hand-out inside the ... cooldown = 1 results, want 0" — the handler released
// the hold and nothing benched, so the same volunteer was re-offered the unit it had
// just closed.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// tb40WURepo backs the SERVICE side of the abandon path over the cache fakes: the
// close write reports the facts a real row would (the honored outcome and the copy's
// started-ness — the repo's own downgrade/verification is pinned by the TB-35
// integration tests) and the retry-ceiling probe reports headroom, so the unit stays
// QUEUED and staged.
type tb40WURepo struct {
	fakeWURepo
	closed workunit.ClosedCopy
}

func (m *tb40WURepo) CloseCopyByVolunteer(_ context.Context, _ types.ID, _ types.ID, _ string, _ *types.ID, _ string) (workunit.ClosedCopy, error) {
	return m.closed, nil
}

func (m *tb40WURepo) DeadLetterIfExhausted(_ context.Context, _ types.ID) (bool, error) {
	return false, nil
}

// tb40Service wires a volunteerService onto a test dispatch cache holding ONE staged
// redundancy-2 candidate with the given copy deadline, an identity warmed for the
// returned volunteer (auth resolves in memory), and a settable clock. Background
// goroutines are not started; the tests drive HandOut / AbandonWorkUnit directly.
func tb40Service(t *testing.T, deadlineSeconds int, closed workunit.ClosedCopy) (svc *volunteerService, c *dispatchCache, ctx context.Context, volID, unitID types.ID, advance func(time.Duration)) {
	t.Helper()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	wuRepo := &tb40WURepo{closed: closed}
	leafRepo := &fakeLeafRepo{}
	c = newTestCache(&wuRepo.fakeWURepo, leafRepo, &fakeAssignRepo{})

	base := time.Now().UTC()
	now := base
	c.now = func() time.Time { return now }

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID = types.NewID()
	c.mu.Lock()
	c.ready = append(c.ready, candidate{
		unit: &workunit.WorkUnit{
			ID:              unitID,
			LeafID:          leafID,
			State:           workunit.WorkUnitStateQueued,
			DeadlineSeconds: deadlineSeconds,
		},
		effectiveRedundancy: 2,
	})
	c.mu.Unlock()

	v := &volunteer.Volunteer{ID: types.NewID(), PublicKey: pub, IsActive: true}
	c.putIdentity(v)

	svc = &volunteerService{
		wuRepo:        wuRepo,
		logger:        testLogger(),
		dispatchCache: c,
	}

	ctx = contextWithGRPCAuthPublicKey(context.Background(), pub)
	return svc, c, ctx, v.ID, unitID, func(d time.Duration) { now = base.Add(d) }
}

// TestAbandonGiveback_BenchesVolunteerOnStagedCandidate pins the systematic TB-40
// cycle: buffer-full give-back → RETURNED close (600 s SQL re-offer cooldown) → the
// volunteer's next poll 30–60 s later. The close must bench the volunteer on the
// staged candidate so that poll is REFUSED in memory — pre-fix the stale snapshot
// admitted it and the hand-out became a phantom at the SQL landing. The bench must
// also carry the outcome time so the pool-exhausted fallback still re-admits the
// volunteer once the unit has sat uncovered past the grace — the escape that keeps a
// small pool from stranding (PB-9), which the blind flush-conflict void bench cannot
// offer (fallbackAt == until).
func TestAbandonGiveback_BenchesVolunteerOnStagedCandidate(t *testing.T) {
	svc, c, ctx, volID, unitID, advance := tb40Service(t, 18000,
		workunit.ClosedCopy{Outcome: "RETURNED", Started: false})

	if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 1 {
		t.Fatalf("initial hand-out = %d results, want 1", len(res))
	}

	if _, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:    unitID.String(),
		VolunteerId:   volID.String(),
		Reason:        "work buffer full (over the hours target)",
		UnrunGiveback: true,
	}); err != nil {
		t.Fatalf("AbandonWorkUnit(giveback): %v", err)
	}

	// The volunteer polls again inside the re-offer cooldown (live cadence: 30–60 s).
	if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 0 {
		t.Fatalf("hand-out inside the re-offer cooldown = %d results, want 0: the give-back close must bench the volunteer on the still-staged candidate, or the SQL landing refuses the reservation and the client buffers a phantom that dies at run-start (TB-40)", len(res))
	}

	// Past the pool-exhausted fallback grace (120 s), still inside the 600 s window,
	// unit still uncovered (nobody fresh took it): the bench must yield, exactly like
	// the SQL cooldown's fallback term. The jump also ages the leaf snapshot past
	// leafSnapshotTTL, which the visibility gate fail-closes on for un-pinned
	// requesters (PB-38b); model the refiller tick's refresh a live head runs so only
	// the bench fallback is under test here.
	advance(3 * time.Minute)
	c.refreshStaleLeafSnapshots(context.Background())
	if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 1 {
		t.Fatal("volunteer still refused past the pool-exhausted fallback grace with the unit uncovered: the close-time bench must carry the outcome time and yield like the SQL cooldown's fallback, or a small pool strands (PB-9)")
	}
}

// TestAbandonMidRun_BenchesVolunteerOnStagedCandidate pins the ABANDONED arm of the
// same defect: a mid-run abandon (the copy had STARTED — a real reliability signal)
// starts the ~one-deadline SQL cooldown, so the closer's next poll must be refused
// in memory too. The stale-snapshot blindness predates the RETURNED outcome and
// applies to every benching close the cache performs.
func TestAbandonMidRun_BenchesVolunteerOnStagedCandidate(t *testing.T) {
	svc, c, ctx, volID, unitID, _ := tb40Service(t, 18000,
		workunit.ClosedCopy{Outcome: "ABANDONED", Started: true})

	if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 1 {
		t.Fatalf("initial hand-out = %d results, want 1", len(res))
	}

	if _, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  unitID.String(),
		VolunteerId: volID.String(),
		Reason:      "process exited with code 137",
	}); err != nil {
		t.Fatalf("AbandonWorkUnit(mid-run): %v", err)
	}

	if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 0 {
		t.Fatalf("hand-out inside the abandon cooldown = %d results, want 0: a started-copy ABANDONED close must bench the volunteer on the still-staged candidate (TB-40)", len(res))
	}
}

// TestAbandonGracefulUnstarted_DoesNotBench pins the #59 parity boundary the fix
// must NOT cross: a graceful return of un-started buffered work (plain abandon,
// started_at NULL — e.g. "volunteer shutdown") is not a reliability signal and does
// not feed the SQL cooldown, so the close-time bench must record NOTHING — otherwise
// a one-volunteer pool is refused in memory what the SQL landing would grant at
// once. This is a guard against over-benching (the first cut of the TB-40 fix failed
// exactly this property in TestDispatchCache_GracefulBufferReturn_ReReservable),
// not a differential regression test: it passes on the pre-fix tree too.
func TestAbandonGracefulUnstarted_DoesNotBench(t *testing.T) {
	svc, c, ctx, volID, unitID, _ := tb40Service(t, 18000,
		workunit.ClosedCopy{Outcome: "ABANDONED", Started: false})

	if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 1 {
		t.Fatalf("initial hand-out = %d results, want 1", len(res))
	}

	if _, err := svc.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  unitID.String(),
		VolunteerId: volID.String(),
		Reason:      "volunteer shutdown",
	}); err != nil {
		t.Fatalf("AbandonWorkUnit(graceful): %v", err)
	}

	if res, _ := c.HandOut(volID, capableOpts(volID, 0), 1); len(res) != 1 {
		t.Fatal("volunteer refused after a graceful un-started return: the close-time bench must mirror the SQL gate's #59 arm (no bench for ABANDONED with started_at NULL), or a one-volunteer pool strands on every buffer return")
	}
}
