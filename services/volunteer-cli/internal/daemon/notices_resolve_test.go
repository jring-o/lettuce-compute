package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-50 regression tests: a notice is a condition, so it must end. The ring
// used to store only emissions — nothing ever said "this is over" — so a
// "connected but getting no work" warning sat under the desktop app's Needs
// attention for 12–16 hours while units arrived and completed, and every
// other notice (disk gate, leaf breaker, thermal throttle, runtime breaker,
// update required) outlived its condition the same way. These tests pin the
// ring's resolve verb and each condition's end site calling it.

// noticeByCode returns the single notice with the given code, failing the
// test if there is not exactly one.
func noticeByCode(t *testing.T, l *NoticeLog, code string) Notice {
	t.Helper()
	notices, _ := l.Since(0)
	var found []Notice
	for _, n := range notices {
		if n.Code == code {
			found = append(found, n)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d notices with code %q, want exactly 1: %+v", len(found), code, notices)
	}
	return found[0]
}

// --- the ring ---

func TestNoticeLog_ResolveStampsTheLiveNoticeAndKeepsIt(t *testing.T) {
	start := time.Date(2026, 9, 1, 4, 30, 0, 0, time.UTC)
	l, now := newTestNoticeLog(start)

	l.Notify(NoticeWarn, "no_work", "no work", "", "")
	*now = now.Add(3 * time.Hour)
	if got := l.Resolve("no_work", "", ""); got != 1 {
		t.Fatalf("Resolve returned %d, want 1", got)
	}

	n := noticeByCode(t, l, "no_work")
	if n.ResolvedAt == nil || !n.ResolvedAt.Equal(start.Add(3*time.Hour)) {
		t.Fatalf("resolved_at = %v, want the resolve time %v", n.ResolvedAt, start.Add(3*time.Hour))
	}
	if n.ID != 1 || n.Count != 1 || !n.At.Equal(start) {
		t.Errorf("resolving must not change id, count or at: %+v", n)
	}
	if _, latest := l.Since(0); latest != 1 {
		t.Errorf("latest_id = %d after a resolve, want 1 (no new id)", latest)
	}

	// Resolving again, or a condition with no live notice, is a no-op.
	if got := l.Resolve("no_work", "", ""); got != 0 {
		t.Errorf("second Resolve returned %d, want 0", got)
	}
	if got := l.Resolve("disk_gate_blocked", "", ""); got != 0 {
		t.Errorf("Resolve of an absent condition returned %d, want 0", got)
	}
}

// An empty head or leaf is a wildcard: the disk gate clears for every leaf at
// once, while a recovering leaf resolves only its own notice.
func TestNoticeLog_ResolveScopesByHeadAndLeaf(t *testing.T) {
	l, _ := newTestNoticeLog(time.Date(2026, 9, 1, 4, 30, 0, 0, time.UTC))

	l.Notify(NoticeWarn, "leaf_failing", "m", "", "leaf-1")
	l.Notify(NoticeWarn, "leaf_failing", "m", "", "leaf-2")
	l.Notify(NoticeWarn, "update_required", "m", "head-a", "")
	l.Notify(NoticeWarn, "update_required", "m", "head-b", "")
	l.Notify(NoticeWarn, "disk_gate_blocked", "m", "", "leaf-1")
	l.Notify(NoticeWarn, "disk_gate_blocked", "m", "", "")

	if got := l.Resolve("leaf_failing", "", "leaf-1"); got != 1 {
		t.Errorf("Resolve(leaf_failing, leaf-1) = %d, want 1", got)
	}
	if got := l.Resolve("update_required", "head-b", ""); got != 1 {
		t.Errorf("Resolve(update_required, head-b) = %d, want 1", got)
	}
	if got := l.Resolve("disk_gate_blocked", "", ""); got != 2 {
		t.Errorf("Resolve(disk_gate_blocked, any leaf) = %d, want 2", got)
	}

	notices, _ := l.Since(0)
	resolved := map[string]bool{}
	for _, n := range notices {
		resolved[n.Code+"|"+n.Head+"|"+n.Leaf] = n.ResolvedAt != nil
	}
	want := map[string]bool{
		"leaf_failing||leaf-1":      true,
		"leaf_failing||leaf-2":      false,
		"update_required|head-a|":   false,
		"update_required|head-b|":   true,
		"disk_gate_blocked||leaf-1": true,
		"disk_gate_blocked||":       true,
	}
	for key, wantResolved := range want {
		if resolved[key] != wantResolved {
			t.Errorf("%s resolved = %v, want %v", key, resolved[key], wantResolved)
		}
	}
}

// A condition that returns within the dedupe window of its resolution reopens
// the same notice (one episode, rising count, stable id — so the desktop
// toaster does not fire again); one that returns after the window is a new
// notice.
func TestNoticeLog_ReemissionReopensWithinWindowAndIsNewAfterIt(t *testing.T) {
	start := time.Date(2026, 9, 1, 4, 30, 0, 0, time.UTC)
	l, now := newTestNoticeLog(start)

	l.Notify(NoticeWarn, "no_work", "first", "", "")
	*now = now.Add(2 * time.Minute)
	l.Resolve("no_work", "", "")
	*now = now.Add(5 * time.Minute)
	l.Notify(NoticeWarn, "no_work", "again", "", "")

	n := noticeByCode(t, l, "no_work")
	if n.ID != 1 || n.Count != 2 || n.ResolvedAt != nil || n.Message != "again" {
		t.Fatalf("re-emission 5 min after resolving must reopen the notice: %+v", n)
	}

	// The window runs from the last change (the resolution), not the first
	// emission: resolve now, return after the window has passed.
	*now = now.Add(time.Minute)
	l.Resolve("no_work", "", "")
	*now = now.Add(noticeDedupeWindow + time.Second)
	l.Notify(NoticeWarn, "no_work", "third", "", "")

	notices, latest := l.Since(0)
	if len(notices) != 2 || latest != 2 {
		t.Fatalf("got %d notices (latest %d), want 2: the old episode resolved and a fresh one", len(notices), latest)
	}
	if notices[0].ID != 2 || notices[0].Count != 1 || notices[0].ResolvedAt != nil {
		t.Errorf("newest = %+v, want a fresh live notice with id 2", notices[0])
	}
	if notices[1].ID != 1 || notices[1].ResolvedAt == nil {
		t.Errorf("oldest = %+v, want the resolved id 1 still in the ring", notices[1])
	}
}

func TestNoticeLog_NilResolveIsNoop(t *testing.T) {
	var l *NoticeLog
	if got := l.Resolve("no_work", "", ""); got != 0 {
		t.Errorf("nil ring Resolve = %d, want 0", got)
	}
}

// --- the fetcher's end sites ---

// The tester's timeline: "no work" raised during a pause, then 44 units
// completed, the notice still shown 12 hours later. Work arriving is the
// condition ending.
func TestFetcher_NoWorkNoticeResolvesWhenWorkArrives(t *testing.T) {
	srv := &ServerConnection{Client: &mockClient{}, VolunteerID: "vol-1", Name: "server-a", Available: true}
	d := newFetcherTestDaemon([]*ServerConnection{srv})
	d.notices = NewNoticeLog()
	f := NewFetcher(d, NewPreFetchQueue(8, d.logger), d.weightedSelector, d.leafCache)

	for i := 0; i < noWorkWarnThreshold; i++ {
		f.noteEmptyRound()
	}
	if n := noticeByCode(t, d.notices, "no_work"); n.ResolvedAt != nil {
		t.Fatalf("no_work notice resolved before any work arrived: %+v", n)
	}

	// The fetcher is recreated on every pause/resume; the notice an earlier
	// instance raised must resolve when THIS instance receives work.
	resumed := NewFetcher(d, NewPreFetchQueue(8, d.logger), d.weightedSelector, d.leafCache)
	resumed.noteWorkArrived()

	if n := noticeByCode(t, d.notices, "no_work"); n.ResolvedAt == nil {
		t.Fatalf("no_work notice still live after work arrived: %+v", n)
	}

	// A later idle streak raises the diagnostic again (the notice reopens —
	// the same episode within the window — or a new one starts after it).
	for i := 0; i < noWorkWarnThreshold; i++ {
		resumed.noteEmptyRound()
	}
	if n := noticeByCode(t, d.notices, "no_work"); n.ResolvedAt != nil || n.Count != 2 {
		t.Fatalf("a new idle streak must reopen the notice: %+v", n)
	}
}

// A head that serves a request is not rejecting this build: the too-old
// notice it raised (at registration or on an earlier request) is over.
func TestFetcher_UpdateRequiredNoticeResolvesWhenHeadServes(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return &lettucev1.RequestWorkUnitResponse{}, nil
		},
	}
	srv := &ServerConnection{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}
	d := newFetcherTestDaemon([]*ServerConnection{srv})
	d.notices = NewNoticeLog()
	d.notices.Notify(NoticeWarn, "update_required", "too old", "server-a", "")
	d.notices.Notify(NoticeWarn, "update_required", "too old", "server-b", "")

	f := NewFetcher(d, NewPreFetchQueue(8, d.logger), d.weightedSelector, d.leafCache)
	if _, err := f.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne: %v", err)
	}

	notices, _ := d.notices.Since(0)
	for _, n := range notices {
		switch n.Head {
		case "server-a":
			if n.ResolvedAt == nil {
				t.Errorf("server-a served a request but its update_required notice is still live: %+v", n)
			}
		case "server-b":
			if n.ResolvedAt != nil {
				t.Errorf("server-b was never contacted but its notice resolved: %+v", n)
			}
		}
	}
}

