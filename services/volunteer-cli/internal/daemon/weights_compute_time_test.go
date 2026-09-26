package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Head and leaf weights promise a share of this machine's compute time: a
// head at weight 200 gets about twice the time of one at 100. The selector
// used to balance the NUMBER of units fetched, so leaves whose units take
// different times were split by count, not by time, and the leaf (or head)
// with the most deficit was asked for the whole buffer at once. These tests
// drive the real fetcher and the daemon's buffer arithmetic against a mock
// head, then measure what was buffered in hours.

// weightsHead is a mock head that serves whatever it is asked for: up to
// MaxAssignments units of the requested leaf, each with a fresh id. It
// records the leaf and the size of every ask.
type weightsHead struct {
	mu     sync.Mutex
	next   int
	empty  map[string]bool // leaf ids this head has no work for
	asks   []weightsAsk
	client *mockClient
}

type weightsAsk struct {
	leafID string
	max    int32
}

func newWeightsHead(emptyLeafs ...string) *weightsHead {
	h := &weightsHead{empty: map[string]bool{}}
	for _, id := range emptyLeafs {
		h.empty[id] = true
	}
	h.client = &mockClient{
		requestWorkUnitFn: func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			leafID := ""
			if len(req.LeafIds) > 0 {
				leafID = req.LeafIds[0]
			}
			h.asks = append(h.asks, weightsAsk{leafID: leafID, max: req.MaxAssignments})
			if h.empty[leafID] {
				return &lettucev1.RequestWorkUnitResponse{}, nil
			}
			var out []*lettucev1.WorkUnitAssignment
			for i := int32(0); i < req.MaxAssignments; i++ {
				h.next++
				out = append(out, &lettucev1.WorkUnitAssignment{
					WorkUnitId:    fmt.Sprintf("00000000-0000-4000-8000-%012d", h.next),
					LeafId:        leafID,
					Runtime:       "native",
					ExecutionSpec: &lettucev1.ExecutionSpec{},
				})
			}
			return &lettucev1.RequestWorkUnitResponse{Assignments: out}, nil
		},
	}
	return h
}

func (h *weightsHead) recordedAsks() []weightsAsk {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]weightsAsk(nil), h.asks...)
}

// weightsLeaf is one leaf of a test head: its id, slug, weight, and the
// seconds its units take here (the learned median).
type weightsLeaf struct {
	id, slug string
	weight   int
	seconds  float64
}

// weightsHeadSpec is one attached head of a test machine.
type weightsHeadSpec struct {
	name   string
	weight int
	leafs  []weightsLeaf
	head   *weightsHead
}

// newWeightsDaemon builds a one-slot daemon with a work_buffer_hours target
// over the given heads, every leaf's unit length already learned here.
func newWeightsDaemon(t *testing.T, hours float64, heads ...weightsHeadSpec) *Daemon {
	t.Helper()
	var servers []*ServerConnection
	for _, h := range heads {
		servers = append(servers, &ServerConnection{Client: h.head.client, VolunteerID: "vol-1", Name: h.name, Available: true})
	}
	d := newFetcherTestDaemon(servers)
	d.cfg.WorkBufferHours = hours
	d.cfg.MaxConcurrentTasks = 1
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)
	d.slotManager = NewSlotManager(1, d.logger)
	d.durations = LoadDurationTracker(t.TempDir())
	headWeights := map[string]int{}
	for _, h := range heads {
		headWeights[h.name] = h.weight
		var cached []CachedLeafInfo
		leafWeights := map[string]int{}
		for _, l := range h.leafs {
			cached = append(cached, CachedLeafInfo{ID: l.id, Slug: l.slug, Name: l.slug, State: "ACTIVE"})
			leafWeights[l.slug] = l.weight
			for i := 0; i < 5; i++ {
				d.durations.Record(l.id, 0, l.seconds)
			}
		}
		d.leafCache.PopulateForTest(h.name, &CachedHeadInfo{Name: h.name, Leafs: cached, DefaultWeights: leafWeights})
		d.weightedSelector.SetLeafWeights(h.name, leafWeights)
	}
	d.weightedSelector.SetHeadWeights(headWeights)
	return d
}

