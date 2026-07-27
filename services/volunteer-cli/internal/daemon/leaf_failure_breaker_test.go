package daemon

import (
	"context"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Regression tests for TB-10 — a failing leaf is fetched, failed and abandoned
// in a tight silent loop.
//
// Live evidence from lbry.science: 17 native units fetched and abandoned by one
// tester's two accounts, zero completed, consecutive abandons ~200 ms apart
// (20:24:40.16 → .37 → .57 → .78 → .99 → …) with the same work_unit_ids
// recurring across both accounts. The head requeued each failure immediately and
// the client took another straight away. The runtime-level breaker could not
// stop it: the runtime WAS registered and Prepare DID succeed, so no
// capability-driven abandon was ever recorded. Nothing bounded the loop.

// failureTestClock is a hand-advanced clock, so the cooldown is exercised
// without a sleeping test.
type failureTestClock struct{ t time.Time }

func (c *failureTestClock) now() time.Time          { return c.t }
func (c *failureTestClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// breakerDaemon builds the fetcher-test daemon with a failure tracker on the
// given clock, plus a head offering the named leafs.
func breakerDaemon(t *testing.T, clock *failureTestClock, leafSlugs ...string) (*Daemon, *ServerConnection, *int) {
	t.Helper()

	requests := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			requests++
			// Answer with no work: this test counts REQUESTS, which is what the
			// burn loop consumed. Buffering units would only add noise.
			return &lettucev1.RequestWorkUnitResponse{}, nil
		},
	}
	srv := &ServerConnection{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}
	d := newFetcherTestDaemon([]*ServerConnection{srv})
	d.leafFailures = newLeafFailureTracker(clock.now)

	leafs := make([]CachedLeafInfo, 0, len(leafSlugs))
	weights := make(map[string]int, len(leafSlugs))
	for _, slug := range leafSlugs {
		leafs = append(leafs, CachedLeafInfo{
			ID: slug, Slug: slug, Name: slug, State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"linux_amd64": "u"}},
		})
		weights[slug] = 100
	}
	d.leafCache.PopulateForTest(srv.Name, &CachedHeadInfo{
		Name: srv.Name, Leafs: leafs, DefaultWeights: weights,
	})
	d.weightedSelector.SetLeafWeights(srv.Name, weights)

	return d, srv, &requests
}

// failLeaf drives the daemon's real failure-recording path n times for a leaf,
// exactly as a non-zero exit would.
func failLeaf(d *Daemon, leafID string, n int) {
	for i := 0; i < n; i++ {
		d.noteLeafFailure(&runtime.WorkUnit{ID: "wu", LeafID: leafID}, "non-zero exit code 2")
	}
}

// TestFetcherStopsRequestingALeafThatKeepsFailingLocally is the core TB-10
// regression: after a run of local failures the fetcher must stop asking for
// that leaf, so the fetch → fail → abandon → requeue loop terminates instead of
// running for hours at a few hundred milliseconds a turn.
func TestFetcherStopsRequestingALeafThatKeepsFailingLocally(t *testing.T) {
	clock := &failureTestClock{t: time.Unix(1_700_000_000, 0)}
	d, _, requests := breakerDaemon(t, clock, "broken-leaf")

	queue := NewPreFetchQueue(8, d.logger)
	f := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	f.now = clock.now

	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	if *requests != 1 {
		t.Fatalf("baseline: %d requests, want 1 — the leaf should be requested while it is healthy", *requests)
	}

	// The leaf's units now fail on this machine, every time.
	failLeaf(d, "broken-leaf", leafFailurePauseThreshold)

	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne after failures: %v", err)
	}
	if *requests != 1 {
		t.Fatalf("%d requests after %d consecutive failures, want 1 — the fetcher is still asking for a leaf that fails every time (this is the burn loop)",
			*requests, leafFailurePauseThreshold)
	}
}

// TestFetcherRetriesAFailingLeafAfterTheCooldown: the pause must be
// time-bounded. A broken artifact is usually fixed head-side, and a volunteer
// that stopped asking forever would then sit out the fix until it restarted.
func TestFetcherRetriesAFailingLeafAfterTheCooldown(t *testing.T) {
	clock := &failureTestClock{t: time.Unix(1_700_000_000, 0)}
	d, _, requests := breakerDaemon(t, clock, "broken-leaf")

	queue := NewPreFetchQueue(8, d.logger)
	f := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	f.now = clock.now

	failLeaf(d, "broken-leaf", leafFailurePauseThreshold)
	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne while paused: %v", err)
	}
	if *requests != 0 {
		t.Fatalf("%d requests while paused, want 0", *requests)
	}

	clock.advance(leafFailureCooldown + time.Second)
	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne after cooldown: %v", err)
	}
	if *requests != 1 {
		t.Fatalf("%d requests after the cooldown elapsed, want 1 — a paused leaf must be re-probed", *requests)
	}
}

