package daemon

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newMockPreFetchItem(id string, deadlineSeconds int32, fetchedAt time.Time) *PreFetchItem {
	mr := &trackingRuntime{canHandle: true}
	return &PreFetchItem{
		WU: &runtime.WorkUnit{
			ID:              id,
			LeafID:          "proj-1",
			DeadlineSeconds: deadlineSeconds,
		},
		WUResp:  &lettucev1.WorkUnitAssignment{},
		Prep:    &runtime.PrepareResult{WorkDir: "/tmp/" + id},
		Runtime: mr,
		Conn:    &ServerConnection{Name: "test-server", VolunteerID: "vol-1"},
		FetchedAt: fetchedAt,
	}
}

// trackingRuntime tracks cleanup calls for verification.
type trackingRuntime struct {
	canHandle    bool
	mu           sync.Mutex
	cleanupCalls int
}

func (r *trackingRuntime) Prepare(_ context.Context, _ *runtime.WorkUnit) (*runtime.PrepareResult, error) {
	return &runtime.PrepareResult{WorkDir: "/tmp/work"}, nil
}

func (r *trackingRuntime) Execute(_ context.Context, _ *runtime.WorkUnit, _ *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
	return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0}, nil
}

func (r *trackingRuntime) Cleanup(_ *runtime.PrepareResult) error {
	r.mu.Lock()
	r.cleanupCalls++
	r.mu.Unlock()
	return nil
}

func (r *trackingRuntime) CanHandle(_ *runtime.ExecutionSpec) bool { return r.canHandle }
func (r *trackingRuntime) Name() string                           { return "native" }

func (r *trackingRuntime) getCleanupCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cleanupCalls
}

func TestPreFetchQueue_PushPop(t *testing.T) {
	q := NewPreFetchQueue(3, newTestLogger())

	itemA := newMockPreFetchItem("wu-a", 100, time.Now())
	itemB := newMockPreFetchItem("wu-b", 100, time.Now())
	itemC := newMockPreFetchItem("wu-c", 100, time.Now())
	itemD := newMockPreFetchItem("wu-d", 100, time.Now())

	// Push 3 items — all succeed.
	if err := q.Push(itemA); err != nil {
		t.Fatalf("push A: %v", err)
	}
	if err := q.Push(itemB); err != nil {
		t.Fatalf("push B: %v", err)
	}
	if err := q.Push(itemC); err != nil {
		t.Fatalf("push C: %v", err)
	}

	// Push 4th — error (full).
	if err := q.Push(itemD); err == nil {
		t.Error("expected error pushing to full queue")
	}

	// Pop returns first item (FIFO).
	got := q.Pop()
	if got == nil || got.WU.ID != "wu-a" {
		t.Errorf("first pop: got %v, want wu-a", got)
	}

	if q.Len() != 2 {
		t.Errorf("len = %d, want 2", q.Len())
	}

	// Pop remaining.
	got = q.Pop()
	if got == nil || got.WU.ID != "wu-b" {
		t.Errorf("second pop: got %v, want wu-b", got)
	}
	got = q.Pop()
	if got == nil || got.WU.ID != "wu-c" {
		t.Errorf("third pop: got %v, want wu-c", got)
	}

	// Pop from empty.
	got = q.Pop()
	if got != nil {
		t.Error("expected nil from empty queue")
	}
}

func TestPreFetchQueue_DropExpiring(t *testing.T) {
	q := NewPreFetchQueue(5, newTestLogger())

	now := time.Now()

	// Item A: 100s ago, deadline 110s -> 91% elapsed -> DROP
	itemA := newMockPreFetchItem("wu-a", 110, now.Add(-100*time.Second))
	// Item B: 50s ago, deadline 110s -> 45% elapsed -> KEEP
	itemB := newMockPreFetchItem("wu-b", 110, now.Add(-50*time.Second))
	// Item C: 105s ago, deadline 110s -> 95% elapsed -> DROP
	itemC := newMockPreFetchItem("wu-c", 110, now.Add(-105*time.Second))

	q.Push(itemA)
	q.Push(itemB)
	q.Push(itemC)

	q.DropExpiring(0.1)

	if q.Len() != 1 {
		t.Fatalf("len after drop = %d, want 1", q.Len())
	}

	remaining := q.Pop()
	if remaining == nil || remaining.WU.ID != "wu-b" {
		t.Errorf("remaining item = %v, want wu-b", remaining)
	}

	// Verify cleanup was called on dropped items.
	rtA := itemA.Runtime.(*trackingRuntime)
	rtC := itemC.Runtime.(*trackingRuntime)
	if rtA.getCleanupCalls() != 1 {
		t.Errorf("item A cleanup calls = %d, want 1", rtA.getCleanupCalls())
	}
	if rtC.getCleanupCalls() != 1 {
		t.Errorf("item C cleanup calls = %d, want 1", rtC.getCleanupCalls())
	}
}

func TestPreFetchQueue_Clear(t *testing.T) {
	q := NewPreFetchQueue(3, newTestLogger())

	q.Push(newMockPreFetchItem("wu-a", 100, time.Now()))
	q.Push(newMockPreFetchItem("wu-b", 100, time.Now()))

	items := q.Clear()
	if len(items) != 2 {
		t.Fatalf("clear returned %d items, want 2", len(items))
	}
	if q.Len() != 0 {
		t.Errorf("len after clear = %d, want 0", q.Len())
	}
}

func TestPreFetchQueue_IsFull(t *testing.T) {
	q := NewPreFetchQueue(2, newTestLogger())

	if q.IsFull() {
		t.Error("empty queue should not be full")
	}

	q.Push(newMockPreFetchItem("wu-a", 100, time.Now()))
	if q.IsFull() {
		t.Error("1/2 queue should not be full")
	}

	q.Push(newMockPreFetchItem("wu-b", 100, time.Now()))
	if !q.IsFull() {
		t.Error("2/2 queue should be full")
	}

	q.Pop()
	if q.IsFull() {
		t.Error("1/2 queue should not be full after pop")
	}
}
