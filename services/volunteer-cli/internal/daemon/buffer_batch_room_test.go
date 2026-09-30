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

// Every unit a head hands out must end in the work buffer or back at the head, and
// the fetcher must never ask for more than the buffer can hold or the head will
// give.
//
// On a 256-thread host whose head let it hold 514 copies, the hours target wanted
// about 2,400 units while the queue held 256. Nothing in the fetch gate read the
// queue's depth, so the fetcher kept asking for 64 at a time. The first unit of a
// batch that found the queue full was given back and the rest of the batch was
// forgotten: neither buffered nor given back, and so missing from the held set the
// next request reported. The head reclaimed each of them a minute later. In one
// hour the head handed out 725 copies and 176 started.
//
// The tests here use only entry points that existed before the fix (bufferBatch,
// fetchOne, Run), so they compile against the old code and fail there on their
// assertions.

// roomTestHead is a head that records every request and give-back it receives and
// answers each request with the next reply in its script (an empty reply once the
// script runs out).
type roomTestHead struct {
	*mockClient
	mu       sync.Mutex
	asks     []*lettucev1.RequestWorkUnitRequest
	abandons []*lettucev1.AbandonWorkUnitRequest
	replies  []*lettucev1.RequestWorkUnitResponse
}

func newRoomTestHead(replies ...*lettucev1.RequestWorkUnitResponse) *roomTestHead {
	h := &roomTestHead{mockClient: &mockClient{}, replies: replies}
	h.mockClient.requestWorkUnitFn = func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.asks = append(h.asks, req)
		if len(h.replies) == 0 {
			return &lettucev1.RequestWorkUnitResponse{}, nil
		}
		resp := h.replies[0]
		h.replies = h.replies[1:]
		return resp, nil
	}
	h.mockClient.abandonFn = func(_ context.Context, req *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error) {
		h.mu.Lock()
		h.abandons = append(h.abandons, req)
		h.mu.Unlock()
		return &lettucev1.AbandonWorkUnitResponse{Requeued: true}, nil
	}
	return h
}

func (h *roomTestHead) recordedAsks() []*lettucev1.RequestWorkUnitRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*lettucev1.RequestWorkUnitRequest(nil), h.asks...)
}

func (h *roomTestHead) recordedAbandons() []*lettucev1.AbandonWorkUnitRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*lettucev1.AbandonWorkUnitRequest(nil), h.abandons...)
}

// roomTestUnits is n native units of leaf-1 with ids numbered from first.
func roomTestUnits(first, n int) []*lettucev1.WorkUnitAssignment {
	out := make([]*lettucev1.WorkUnitAssignment, 0, n)
	for i := first; i < first+n; i++ {
		out = append(out, &lettucev1.WorkUnitAssignment{
			WorkUnitId:    fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			LeafId:        "leaf-1",
			Runtime:       "native",
			ExecutionSpec: &lettucev1.ExecutionSpec{},
		})
	}
	return out
}

// roomTestHost is a machine with the given number of slots, a two-hour buffer and a
// queue of the given depth shared by the fetcher and the buffer arithmetic. Its one
// leaf, leaf-1, has completed here in unitSeconds (so the hours target counts in
// units of that length).
func roomTestHost(t *testing.T, head *roomTestHead, slots, depth int, unitSeconds float64) (*Daemon, *Fetcher, *ServerConnection) {
	t.Helper()
	conn := &ServerConnection{Client: head, VolunteerID: "vol-1", Name: "head-a", Available: true,
		Config: config.ServerConfig{GRPCAddress: "head-a:443"}}
	d := newFetcherTestDaemon([]*ServerConnection{conn})
	d.cfg.WorkBufferHours = 2
	setTestSlots(d.cfg, slots)
	d.slotManager = NewSlotManager(slots, d.logger)
	d.prefetchQueue = NewPreFetchQueue(depth, d.logger)
	d.durations = LoadDurationTracker(t.TempDir())
	for i := 0; i < 5; i++ {
		d.durations.Record("leaf-1", 0, unitSeconds)
	}
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	return d, f, conn
}

// fillQueue buffers n placeholder units of leaf-1 that no head handed out.
func fillQueue(t *testing.T, q *PreFetchQueue, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("00000000-0000-4000-9000-%012d", i+1)
		if err := q.Push(&PreFetchItem{WU: &runtime.WorkUnit{ID: id, LeafID: "leaf-1"}}); err != nil {
			t.Fatalf("filling the queue: %v", err)
		}
	}
}

