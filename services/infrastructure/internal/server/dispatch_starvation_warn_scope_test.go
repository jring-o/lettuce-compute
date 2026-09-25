package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// The throttled "no work handed out" WARN is the head's only Info-level record of why a
// machine got nothing, so it must describe the request it answers. A client names the
// leaves it wants (one per request whenever it has the head's leaf list), and a unit of
// any other leaf is out of that request's scope: it must be tallied as leaf_filter, never
// under an account- or machine-specific reason the request never met. The per-machine
// throttle must likewise be spent only by a WARN that was actually written.

// debugCapturingLogger is capturingLogger at Debug level, so one buffer carries both the
// WARN a production head writes and the Debug reject tally that explains it.
func debugCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// logRecords parses every JSON record in buf.
func logRecords(buf *bytes.Buffer) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// noWorkWarns returns the "no work handed out" WARN records in buf.
func noWorkWarns(buf *bytes.Buffer) []map[string]any {
	var out []map[string]any
	for _, rec := range logRecords(buf) {
		msg, _ := rec["msg"].(string)
		if lvl, _ := rec["level"].(string); lvl == "WARN" && strings.HasPrefix(msg, "no work handed out") {
			out = append(out, rec)
		}
	}
	return out
}

// rejectTallies returns the Debug "hand-out empty: reject tally" records in buf.
func rejectTallies(buf *bytes.Buffer) []map[string]any {
	var out []map[string]any
	for _, rec := range logRecords(buf) {
		if msg, _ := rec["msg"].(string); msg == "hand-out empty: reject tally" {
			out = append(out, rec)
		}
	}
	return out
}

func recInt(rec map[string]any, key string) int {
	v, _ := rec[key].(float64)
	return int(v)
}

// scopeCache is a cache with a Debug capturing logger and a controllable clock.
func scopeCache(t *testing.T) (*dispatchCache, *fakeLeafRepo, *bytes.Buffer, *time.Time) {
	t.Helper()
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(&fakeWURepo{}, leafRepo, &fakeAssignRepo{})
	logger, buf := debugCapturingLogger()
	c.logger = logger
	now := time.Now().UTC()
	c.now = func() time.Time { return now }
	return c, leafRepo, buf, &now
}

// A machine asking only for a leaf that has no units, while another leaf's units all
// carry this account's result, was logged as "every ready unit refuses this account
// specifically", with the other leaf's units counted as already_contributed. That leaf
// was never asked for, so the answer is simply "no units of the requested leaf": the
// other leaf's units are out of scope, and there is nothing to WARN about.
func TestNoWorkWarn_RequestForAnEmptyLeafIgnoresOtherLeavesUnits(t *testing.T) {
	c, leafRepo, buf, _ := scopeCache(t)
	emptyLeaf, otherLeaf := types.NewID(), types.NewID()
	c.warm(nativeLeaf(emptyLeaf, 2, false, 0), leafRepo)
	c.warm(nativeLeaf(otherLeaf, 2, false, 0), leafRepo)
	vol := types.NewID()
	for i := 0; i < 3; i++ {
		c.stageUnitSets(types.NewID(), otherLeaf, 2, 1, []types.ID{vol}, nil)
	}

	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{emptyLeaf}
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 0 {
		t.Fatalf("hand-out = %d units, want 0 (the requested leaf has none)", len(res))
	}

	if warns := noWorkWarns(buf); len(warns) != 0 {
		t.Fatalf("no-work WARN for a request naming an empty leaf = %d, want 0 — the other "+
			"leaf's units are out of this request's scope; got:\n%s", len(warns), buf.String())
	}
	tallies := rejectTallies(buf)
	if len(tallies) != 1 {
		t.Fatalf("reject tallies = %d, want 1; log:\n%s", len(tallies), buf.String())
	}
	tally := tallies[0]
	if got := recInt(tally, rejectLeafFilter.String()); got != 3 {
		t.Errorf("tally %s = %d, want 3 (every unit is another leaf's)", rejectLeafFilter, got)
	}
	if got := recInt(tally, rejectAlreadyContributed.String()); got != 0 {
		t.Errorf("tally %s = %d, want 0 — a unit the request did not ask for is counted "+
			"under the leaf filter, not under a reason about this account", rejectAlreadyContributed, got)
	}
}

