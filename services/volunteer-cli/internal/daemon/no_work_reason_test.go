package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// A head now says why it sent no work when the cause is this machine's or this
// account's own state. Each stated reason raises its own notice, keyed to the head
// (and the leaf, where the reason is about a leaf's units), and never feeds the
// generic "connected but getting no work" streak, whose advice (runtimes, disk, the
// schedule) is wrong for all of them. On INFLIGHT_CAP the fetcher leaves the head
// alone until one of this machine's copies of its units is done. A head that states
// no reason behaves exactly as before.

// reasonHost is a volunteer with slots slots and work_buffer_hours 2 on heads
// "head-0".."head-(heads-1)", each with one native leaf, ten-minute units (the hours
// target stays out of reach, so the fetcher keeps asking), running units in the
// first slots and queued units in the buffer. Unit i belongs to head i%heads. Every
// head answers every request with reply(head index) — an empty reply unless reply
// says otherwise.
func reasonHost(t *testing.T, heads, slots, running, queued int, reply func(h int) *lettucev1.RequestWorkUnitResponse) (*Daemon, []*mockClient, *bytes.Buffer) {
	t.Helper()
	var servers []*ServerConnection
	var clients []*mockClient
	for h := 0; h < heads; h++ {
		h := h
		mc := &mockClient{}
		mc.requestWorkUnitFn = func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if reply == nil {
				return &lettucev1.RequestWorkUnitResponse{}, nil
			}
			return reply(h), nil
		}
		clients = append(clients, mc)
		srv := &ServerConnection{Client: mc, VolunteerID: "vol-1", Name: fmt.Sprintf("head-%d", h), Available: true}
		srv.Config.GRPCAddress = fmt.Sprintf("head-%d:443", h)
		servers = append(servers, srv)
	}
	d := newFetcherTestDaemon(servers)
	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	d.notices = NewNoticeLog()
	d.headStatus = NewHeadStatusTracker()
	d.cfg.WorkBufferHours = 2
	setTestSlots(d.cfg, slots)
	d.cfg.ResourceLimits.MaxCPUCores = slots // keep the CPU budget out of the picture
	d.cfg.ResourceLimits.MaxMemoryMB = 1 << 20
	d.benchmarkFPOPS = 1
	d.slotManager = NewSlotManager(slots, d.logger)
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	t.Cleanup(func() { freeSystemMemoryMB = orig })
	for h := 0; h < heads; h++ {
		name := fmt.Sprintf("head-%d", h)
		leaf := fmt.Sprintf("leaf-%d", h)
		d.leafCache.PopulateForTest(name, &CachedHeadInfo{
			Name: name,
			Leafs: []CachedLeafInfo{{ID: leaf, Slug: leaf, Name: "Leaf " + fmt.Sprint(h), State: "ACTIVE",
				ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"linux-amd64": "https://example.org/leaf"}}}},
			DefaultWeights: map[string]int{leaf: 100},
		})
		d.weightedSelector.SetLeafWeights(name, map[string]int{leaf: 100})
	}
	unit := func(i int) *runtime.WorkUnit {
		h := i % heads
		return &runtime.WorkUnit{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), LeafID: fmt.Sprintf("leaf-%d", h),
			RscFpopsEst: 608, SourceHead: fmt.Sprintf("head-%d", h)}
	}
	for i := 0; i < running; i++ {
		occupy(d, i, unit(1000+i), nil)
	}
	for i := 0; i < queued; i++ {
		if err := d.prefetchQueue.Push(&PreFetchItem{WU: unit(i), FetchedAt: time.Now()}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	if d.workBufferFull() {
		t.Fatal("buffer reports full; the scenario needs the hours target out of reach")
	}
	return d, clients, &buf
}

// capReply is a head at its in-flight cap: this machine may hold capN copies and holds
// held.
func capReply(capN, held int) *lettucev1.RequestWorkUnitResponse {
	return &lettucev1.RequestWorkUnitResponse{
		NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_INFLIGHT_CAP,
		InflightCap:  int32(capN),
		InflightHeld: int32(held),
	}
}

func totalRequests(clients []*mockClient) int {
	n := 0
	for _, mc := range clients {
		n += mc.getRequestCalls()
	}
	return n
}

// liveNotices returns the unresolved notices with the given code.
func liveNotices(l *NoticeLog, code string) []Notice {
	all, _ := l.Since(0)
	var out []Notice
	for _, n := range all {
		if n.Code == code && n.ResolvedAt == nil {
			out = append(out, n)
		}
	}
	return out
}

// The audit's seven C4 rows, now asserted. Every head holds this machine at a cap of
// ten (running and buffered copies together). A head that SAYS so never lets the
// generic notice fire, at any slot count; a head too old to say so keeps today's
// behavior exactly — the full-buffer guard holds up to five slots per head and cannot above
// it.
func TestNoWorkNotice_InflightCapAtAnySlotCount(t *testing.T) {
	rows := []struct {
		name                          string
		heads, slots, running, queued int
		oldHeadNotice                 int // what a head that gives no reason produces
	}{
		{"64 slots, 3 heads, 30 running + 0 queued", 3, 64, 30, 0, 1},
		{"64 slots, 3 heads, 20 running + 10 queued", 3, 64, 20, 10, 1},
		{"64 slots, 3 heads, 0 running + 30 queued", 3, 64, 0, 30, 1},
		{"6 slots, 1 head, 6 running + 4 queued", 1, 6, 6, 4, 1},
		{"16 slots, 3 heads, 16 running + 14 queued", 3, 16, 16, 14, 1},
		{"5 slots, 1 head, 5 running + 5 queued", 1, 5, 5, 5, 0},
		{"15 slots, 3 heads, 15 running + 15 queued", 3, 15, 15, 15, 0},
	}
	for _, tc := range rows {
		t.Run(tc.name+", head states the cap", func(t *testing.T) {
			held := (tc.running + tc.queued) / tc.heads
			d, clients, buf := reasonHost(t, tc.heads, tc.slots, tc.running, tc.queued,
				func(int) *lettucev1.RequestWorkUnitResponse { return capReply(10, held) })
			runNoWorkFetcher(d)
			if totalRequests(clients) == 0 {
				t.Fatal("no head was asked; the test proves nothing")
			}
			if n, last := countNoticesByCode(d.notices, "no_work"); n != 0 {
				t.Errorf("generic no_work notice raised beside heads that stated their cap: %q", last.Message)
			}
			if strings.Contains(buf.String(), "connected but getting no work") {
				t.Error("generic no-work WARN logged beside heads that stated their cap")
			}
		})
		t.Run(tc.name+", head too old to say", func(t *testing.T) {
			d, _, _ := reasonHost(t, tc.heads, tc.slots, tc.running, tc.queued, nil)
			runNoWorkFetcher(d)
			if n, _ := countNoticesByCode(d.notices, "no_work"); n != tc.oldHeadNotice {
				t.Errorf("generic no_work notices = %d, want %d (unchanged from before the reason)", n, tc.oldHeadNotice)
			}
		})
	}
}

// The filed C4 shape: six slots, one head at its cap of ten (six running, four
// queued). The head is asked once, answers INFLIGHT_CAP, and is not asked again until
// one of this machine's copies of its units is done. Every slot is busy, so the cap
// raises no warning, but `status` has the head's reason.
func TestInflightCap_NextRequestWaitsForACompletion(t *testing.T) {
	d, clients, _ := reasonHost(t, 1, 6, 6, 4, func(int) *lettucev1.RequestWorkUnitResponse { return capReply(10, 10) })
	mc := clients[0]

	runNoWorkFetcher(d)
	if got := mc.getRequestCalls(); got != 1 {
		t.Fatalf("head asked %d times in 300 ms, want 1 — after INFLIGHT_CAP it must wait for a copy to finish", got)
	}
	if n := len(liveNotices(d.notices, noticeInflightCap)); n != 0 {
		t.Errorf("inflight_cap notices = %d, want 0 (every slot is busy; the cap costs nothing)", n)
	}
	nw := d.headStatus.Get("head-0:443").NoWork
	if nw.Reason != noticeInflightCap || !strings.Contains(nw.Message, "lets this machine hold 10 tasks at a time right now, and this machine holds 10") {
		t.Errorf("head's last reason = %+v, want the cap with its figures", nw)
	}

	// Nothing finished: a second fetcher (a pause/resume) still leaves the head alone.
	runNoWorkFetcher(d)
	if got := mc.getRequestCalls(); got != 1 {
		t.Fatalf("head asked %d times with nothing finished, want still 1", got)
	}

	// One running unit finishes: the head is asked again.
	release(d, 0)
	runNoWorkFetcher(d)
	if got := mc.getRequestCalls(); got != 2 {
		t.Fatalf("head asked %d times after a copy finished, want 2", got)
	}
}

// The cap wait also ends on its own after capWaitMax, so a stale count or a cap that
// rose without a completion here never parks a head for good.
func TestInflightCap_WaitEndsAtItsTimeLimit(t *testing.T) {
	d, _, _ := reasonHost(t, 1, 1, 1, 0, nil)
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	now := time.Now()
	f.now = func() time.Time { return now }
	head := d.multiClient.Servers()[0]
	resp := capReply(1, 1)
	f.noteNoWorkReason(head, CachedLeafInfo{ID: "leaf-0", Name: "Leaf 0"}, resp)
	if !f.capWaiting(head) {
		t.Fatal("head not held back after INFLIGHT_CAP")
	}
	now = now.Add(capWaitMax - time.Second)
	if !f.capWaiting(head) {
		t.Fatal("head released before capWaitMax with nothing finished")
	}
	now = now.Add(2 * time.Second)
	if f.capWaiting(head) {
		t.Fatal("head still held back after capWaitMax")
	}
}

// Above ten slots per head the cap leaves slots idle with nothing to run: the owner is
// told that, per head, with the head's figures — not that the heads have no units.
func TestInflightCap_IdleSlotsAreToldTheCap(t *testing.T) {
	d, _, _ := reasonHost(t, 3, 64, 30, 0, func(int) *lettucev1.RequestWorkUnitResponse { return capReply(10, 10) })
	runNoWorkFetcher(d)
	caps := liveNotices(d.notices, noticeInflightCap)
	if len(caps) != 3 {
		t.Fatalf("inflight_cap notices = %d, want one per head (3)", len(caps))
	}
	heads := map[string]bool{}
	for _, n := range caps {
		heads[n.Head] = true
		if n.Level != NoticeWarn {
			t.Errorf("%s: level %q, want warn (slots are idle)", n.Head, n.Level)
		}
		if !strings.Contains(n.Message, "so 34 of its 64 slots are idle") || !strings.Contains(n.Message, "This is the head's limit, not a setting on your computer.") {
			t.Errorf("%s: message %q", n.Head, n.Message)
		}
	}
	if !heads["head-0"] || !heads["head-1"] || !heads["head-2"] {
		t.Errorf("notices name heads %v, want head-0..head-2", heads)
	}
	if n, last := countNoticesByCode(d.notices, "no_work"); n != 0 {
		t.Errorf("generic no_work notice raised: %q", last.Message)
	}
}

// Each other reason raises its own notice at its level — a warning for the account's
// standing and the speed on record, information, one per head, for "already
// contributed" and "bench cooldown" even with every slot idle — and the generic notice
// never fires however often the head is asked.
func TestNoWorkReason_EachReasonHasItsOwnNotice(t *testing.T) {
	end := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		resp      *lettucev1.RequestWorkUnitResponse
		code      string
		leaf      string
		level     string
		wantParts []string
	}{
		{"already contributed, slot idle", &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED},
			noticeAlreadyContributed, "", NoticeInfo,
			[]string{"This account already has a result on, or holds a copy of, every task head-0 has ready for Leaf 0.", "New tasks will reach you."}},
		{"bench cooldown, slot idle", &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_BENCH_COOLDOWN},
			noticeBenchCooldown, "", NoticeInfo,
			[]string{"A recent copy of a Leaf 0 task on head-0, run by this account, did not finish", "Nothing to change unless it keeps happening."}},
		{"account benched until a time", &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ACCOUNT_BENCHED, BenchedUntilUnix: end.Unix()},
			noticeAccountBenched, "", NoticeWarn,
			[]string{"head-0 has paused sending work to this account until 2026-10-01 12:30 UTC. It resumes on its own."}},
		{"account benched indefinitely", &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ACCOUNT_BENCHED},
			noticeAccountBenched, "", NoticeWarn,
			[]string{"head-0 has paused sending work to this account. No end time is set: it lasts until the head's operator lifts it."}},
		{"infeasible deadline", &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_INFEASIBLE_DEADLINE, DeadlineSeconds: 600, EstimatedSeconds: 1000},
			noticeInfeasibleDeadline, "leaf-0", NoticeWarn,
			[]string{"Leaf 0's tasks on head-0 must finish within 10 minutes. At the speed head-0 has on record for your account, one would take about 17 minutes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, clients, buf := reasonHost(t, 1, 1, 0, 0, func(int) *lettucev1.RequestWorkUnitResponse { return tc.resp })
			runNoWorkFetcher(d)
			if got := totalRequests(clients); got < noWorkWarnThreshold {
				t.Fatalf("head asked %d times — fewer than the generic streak needs, so the test proves nothing", got)
			}
			if n, last := countNoticesByCode(d.notices, "no_work"); n != 0 {
				t.Errorf("generic no_work notice raised beside a stated reason: %q", last.Message)
			}
			if strings.Contains(buf.String(), "connected but getting no work") {
				t.Error("generic no-work WARN logged beside a stated reason")
			}
			live := liveNotices(d.notices, tc.code)
			if len(live) != 1 {
				t.Fatalf("%s notices = %d, want 1", tc.code, len(live))
			}
			n := live[0]
			if n.Head != "head-0" || n.Leaf != tc.leaf || n.Level != tc.level {
				t.Errorf("notice head/leaf/level = %q/%q/%q, want head-0/%q/%q", n.Head, n.Leaf, n.Level, tc.leaf, tc.level)
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(n.Message, part) {
					t.Errorf("message %q lacks %q", n.Message, part)
				}
			}
			if strings.Contains(n.Message, "lettuce-volunteer") {
				t.Errorf("message %q names a terminal command; the app shows it as it is", n.Message)
			}
			if nw := d.headStatus.Get("head-0:443").NoWork; nw.Reason != tc.code || nw.Message != n.Message {
				t.Errorf("head's last reason = %+v, want %s with the notice's wording", nw, tc.code)
			}
		})
	}
}

