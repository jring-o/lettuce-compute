package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-19: the two buffer-expiry guards sat AFTER the fetcher loop's shouldFetch
// early-continue, so they went dormant during exactly the long idle stretches in
// which a buffered unit ages out — a closed schedule window, a tripped disk
// gate. Units were then held past the point the head had re-staged them.
//
// Observed across three hosts on one build: the host running scheduling_mode
// ALWAYS dropped its stale unit on the second it came due, while two SCHEDULED
// hosts dropped theirs 5 min 47 s and 27 min 7 s AFTER their head-side
// reservation had already lapsed, both in a burst on the first loop iteration
// after the window reopened.

func quietFetcher(q *PreFetchQueue, shouldFetch func() bool, now func() time.Time) *Fetcher {
	return &Fetcher{
		queue:           q,
		logger:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
		shouldFetchFunc: shouldFetch,
		workBufferFullFn: func() bool {
			return true // never actually request work in these tests
		},
		now:     now,
		backoff: time.Millisecond,
	}
}

// staleItem is a buffered unit fetched long enough ago to be past the 90%
// deadline mark that DropExpiring enforces.
func staleItem(id string, fetchedAt time.Time, deadlineSec int32) *PreFetchItem {
	return &PreFetchItem{
		WU:        &runtime.WorkUnit{ID: id, LeafID: "leaf-1", DeadlineSeconds: deadlineSec},
		FetchedAt: fetchedAt,
	}
}

// The regression: with fetching disallowed, the sweep must still run.
func TestFetcherRun_SweepsBufferWhileNotFetching(t *testing.T) {
	q := NewPreFetchQueue(8, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	// Fetched 5 h 6 min ago against a 5 h deadline: past the 90% drop point, and
	// past the head-side reservation window too.
	if err := q.Push(staleItem("stale-1", time.Now().Add(-18360*time.Second), 18000)); err != nil {
		t.Fatalf("push: %v", err)
	}

	f := quietFetcher(q, func() bool { return false }, time.Now) // scheduler says don't run

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()

	deadline := time.After(1500 * time.Millisecond)
	for q.Len() > 0 {
		select {
		case <-deadline:
			t.Fatal("buffered unit still held while shouldFetch is false: the expiry guards only run when the client is already fetching, so a closed schedule window or a tripped disk gate lets units outlive their head-side reservation")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// The already-correct path must stay correct.
func TestFetcherRun_SweepsBufferWhileFetching(t *testing.T) {
	q := NewPreFetchQueue(8, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err := q.Push(staleItem("stale-2", time.Now().Add(-18360*time.Second), 18000)); err != nil {
		t.Fatalf("push: %v", err)
	}

	f := quietFetcher(q, func() bool { return true }, time.Now)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()

	deadline := time.After(1500 * time.Millisecond)
	for q.Len() > 0 {
		select {
		case <-deadline:
			t.Fatal("buffered unit still held while fetching")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// A fresh unit must survive the sweep — otherwise the tests above would pass on
// a sweep that simply emptied the buffer.
func TestSweepBuffer_KeepsFreshUnits(t *testing.T) {
	q := NewPreFetchQueue(8, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err := q.Push(staleItem("fresh", time.Now().Add(-60*time.Second), 18000)); err != nil {
		t.Fatalf("push: %v", err)
	}

	quietFetcher(q, func() bool { return false }, time.Now).sweepBuffer()

	if q.Len() != 1 {
		t.Errorf("queue length %d, want 1: a unit one minute into a five-hour deadline must not be dropped", q.Len())
	}
}

// sweepBuffer is called from both the fetcher loop and the daemon's maintenance
// ticker, so it has to be safe to run repeatedly on the same queue.
func TestSweepBuffer_Idempotent(t *testing.T) {
	q := NewPreFetchQueue(8, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err := q.Push(staleItem("stale-3", time.Now().Add(-18360*time.Second), 18000)); err != nil {
		t.Fatalf("push: %v", err)
	}
	f := quietFetcher(q, func() bool { return false }, time.Now)

	f.sweepBuffer()
	f.sweepBuffer()
	f.sweepBuffer()

	if q.Len() != 0 {
		t.Errorf("queue length %d, want 0", q.Len())
	}
}