// bufferedHoursByLeaf sums the buffer's booked seconds per leaf id, in hours.
func bufferedHoursByLeaf(d *Daemon, items []*PreFetchItem) map[string]float64 {
	out := map[string]float64{}
	for _, it := range items {
		out[it.WU.LeafID] += d.estSecondsForUnit(it.WU.LeafID, it.WU.RscFpopsEst) / 3600
	}
	return out
}

// simulateFills runs rounds fetch rounds, emptying the buffer after each as
// if the slot had run everything in it, and returns the hours fetched per
// leaf id over the whole run.
func simulateFills(t *testing.T, d *Daemon, rounds int) map[string]float64 {
	t.Helper()
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	total := map[string]float64{}
	for i := 0; i < rounds; i++ {
		for _, srv := range d.multiClient.Servers() {
			srv.NextContactAt = time.Time{}
		}
		if _, err := f.fetchOne(context.Background()); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		for id, h := range bufferedHoursByLeaf(d, d.prefetchQueue.Clear()) {
			total[id] += h
		}
	}
	return total
}

func assertRatio(t *testing.T, what string, got, want float64) {
	t.Helper()
	if got < want*0.8 || got > want*1.25 {
		t.Errorf("%s = %.2f, want about %.2f (within 20%%)", what, got, want)
	}
}

// TestWeights_LeafsSplitComputeTimeNotUnitCounts: two leaves on one head,
// one with 10-minute units and one with 50-minute units. At equal weights the
// machine's time splits 1 : 1, and at 25 / 100 it splits 1 : 4. Counting
// units instead gave the long leaf five times the short leaf's time at equal
// weights.
func TestWeights_LeafsSplitComputeTimeNotUnitCounts(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		shortWeight, longWeight int
		wantLongOverShortHours  float64
	}{
		{"equal weights", 100, 100, 1},
		{"25 against 100", 25, 100, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := newWeightsHead()
			d := newWeightsDaemon(t, 2, weightsHeadSpec{name: "server-a", weight: 100, head: head, leafs: []weightsLeaf{
				{id: "leaf-short", slug: "short", weight: tc.shortWeight, seconds: 600},
				{id: "leaf-long", slug: "long", weight: tc.longWeight, seconds: 3000},
			}})
			hours := simulateFills(t, d, 60)
			if hours["leaf-short"] == 0 || hours["leaf-long"] == 0 {
				t.Fatalf("hours fetched = %v, want both leaves", hours)
			}
			assertRatio(t, "long-leaf hours / short-leaf hours", hours["leaf-long"]/hours["leaf-short"], tc.wantLongOverShortHours)
		})
	}
}

// TestWeights_HeadsSplitComputeTimeNotUnitCounts is the same promise one
// level up: a head at weight 25 whose units take 10 minutes beside a head at
// weight 100 whose units take 50 minutes gets a fifth of the machine's time.
// Counting units gave it a twenty-first.
func TestWeights_HeadsSplitComputeTimeNotUnitCounts(t *testing.T) {
	small, big := newWeightsHead(), newWeightsHead()
	d := newWeightsDaemon(t, 2,
		weightsHeadSpec{name: "lbry", weight: 25, head: small, leafs: []weightsLeaf{{id: "leaf-bb", slug: "bb", weight: 100, seconds: 600}}},
		weightsHeadSpec{name: "scios", weight: 100, head: big, leafs: []weightsLeaf{{id: "leaf-grep", slug: "grep", weight: 100, seconds: 3000}}},
	)
	hours := simulateFills(t, d, 80)
	if hours["leaf-bb"] == 0 || hours["leaf-grep"] == 0 {
		t.Fatalf("hours fetched = %v, want work from both heads", hours)
	}
	assertRatio(t, "weight-100 head hours / weight-25 head hours", hours["leaf-grep"]/hours["leaf-bb"], 4)
}

