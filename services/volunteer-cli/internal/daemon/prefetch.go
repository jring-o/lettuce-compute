package daemon

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// PreFetchItem is a fetched+prepared WU waiting for an open slot.
type PreFetchItem struct {
	WU      *runtime.WorkUnit
	WUResp  *lettucev1.WorkUnitAssignment
	Prep    *runtime.PrepareResult
	Runtime runtime.Runtime
	Conn    *ServerConnection
	FetchedAt time.Time
}

// PreFetchQueue is a thread-safe queue of pre-fetched work units.
type PreFetchQueue struct {
	mu       sync.Mutex
	items    []*PreFetchItem
	maxDepth int
	logger   *slog.Logger
	notify   chan struct{} // signaled when an item is pushed
}

// NewPreFetchQueue creates a new pre-fetch queue with the given max depth.
func NewPreFetchQueue(maxDepth int, logger *slog.Logger) *PreFetchQueue {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	return &PreFetchQueue{
		maxDepth: maxDepth,
		logger:   logger,
		notify:   make(chan struct{}, 1),
	}
}

// Notify returns a channel that is signaled when an item is pushed.
func (q *PreFetchQueue) Notify() <-chan struct{} {
	return q.notify
}

// Push adds an item to the back of the queue. Returns error if full.
func (q *PreFetchQueue) Push(item *PreFetchItem) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= q.maxDepth {
		return fmt.Errorf("prefetch queue is full (%d/%d)", len(q.items), q.maxDepth)
	}
	q.items = append(q.items, item)
	// Non-blocking signal that a new item is available.
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return nil
}

// PushBack re-inserts an item at the front of the queue without signaling notify.
// Used when an item was popped but can't be processed yet (e.g., insufficient resources).
func (q *PreFetchQueue) PushBack(item *PreFetchItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append([]*PreFetchItem{item}, q.items...)
}

// Pop removes and returns the front item (FIFO). Returns nil if empty.
func (q *PreFetchQueue) Pop() *PreFetchItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item
}

// Len returns the number of items in the queue.
func (q *PreFetchQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Items returns a snapshot of the current queue items.
func (q *PreFetchQueue) Items() []*PreFetchItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*PreFetchItem, len(q.items))
	copy(out, q.items)
	return out
}

// IsFull returns true if the queue is at max capacity.
func (q *PreFetchQueue) IsFull() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) >= q.maxDepth
}

// DropExpiring removes items whose deadline is nearly expired.
// threshold is the fraction of deadline remaining below which items are dropped
// (e.g., 0.1 means drop when < 10% of deadline remains).
// Cleanup is called on dropped items' runtimes.
func (q *PreFetchQueue) DropExpiring(threshold float64) {
	q.mu.Lock()
	var kept []*PreFetchItem
	var dropped []*PreFetchItem
	for _, item := range q.items {
		if item.WU.DeadlineSeconds <= 0 {
			kept = append(kept, item)
			continue
		}
		elapsed := time.Since(item.FetchedAt)
		deadline := time.Duration(item.WU.DeadlineSeconds) * time.Second
		ratio := float64(elapsed) / float64(deadline)
		if ratio > (1.0 - threshold) {
			dropped = append(dropped, item)
		} else {
			kept = append(kept, item)
		}
	}
	q.items = kept
	q.mu.Unlock()

	// Cleanup dropped items outside the lock.
	for _, item := range dropped {
		q.logger.Info("dropping expired prefetch item",
			"work_unit_id", item.WU.ID,
			"deadline_seconds", item.WU.DeadlineSeconds,
			"fetched_at", item.FetchedAt,
		)
		// The head reclaims this unit via its deadline (these items always have
		// DeadlineSeconds > 0); we just clean up the local work dir.
		if item.Runtime != nil && item.Prep != nil {
			if err := item.Runtime.Cleanup(item.Prep); err != nil {
				q.logger.Warn("cleanup failed for dropped item", "work_unit_id", item.WU.ID, "error", err)
			}
		}
	}
}

// DropLapsedReservations removes buffered items whose head-side reservation
// window (reserved_until_unix) has lapsed or is within `margin` of lapsing.
//
// With per-task heartbeats removed, the reservation window is sized ONCE at
// hand-out and is NEVER renewed: it is the deadline-based lease for a buffered
// (not-yet-run-started) unit. A unit therefore approaches lapse simply by sitting
// in the buffer, and once reserved_until passes, the head's lapsed-reservation
// sweep re-stages the unit for re-dispatch. If we later pop such a unit, its
// run-start StartWork would return Ok=false (no longer reserved for us) after a
// wasted prepare — possibly racing a SECOND volunteer the head already handed it
// to — so we drop it from the buffer `margin` ahead of lapse instead. Items with
// no reservation window (ReservedUntilUnix == 0) are left untouched. now is
// injected for tests.
func (q *PreFetchQueue) DropLapsedReservations(margin time.Duration, now time.Time) {
	q.mu.Lock()
	var kept []*PreFetchItem
	var dropped []*PreFetchItem
	cutoff := now.Add(margin).Unix()
	for _, item := range q.items {
		if item.WU == nil || item.WU.ReservedUntilUnix == 0 {
			kept = append(kept, item)
			continue
		}
		if item.WU.ReservedUntilUnix <= cutoff {
			dropped = append(dropped, item)
		} else {
			kept = append(kept, item)
		}
	}
	q.items = kept
	q.mu.Unlock()

	for _, item := range dropped {
		q.logger.Warn("dropping buffered item with lapsed reservation window",
			"work_unit_id", item.WU.ID,
			"reserved_until_unix", item.WU.ReservedUntilUnix,
		)
		if item.Runtime != nil && item.Prep != nil {
			if err := item.Runtime.Cleanup(item.Prep); err != nil {
				q.logger.Warn("cleanup failed for lapsed-reservation item", "work_unit_id", item.WU.ID, "error", err)
			}
		}
	}
}

// Clear removes all items and returns them so the caller can clean up.
func (q *PreFetchQueue) Clear() []*PreFetchItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}
