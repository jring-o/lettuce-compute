package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// The "connected but getting no work" diagnostic counted every empty answer
// from a head, whatever the buffer held. Since the hours buffer sizes itself
// from a leaf's measured unit length, a fast host on short units wants more
// units than a head's per-machine in-flight cap (default 10) allows: two hours
// of ten-minute units on one slot is twelve. The head refuses every further
// request with an empty answer and no reason, so a healthy host with one unit
// running and nine queued was told the heads had "no units matching this
// machine" and to check its runtimes, disk and schedule. An empty answer now
// counts only while the machine does not already hold its next unit for every
// slot.

// noWorkHost is a volunteer with slots slots, work_buffer_hours 2 and a
// benchmark of 1, one native leaf on a head that answers every request with
// nothing, running units occupying the first running slots and queued ten-
// minute units waiting in the buffer. With at most ten units held the hours
// target (7,200 s per slot) is never met, so the fetcher keeps asking — the
// arithmetic of a host at the head's cap. Each unit declares memMB of memory
// against a max_memory_mb of 1,024.
func noWorkHost(t *testing.T, slots, running, queued int, memMB int32) (*Daemon, *mockClient, *bytes.Buffer) {
	t.Helper()
	mc, _ := tb49CountingHead()
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	d := newFetcherTestDaemon(servers)
	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	d.notices = NewNoticeLog()
	d.cfg.WorkBufferHours = 2
	d.cfg.MaxConcurrentTasks = slots
	d.benchmarkFPOPS = 1
	d.cfg.ResourceLimits.MaxMemoryMB = 1024
	d.slotManager = NewSlotManager(slots, d.logger)
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false } // the configured budget decides
	t.Cleanup(func() { freeSystemMemoryMB = orig })
	d.leafCache.PopulateForTest("server-a", &CachedHeadInfo{
		Name: "server-a",
		Leafs: []CachedLeafInfo{{ID: "leaf-1", Slug: "leaf-1", Name: "Leaf", State: "ACTIVE",
			ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"linux-amd64": "https://example.org/leaf"}}}},
		DefaultWeights: map[string]int{"leaf-1": 100},
	})
	d.weightedSelector.SetLeafWeights("server-a", map[string]int{"leaf-1": 100})
	unit := func(i int) *runtime.WorkUnit {
		return &runtime.WorkUnit{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), LeafID: "leaf-1", RscFpopsEst: 608,
			ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: memMB}}
	}
	for i := 0; i < running; i++ {
		occupy(d, i, unit(100+i), nil)
	}
	for i := 0; i < queued; i++ {
		if err := d.prefetchQueue.Push(&PreFetchItem{WU: unit(i), FetchedAt: time.Now()}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	if d.workBufferFull() {
		t.Fatal("buffer reports full — the hours target must be out of reach, as it is under the head's cap")
	}
	return d, mc, &buf
}

// runNoWorkFetcher runs the fetcher for 300 ms against the empty head.
func runNoWorkFetcher(d *Daemon) {
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	f.Run(ctx)
}

// TestNoWorkNotice_NotRaisedBesideAFullQueue is a one-slot host holding what
// a cap of ten allows — one unit running, nine queued — asking a head that
// refuses it every time. The head was asked (the fetcher still tops up the
// buffer), but the machine is not short of work, so neither the WARN nor the
// notice appears. Before, both did after five empty answers.
func TestNoWorkNotice_NotRaisedBesideAFullQueue(t *testing.T) {
	d, mc, buf := noWorkHost(t, 1, 1, 9, 0)
	runNoWorkFetcher(d)

	if mc.getRequestCalls() < noWorkWarnThreshold {
		t.Fatalf("head asked %d time(s) — fewer than the streak needs, so the test proves nothing", mc.getRequestCalls())
	}
	if n, last := countNoticesByCode(d.notices, "no_work"); n != 0 {
		t.Errorf("no_work notice raised with one unit running and nine queued on a one-slot host: %q", last.Message)
	}
	if strings.Contains(buf.String(), "connected but getting no work") {
		t.Error("no-work WARN logged beside a full queue")
	}
	if got := d.prefetchQueue.Len(); got != 9 {
		t.Errorf("buffer holds %d queued units, want the 9 preloaded", got)
	}
}

// TestNoWorkNotice_StillRaisedWhenTheMachineRunsShort: the diagnostic keeps
// its job wherever the machine is, or is about to be, without work — an empty
// buffer beside an idle slot, nothing queued behind the running unit,
// fewer queued units than slots, or a slot idle beside queued units it
// cannot start.
func TestNoWorkNotice_StillRaisedWhenTheMachineRunsShort(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		slots, running, queued int
		memMB                  int32
	}{
		{"nothing held", 1, 0, 0, 0},
		{"nothing queued behind the running unit", 1, 1, 0, 0},
		{"fewer queued than slots", 2, 2, 1, 0},
		// Two 600 MB units queued for two slots, but a 1,024 MB budget runs
		// one beside the running unit: a slot idles beside work it cannot
		// start, so the buffer is not work in hand for it.
		{"a slot idle beside queued work it cannot start", 2, 1, 2, 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := noWorkHost(t, tc.slots, tc.running, tc.queued, tc.memMB)
			runNoWorkFetcher(d)
			if n, _ := countNoticesByCode(d.notices, "no_work"); n != 1 {
				t.Errorf("no_work notice raised %d time(s), want 1", n)
			}
		})
	}
}
