package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// TB-13 regression tests: a machine that dies while holding RUN-STARTED work must have
// those copies released as soon as it reports not holding them, instead of keeping the
// reservation for the leaf's whole deadline (5 h on current leaves) — long enough to fill
// a new volunteer's in-flight quota and refuse it all work with no explanation anywhere.
//
// These drive the dispatch cache's reconcile against fakes (no DB). The SQL half — that
// a started copy is actually closed — is covered by the integration test
// TestReleaseStaleHeldCopies_FreesStrandedStartedCopy in internal/workunit.

// TestReconcileHeldCopies_ReleasesStrandedStartedCopy is the TB-13 case at the cache
// boundary: the machine's report reaches the release call, and the copies it comes back
// with — INCLUDING run-started ones — are dropped from the in-memory ledger so the
// machine stops being charged for them.
//
// Before the fix the release call could not return a started copy at all (its SQL
// excluded started_at IS NOT NULL and its result carried no started flag), so this test
// does not compile against the pre-fix repository interface — which is the strongest
// statement of the defect available at this layer.
func TestReconcileHeldCopies_ReleasesStrandedStartedCopy(t *testing.T) {
	wuRepo := &fakeWURepo{}
	relRepo := &fakeReliabilityRepo{}
	c := newTestCache(wuRepo, &fakeLeafRepo{}, &fakeAssignRepo{})
	c.deps.reliabilityRepo = relRepo

	now := time.Now()
	c.now = func() time.Time { return now }

	account := types.NewID()
	host := types.NewID()
	started := types.NewID()

	// The machine died holding one started unit and now reports holding NOTHING.
	c.NoteVolunteerHeld(account, host, nil)

	var gotHost types.ID
	var gotHeld []types.ID
	wuRepo.releaseFn = func(h types.ID, held []types.ID, _ time.Time) ([]workunit.ReleasedCopy, error) {
		gotHost, gotHeld = h, held
		return []workunit.ReleasedCopy{{WorkUnitID: started, Started: true}}, nil
	}

	c.reconcileHeldCopies(context.Background())

	if gotHost != host {
		t.Fatalf("release called for host %v, want %v", gotHost, host)
	}
	if len(gotHeld) != 0 {
		t.Fatalf("held set = %v, want empty (the machine reported holding nothing)", gotHeld)
	}
	if wuRepo.releaseCalls != 1 {
		t.Fatalf("release calls = %d, want 1", wuRepo.releaseCalls)
	}

	// A lost RUNNING copy is wasted compute: the same bad reliability signal the deadline
	// sweep would have recorded hours later, recorded now instead of dropped.
	got := relRepo.recorded
	if len(got) != 1 || got[0].hostID != host || got[0].good {
		t.Fatalf("reliability outcomes = %+v, want one bad outcome for host %v", got, host)
	}
}

// TestReconcileHeldCopies_UnstartedReleaseIsNotAReliabilitySignal guards the other half
// of the rule (#59): returning BUFFERED work the machine never ran is cooperative, so it
// must not cost the machine reliability. Only run-started losses do.
func TestReconcileHeldCopies_UnstartedReleaseIsNotAReliabilitySignal(t *testing.T) {
	wuRepo := &fakeWURepo{}
	relRepo := &fakeReliabilityRepo{}
	c := newTestCache(wuRepo, &fakeLeafRepo{}, &fakeAssignRepo{})
	c.deps.reliabilityRepo = relRepo

	now := time.Now()
	c.now = func() time.Time { return now }

	account, host := types.NewID(), types.NewID()
	buffered, alsoBuffered, running := types.NewID(), types.NewID(), types.NewID()
	c.NoteVolunteerHeld(account, host, nil)

	wuRepo.releaseFn = func(types.ID, []types.ID, time.Time) ([]workunit.ReleasedCopy, error) {
		return []workunit.ReleasedCopy{
			{WorkUnitID: buffered, Started: false},
			{WorkUnitID: running, Started: true},
			{WorkUnitID: alsoBuffered, Started: false},
		}, nil
	}

	c.reconcileHeldCopies(context.Background())

	got := relRepo.recorded
	if len(got) != 1 {
		t.Fatalf("reliability outcomes = %+v, want exactly 1 (only the run-started copy)", got)
	}
	if got[0].good {
		t.Errorf("a lost running copy must record a BAD outcome, got good")
	}
}

