package daemon

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// A queued unit's countdown. The app's queued card counted the full deadline
// down from the moment the unit was fetched, but the buffer returns a unit
// unrun once 90 % of that deadline has passed without a slot starting it (or
// a minute before its reservation lapses, if that is sooner), and a started
// unit's deadline is counted afresh from its start. So a card could read "30
// minutes" for a unit about to be given up. The status API now carries the
// moment the buffer gives each queued unit up.

// TestQueuedTaskStartBy_IsWhenTheBufferGivesTheUnitUp: a five-hour unit
// fetched now must start within four and a half hours; one whose reservation
// lapses sooner must start a minute before the lapse; and a unit with neither
// has no start-by time.
func TestQueuedTaskStartBy_IsWhenTheBufferGivesTheUnitUp(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fetched := time.Date(2026, 9, 24, 4, 48, 18, 0, time.UTC)
	lapse := fetched.Add(2 * time.Hour)
	q := NewPreFetchQueue(8, logger)
	for _, item := range []*PreFetchItem{
		{WU: &runtime.WorkUnit{ID: "five-hours", DeadlineSeconds: 5 * 3600}, FetchedAt: fetched},
		{WU: &runtime.WorkUnit{ID: "reservation-first", DeadlineSeconds: 5 * 3600, ReservedUntilUnix: lapse.Unix()}, FetchedAt: fetched},
		{WU: &runtime.WorkUnit{ID: "open-ended"}, FetchedAt: fetched},
	} {
		if err := q.Push(item); err != nil {
			t.Fatalf("Push %s: %v", item.WU.ID, err)
		}
	}
	d := &Daemon{prefetchQueue: q}

	want := map[string]time.Time{
		"five-hours":        fetched.Add(4*time.Hour + 30*time.Minute),
		"reservation-first": lapse.Add(-reservationDropMargin),
		"open-ended":        {},
	}
	tasks := d.GetQueuedTasks()
	if len(tasks) != len(want) {
		t.Fatalf("GetQueuedTasks returned %d units, want %d", len(tasks), len(want))
	}
	for _, task := range tasks {
		if !task.StartBy.Equal(want[task.WorkUnitID]) {
			t.Errorf("%s: StartBy = %v, want %v", task.WorkUnitID, task.StartBy, want[task.WorkUnitID])
		}
	}
}

// TestQueuedTaskStartBy_AgreesWithTheDrop: the start-by time is the moment the
// buffer sweep actually drops the unit — a unit two seconds short of it stays
// buffered, one two seconds past it is dropped.
func TestQueuedTaskStartBy_AgreesWithTheDrop(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	const deadline = 5 * 3600
	dropAfter := time.Duration(float64(deadline*time.Second) * (1 - expiringDropThreshold))
	now := time.Now()
	early := &PreFetchItem{WU: &runtime.WorkUnit{ID: "early", DeadlineSeconds: deadline}, FetchedAt: now.Add(-dropAfter + 2*time.Second)}
	late := &PreFetchItem{WU: &runtime.WorkUnit{ID: "late", DeadlineSeconds: deadline}, FetchedAt: now.Add(-dropAfter - 2*time.Second)}
	q := NewPreFetchQueue(8, logger)
	for _, item := range []*PreFetchItem{early, late} {
		if err := q.Push(item); err != nil {
			t.Fatalf("Push %s: %v", item.WU.ID, err)
		}
	}
	if by := early.StartBy(expiringDropThreshold, reservationDropMargin); !by.After(now) {
		t.Fatalf("early unit's StartBy %v is not after now %v", by, now)
	}
	if by := late.StartBy(expiringDropThreshold, reservationDropMargin); !by.Before(now) {
		t.Fatalf("late unit's StartBy %v is not before now %v", by, now)
	}

	q.DropExpiring(expiringDropThreshold)
	items := q.Items()
	if len(items) != 1 || items[0].WU.ID != "early" {
		ids := make([]string, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.WU.ID)
		}
		t.Errorf("after the sweep the buffer holds %v, want only the unit whose StartBy has not passed", ids)
	}
}
