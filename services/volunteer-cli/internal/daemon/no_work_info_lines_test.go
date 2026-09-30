package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// A machine that has done everything its heads have ready is told so once per head,
// as information: "already contributed" and "bench cooldown" ask nothing of the
// volunteer, so they never reach the app's "Needs attention", whatever the idle slots,
// and a head answering them for several leaves is one line naming the leaves, not a
// warning card per leaf with a climbing count.

// infoLinesHost is reasonHost with the given leaves (by name) on each head instead of
// one, every slot idle, and each head answering every request with reply(head, leaf
// id). It returns how often each head was asked for each leaf.
func infoLinesHost(t *testing.T, leaves [][]string, slots int, reply func(h int, leafID string) *lettucev1.RequestWorkUnitResponse) (*Daemon, func(h int, leafID string) int) {
	t.Helper()
	d, clients, _ := reasonHost(t, len(leaves), slots, 0, 0, nil)
	var mu sync.Mutex
	asked := map[string]int{}
	for h, names := range leaves {
		h := h
		head := fmt.Sprintf("head-%d", h)
		var cached []CachedLeafInfo
		weights := map[string]int{}
		for i, name := range names {
			id := fmt.Sprintf("leaf-%d-%d", h, i)
			cached = append(cached, CachedLeafInfo{ID: id, Slug: id, Name: name, State: "ACTIVE",
				ExecutionSpec: &CachedExecutionSpec{Binaries: map[string]string{"linux-amd64": "https://example.org/leaf"}}})
			weights[id] = 100
		}
		d.leafCache.PopulateForTest(head, &CachedHeadInfo{Name: head, Leafs: cached, DefaultWeights: weights})
		d.weightedSelector.SetLeafWeights(head, weights)
		clients[h].requestWorkUnitFn = func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			ids := req.GetLeafIds()
			mu.Lock()
			for _, id := range ids {
				asked[fmt.Sprintf("%d|%s", h, id)]++
			}
			mu.Unlock()
			if len(ids) != 1 {
				return &lettucev1.RequestWorkUnitResponse{}, nil
			}
			return reply(h, ids[0]), nil
		}
	}
	return d, func(h int, leafID string) int {
		mu.Lock()
		defer mu.Unlock()
		return asked[fmt.Sprintf("%d|%s", h, leafID)]
	}
}

func reasonReply(r lettucev1.NoWorkReason) *lettucev1.RequestWorkUnitResponse {
	return &lettucev1.RequestWorkUnitResponse{NoWorkReason: r}
}

// Two heads, three leaves each, every slot idle, every answer "already contributed"
// (the field report: five yellow cards, one per head and leaf, counting up). The ring
// holds no warning for it, one Info notice per head, and each head's status is one
// line naming its three leaves with no terminal command in it.
func TestNoWorkReason_AlreadyContributedIsOneInfoLinePerHead(t *testing.T) {
	leaves := [][]string{
		{"Beyblade Arena (native)", "Beyblade Arena", "Prime Gaps"},
		{"GREP f14 (CPU)", "GREP f13 (CPU)", "GREP V1 (GPU)"},
	}
	d, asked := infoLinesHost(t, leaves, 8, func(int, string) *lettucev1.RequestWorkUnitResponse {
		return reasonReply(lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED)
	})
	runNoWorkFetcher(d)
	for h, names := range leaves {
		for i := range names {
			if n := asked(h, fmt.Sprintf("leaf-%d-%d", h, i)); n < 2 {
				t.Fatalf("head-%d asked %d time(s) for leaf %d — the scenario needs every leaf asked repeatedly", h, n, i)
			}
		}
	}

	all, _ := d.notices.Since(0)
	for _, n := range all {
		if (n.Code == noticeAlreadyContributed || n.Code == noticeBenchCooldown) && n.Level != NoticeInfo {
			t.Errorf("%s notice for %s/%s at %s (count %d), want info: nothing here is the volunteer's to act on",
				n.Code, n.Head, n.Leaf, n.Level, n.Count)
		}
	}
	live := liveNotices(d.notices, noticeAlreadyContributed)
	if len(live) != 2 {
		t.Errorf("live already_contributed notices = %d, want one per head (2)", len(live))
	}
	for _, n := range live {
		if n.Leaf != "" {
			t.Errorf("already_contributed notice keyed to leaf %q on %s, want one per head", n.Leaf, n.Head)
		}
	}

	wantLeaves := []string{
		"Beyblade Arena, Beyblade Arena (native) and Prime Gaps",
		"GREP V1 (GPU), GREP f13 (CPU) and GREP f14 (CPU)",
	}
	for h := range leaves {
		nw := d.headStatus.Get(fmt.Sprintf("head-%d:443", h)).NoWork
		if nw.Reason != noticeAlreadyContributed {
			t.Errorf("head-%d reason = %q, want already_contributed", h, nw.Reason)
		}
		want := fmt.Sprintf("every task head-%d has ready for %s.", h, wantLeaves[h])
		if !strings.Contains(nw.Message, want) {
			t.Errorf("head-%d line = %q, want it to name every leaf: %q", h, nw.Message, want)
		}
		if strings.Contains(nw.Message, "lettuce-volunteer") {
			t.Errorf("head-%d line names a terminal command, which the app shows as it is: %q", h, nw.Message)
		}
	}
}

// A head that has nothing new on one leaf and is holding this account off another
// after a failed copy: both are information, one notice each for the head, and the
// head's line says both. It is not "already contributed" as a whole, because the
// held-back leaf is not one the account has finished.
func TestNoWorkReason_BenchCooldownBesideAlreadyContributedIsOneLine(t *testing.T) {
	d, asked := infoLinesHost(t, [][]string{{"Leaf A", "Leaf B"}}, 4, func(_ int, leafID string) *lettucev1.RequestWorkUnitResponse {
		if leafID == "leaf-0-0" {
			return reasonReply(lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED)
		}
		return reasonReply(lettucev1.NoWorkReason_NO_WORK_REASON_BENCH_COOLDOWN)
	})
	runNoWorkFetcher(d)
	if asked(0, "leaf-0-0") == 0 || asked(0, "leaf-0-1") == 0 {
		t.Fatal("a leaf was never asked; the test proves nothing")
	}

	all, _ := d.notices.Since(0)
	for _, n := range all {
		if (n.Code == noticeAlreadyContributed || n.Code == noticeBenchCooldown) && n.Level != NoticeInfo {
			t.Errorf("%s notice for %s/%s at %s, want info", n.Code, n.Head, n.Leaf, n.Level)
		}
	}
	for _, code := range []string{noticeAlreadyContributed, noticeBenchCooldown} {
		if live := liveNotices(d.notices, code); len(live) != 1 || live[0].Leaf != "" {
			t.Errorf("live %s notices = %+v, want one for the head", code, live)
		}
	}
	nw := d.headStatus.Get("head-0:443").NoWork
	if nw.Reason != noticeBenchCooldown {
		t.Errorf("reason = %q, want bench_cooldown (not everything is done)", nw.Reason)
	}
	for _, part := range []string{"every task head-0 has ready for Leaf A.", "A recent copy of a Leaf B task on head-0, run by this account, did not finish"} {
		if !strings.Contains(nw.Message, part) {
			t.Errorf("line %q lacks %q", nw.Message, part)
		}
	}
}