// TestReconcileHeldCopies_ReliabilityFailureDoesNotAbortRelease keeps the reliability
// signal best-effort: it is pure dispatch shaping, so a store error must not cost the
// release its logging or stop later hosts being reconciled. The release itself already
// landed in Postgres before this point.
func TestReconcileHeldCopies_ReliabilityFailureDoesNotAbortRelease(t *testing.T) {
	wuRepo := &fakeWURepo{}
	relRepo := &fakeReliabilityRepo{recordErr: context.DeadlineExceeded}
	c := newTestCache(wuRepo, &fakeLeafRepo{}, &fakeAssignRepo{})
	c.deps.reliabilityRepo = relRepo

	now := time.Now()
	c.now = func() time.Time { return now }

	accountA, hostA := types.NewID(), types.NewID()
	accountB, hostB := types.NewID(), types.NewID()
	c.NoteVolunteerHeld(accountA, hostA, nil)
	c.NoteVolunteerHeld(accountB, hostB, nil)

	wuRepo.releaseFn = func(types.ID, []types.ID, time.Time) ([]workunit.ReleasedCopy, error) {
		return []workunit.ReleasedCopy{{WorkUnitID: types.NewID(), Started: true}}, nil
	}

	c.reconcileHeldCopies(context.Background())

	if wuRepo.releaseCalls != 2 {
		t.Fatalf("release calls = %d, want 2 (a reliability error must not skip the other machine)", wuRepo.releaseCalls)
	}
}

// --- the "say it" half: an in-flight-cap starvation must be visible in the head log ---

// capturingLogger returns a JSON logger writing into buf, plus buf. Info level so the
// test sees exactly what a production head would (the pre-existing reject tally is
// Debug-only and therefore invisible there).
func capturingLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})), buf
}

// starveLines returns the starvation WARN records captured in buf.
func starveLines(buf *bytes.Buffer) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if msg, _ := rec["msg"].(string); strings.Contains(msg, "in-flight cap") {
			out = append(out, rec)
		}
	}
	return out
}

// TestHandOut_StarvedByOwnInflightCap_IsLogged is TB-13's "regardless of which — say it":
// a volunteer refused work because its OWN in-flight quota is consumed (by stale claims or
// by copies it really holds) got an empty response indistinguishable from "no work exists",
// and on a production head nothing was logged at all. The head must now name the cause.
func TestHandOut_StarvedByOwnInflightCap_IsLogged(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	logger, buf := capturingLogger()
	c.logger = logger

	now := time.Now()
	c.now = func() time.Time { return now }

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)

	vol, host := types.NewID(), types.NewID()
	// The machine is already at its cap of 2 — the tester's exact shape: a cold-start
	// quota of 2 fully consumed by claims it no longer holds.
	c.mu.Lock()
	c.inflight[host] = 2
	c.mu.Unlock()
	c.stageUnit(types.NewID(), leafID, 2, 0)

	opts := hostOpts(vol, host, 2)
	if r, _ := c.HandOut(vol, opts, 1); len(r) != 0 {
		t.Fatalf("hand-out = %d units, want 0 (machine is at its in-flight cap)", len(r))
	}

	lines := starveLines(buf)
	if len(lines) != 1 {
		t.Fatalf("starvation log lines = %d, want 1 — a machine starved by its own quota must "+
			"say so at a level a production head records; got log:\n%s", len(lines), buf.String())
	}
	rec := lines[0]
	if lvl, _ := rec["level"].(string); lvl != "WARN" {
		t.Errorf("starvation line level = %q, want WARN", lvl)
	}
	if got, _ := rec["host_id"].(string); got != host.String() {
		t.Errorf("starvation line host_id = %q, want %q", got, host.String())
	}
	if got, _ := rec["inflight"].(float64); int(got) != 2 {
		t.Errorf("starvation line inflight = %v, want 2", rec["inflight"])
	}
	if got, _ := rec["inflight_cap"].(float64); int(got) != 2 {
		t.Errorf("starvation line inflight_cap = %v, want 2", rec["inflight_cap"])
	}
	// The refusal tally is carried under a prefixed key so the count of candidates refused
	// by the cap cannot be read as the cap itself.
	tallyKey := "refused_" + rejectInflightCap.String()
	if got, _ := rec[tallyKey].(float64); int(got) < 1 {
		t.Errorf("starvation line must carry the reject tally %q, got %v", tallyKey, rec)
	}
}

