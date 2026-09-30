package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Regression tests for skipping units this machine cannot finish before their
// deadline, written against entry points that existed before the skip did
// (bufferAccepts, the fetcher's buffer sweep and Run, the duration tracker's
// Record), so they compile against the earlier client and fail there on what
// it does. The field case: a slow host whose GREP units took about 5 hours
// against a 6-hour deadline had 4 of 10 runs stopped at the deadline.

// slowGREPHost is a fetch-test machine attached to "head-1", serving a GREP
// leaf of one core whose last four runs here each took 5 hours.
func slowGREPHost(t *testing.T, mc *mockClient) *Daemon {
	t.Helper()
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "head-1", Available: true}}
	d := newFetcherTestDaemon(servers)
	d.cfg.WorkBufferHours = 2
	d.cfg.ResourceLimits.MaxCPUCores = 4
	d.cfg.ResourceLimits.MaxMemoryMB = 0
	d.cfg.Servers = []config.ServerConfig{{Name: "head-1", GRPCAddress: "head-1:443"}}
	d.slotManager = NewSlotManager(4, d.logger)
	d.prefetchQueue = NewPreFetchQueue(minWorkBufferQueueDepth, d.logger)
	d.durations = LoadDurationTracker(t.TempDir())
	for i := 0; i < 4; i++ {
		d.durations.Record("leaf-grep", 0, 5*3600)
	}
	d.leafCache.PopulateForTest("head-1", &CachedHeadInfo{Name: "head-1", Leafs: []CachedLeafInfo{
		{ID: "leaf-grep", Slug: "grep", Name: "GREP", State: "ACTIVE",
			ResourceRequirements: &CachedResourceRequirements{MinCPUCores: 1, MaxCPUCores: 1}},
	}, DefaultWeights: map[string]int{"grep": 100}})
	d.weightedSelector.SetLeafWeights("head-1", map[string]int{"grep": 100})
	return d
}

// grepUnitWithDeadline is a GREP unit with the given deadline.
func grepUnitWithDeadline(i int, deadline time.Duration) *runtime.WorkUnit {
	return &runtime.WorkUnit{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), LeafID: "leaf-grep",
		SourceHead: "head-1", DeadlineSeconds: int32(deadline.Seconds())}
}

// TestDeadline_ArrivingUnitThatCannotFinishIsReturned: a GREP unit arriving
// with a 6-hour deadline, on a machine where GREP takes 5 hours (6 h 15 min
// with the 25 % margin), is refused by the buffer before any preparation,
// and the reason says so. A unit with an 8-hour deadline is kept. Pre-fix
// both were kept, and the first ran into its deadline.
func TestDeadline_ArrivingUnitThatCannotFinishIsReturned(t *testing.T) {
	d := slowGREPHost(t, &mockClient{})
	if ok, why := d.bufferAccepts(grepUnitWithDeadline(1, 6*time.Hour)); ok || !strings.Contains(why, "deadline") {
		t.Errorf("a GREP unit with a 6 h deadline on a 5 h machine: accepted=%v (%q); want it returned, naming its deadline", ok, why)
	}
	if ok, why := d.bufferAccepts(grepUnitWithDeadline(2, 8*time.Hour)); !ok {
		t.Errorf("a GREP unit with an 8 h deadline was refused (%s); 6 h 15 min fits in 8 h", why)
	}
}

// TestDeadline_BufferedUnitIsGivenBackUnRun: a GREP unit already in the
// buffer that cannot finish before its deadline is taken out by the buffer
// sweep and given back to its head flagged un-run, so the head closes it
// budget-neutral and another volunteer gets it at once. Pre-fix it stayed,
// to be started and stopped at its deadline.
func TestDeadline_BufferedUnitIsGivenBackUnRun(t *testing.T) {
	var mu sync.Mutex
	var abandons []*lettucev1.AbandonWorkUnitRequest
	mc := &mockClient{abandonFn: func(_ context.Context, req *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error) {
		mu.Lock()
		abandons = append(abandons, req)
		mu.Unlock()
		return &lettucev1.AbandonWorkUnitResponse{}, nil
	}}
	d := slowGREPHost(t, mc)
	conn := d.multiClient.Servers()[0]
	if err := d.prefetchQueue.Push(&PreFetchItem{WU: grepUnitWithDeadline(1, 6*time.Hour), Conn: conn, FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := d.prefetchQueue.Push(&PreFetchItem{WU: grepUnitWithDeadline(2, 8*time.Hour), Conn: conn, FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache).sweepBuffer()

	if got := d.prefetchQueue.Len(); got != 1 {
		t.Errorf("buffer holds %d units after the sweep, want 1 (the 8 h unit)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(abandons) != 1 {
		t.Fatalf("units given back = %d, want 1", len(abandons))
	}
	if req := abandons[0]; !req.UnrunGiveback || !strings.Contains(req.Reason, "deadline") || req.WorkUnitId != grepUnitWithDeadline(1, 0).ID {
		t.Errorf("give-back = %+v; want the 6 h unit, flagged un-run, the reason naming its deadline", req)
	}
}

// TestDeadline_FetcherStopsAskingForALeafItCannotFinish: the head offers a
// GREP unit with a 6-hour deadline on every request. The first arrives, is
// returned un-run — and teaches the client GREP's deadline — and GREP is not
// asked for again. Pre-fix the client kept asking and buffered every unit.
func TestDeadline_FetcherStopsAskingForALeafItCannotFinish(t *testing.T) {
	var mu sync.Mutex
	next := 0
	mc := &mockClient{
		requestWorkUnitFn: func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			mu.Lock()
			next++
			id := fmt.Sprintf("00000000-0000-4000-8000-%012d", next)
			mu.Unlock()
			return &lettucev1.RequestWorkUnitResponse{Assignments: []*lettucev1.WorkUnitAssignment{{
				WorkUnitId: id, LeafId: "leaf-grep", Runtime: "native", DeadlineSeconds: 6 * 3600,
				InputData: []byte("{}"), ExecutionSpec: &lettucev1.ExecutionSpec{},
			}}}, nil
		},
	}
	d := slowGREPHost(t, mc)
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	f.Run(ctx)

	if calls := mc.getRequestCalls(); calls != 1 {
		t.Errorf("RequestWorkUnit calls = %d, want 1: after the first GREP unit showed its 6 h deadline, GREP (5 h here) is not asked for again", calls)
	}
	if got := d.prefetchQueue.Len(); got != 0 {
		t.Errorf("buffer holds %d GREP units, want 0", got)
	}
	if got := mc.getAbandonCalls(); got != 1 {
		t.Errorf("units given back = %d, want 1 (the one that arrived)", got)
	}
}
