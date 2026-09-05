package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
)

// TB-70 regressions: a disk gate that blocks SOME enabled leafs while others
// keep fetching must say so — per leaf, in the notice ring and above Debug in
// the log. Before the fix shouldFetch returned true at the first leaf whose
// gate passed and the fetcher's per-leaf skip logged at Debug, so a tester
// lost a night of his biggest leaf's work to a gate only the per-leaf API
// verdict mentioned: 963 Debug skips, zero WARNs, zero notices.

// tb70Daemon is the tester's shape reduced to two native leafs on one head:
// a 15,000 MB leaf and a 1,024 MB leaf under a 10 GB allowance with no
// measured usage, so the first is gated and the second fetches. It returns
// the daemon, its log buffer and the limiter the caller can tighten.
func tb70Daemon(t *testing.T, leafs []*lettucev1.LeafInfo) (*Daemon, *bytes.Buffer, *thresholdLimiter) {
	t.Helper()
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	lim := &thresholdLimiter{availMB: 1 << 30}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, lim, scheduler)
	d.cfg.ResourceLimits.MaxDiskGB = 10 // 10,240 MB allowance
	d.notices = NewNoticeLog()

	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{Leafs: leafs}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}

	// Pre-measured usage of 0 MB: the gate is the leaf's own need against the
	// allowance, nothing else.
	d.diskUsageMu.Lock()
	d.diskUsageChecked = time.Now()
	d.diskUsageMB, d.diskUsageOK = 0, true
	d.diskUsageMu.Unlock()

	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return d, &buf, lim
}

func tb70Leaf(id, slug string, minDiskMB int64) *lettucev1.LeafInfo {
	return &lettucev1.LeafInfo{
		Id: id, Slug: slug, State: "ACTIVE",
		ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: minDiskMB},
	}
}

// diskGateNotices returns every disk_gate_blocked notice keyed by leaf id
// ("" for the daemon-wide one).
func diskGateNotices(l *NoticeLog) map[string]Notice {
	out := map[string]Notice{}
	notices, _ := l.Since(0)
	for _, n := range notices {
		if n.Code == "disk_gate_blocked" {
			out[n.Leaf] = n
		}
	}
	return out
}

// The tester's case: one gated leaf beside one that fetches. The gated leaf
// gets its own notice and WARN, once, naming it and the allowance that clears
// it; the passing leaf gets nothing; fetching goes on. Raising the allowance
// ends the notice, lowering it again raises it again.
func TestTB70_PartiallyGatedCatalogRaisesTheGatedLeafsOwnNotice(t *testing.T) {
	d, buf, _ := tb70Daemon(t, []*lettucev1.LeafInfo{
		tb70Leaf("leaf-grep", "extract2-student-crowd-v1", 15000),
		tb70Leaf("leaf-bb", "beyblade-arena", 1024),
	})

	for i := 0; i < 4; i++ {
		if !d.shouldFetch() {
			t.Fatal("shouldFetch = false, want true — the Beyblade leaf fits and must keep fetching")
		}
	}

	got := diskGateNotices(d.notices)
	if len(got) != 1 {
		t.Fatalf("got %d disk_gate_blocked notices after 4 polls with one gated leaf, want exactly 1 (for the gated leaf): %+v", len(got), got)
	}
	n, ok := got["leaf-grep"]
	if !ok {
		t.Fatalf("the disk_gate_blocked notice does not name the gated leaf: %+v", got)
	}
	if n.ResolvedAt != nil || n.Head != "default" || n.Count != 1 {
		t.Errorf("notice = %+v, want live, on head default, emitted once across repeated polls", n)
	}
	if !strings.Contains(n.Message, `"extract2-student-crowd-v1"`) || !strings.Contains(n.Message, "max_disk_gb = 15 (currently 10)") {
		t.Errorf("notice must name the leaf and the allowance that covers it; got: %s", n.Message)
	}
	if _, ok := got["leaf-bb"]; ok {
		t.Errorf("the passing leaf got a disk_gate_blocked notice: %+v", got["leaf-bb"])
	}

	s := buf.String()
	if c := strings.Count(s, "not fetching leaf"); c != 1 {
		t.Errorf("per-leaf disk-gate WARN count across 4 polls = %d, want exactly 1; log: %s", c, s)
	}
	if !strings.Contains(s, "extract2-student-crowd-v1") {
		t.Errorf("the per-leaf WARN must name the gated leaf; log: %s", s)
	}
	if strings.Contains(s, "not fetching work") {
		t.Errorf("the machine-wide WARN fired although a leaf keeps fetching; log: %s", s)
	}

	// The tester raised the allowance to the figure the gate asked for.
	d.cfg.ResourceLimits.MaxDiskGB = 15
	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false after raising the allowance")
	}
	if n := diskGateNotices(d.notices)["leaf-grep"]; n.ResolvedAt == nil {
		t.Errorf("the gated leaf's notice is still live after its gate passed: %+v", n)
	}
	if !strings.Contains(buf.String(), "disk gate cleared for leaf") {
		t.Errorf("expected the per-leaf clear to be logged; log: %s", buf.String())
	}

	// Lowered again: the per-leaf latch re-armed, so it warns again.
	d.cfg.ResourceLimits.MaxDiskGB = 10
	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false after lowering the allowance — Beyblade still fits")
	}
	if n := diskGateNotices(d.notices)["leaf-grep"]; n.ResolvedAt != nil || n.Count != 2 {
		t.Errorf("the gated leaf's notice after a re-stall = %+v, want live again with count 2", n)
	}
	if c := strings.Count(buf.String(), "not fetching leaf"); c != 2 {
		t.Errorf("per-leaf disk-gate WARN count after a re-stall = %d, want 2", c)
	}
}