// The control: the same account asking for the leaf whose units all carry its result
// still gets the account WARN, now counting that leaf's units only.
func TestNoWorkWarn_RequestedLeafAllContributedStillWarns(t *testing.T) {
	c, leafRepo, buf, _ := scopeCache(t)
	emptyLeaf, contributedLeaf := types.NewID(), types.NewID()
	c.warm(nativeLeaf(emptyLeaf, 2, false, 0), leafRepo)
	c.warm(nativeLeaf(contributedLeaf, 2, false, 0), leafRepo)
	vol := types.NewID()
	for i := 0; i < 3; i++ {
		c.stageUnitSets(types.NewID(), contributedLeaf, 2, 1, []types.ID{vol}, nil)
	}

	opts := capableOpts(vol, 0)
	opts.LeafIDs = []types.ID{contributedLeaf}
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 0 {
		t.Fatalf("hand-out = %d units, want 0", len(res))
	}
	warns := noWorkWarns(buf)
	if len(warns) != 1 {
		t.Fatalf("no-work WARNs = %d, want 1; log:\n%s", len(warns), buf.String())
	}
	w := warns[0]
	if msg, _ := w["msg"].(string); !strings.Contains(msg, "refuses this account specifically") {
		t.Errorf("WARN msg = %q, want the account arm", msg)
	}
	if got := recInt(w, "refused_"+rejectAlreadyContributed.String()); got != 3 {
		t.Errorf("refused_%s = %d, want 3", rejectAlreadyContributed, got)
	}
}

// A machine at its in-flight cap asking for a leaf with no units got "machine is at its
// own in-flight cap", counted from other leaves' units. The cap is true but is not why
// that request got nothing: no cap level could have served an empty leaf.
func TestNoWorkWarn_AtCapAskingForAnEmptyLeafIsNotBlamedOnTheCap(t *testing.T) {
	c, leafRepo, buf, _ := scopeCache(t)
	emptyLeaf, otherLeaf := types.NewID(), types.NewID()
	c.warm(nativeLeaf(emptyLeaf, 2, false, 0), leafRepo)
	c.warm(nativeLeaf(otherLeaf, 2, false, 0), leafRepo)
	for i := 0; i < 3; i++ {
		c.stageUnit(types.NewID(), otherLeaf, 2, 0)
	}
	vol, host := types.NewID(), types.NewID()
	c.mu.Lock()
	c.inflight[host] = 2
	c.mu.Unlock()

	opts := hostOpts(vol, host, 2)
	opts.LeafIDs = []types.ID{emptyLeaf}
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 0 {
		t.Fatalf("hand-out = %d units, want 0", len(res))
	}
	if warns := noWorkWarns(buf); len(warns) != 0 {
		t.Fatalf("no-work WARNs = %d, want 0 — the requested leaf is empty, whatever the "+
			"machine's cap; got:\n%s", len(warns), buf.String())
	}
}