// The runtime breaker's notice ends when a Prepare for that runtime succeeds
// again (the same event that un-pauses it).
func TestFetcher_PrepareFailedNoticeResolvesOnRuntimeRecovery(t *testing.T) {
	srv := &ServerConnection{Client: &mockClient{}, VolunteerID: "vol-1", Name: "server-a", Available: true}
	d := newFetcherTestDaemon([]*ServerConnection{srv})
	d.notices = NewNoticeLog()
	f := NewFetcher(d, NewPreFetchQueue(8, d.logger), d.weightedSelector, d.leafCache)

	for i := 0; i < runtimeAbandonPauseThreshold; i++ {
		f.recordRuntimeAbandon("container", errors.New("engine not running"))
	}
	if n := noticeByCode(t, d.notices, "prepare_failed"); n.ResolvedAt != nil {
		t.Fatalf("prepare_failed resolved before the runtime recovered: %+v", n)
	}

	f.resetRuntimeAbandon("container")
	if n := noticeByCode(t, d.notices, "prepare_failed"); n.ResolvedAt == nil {
		t.Fatalf("prepare_failed still live after a successful Prepare un-paused the runtime: %+v", n)
	}
}

// --- the daemon's end sites ---

func TestDaemon_DiskGateNoticeResolvesWhenTheGateClears(t *testing.T) {
	srv := &ServerConnection{Client: &mockClient{}, VolunteerID: "vol-1", Name: "server-a", Available: true}
	d := newFetcherTestDaemon([]*ServerConnection{srv})
	d.notices = NewNoticeLog()

	// A stall no single leaf owns (the data-dir floor) is a daemon-wide notice.
	d.stallDiskGateDaemonWide("free space is below the floor", 20)
	if n := noticeByCode(t, d.notices, "disk_gate_blocked"); n.ResolvedAt != nil || n.Leaf != "" || n.Head != "" {
		t.Fatalf("disk_gate_blocked notice = %+v, want live and daemon-wide", n)
	}

	d.clearDiskGateWarning()
	if n := noticeByCode(t, d.notices, "disk_gate_blocked"); n.ResolvedAt == nil {
		t.Fatalf("disk_gate_blocked still live after the gate cleared: %+v", n)
	}
}

