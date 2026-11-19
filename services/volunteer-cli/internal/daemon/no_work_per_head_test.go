package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// The "connected but getting no work" diagnostic, per head.
//
// The reproduction: a volunteer attached to three heads, two of which answer
// every request with "this account already has a result on every task" and a
// 30-second retry delay, and one older head that answers with nothing and no
// reason. After the first round only the older head was contactable, so most
// rounds asked it alone, none of them explained, and five of them raised the
// machine-wide warning "the attached heads have no units matching this
// machine right now (9 attached leaf(s) …)", advice about runtimes, disk space
// and the schedule, beside the other two heads' own lines. It named no head,
// and it lasted until work arrived, even after the older head began to give
// reasons.

// perHeadHost is a volunteer with four idle slots, nothing buffered and
// work_buffer_hours 2, attached to the named heads, head h serving
// leafCounts[h] native leafs named "<head> leaf <i>" (the first leaf of the
// last head is named lastLeafName). reply(h) is head h's answer to every
// request.
func perHeadHost(t *testing.T, names []string, leafCounts []int, lastLeafName string, reply func(h int) *lettucev1.RequestWorkUnitResponse) (*Daemon, []*ServerConnection, *bytes.Buffer) {
	t.Helper()
	var servers []*ServerConnection
	for h, name := range names {
		h := h
		mc := &mockClient{}
		mc.requestWorkUnitFn = func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return reply(h), nil
		}
		srv := &ServerConnection{Client: mc, VolunteerID: "vol-1", Name: name, Available: true}
		srv.Config.GRPCAddress = name + ":443"
		servers = append(servers, srv)
	}
	d := newFetcherTestDaemon(servers)
	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	d.notices = NewNoticeLog()
	d.headStatus = NewHeadStatusTracker()
	d.cfg.WorkBufferHours = 2
	setTestSlots(d.cfg, 4)
	d.cfg.ResourceLimits.MaxCPUCores = 4
	d.cfg.ResourceLimits.MaxMemoryMB = 1 << 20
	d.benchmarkFPOPS = 1
	d.slotManager = NewSlotManager(4, d.logger)
	d.prefetchQueue = NewPreFetchQueue(minWorkBufferQueueDepth, d.logger)
	orig := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { return 0, false }
	t.Cleanup(func() { freeSystemMemoryMB = orig })
	for h, name := range names {
		var leafs []CachedLeafInfo
		weights := map[string]int{}
		for i := 0; i < leafCounts[h]; i++ {
			id := fmt.Sprintf("%s-leaf-%d", name, i)
			label := fmt.Sprintf("%s leaf %d", name, i)
			if h == len(names)-1 && i == 0 && lastLeafName != "" {
				label = lastLeafName
			}
			leafs = append(leafs, CachedLeafInfo{ID: id, Slug: id, Name: label, State: "ACTIVE",
				ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"linux-amd64": "https://example.org/" + id}}})
			weights[id] = 100
		}
		d.leafCache.PopulateForTest(name, &CachedHeadInfo{Name: name, Leafs: leafs, DefaultWeights: weights})
		d.weightedSelector.SetLeafWeights(name, weights)
	}
	return d, servers, &buf
}

// runUntilAsked runs a fresh fetcher on d until ctx ends. The tests end ctx
// from a head's reply once it has answered a set number of requests
// (requestBudget), so the number of rounds is set by the answers, not by the
// wall clock; the round in which the budget runs out is still accounted.
func runUntilAsked(d *Daemon, ctx context.Context) {
	f := NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
	f.backoff = time.Millisecond
	f.maxBackoff = 2 * time.Millisecond
	f.Run(ctx)
}

// requestBudget cancels a context once n requests to the counted head have
// been answered.
type requestBudget struct {
	mu     sync.Mutex
	n      int
	cancel context.CancelFunc
}

func (b *requestBudget) spend() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n--
	if b.n <= 0 && b.cancel != nil {
		b.cancel()
	}
}

const (
	sciosName    = "infra.scios.tech"
	lbryName     = "lbry.science"
	gridlabsName = "gridlabs.example"
)

// liveMachineWideNoWork is the live no_work notices that name no head.
func liveMachineWideNoWork(l *NoticeLog) []Notice {
	var out []Notice
	for _, n := range liveNotices(l, "no_work") {
		if n.Head == "" {
			out = append(out, n)
		}
	}
	return out
}

// threeHeads is the reproduction: two current heads with 6 and 2 leafs that
// have already had a result from this account on everything and ask for a
// 30-second delay, and the older head with one leaf (AI inference) that
// answers with nothing and no reason, at once. budget is spent on every
// request to the older head.
func threeHeads(t *testing.T, budget *requestBudget) (*Daemon, []*ServerConnection, *bytes.Buffer) {
	return perHeadHost(t, []string{sciosName, lbryName, gridlabsName}, []int{6, 2, 1}, "AI inference", func(h int) *lettucev1.RequestWorkUnitResponse {
		if h < 2 {
			return &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED, RetryAfterSeconds: 30}
		}
		budget.spend()
		return &lettucev1.RequestWorkUnitResponse{}
	})
}