// Every leaf gated: each leaf carries its own notice, there is no daemon-wide
// twin beside them, and the machine-wide "stays idle" WARN still fires once.
// When one leaf's gate opens, only that leaf's notice ends.
func TestTB70_EveryLeafGatedRaisesOneNoticePerLeafAndNoDaemonWideTwin(t *testing.T) {
	d, buf, _ := tb70Daemon(t, []*lettucev1.LeafInfo{
		tb70Leaf("leaf-grep", "extract2-student-crowd-v1", 15000),
		tb70Leaf("leaf-f13", "extract2-student-crowd-f13", 12000),
	})

	for i := 0; i < 3; i++ {
		if d.shouldFetch() {
			t.Fatal("shouldFetch = true, want false — both leafs exceed the allowance")
		}
	}

	got := diskGateNotices(d.notices)
	if len(got) != 2 {
		t.Fatalf("got %d disk_gate_blocked notices with two gated leafs, want exactly 2 (one per leaf): %+v", len(got), got)
	}
	for _, id := range []string{"leaf-grep", "leaf-f13"} {
		if n, ok := got[id]; !ok || n.ResolvedAt != nil {
			t.Errorf("leaf %s: want its own live notice, got %+v", id, got)
		}
	}
	if n, ok := got[""]; ok {
		t.Errorf("a daemon-wide disk_gate_blocked notice was raised beside the per-leaf ones: %+v", n)
	}
	if c := strings.Count(buf.String(), "not fetching work"); c != 1 {
		t.Errorf("machine-wide WARN count across 3 polls = %d, want exactly 1", c)
	}

	// 12 GB = 12,288 MB covers the f13 leaf and not the 15,000 MB one.
	d.cfg.ResourceLimits.MaxDiskGB = 12
	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false after the allowance covers one leaf")
	}
	got = diskGateNotices(d.notices)
	if n := got["leaf-f13"]; n.ResolvedAt == nil {
		t.Errorf("the leaf whose gate opened still has a live notice: %+v", n)
	}
	if n := got["leaf-grep"]; n.ResolvedAt != nil {
		t.Errorf("the leaf that is still gated had its notice ended by the other leaf clearing: %+v", n)
	}
	if !strings.Contains(buf.String(), "disk space recovered") {
		t.Errorf("expected the machine-wide clear to be logged once fetching resumed; log: %s", buf.String())
	}
}