// The filed reproduction: a batch of eight arrives at a queue that holds three.
// Three are buffered and the other five go back to the head as un-run give-backs,
// counted in returned so the next ask shrinks. None is dropped, and none of the five
// costs a Prepare. Before the fix: three buffered, one given back, four dropped.
func TestBufferBatch_FullQueueGivesBackEveryUnitItCannotHold(t *testing.T) {
	hc := newTB80RecordingHead()
	head := &ServerConnection{Client: hc, VolunteerID: "vol-1", Name: "server-a", Available: true,
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	cr := &mockRuntime{canHandle: true, name: "container"}
	d := tb80Daemon(t, head, cr)
	q := NewPreFetchQueue(3, d.logger)
	f := NewFetcher(d, q, d.weightedSelector, d.leafCache)

	leaf := d.leafCache.GetLeafs("server-a")[0]
	pushed, returned := f.bufferBatch(context.Background(), head, leaf, tb80ContainerBatch(8))

	abandons := hc.recorded()
	if pushed+len(abandons) != 8 {
		t.Fatalf("batch 8: buffered %d, given back %d, silently dropped %d; want every unit buffered or given back",
			pushed, len(abandons), 8-pushed-len(abandons))
	}
	if pushed != 3 || q.Len() != 3 {
		t.Errorf("buffered %d (queue length %d), want 3: the queue holds three", pushed, q.Len())
	}
	if returned != 5 {
		t.Errorf("returned = %d, want 5: every give-back counts, or the next ask is sized on a batch that looks nearly kept", returned)
	}
	for _, a := range abandons {
		if !a.UnrunGiveback {
			t.Errorf("unit %s given back without the un-run flag: the head would bill the copy (reason %q)", a.WorkUnitId, a.Reason)
		}
	}
	cr.mu.Lock()
	prepares := cr.prepareCalls
	cr.mu.Unlock()
	if prepares != 3 {
		t.Errorf("Prepare ran %d times, want 3: a unit the queue has no room for goes back before any Prepare cost", prepares)
	}
}

// A queue can fill while a unit is being prepared: a unit the volunteer restarted goes
// back in at the front whatever the depth. The unit whose push is then refused goes
// back to the head with its work directory cleaned up, and so does every unit after
// it. Before the fix the refused unit went back and the loop stopped, dropping the rest.
func TestBufferBatch_PushRefusedMidBatchGivesBackTheRest(t *testing.T) {
	hc := newTB80RecordingHead()
	head := &ServerConnection{Client: hc, VolunteerID: "vol-1", Name: "server-a", Available: true,
		Config: config.ServerConfig{GRPCAddress: "head-a:443", TrustedRuntimes: []string{"CONTAINER"}}}
	var q *PreFetchQueue
	prepares := 0
	cr := &mockRuntime{canHandle: true, name: "container",
		prepareFn: func(context.Context, *runtime.WorkUnit) (*runtime.PrepareResult, error) {
			prepares++
			if prepares == 2 {
				q.PushFront(&PreFetchItem{WU: &runtime.WorkUnit{ID: "00000000-0000-4000-9000-000000000001"}, RunStarted: true})
			}
			return &runtime.PrepareResult{WorkDir: "/tmp/work"}, nil
		}}
	d := tb80Daemon(t, head, cr)
	q = NewPreFetchQueue(2, d.logger)
	f := NewFetcher(d, q, d.weightedSelector, d.leafCache)

	leaf := d.leafCache.GetLeafs("server-a")[0]
	pushed, returned := f.bufferBatch(context.Background(), head, leaf, tb80ContainerBatch(4))

	abandons := hc.recorded()
	if pushed+len(abandons) != 4 {
		t.Fatalf("batch 4: buffered %d, given back %d, silently dropped %d; want every unit buffered or given back",
			pushed, len(abandons), 4-pushed-len(abandons))
	}
	if pushed != 1 || returned != 3 {
		t.Errorf("buffered %d, returned %d; want 1 and 3", pushed, returned)
	}
	for _, a := range abandons {
		if !a.UnrunGiveback || !strings.Contains(a.Reason, "buffer full") {
			t.Errorf("unit %s given back as (un-run %v, %q), want an un-run give-back naming the full buffer", a.WorkUnitId, a.UnrunGiveback, a.Reason)
		}
	}
	if got := cr.getCleanupCalls(); got != 1 {
		t.Errorf("Cleanup ran %d times, want 1: the one prepared unit that could not be pushed", got)
	}
}

// A queue at its depth is a full buffer: the fetcher sends no request, whatever the
// hours arithmetic says, because nothing a head sent could be buffered. Before the
// fix the gate read only the hours, and a full queue beside an hours deficit asked
// every round.
func TestFetcher_NoRequestWhileQueueIsAtItsDepth(t *testing.T) {
	head := newRoomTestHead()
	conn := &ServerConnection{Client: head, VolunteerID: "vol-1", Name: "head-a", Available: true}
	d := newFetcherTestDaemon([]*ServerConnection{conn})
	q := NewPreFetchQueue(2, d.logger)
	fillQueue(t, q, 2)
	f := NewFetcher(d, q, d.weightedSelector, d.leafCache)
	f.backoff = 5 * time.Millisecond
	f.shouldFetchFunc = nil
	f.workBufferFullFn = func() bool { return false } // the hours target is far off

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	f.Run(ctx)

	if asks := head.recordedAsks(); len(asks) != 0 {
		t.Errorf("sent %d requests while the queue was at its depth (first asked for %d); want none",
			len(asks), asks[0].MaxAssignments)
	}
}

// The filed configuration: 256 slots, a two-hour buffer, 13-minute units, a queue
// of 256 with room for six. The hours target wants over two thousand units, so its
// ask is the batch ceiling, 64; the request must ask for the six the queue can take.
func TestAsk_BoundedByQueueRoom(t *testing.T) {
	head := newRoomTestHead()
	d, f, _ := roomTestHost(t, head, 256, 256, 780)
	fillQueue(t, d.prefetchQueue, 250)
	if d.workBufferHoursFull() {
		t.Fatal("250 thirteen-minute units reported full against a 256-slot two-hour target: harness drift")
	}

	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	asks := head.recordedAsks()
	if len(asks) != 1 {
		t.Fatalf("sent %d requests, want 1", len(asks))
	}
	if got := asks[0].MaxAssignments; got != 6 {
		t.Errorf("asked for %d with room for 6 in the queue; want 6", got)
	}
}

// Every reply states the machine's in-flight cap on that head. The next request asks
// for no more than the room left under it, and a head whose cap the machine has
// reached is not asked at all. Before the fix the stated cap was read only on an empty
// INFLIGHT_CAP reply, so the fetcher asked for the hours ask and learned the cap only
// from the refusal.
func TestAsk_BoundedByTheCapTheHeadStates(t *testing.T) {
	head := newRoomTestHead(
		&lettucev1.RequestWorkUnitResponse{Assignments: roomTestUnits(1, 5), InflightCap: 8, InflightHeld: 5},
		&lettucev1.RequestWorkUnitResponse{Assignments: roomTestUnits(6, 3), InflightCap: 8, InflightHeld: 8},
	)
	_, f, _ := roomTestHost(t, head, 8, 256, 780)

	for round := 1; round <= 3; round++ {
		if _, err := f.fetchOne(context.Background()); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	asks := head.recordedAsks()
	if len(asks) == 0 || asks[0].MaxAssignments <= 3 {
		t.Fatalf("first ask = %v; want the hours ask, above the room the head will state (harness drift)", asks)
	}
	if len(asks) < 2 {
		t.Fatalf("sent %d requests, want a second one for the room left under the stated cap", len(asks))
	}
	if got := asks[1].MaxAssignments; got != 3 {
		t.Errorf("second ask = %d with the head stating a cap of 8 and 5 held; want 3", got)
	}
	if len(asks) != 2 {
		t.Errorf("sent %d requests; once this machine holds the head's stated cap of 8, it must not ask that head again (third asked for %d)",
			len(asks), asks[len(asks)-1].MaxAssignments)
	}
}

// A stated cap is honored for capWaitMax: the head's reliability quota can raise it without
// a copy finishing here, and only a request finds that out. Once it is that old the head is
// asked again, bounded by the queue and the hours, and its reply states the cap afresh.
func TestAsk_AStatedCapGoesStaleAfterTheCapWait(t *testing.T) {
	head := newRoomTestHead(
		&lettucev1.RequestWorkUnitResponse{Assignments: roomTestUnits(1, 8), InflightCap: 8, InflightHeld: 8},
	)
	_, f, _ := roomTestHost(t, head, 8, 256, 780)
	now := time.Now()
	f.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		if _, err := f.fetchOne(context.Background()); err != nil {
			t.Fatalf("fetchOne: %v", err)
		}
	}
	if n := len(head.recordedAsks()); n != 1 {
		t.Fatalf("sent %d requests; at the stated cap the head must not be asked again yet", n)
	}

	now = now.Add(capWaitMax)
	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	if n := len(head.recordedAsks()); n != 2 {
		t.Errorf("sent %d requests; a cap stated %s ago must not keep the head parked", n, capWaitMax)
	}
}