// Already contributed is information with every slot busy too.
func TestNoWorkReason_InfoWhileEverySlotIsBusy(t *testing.T) {
	d, _, _ := reasonHost(t, 1, 1, 1, 0, func(int) *lettucev1.RequestWorkUnitResponse {
		return &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED}
	})
	runNoWorkFetcher(d)
	live := liveNotices(d.notices, noticeAlreadyContributed)
	if len(live) != 1 || live[0].Level != NoticeInfo {
		t.Fatalf("already_contributed notices = %+v, want one at info", live)
	}
}

// Reasons are kept per head: one head's reason does not hide another's, and work from
// one head ends only its own.
func TestNoWorkReason_KeyedPerHead(t *testing.T) {
	d, _, _ := reasonHost(t, 2, 1, 0, 0, nil)
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	servers := d.multiClient.Servers()
	h0, h1 := servers[0], servers[1]
	leaf0 := CachedLeafInfo{ID: "leaf-0", Name: "Leaf 0"}
	leaf1 := CachedLeafInfo{ID: "leaf-1", Name: "Leaf 1"}

	if !f.noteNoWorkReason(h0, leaf0, &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED}) {
		t.Fatal("a stated reason reported as unexplained")
	}
	f.noteNoWorkReason(h1, leaf1, &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED})
	if n := len(liveNotices(d.notices, noticeAlreadyContributed)); n != 2 {
		t.Fatalf("already_contributed notices = %d, want one per head (2)", n)
	}

	f.noteHeadServed(h0, leaf0)
	live := liveNotices(d.notices, noticeAlreadyContributed)
	if len(live) != 1 || live[0].Head != "head-1" {
		t.Fatalf("after head-0 sent work, live notices = %+v, want head-1's only", live)
	}
	if nw := d.headStatus.Get("head-0:443").NoWork; nw.Reason != "" {
		t.Errorf("head-0's reason kept after it sent work: %+v", nw)
	}
	if nw := d.headStatus.Get("head-1:443").NoWork; nw.Reason != noticeAlreadyContributed {
		t.Errorf("head-1's reason = %+v, want already_contributed", nw)
	}

	// An answer without a reason for the same leaf ends that leaf's reason.
	if f.noteNoWorkReason(h1, leaf1, &lettucev1.RequestWorkUnitResponse{}) {
		t.Fatal("an empty reply with no reason reported as explained")
	}
	if n := len(liveNotices(d.notices, noticeAlreadyContributed)); n != 0 {
		t.Errorf("already_contributed notices = %d after head-1 answered without a reason, want 0", n)
	}
	if nw := d.headStatus.Get("head-1:443").NoWork; nw.Reason != "" {
		t.Errorf("head-1's reason kept after it answered without one: %+v", nw)
	}
}