// The data-dir floor tripping and recovering is a daemon-wide episode. It
// must not end the stall of a leaf whose own gate still refuses.
func TestTB70_FloorStallDoesNotEndALeafsOwnStall(t *testing.T) {
	d, _, lim := tb70Daemon(t, []*lettucev1.LeafInfo{
		tb70Leaf("leaf-grep", "extract2-student-crowd-v1", 15000),
		tb70Leaf("leaf-bb", "beyblade-arena", 1024),
	})

	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false, want true — the Beyblade leaf fits")
	}
	if n := diskGateNotices(d.notices)["leaf-grep"]; n.ResolvedAt != nil || n.Leaf != "leaf-grep" {
		t.Fatalf("want a live per-leaf notice for the gated leaf first, got %+v", n)
	}

	// The whole volume drops below the floor: nothing runs.
	lim.availMB = DiskFloorMB - 1
	if d.shouldFetch() {
		t.Fatal("shouldFetch = true below the floor")
	}
	got := diskGateNotices(d.notices)
	if n, ok := got[""]; !ok || n.ResolvedAt != nil {
		t.Fatalf("want a live daemon-wide notice for the floor stall, got %+v", got)
	}
	if n := got["leaf-grep"]; n.ResolvedAt != nil {
		t.Errorf("the floor stall ended the gated leaf's own notice: %+v", n)
	}

	// The floor recovers: the daemon-wide episode ends, the leaf's goes on.
	lim.availMB = 1 << 30
	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false after the floor recovered")
	}
	got = diskGateNotices(d.notices)
	if n := got[""]; n.ResolvedAt == nil {
		t.Errorf("the daemon-wide floor notice is still live after the floor recovered: %+v", n)
	}
	if n := got["leaf-grep"]; n.ResolvedAt != nil {
		t.Errorf("the floor recovering ended the still-gated leaf's notice: %+v", n)
	}
}

// A gated leaf that leaves the enabled set (disabled here, or retired by its
// head) is no longer held back by its gate: its stall ends.
func TestTB70_GatedLeafLeavingTheCatalogEndsItsStall(t *testing.T) {
	d, buf, _ := tb70Daemon(t, []*lettucev1.LeafInfo{
		tb70Leaf("leaf-grep", "extract2-student-crowd-v1", 15000),
		tb70Leaf("leaf-bb", "beyblade-arena", 1024),
	})
	mc := d.multiClient.Servers()[0].Client.(*mockClient)

	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false, want true — the Beyblade leaf fits")
	}
	if n := diskGateNotices(d.notices)["leaf-grep"]; n.ResolvedAt != nil || n.Leaf != "leaf-grep" {
		t.Fatalf("want a live per-leaf notice for the gated leaf first, got %+v", n)
	}

	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{Leafs: []*lettucev1.LeafInfo{tb70Leaf("leaf-bb", "beyblade-arena", 1024)}}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("re-seed leaf cache: %v", err)
	}
	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false after the gated leaf left the catalog")
	}
	if n := diskGateNotices(d.notices)["leaf-grep"]; n.ResolvedAt == nil {
		t.Errorf("the departed leaf's notice is still live: %+v", n)
	}
	if !strings.Contains(buf.String(), "no longer enabled here") {
		t.Errorf("expected the departure to be logged; log: %s", buf.String())
	}
}

// ResolveDaemonWide ends the head-less, leaf-less notice of a code and no
// other — Resolve's empty arguments are wildcards, which is the wrong tool
// when the same code also has per-leaf forms that must outlive it.
func TestNoticeLog_ResolveDaemonWideLeavesPerLeafNoticesLive(t *testing.T) {
	l := NewNoticeLog()
	l.Notify(NoticeWarn, "disk_gate_blocked", "leaf", "head-a", "leaf-1")
	l.Notify(NoticeWarn, "disk_gate_blocked", "floor", "", "")
	l.Notify(NoticeWarn, "disk_gate_blocked", "head", "head-a", "")
	l.Notify(NoticeWarn, "no_work", "other code", "", "")

	if got := l.ResolveDaemonWide("disk_gate_blocked"); got != 1 {
		t.Fatalf("ResolveDaemonWide = %d, want 1", got)
	}
	notices, _ := l.Since(0)
	for _, n := range notices {
		daemonWide := n.Code == "disk_gate_blocked" && n.Head == "" && n.Leaf == ""
		if daemonWide != (n.ResolvedAt != nil) {
			t.Errorf("notice %+v: resolved = %v, want %v", n, n.ResolvedAt != nil, daemonWide)
		}
	}
	if got := l.ResolveDaemonWide("disk_gate_blocked"); got != 0 {
		t.Errorf("second ResolveDaemonWide = %d, want 0 (nothing live)", got)
	}
}