// TestNoWorkNotice_OneReasonlessHeadAmongHeadsThatSayWhy is the reproduction
// with its outcome inverted. Twelve rounds ask the older head, the first of
// them the other two heads as well: no machine-wide notice is raised, and the
// older head gets a notice of its own that names it and its one leaf, not the
// nine. Once it starts giving reasons, its notice is resolved without any
// work arriving. Pre-fix: the machine-wide notice "(9 attached leaf(s) …)",
// keyed to no head, still live after the older head gave reasons.
func TestNoWorkNotice_OneReasonlessHeadAmongHeadsThatSayWhy(t *testing.T) {
	budget := &requestBudget{n: 12}
	d, servers, buf := threeHeads(t, budget)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	budget.cancel = cancel
	runUntilAsked(d, ctx)
	cancel()

	if n := len(liveNotices(d.notices, noticeAlreadyContributed)); n != 2 {
		t.Fatalf("already_contributed notices = %d, want one per current head (2); the scenario is not set up", n)
	}
	if mw := liveMachineWideNoWork(d.notices); len(mw) != 0 {
		t.Errorf("machine-wide no_work notice raised beside two heads that said why they send nothing: %q", mw[0].Message)
	}
	live := liveNotices(d.notices, "no_work")
	if len(live) != 1 {
		t.Fatalf("live no_work notices = %d (%+v), want one, for %s", len(live), live, gridlabsName)
	}
	n := live[0]
	if n.Head != gridlabsName || n.Level != NoticeWarn {
		t.Errorf("no_work notice head/level = %q/%q, want %q/warn", n.Head, n.Level, gridlabsName)
	}
	for _, part := range []string{gridlabsName, "AI inference", "gives no reason"} {
		if !strings.Contains(n.Message, part) {
			t.Errorf("no_work notice %q does not say %q", n.Message, part)
		}
	}
	for _, wrong := range []string{"9 attached", "attached heads", "disk space", "schedule", sciosName, lbryName} {
		if strings.Contains(n.Message, wrong) {
			t.Errorf("no_work notice %q says %q: it speaks for more than %s", n.Message, wrong, gridlabsName)
		}
	}
	if c := strings.Count(buf.String(), "connected but getting no work"); c != 1 {
		t.Errorf("'connected but getting no work' logged %d time(s), want once", c)
	}

	// The older head is upgraded and starts saying why. Every head may be
	// asked again; a new fetcher runs (as after a pause and resume).
	for _, srv := range servers {
		srv.NextContactAt = time.Time{}
	}
	upgraded := &requestBudget{n: 3}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	upgraded.cancel = cancel
	servers[2].Client.(*mockClient).requestWorkUnitFn = func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		upgraded.spend()
		return &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED}, nil
	}
	runUntilAsked(d, ctx)
	cancel()
	if live := liveNotices(d.notices, "no_work"); len(live) != 0 {
		t.Errorf("no_work notice still live after %s began to say why it sends nothing, with no work arrived: %q", live[0].Head, live[0].Message)
	}
}

// TestNoWorkNotice_ResolvedWhenItsHeadIsDetached: the older head's notice is
// resolved once the head is no longer attached, without any work arriving.
// Pre-fix the notice, machine-wide, outlived the head.
func TestNoWorkNotice_ResolvedWhenItsHeadIsDetached(t *testing.T) {
	budget := &requestBudget{n: 12}
	d, servers, _ := threeHeads(t, budget)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	budget.cancel = cancel
	runUntilAsked(d, ctx)
	cancel()
	if live := liveNotices(d.notices, "no_work"); len(live) == 0 {
		t.Fatal("setup: no no_work notice raised for the older head")
	}

	// Detached: the fetcher now works with the two current heads only. They
	// are asked again and say why they send nothing.
	for _, srv := range servers[:2] {
		srv.NextContactAt = time.Time{}
	}
	d.multiClient = NewMultiServerClient(servers[:2], d.logger)
	asked := &requestBudget{n: 2}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	asked.cancel = cancel
	for _, srv := range servers[:2] {
		srv.Client.(*mockClient).requestWorkUnitFn = func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			asked.spend()
			return &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED, RetryAfterSeconds: 30}, nil
		}
	}
	runUntilAsked(d, ctx)
	cancel()
	if live := liveNotices(d.notices, "no_work"); len(live) != 0 {
		t.Errorf("no_work notice still live after its head was detached: head %q, %q", live[0].Head, live[0].Message)
	}
}

// TestNoWorkNotice_ReasonsBetweenSilentAnswersEndTheStreak: one head whose
// answers alternate — no reason, then "already contributed" — so no two
// answers without a reason are ever adjacent. The streak is consecutive, as
// its comment always said, so no notice is raised in twenty rounds. Pre-fix
// an explained round neither counted nor ended the streak, and the ten
// reasonless answers raised it.
func TestNoWorkNotice_ReasonsBetweenSilentAnswersEndTheStreak(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	budget := &requestBudget{n: 20}
	d, _, _ := perHeadHost(t, []string{"head-a"}, []int{1}, "", func(int) *lettucev1.RequestWorkUnitResponse {
		mu.Lock()
		calls++
		odd := calls%2 == 1
		mu.Unlock()
		budget.spend()
		if odd {
			return &lettucev1.RequestWorkUnitResponse{}
		}
		return &lettucev1.RequestWorkUnitResponse{NoWorkReason: lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	budget.cancel = cancel
	runUntilAsked(d, ctx)
	cancel()
	mu.Lock()
	got := calls
	mu.Unlock()
	if got < 20 {
		t.Fatalf("head asked %d times, want 20; the test proves nothing", got)
	}
	if n, last := countNoticesByCode(d.notices, "no_work"); n != 0 {
		t.Errorf("no_work notice raised by answers with a reason between every two without one: %q", last.Message)
	}
}