// A reason value this build does not know (a newer head) is treated as no reason.
func TestNoWorkReason_UnknownValueIsNoReason(t *testing.T) {
	d, _, _ := reasonHost(t, 1, 1, 0, 0, nil)
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	if f.noteNoWorkReason(d.multiClient.Servers()[0], CachedLeafInfo{ID: "leaf-0"}, &lettucev1.RequestWorkUnitResponse{NoWorkReason: 99}) {
		t.Fatal("an unknown reason value reported as explained")
	}
}

// The wording's arithmetic and grammar.
func TestNoWorkReasonMessage_Wording(t *testing.T) {
	for _, tc := range []struct {
		name        string
		leaf        string
		resp        *lettucev1.RequestWorkUnitResponse
		idle, slots int
		want        string
	}{
		{"cap, no idle slot", "L", capReply(10, 10), 0, 6,
			"lbry lets this machine hold 10 tasks at a time right now, and this machine holds 10. More will come as these finish. This is the head's limit, not a setting on your computer."},
		{"cap, one idle slot", "L", capReply(1, 1), 1, 2,
			"lbry lets this machine hold 1 task at a time right now, and this machine holds 1, so 1 of its 2 slots is idle. More will come as these finish. This is the head's limit, not a setting on your computer."},
		{"contributed, any-leaf request", "", &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED}, 0, 1,
			"This account already has a result on, or holds a copy of, every task lbry has ready for this machine. Each task needs results from different volunteers, so these are waiting for others. New tasks will reach you."},
		{"contributed, one leaf", "L", &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED}, 3, 4,
			"This account already has a result on, or holds a copy of, every task lbry has ready for L. Each task needs results from different volunteers, so these are waiting for others. New tasks will reach you."},
		{"deadline without an estimate", "L", &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_INFEASIBLE_DEADLINE, DeadlineSeconds: 3 * 3600}, 0, 1,
			"L's tasks on lbry must finish within 3 hours, and at the speed lbry has on record for your account one would not, so the head gives them to faster machines. Nothing to change on your side."},
		{"deadline, hours and minutes", "L", &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_INFEASIBLE_DEADLINE, DeadlineSeconds: 6 * 3600, EstimatedSeconds: 7*3600 + 5*60}, 0, 1,
			"L's tasks on lbry must finish within 6 hours. At the speed lbry has on record for your account, one would take about 7 h 5 min, so the head gives them to faster machines. Nothing to change on your side."},
		{"no reason", "L", &lettucev1.RequestWorkUnitResponse{}, 0, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := noWorkReasonMessage("lbry", tc.leaf, tc.resp, tc.idle, tc.slots); got != tc.want {
				t.Errorf("message =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// A head holding this machine at its cap has work for it, so the machine is not
// "getting no work" even while another head answers empty without a reason: the
// generic notice would tell the owner the heads have no units for the machine.
func TestNoWorkNotice_NotRaisedWhileAHeadHoldsTheCap(t *testing.T) {
	d, clients, _ := reasonHost(t, 2, 12, 12, 0, func(h int) *lettucev1.RequestWorkUnitResponse {
		if h == 0 {
			return capReply(6, 6)
		}
		return &lettucev1.RequestWorkUnitResponse{}
	})
	runNoWorkFetcher(d)
	if got := clients[1].getRequestCalls(); got < noWorkWarnThreshold {
		t.Fatalf("the empty head was asked %d times — fewer than the streak needs, so the test proves nothing", got)
	}
	if n, last := countNoticesByCode(d.notices, "no_work"); n != 0 {
		t.Errorf("generic no_work notice raised while head-0 holds this machine at its cap: %q", last.Message)
	}
}

// Work from a head ends what its earlier empty answers said, through the real request
// path: the reason's notice is resolved and `status` stops showing it.
func TestNoWorkReason_EndsWhenTheHeadSendsWork(t *testing.T) {
	calls := 0
	d, _, _ := reasonHost(t, 1, 1, 0, 0, func(int) *lettucev1.RequestWorkUnitResponse {
		calls++
		if calls == 1 {
			return &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED}
		}
		return &lettucev1.RequestWorkUnitResponse{Assignments: []*lettucev1.WorkUnitAssignment{{
			WorkUnitId: "00000000-0000-4000-8000-000000000777", LeafId: "leaf-0", Runtime: "native",
			InputData: []byte("input"), ExecutionSpec: &lettucev1.ExecutionSpec{},
		}}}
	})
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	head := d.multiClient.Servers()[0]
	leaf := CachedLeafInfo{ID: "leaf-0", Slug: "leaf-0", Name: "Leaf 0"}

	if pushed, _ := f.requestAndBuffer(context.Background(), head, leaf, []string{leaf.ID}, nil, 1); pushed != 0 {
		t.Fatalf("first answer buffered %d units, want 0", pushed)
	}
	if n := len(liveNotices(d.notices, noticeAlreadyContributed)); n != 1 {
		t.Fatalf("already_contributed notices after the empty answer = %d, want 1", n)
	}
	if pushed, _ := f.requestAndBuffer(context.Background(), head, leaf, []string{leaf.ID}, nil, 1); pushed != 1 {
		t.Fatalf("second answer buffered %d units, want 1", pushed)
	}
	if n := len(liveNotices(d.notices, noticeAlreadyContributed)); n != 0 {
		t.Errorf("already_contributed notices after the head sent work = %d, want 0", n)
	}
	if nw := d.headStatus.Get("head-0:443").NoWork; nw.Reason != "" {
		t.Errorf("head's reason kept after it sent work: %+v", nw)
	}
}

// A cap warning says slots are idle; once no slot is idle it no longer applies, and
// work arriving from any head ends it.
func TestInflightCap_WarningEndsOnceNoSlotIsIdle(t *testing.T) {
	d, _, _ := reasonHost(t, 1, 2, 1, 0, nil)
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	head := d.multiClient.Servers()[0]
	f.noteNoWorkReason(head, CachedLeafInfo{ID: "leaf-0", Name: "Leaf 0"}, capReply(1, 1))
	if n := len(liveNotices(d.notices, noticeInflightCap)); n != 1 {
		t.Fatalf("inflight_cap notices with a slot idle = %d, want 1", n)
	}
	occupy(d, 1, &runtime.WorkUnit{ID: "00000000-0000-4000-8000-000000000888", LeafID: "leaf-x", SourceHead: "elsewhere"}, nil)
	f.noteWorkArrived()
	if n := len(liveNotices(d.notices, noticeInflightCap)); n != 0 {
		t.Errorf("inflight_cap notices once every slot is busy = %d, want 0", n)
	}
}