// TestLeafFailureBreakerIsPerLeafNotPerRuntime is the design guard. One broken
// native artifact must not stop this machine from running every OTHER native
// leaf — that would turn a single leaf's bug into a total native outage for the
// volunteer, which is a worse version of the symptom being fixed.
func TestLeafFailureBreakerIsPerLeafNotPerRuntime(t *testing.T) {
	clock := &failureTestClock{t: time.Unix(1_700_000_000, 0)}
	d, _, requests := breakerDaemon(t, clock, "broken-leaf", "healthy-leaf")

	queue := NewPreFetchQueue(8, d.logger)
	f := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	f.now = clock.now

	failLeaf(d, "broken-leaf", leafFailurePauseThreshold)

	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	// fetchOne walks every enabled leaf on the head when none returns work, so
	// exactly one request should have been issued: the healthy leaf's.
	if *requests != 1 {
		t.Fatalf("%d requests, want exactly 1 (the healthy leaf's) — a sibling leaf of the same runtime must keep flowing", *requests)
	}
}

// TestASuccessfulRunClearsTheFailureStreak: the counter is CONSECUTIVE
// failures. A leaf that fails intermittently — a flaky input, a transient local
// condition — must never accumulate its way to a pause across successful runs.
func TestASuccessfulRunClearsTheFailureStreak(t *testing.T) {
	clock := &failureTestClock{t: time.Unix(1_700_000_000, 0)}
	d, _, requests := breakerDaemon(t, clock, "flaky-leaf")

	queue := NewPreFetchQueue(8, d.logger)
	f := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	f.now = clock.now

	for i := 0; i < leafFailurePauseThreshold*3; i++ {
		failLeaf(d, "flaky-leaf", leafFailurePauseThreshold-1)
		d.noteLeafSuccess(&runtime.WorkUnit{ID: "wu", LeafID: "flaky-leaf"})
	}

	*requests = 0
	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	if *requests != 1 {
		t.Fatalf("%d requests, want 1 — a leaf that keeps recovering must not be paused", *requests)
	}
}

// TestLeafFailureSnapshotSurvivesRecovery: `status` must still be able to say a
// leaf HAS been failing after it recovers. Clearing the running total on the
// first success would erase the evidence a volunteer needs to report a flaky
// leaf at all.
func TestLeafFailureSnapshotSurvivesRecovery(t *testing.T) {
	clock := &failureTestClock{t: time.Unix(1_700_000_000, 0)}
	tracker := newLeafFailureTracker(clock.now)

	tracker.RecordFailure("leaf-a", "non-zero exit code 2")
	tracker.RecordFailure("leaf-a", "non-zero exit code 2")
	tracker.RecordSuccess("leaf-a")

	snap := tracker.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d entries, want 1", len(snap))
	}
	if snap[0].Total != 2 {
		t.Errorf("total failures = %d, want 2 — the record of past failures must outlive the streak", snap[0].Total)
	}
	if snap[0].Consecutive != 0 {
		t.Errorf("consecutive = %d, want 0 after a success", snap[0].Consecutive)
	}
	if snap[0].Paused {
		t.Errorf("leaf should not be paused after a success")
	}
}

// TestLeafFailureBreakerTripsExactlyOnce guards the loud WARN against becoming
// noise: the trip is reported on the failure that crosses the threshold and not
// on any of the ones after it.
func TestLeafFailureBreakerTripsExactlyOnce(t *testing.T) {
	clock := &failureTestClock{t: time.Unix(1_700_000_000, 0)}
	tracker := newLeafFailureTracker(clock.now)

	trips := 0
	for i := 0; i < leafFailurePauseThreshold*4; i++ {
		if _, tripped := tracker.RecordFailure("leaf-a", "boom"); tripped {
			trips++
		}
	}
	if trips != 1 {
		t.Errorf("breaker reported %d trips over %d failures, want exactly 1", trips, leafFailurePauseThreshold*4)
	}
}