func TestDaemon_LeafFailingNoticeResolvesWhenTheLeafRecovers(t *testing.T) {
	clock := &failureTestClock{t: time.Unix(1_700_000_000, 0)}
	d, _, _ := breakerDaemon(t, clock, "broken-leaf", "other-leaf")
	d.notices = NewNoticeLog()

	failLeaf(d, "broken-leaf", leafFailurePauseThreshold)
	failLeaf(d, "other-leaf", leafFailurePauseThreshold)
	if notices, _ := d.notices.Since(0); len(notices) != 2 {
		t.Fatalf("got %d leaf_failing notices, want 2: %+v", len(notices), notices)
	}

	// The breaker re-probes after the cooldown; a clean run then un-pauses
	// the leaf, and only that leaf's notice ends.
	clock.advance(leafFailureCooldown + time.Second)
	d.noteLeafSuccess(&runtime.WorkUnit{ID: "wu", LeafID: "broken-leaf"})

	notices, _ := d.notices.Since(0)
	for _, n := range notices {
		switch n.Leaf {
		case "broken-leaf":
			if n.ResolvedAt == nil {
				t.Errorf("broken-leaf recovered but its leaf_failing notice is still live: %+v", n)
			}
		case "other-leaf":
			if n.ResolvedAt != nil {
				t.Errorf("other-leaf never recovered but its notice resolved: %+v", n)
			}
		}
	}
}

func TestDaemon_BufferUnrunnableNoticeResolvesWhenStarvationEnds(t *testing.T) {
	d := tb32StarvedDaemon(t)
	d.notices = NewNoticeLog()

	d.trackSlotStarvation()
	d.slotStarveMu.Lock()
	d.slotStarvedSince = time.Now().Add(-slotStarveWarnAfter - time.Minute)
	d.slotStarveMu.Unlock()
	d.trackSlotStarvation()
	if n := noticeByCode(t, d.notices, "buffer_unrunnable"); n.ResolvedAt != nil {
		t.Fatalf("buffer_unrunnable resolved while the slot is still starved: %+v", n)
	}

	// An admissible unit ends the starvation.
	if err := d.prefetchQueue.Push(&PreFetchItem{WU: &runtime.WorkUnit{
		ID:            "00000000-0000-4000-8000-0000000000fe",
		LeafID:        "leaf-small",
		RscFpopsEst:   600,
		ExecutionSpec: runtime.ExecutionSpec{MaxMemoryMB: 512},
	}}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	d.trackSlotStarvation()
	if n := noticeByCode(t, d.notices, "buffer_unrunnable"); n.ResolvedAt == nil {
		t.Fatalf("buffer_unrunnable still live after an admissible unit ended the starvation: %+v", n)
	}
}