// At the cap and asking for a leaf that does have units, the cap WARN stands, and its
// refusal count is that leaf's units, not the whole pool's.
func TestNoWorkWarn_AtCapCountsOnlyTheRequestedLeafsUnits(t *testing.T) {
	c, leafRepo, buf, _ := scopeCache(t)
	askedLeaf, otherLeaf := types.NewID(), types.NewID()
	c.warm(nativeLeaf(askedLeaf, 2, false, 0), leafRepo)
	c.warm(nativeLeaf(otherLeaf, 2, false, 0), leafRepo)
	for i := 0; i < 2; i++ {
		c.stageUnit(types.NewID(), askedLeaf, 2, 0)
	}
	for i := 0; i < 3; i++ {
		c.stageUnit(types.NewID(), otherLeaf, 2, 0)
	}
	vol, host := types.NewID(), types.NewID()
	c.mu.Lock()
	c.inflight[host] = 2
	c.mu.Unlock()

	opts := hostOpts(vol, host, 2)
	opts.LeafIDs = []types.ID{askedLeaf}
	if res, _ := c.HandOut(vol, opts, 1); len(res) != 0 {
		t.Fatalf("hand-out = %d units, want 0 (machine at its cap)", len(res))
	}
	warns := noWorkWarns(buf)
	if len(warns) != 1 {
		t.Fatalf("no-work WARNs = %d, want 1; log:\n%s", len(warns), buf.String())
	}
	w := warns[0]
	if msg, _ := w["msg"].(string); !strings.Contains(msg, "in-flight cap") {
		t.Errorf("WARN msg = %q, want the in-flight cap arm", msg)
	}
	if got := recInt(w, "refused_"+rejectInflightCap.String()); got != 2 {
		t.Errorf("refused_%s = %d, want 2 (the requested leaf's units only)", rejectInflightCap, got)
	}
	if got := recInt(w, "refused_"+rejectLeafFilter.String()); got != 3 {
		t.Errorf("refused_%s = %d, want 3 (the other leaf's units)", rejectLeafFilter, got)
	}
}

// An empty answer that writes no WARN must not spend the machine's 5-minute window: the
// client asks one leaf per request, so a request for an empty leaf and a request for a
// leaf that really refuses this account arrive from the same machine within seconds, and
// the second, genuine WARN was being dropped.
func TestNoWorkWarn_SilentEmptyAnswerDoesNotSpendTheThrottle(t *testing.T) {
	c, leafRepo, buf, now := scopeCache(t)
	emptyLeaf, freshLeaf, contributedLeaf := types.NewID(), types.NewID(), types.NewID()
	c.warm(nativeLeaf(emptyLeaf, 2, false, 0), leafRepo)
	c.warm(nativeLeaf(freshLeaf, 2, false, 0), leafRepo)
	c.warm(nativeLeaf(contributedLeaf, 2, false, 0), leafRepo)
	vol, host := types.NewID(), types.NewID()
	// A unit this account may take, but of a leaf the first request does not name: the
	// first answer is empty for a reason that raises no WARN, before and after scoping.
	c.stageUnit(types.NewID(), freshLeaf, 2, 0)

	first := hostOpts(vol, host, 0)
	first.LeafIDs = []types.ID{emptyLeaf}
	if res, _ := c.HandOut(vol, first, 1); len(res) != 0 {
		t.Fatalf("first hand-out = %d units, want 0", len(res))
	}
	if warns := noWorkWarns(buf); len(warns) != 0 {
		t.Fatalf("first request raised %d WARNs, want 0; log:\n%s", len(warns), buf.String())
	}

	// A minute later the same machine asks for a leaf whose only unit carries its result.
	*now = now.Add(time.Minute)
	c.stageUnitSets(types.NewID(), contributedLeaf, 2, 1, []types.ID{vol}, nil)
	second := hostOpts(vol, host, 0)
	second.LeafIDs = []types.ID{contributedLeaf}
	if res, _ := c.HandOut(vol, second, 1); len(res) != 0 {
		t.Fatalf("second hand-out = %d units, want 0", len(res))
	}
	warns := noWorkWarns(buf)
	if len(warns) != 1 {
		t.Fatalf("no-work WARNs after the second request = %d, want 1 — an earlier empty "+
			"answer that wrote nothing must not suppress it; log:\n%s", len(warns), buf.String())
	}
	if got := recInt(warns[0], "refused_"+rejectAlreadyContributed.String()); got != 1 {
		t.Errorf("refused_%s = %d, want 1", rejectAlreadyContributed, got)
	}
}