// TestHandOut_StarvationLogIsThrottledPerMachine keeps the new WARN usable: a starved
// client polls continuously, so the line must be throttled per machine rather than
// printed once per request — and the throttle must be per machine, not global.
func TestHandOut_StarvationLogIsThrottledPerMachine(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	logger, buf := capturingLogger()
	c.logger = logger

	now := time.Now()
	c.now = func() time.Time { return now }

	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)

	volA, hostA := types.NewID(), types.NewID()
	volB, hostB := types.NewID(), types.NewID()
	c.mu.Lock()
	c.inflight[hostA] = 2
	c.inflight[hostB] = 2
	c.mu.Unlock()
	for i := 0; i < 6; i++ {
		c.stageUnit(types.NewID(), leafID, 2, 0)
	}

	// Machine A polls three times inside the throttle window: one line.
	for i := 0; i < 3; i++ {
		c.HandOut(volA, hostOpts(volA, hostA, 2), 1)
	}
	if n := len(starveLines(buf)); n != 1 {
		t.Fatalf("machine A produced %d starvation lines across 3 polls, want 1", n)
	}

	// A DIFFERENT machine is not suppressed by A's line.
	c.HandOut(volB, hostOpts(volB, hostB, 2), 1)
	if n := len(starveLines(buf)); n != 2 {
		t.Fatalf("starvation lines = %d after machine B polled, want 2 (the throttle is per machine)", n)
	}

	// Past the window, machine A reports again — a persisting starvation stays visible.
	c.now = func() time.Time { return now.Add(starveLogInterval + time.Second) }
	c.HandOut(volA, hostOpts(volA, hostA, 2), 1)
	if n := len(starveLines(buf)); n != 3 {
		t.Fatalf("starvation lines = %d after the throttle window elapsed, want 3", n)
	}
}

// TestHandOut_NoStarvationLogWhenWorkIsSimplyAbsent guards against the new WARN crying
// wolf: an empty ready pool (no work exists) refuses nobody by the in-flight cap, so it
// must stay silent. A diagnostic that fires on a healthy head trains operators to ignore
// it — the same failure TB-12 caught in doctor.
func TestHandOut_NoStarvationLogWhenWorkIsSimplyAbsent(t *testing.T) {
	wuRepo := &fakeWURepo{}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})
	logger, buf := capturingLogger()
	c.logger = logger

	vol, host := types.NewID(), types.NewID()
	c.mu.Lock()
	c.inflight[host] = 2
	c.mu.Unlock()

	// Nothing staged: the machine is at its cap, but no candidate was refused because of
	// it — there were no candidates.
	if r, _ := c.HandOut(vol, hostOpts(vol, host, 2), 1); len(r) != 0 {
		t.Fatalf("hand-out = %d, want 0 (empty pool)", len(r))
	}
	if n := len(starveLines(buf)); n != 0 {
		t.Fatalf("starvation lines = %d on an empty pool, want 0:\n%s", n, buf.String())
	}
}
