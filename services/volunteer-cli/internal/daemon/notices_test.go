package daemon

import (
	"fmt"
	"testing"
	"time"
)

// newTestNoticeLog returns a notice ring on a controllable clock.
func newTestNoticeLog(start time.Time) (*NoticeLog, *time.Time) {
	now := start
	l := NewNoticeLog()
	l.now = func() time.Time { return now }
	return l, &now
}

func TestNoticeLog_AppendsAndOrdersNewestFirst(t *testing.T) {
	l, now := newTestNoticeLog(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))

	l.Notify(NoticeWarn, "no_work", "first", "", "")
	*now = now.Add(time.Second)
	l.Notify(NoticeWarn, "leaf_failing", "second", "", "leaf-1")
	*now = now.Add(time.Second)
	l.Notify(NoticeInfo, "thermal_throttle", "third", "", "")

	notices, latest := l.Since(0)
	if latest != 3 {
		t.Fatalf("latest_id = %d, want 3", latest)
	}
	if len(notices) != 3 {
		t.Fatalf("got %d notices, want 3", len(notices))
	}
	if notices[0].Code != "thermal_throttle" || notices[1].Code != "leaf_failing" || notices[2].Code != "no_work" {
		t.Errorf("order = %s, %s, %s; want newest first", notices[0].Code, notices[1].Code, notices[2].Code)
	}
	if notices[2].ID != 1 || notices[2].Count != 1 || notices[2].Level != NoticeWarn {
		t.Errorf("first notice = %+v; want id 1, count 1, level warn", notices[2])
	}
	if !notices[2].FirstAt.Equal(notices[2].At) {
		t.Errorf("a fresh notice must have first_at == at, got %v / %v", notices[2].FirstAt, notices[2].At)
	}
}

// A condition that re-fires within the window refreshes the existing notice
// (count, time, message) rather than appending a duplicate — the id is stable.
func TestNoticeLog_DedupesWithinWindow(t *testing.T) {
	start := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	l, now := newTestNoticeLog(start)

	l.Notify(NoticeWarn, "update_required", "too old (v1)", "head-a", "")
	*now = now.Add(5 * time.Minute)
	l.Notify(NoticeWarn, "update_required", "too old (v2)", "head-a", "")

	notices, latest := l.Since(0)
	if len(notices) != 1 {
		t.Fatalf("got %d notices, want 1 (the second emission must refresh the first)", len(notices))
	}
	n := notices[0]
	if n.ID != 1 || latest != 1 {
		t.Errorf("id = %d, latest = %d; a refresh must not assign a new id", n.ID, latest)
	}
	if n.Count != 2 {
		t.Errorf("count = %d, want 2", n.Count)
	}
	if n.Message != "too old (v2)" {
		t.Errorf("message = %q, want the latest emission's message", n.Message)
	}
	if !n.FirstAt.Equal(start) {
		t.Errorf("first_at = %v, want the original emission time %v", n.FirstAt, start)
	}
	if !n.At.Equal(start.Add(5 * time.Minute)) {
		t.Errorf("at = %v, want the refresh time", n.At)
	}
}

// Same code but a different head or leaf is a different condition.
func TestNoticeLog_DedupeKeyIncludesHeadAndLeaf(t *testing.T) {
	l, _ := newTestNoticeLog(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))

	l.Notify(NoticeWarn, "update_required", "m", "head-a", "")
	l.Notify(NoticeWarn, "update_required", "m", "head-b", "")
	l.Notify(NoticeWarn, "leaf_failing", "m", "", "leaf-1")
	l.Notify(NoticeWarn, "leaf_failing", "m", "", "leaf-2")

	notices, _ := l.Since(0)
	if len(notices) != 4 {
		t.Fatalf("got %d notices, want 4 distinct conditions", len(notices))
	}
}

// Past the window the same condition is a new notice with a new id.
func TestNoticeLog_NewNoticeAfterWindow(t *testing.T) {
	l, now := newTestNoticeLog(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))

	l.Notify(NoticeWarn, "no_work", "m", "", "")
	*now = now.Add(noticeDedupeWindow + time.Second)
	l.Notify(NoticeWarn, "no_work", "m", "", "")

	notices, latest := l.Since(0)
	if len(notices) != 2 || latest != 2 {
		t.Fatalf("got %d notices (latest %d), want 2 separate notices once the window has passed", len(notices), latest)
	}
	if notices[0].ID != 2 || notices[0].Count != 1 {
		t.Errorf("newest = %+v; want a fresh id 2 with count 1", notices[0])
	}
}

// ?since=<id> returns only notices created after that id; a refreshed notice
// keeps its id and so is not re-delivered.
func TestNoticeLog_SinceFiltersByID(t *testing.T) {
	l, now := newTestNoticeLog(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))

	l.Notify(NoticeWarn, "a", "m", "", "")
	l.Notify(NoticeWarn, "b", "m", "", "")
	l.Notify(NoticeWarn, "c", "m", "", "")

	notices, latest := l.Since(2)
	if latest != 3 {
		t.Errorf("latest_id = %d, want 3", latest)
	}
	if len(notices) != 1 || notices[0].Code != "c" {
		t.Fatalf("since=2 returned %+v, want only the notice with id 3", notices)
	}

	// Refresh "a" (id 1): still filtered out by since=2.
	*now = now.Add(time.Second)
	l.Notify(NoticeWarn, "a", "m2", "", "")
	notices, _ = l.Since(2)
	if len(notices) != 1 {
		t.Errorf("a refreshed notice must keep its id and stay below the since cursor; got %+v", notices)
	}

	notices, _ = l.Since(3)
	if len(notices) != 0 {
		t.Errorf("since=latest must return nothing, got %+v", notices)
	}
}

// The ring holds noticeRingCapacity entries and drops the oldest beyond that.
func TestNoticeLog_RingIsBounded(t *testing.T) {
	l, now := newTestNoticeLog(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))

	for i := 0; i < noticeRingCapacity+10; i++ {
		// Distinct leafs so nothing de-duplicates.
		l.Notify(NoticeWarn, "leaf_failing", "m", "", fmt.Sprintf("leaf-%d", i))
		*now = now.Add(time.Millisecond)
	}

	notices, latest := l.Since(0)
	if len(notices) != noticeRingCapacity {
		t.Fatalf("ring holds %d notices, want %d", len(notices), noticeRingCapacity)
	}
	if latest != uint64(noticeRingCapacity+10) {
		t.Errorf("latest_id = %d, want %d (ids keep counting past the ring)", latest, noticeRingCapacity+10)
	}
	oldest := notices[len(notices)-1]
	if oldest.ID != 11 {
		t.Errorf("oldest retained id = %d, want 11 (the first ten were dropped)", oldest.ID)
	}
}

// Every emission site calls Notify without a guard, so a nil ring must be a
// harmless no-op and a nil ring's Since must answer "nothing".
func TestNoticeLog_NilIsNoop(t *testing.T) {
	var l *NoticeLog
	l.Notify(NoticeWarn, "x", "m", "", "")
	notices, latest := l.Since(0)
	if notices == nil || len(notices) != 0 || latest != 0 {
		t.Errorf("nil ring: got %v / %d, want an empty (non-nil) list and latest 0", notices, latest)
	}
}