// TestWeights_OneFillHoldsEveryLeafInItsShare: from an empty buffer, one
// fill of a two-leaf head at equal weights holds both leaves, about half the
// hours each. The first ask is for the leaf's own share of the deficit, not
// for the whole buffer: before, the first leaf was asked for all 12 units the
// 2-hour target held, and the buffer held nothing else.
func TestWeights_OneFillHoldsEveryLeafInItsShare(t *testing.T) {
	head := newWeightsHead()
	d := newWeightsDaemon(t, 2, weightsHeadSpec{name: "server-a", weight: 100, head: head, leafs: []weightsLeaf{
		{id: "leaf-a", slug: "a", weight: 100, seconds: 600},
		{id: "leaf-b", slug: "b", weight: 100, seconds: 600},
	}})
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	for i := 0; i < 20 && !d.workBufferHoursFull(); i++ {
		d.multiClient.Servers()[0].NextContactAt = time.Time{}
		if _, err := f.fetchOne(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !d.workBufferHoursFull() {
		t.Fatalf("buffer not full after 20 rounds (%.0f s of %.0f s)", d.bufferedSeconds(), d.bufferTargetSeconds())
	}
	asks := head.recordedAsks()
	if len(asks) == 0 || asks[0].max != 6 {
		t.Errorf("first ask = %+v, want 6 units (half of the 12 the empty 2-hour buffer holds)", asks)
	}
	hours := bufferedHoursByLeaf(d, d.prefetchQueue.Items())
	total := hours["leaf-a"] + hours["leaf-b"]
	for _, id := range []string{"leaf-a", "leaf-b"} {
		if share := hours[id] / total; share < 0.4 || share > 0.6 {
			t.Errorf("after one fill %s holds %.0f%% of the buffered hours (%v), want about half", id, 100*share, hours)
		}
	}
}

// TestWeights_EmptyLeafPassesItsShareOn: a leaf the head has no work for
// leaves its share to the leaves still in play in the same round, so the leaf
// that has work is asked for the whole deficit rather than half of it.
func TestWeights_EmptyLeafPassesItsShareOn(t *testing.T) {
	head := newWeightsHead("leaf-a")
	d := newWeightsDaemon(t, 2, weightsHeadSpec{name: "server-a", weight: 100, head: head, leafs: []weightsLeaf{
		{id: "leaf-a", slug: "a", weight: 100, seconds: 600},
		{id: "leaf-b", slug: "b", weight: 100, seconds: 600},
	}})
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	asks := head.recordedAsks()
	if len(asks) != 2 || asks[0].leafID != "leaf-a" || asks[1].leafID != "leaf-b" {
		t.Fatalf("asks = %+v, want leaf-a (empty) then leaf-b in one round", asks)
	}
	if asks[1].max != 12 {
		t.Errorf("leaf-b asked for %d units after leaf-a came back empty, want 12 (the whole 2-hour deficit)", asks[1].max)
	}
}

// TestWeights_TiedLeafsTakeTurnsWhenARoundStopsEarly: with nothing buffered
// every leaf's deficit is equal, and ties went to the alphabetically first
// slug. A round that stops at its first request (here the head sheds load)
// then asked for that same leaf every time and never for the other. Ties now
// go to the leaf asked least recently.
func TestWeights_TiedLeafsTakeTurnsWhenARoundStopsEarly(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	mc := &mockClient{requestWorkUnitFn: func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		mu.Lock()
		asked = append(asked, req.LeafIds[0])
		mu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "shedding")
	}}
	head := &weightsHead{client: mc}
	d := newWeightsDaemon(t, 2, weightsHeadSpec{name: "server-a", weight: 100, head: head, leafs: []weightsLeaf{
		{id: "leaf-a", slug: "a", weight: 100, seconds: 600},
		{id: "leaf-b", slug: "b", weight: 100, seconds: 600},
	}})
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	for i := 0; i < 4; i++ {
		d.multiClient.Servers()[0].NextContactAt = time.Time{}
		if _, err := f.fetchOne(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 4 {
		t.Fatalf("asked %v, want one request per round", asked)
	}
	counts := map[string]int{}
	for _, id := range asked {
		counts[id]++
	}
	if counts["leaf-a"] != 2 || counts["leaf-b"] != 2 {
		t.Errorf("leaves asked over four stopped rounds = %v, want each asked twice", asked)
	}
}

// TestWeights_RestartContinuesTheBalanceFromHistory: the balance lives in
// memory, so a restart used to begin a fresh race in which the alphabetically
// first head and leaf were asked first, whatever the machine had just done.
// It now starts from the recent history.jsonl: a head and a leaf that had
// most of the machine's time in the last hours are asked after the ones that
// had little. A run older than the balance window counts for nothing.
func TestWeights_RestartContinuesTheBalanceFromHistory(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	now := time.Now().UTC()
	if err := SaveBenchmark(cfg.DataDir, &BenchmarkResult{FPOPS: 1e9, MeasuredAt: now, CPUModel: "test"}); err != nil {
		t.Fatal(err)
	}
	for i, e := range []HistoryEntry{
		// The last few hours: alpha's leaf a ran for five hours, beta ran an hour.
		{LeafID: "leaf-a", ServerName: "alpha", CompletedAt: now.Add(-1 * time.Hour), WallClockSeconds: 3*3600 + 60, CPUSeconds: 3 * 3600},
		{LeafID: "leaf-a", ServerName: "alpha", CompletedAt: now.Add(-3 * time.Hour), WallClockSeconds: 2 * 3600, CPUSeconds: 2 * 3600},
		{LeafID: "leaf-b", ServerName: "alpha", CompletedAt: now.Add(-2 * time.Hour), WallClockSeconds: 600, CPUSeconds: 600},
		{LeafID: "leaf-x", ServerName: "beta", CompletedAt: now.Add(-2 * time.Hour), WallClockSeconds: 3600, CPUSeconds: 3600},
		// A month ago beta ran for days; that is outside the window.
		{LeafID: "leaf-x", ServerName: "beta", CompletedAt: now.Add(-30 * 24 * time.Hour), WallClockSeconds: 90 * 3600, CPUSeconds: 90 * 3600},
	} {
		e.WorkUnitID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		e.ResultAccepted = true
		if err := AppendHistory(cfg.DataDir, e); err != nil {
			t.Fatal(err)
		}
	}

	d := NewDaemon(DaemonConfig{Config: cfg, Logger: logger, Hardware: &lettucev1.HardwareCapabilities{CpuModel: "test"}})
	ws := d.GetWeightedSelector()
	ws.SetHeadWeights(map[string]int{"alpha": 100, "beta": 100})
	ws.SetLeafWeights("alpha", map[string]int{"a": 100, "b": 100})

	alpha := &ServerConnection{Name: "alpha", Available: true}
	beta := &ServerConnection{Name: "beta", Available: true}
	if got := ws.SelectHead([]*ServerConnection{alpha, beta}); got != beta {
		t.Errorf("first head after restart = %s, want beta (alpha had five of the last six hours)", got.Name)
	}
	order := ws.SelectLeafByDeficitOrder("alpha", []CachedLeafInfo{{ID: "leaf-a", Slug: "a"}, {ID: "leaf-b", Slug: "b"}})
	if order[0].Slug != "b" {
		t.Errorf("first leaf on alpha after restart = %s, want b (a had five hours to b's ten minutes)", order[0].Slug)
	}
}
