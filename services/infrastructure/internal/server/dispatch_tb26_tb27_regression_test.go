package server

// TB-26 / TB-27 regression tests (2026-07-29 fleet strand).
//
// TB-26: the bench arm's pool-exhausted fallback (PB-9) gated exhaustion on
// cand.dbActiveCount — a refill-time snapshot that is never refreshed while the
// candidate stays staged (rejected candidates stay in ready, refill excludes staged
// ids). A unit staged at any moment when it had a live copy or a PENDING result
// carried dbActiveCount >= 1 forever, so `exhausted` was frozen false and every
// benched account was refused until `until` — the full leaf deadline — instead of
// the 120s grace the SQL cooldown's own fallback grants. On the beta fleet two
// failed units stranded 10+ hours while their benched accounts polled every ~25min.
//
// TB-27 (WARN half): those refusals were invisible head-side — the starvation WARN
// fired only for the in-flight cap (TB-13) and capability mismatch (TB-21), so a
// machine refused everything because of ITS OWN account state (benched, or already
// a contributor) logged nothing at any production level.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TestHandOut_BenchFallback_IgnoresStaleActiveSnapshot: a benched volunteer must be
// re-admitted once the fallback grace has passed and the cache holds no live copy of
// the unit, REGARDLESS of the refill-time coverage snapshot — the snapshot goes stale
// while the candidate sits staged, and the SQL landing re-checks the cooldown
// authoritatively either way (a wrong admit costs one voided hand-out; the frozen
// refusal had no correction because the refused request never reached the SQL).
func TestHandOut_BenchFallback_IgnoresStaleActiveSnapshot(t *testing.T) {
	// Control — staged with a zero coverage snapshot: the PB-9 fallback as designed.
	// The bench refuses inside the grace and yields after it.
	{
		wuRepo := &fakeWURepo{}
		leafRepo := &fakeLeafRepo{}
		c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

		base := time.Now().UTC()
		now := base
		c.now = func() time.Time { return now }

		leafID := types.NewID()
		c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
		unitID := types.NewID()
		volA := types.NewID()
		c.stageUnitSets(unitID, leafID, 1, 0, nil, []types.ID{volA})

		now = base.Add(60 * time.Second)
		c.refreshStaleLeafSnapshots(context.Background())
		if res, _ := c.HandOut(volA, capableOpts(volA, 0), 1); len(res) != 0 {
			t.Fatalf("control at +60s: benched volA got %d results, want 0 (inside the fallback grace, fresh volunteers keep first refusal)", len(res))
		}

		now = base.Add(121 * time.Second)
		c.refreshStaleLeafSnapshots(context.Background())
		if res, _ := c.HandOut(volA, capableOpts(volA, 0), 1); len(res) != 1 {
			t.Fatalf("control at +121s: benched volA got %d results, want 1 (pool-exhausted fallback re-admits, PB-9)", len(res))
		}
	}

	// TB-26 — identical bench, but the refill snapshot recorded coverage (a live copy
	// or PENDING result at staging time, since resolved; nothing updates the snapshot
	// while the candidate stays staged). The fallback must behave exactly as the
	// control: the frozen snapshot must not hold the bench past the grace.
	{
		wuRepo := &fakeWURepo{}
		leafRepo := &fakeLeafRepo{}
		c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

		base := time.Now().UTC()
		now := base
		c.now = func() time.Time { return now }

		leafID := types.NewID()
		c.warm(nativeLeaf(leafID, 3, false, 0), leafRepo)
		unitID := types.NewID()
		volA := types.NewID()
		c.stageUnitSets(unitID, leafID, 3, 1 /* stale refill snapshot */, nil, []types.ID{volA})

		now = base.Add(121 * time.Second)
		c.refreshStaleLeafSnapshots(context.Background())
		if res, _ := c.HandOut(volA, capableOpts(volA, 0), 1); len(res) != 1 {
			t.Fatalf("TB-26: benched volA got %d results at +121s, want 1 — the stale dbActiveCount snapshot froze the pool-exhausted fallback shut, stranding the unit for the leaf's whole deadline", len(res))
		}
	}
}

// starvationWarnCache builds a cache identical to newTestCache's but logging to buf,
// so the starvation WARN's presence and content can be asserted. The handler is at
// the default Info level — exactly a production head — so the Debug reject tally
// cannot mask a missing WARN.
func starvationWarnCache(buf *bytes.Buffer, wuRepo *fakeWURepo, leafRepo *fakeLeafRepo) *dispatchCache {
	return newDispatchCache(dispatchCacheConfig{
		readyPoolSize:           100,
		lowWatermark:            10,
		refillBatchSize:         50,
		admissionCap:            4,
		flushInterval:           time.Hour,
		flushBatchSize:          200,
		leaseSeconds:            900,
		maxInflightPerVolunteer: 0,
	}, dispatchDeps{
		wuRepo:     wuRepo,
		leafRepo:   leafRepo,
		assignRepo: &fakeAssignRepo{},
	}, slog.New(slog.NewTextHandler(buf, nil)))
}

// TestHandOut_BenchedOnlyStarvation_EmitsWarn: a machine handed nothing because every
// ready unit refuses its ACCOUNT (benched here) must produce the throttled starvation
// WARN with the bench refusal in its tally — before TB-27 these refusals reached no
// sink above Debug and a stranded unit was indistinguishable from an empty queue.
func TestHandOut_BenchedOnlyStarvation_EmitsWarn(t *testing.T) {
	var buf bytes.Buffer
	leafRepo := &fakeLeafRepo{}
	c := starvationWarnCache(&buf, &fakeWURepo{}, leafRepo)

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	unitID := types.NewID()
	volA := types.NewID()
	c.stageUnitSets(unitID, leafID, 1, 0, nil, []types.ID{volA})

	if res, _ := c.HandOut(volA, capableOpts(volA, 0), 1); len(res) != 0 {
		t.Fatalf("benched volA got %d results, want 0", len(res))
	}
	out := buf.String()
	if !strings.Contains(out, "no work handed out") {
		t.Fatalf("no starvation WARN for a benched-only refusal — the TB-26 strand shape stays invisible head-side (TB-27); log output:\n%s", out)
	}
	if !strings.Contains(out, "refused_benched_cooldown=1") {
		t.Fatalf("starvation WARN lacks the bench tally; log output:\n%s", out)
	}
}

// TestHandOut_ContributedOnlyStarvation_EmitsWarn: same surface for the
// already-contributed refusal — the other account-specific reason a client cannot
// see (its result is PENDING head-side; the client only knows it got nothing).
func TestHandOut_ContributedOnlyStarvation_EmitsWarn(t *testing.T) {
	var buf bytes.Buffer
	leafRepo := &fakeLeafRepo{}
	c := starvationWarnCache(&buf, &fakeWURepo{}, leafRepo)

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	unitID := types.NewID()
	volA := types.NewID()
	c.stageUnitSets(unitID, leafID, 2, 1, []types.ID{volA}, nil)

	if res, _ := c.HandOut(volA, capableOpts(volA, 0), 1); len(res) != 0 {
		t.Fatalf("prior contributor volA got %d results, want 0", len(res))
	}
	out := buf.String()
	if !strings.Contains(out, "no work handed out") {
		t.Fatalf("no starvation WARN for an already-contributed refusal (TB-27); log output:\n%s", out)
	}
	if !strings.Contains(out, "refused_already_contributed=1") {
		t.Fatalf("starvation WARN lacks the contributed tally; log output:\n%s", out)
	}
}
