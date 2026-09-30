package daemon

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestHeadStatusTracker_SetClearAndGet(t *testing.T) {
	tr := NewHeadStatusTracker()

	if got := tr.Get("h1:443"); !reflect.DeepEqual(got, HeadStatus{}) {
		t.Fatalf("unknown head = %+v, want the zero status", got)
	}

	tr.SetVersion("h1:443", "v0.9.0")
	tr.MarkUpdateRequired("h1:443")
	got := tr.Get("h1:443")
	if got.HeadVersion != "v0.9.0" || !got.UpdateRequired {
		t.Fatalf("after set+mark = %+v, want version v0.9.0 and update_required", got)
	}

	// A version update must not disturb the flag, and vice versa.
	tr.SetVersion("h1:443", "v0.9.1")
	if got := tr.Get("h1:443"); got.HeadVersion != "v0.9.1" || !got.UpdateRequired {
		t.Errorf("after version change = %+v, want v0.9.1 with update_required still set", got)
	}

	tr.MarkContactOK("h1:443")
	if got := tr.Get("h1:443"); got.UpdateRequired || got.HeadVersion != "v0.9.1" {
		t.Errorf("after contact = %+v, want update_required cleared and version kept", got)
	}

	// Contact with a head never marked must not create an entry or panic.
	tr.MarkContactOK("h2:443")
	if got := tr.Get("h2:443"); !reflect.DeepEqual(got, HeadStatus{}) {
		t.Errorf("contact-only head = %+v, want the zero status", got)
	}
}

func TestHeadStatusTracker_NilAndEmptyAddressAreNoops(t *testing.T) {
	var tr *HeadStatusTracker
	tr.SetVersion("h1:443", "v1")
	tr.MarkUpdateRequired("h1:443")
	tr.MarkContactOK("h1:443")
	if got := tr.Get("h1:443"); !reflect.DeepEqual(got, HeadStatus{}) {
		t.Errorf("nil tracker Get = %+v, want zero", got)
	}

	live := NewHeadStatusTracker()
	live.MarkUpdateRequired("")
	if got := live.Get(""); got.UpdateRequired {
		t.Errorf("an empty address must not be tracked, got %+v", got)
	}
}

// The head's line for the leaves it has nothing new for: every such leaf named,
// compactly past three; a leaf that answers otherwise drops out; a reason that is
// the volunteer's business (here the account benched) is shown instead while it is
// the head's latest; work from the head clears the line.
func TestHeadStatusTracker_OneLineForTheWaitingLeaves(t *testing.T) {
	tr := NewHeadStatusTracker()
	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	set := func(reason, leafID, label string, minute int) {
		tr.SetNoWork("h:443", "scios", HeadNoWork{Reason: reason, Message: "said: " + reason, Leaf: label, LeafID: leafID,
			At: base.Add(time.Duration(minute) * time.Minute)})
	}
	for i, l := range []string{"f14", "f13", "V1 CPU", "V1 GPU"} {
		set(noticeAlreadyContributed, "id-"+l, l, i)
	}
	nw := tr.Get("h:443").NoWork
	if nw.Reason != noticeAlreadyContributed || !reflect.DeepEqual(nw.Leaves, []string{"V1 CPU", "V1 GPU", "f13", "f14"}) ||
		nw.Leaf != "" || !nw.At.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("line = %+v, want all four leaves, at the latest answer", nw)
	}
	if !strings.Contains(nw.Message, "every task scios has ready for V1 CPU, V1 GPU and 2 more.") {
		t.Errorf("message = %q, want the leaves named compactly past three", nw.Message)
	}

	// f14 answers without a reason; V1 GPU's answer becomes a cooldown.
	tr.ClearNoWorkFor("h:443", "id-f14")
	set(noticeBenchCooldown, "id-V1 GPU", "V1 GPU", 4)
	nw = tr.Get("h:443").NoWork
	if nw.Reason != noticeBenchCooldown || !reflect.DeepEqual(nw.Leaves, []string{"V1 CPU", "V1 GPU", "f13"}) {
		t.Fatalf("line = %+v, want three leaves under bench_cooldown", nw)
	}
	for _, part := range []string{"every task scios has ready for V1 CPU and f13.", "A recent copy of a V1 GPU task on scios"} {
		if !strings.Contains(nw.Message, part) {
			t.Errorf("message %q lacks %q", nw.Message, part)
		}
	}
	if got := tr.WaitingMessage("h:443", noticeAlreadyContributed); !strings.HasPrefix(got, "This account already has a result on") || strings.Contains(got, "V1 GPU") {
		t.Errorf("already-contributed line alone = %q", got)
	}

	// The account benched: that is the head's word until something else is said.
	set(noticeAccountBenched, "id-f13", "", 5)
	if nw = tr.Get("h:443").NoWork; nw.Reason != noticeAccountBenched || nw.Message != "said: account_benched" || nw.Leaves != nil {
		t.Fatalf("while benched the line = %+v, want the bench", nw)
	}
	set(noticeAlreadyContributed, "id-V1 CPU", "V1 CPU", 6)
	if nw = tr.Get("h:443").NoWork; nw.Reason != noticeBenchCooldown || !reflect.DeepEqual(nw.Leaves, []string{"V1 CPU", "V1 GPU"}) {
		t.Fatalf("after the bench the line = %+v, want V1 CPU and V1 GPU (f13's answer was the bench)", nw)
	}

	tr.ClearNoWork("h:443")
	if nw = tr.Get("h:443").NoWork; nw.Reason != "" || nw.Leaves != nil {
		t.Errorf("after the head sent work the line = %+v, want none", nw)
	}
	if got := tr.WaitingMessage("h:443", noticeBenchCooldown); got != "" {
		t.Errorf("cooldown line kept after the head sent work: %q", got)
	}
}

// The work path is the second place a head can reject this build as too old
// (the first is registration at start-up). The rejection must set the head's
// update_required flag and emit an update_required notice; a later successful
// request to the same head must clear the flag.
func TestFetcher_TooOldRejectionSetsUpdateRequiredAndSuccessClearsIt(t *testing.T) {
	tooOld := true
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if tooOld {
				return nil, status.Error(codes.FailedPrecondition, "volunteer build is too old for this head; please update")
			}
			return &lettucev1.RequestWorkUnitResponse{}, nil // no work, healthy head
		},
	}
	const addr = "head-a.example.org:443"
	servers := []*ServerConnection{
		{Client: mc, VolunteerID: "vol-1", Name: "head-a", Available: true,
			Config: config.ServerConfig{GRPCAddress: addr, Name: "head-a"}},
	}
	d := newFetcherTestDaemon(servers)
	d.notices = NewNoticeLog()
	d.headStatus = NewHeadStatusTracker()
	queue := NewPreFetchQueue(4, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 10 * time.Millisecond
	fetcher.maxBackoff = 50 * time.Millisecond

	if _, err := fetcher.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	if st := d.headStatus.Get(addr); !st.UpdateRequired {
		t.Fatalf("after a too-old rejection, head status = %+v; want update_required", st)
	}
	notices, _ := d.notices.Since(0)
	if len(notices) != 1 || notices[0].Code != "update_required" || notices[0].Head != "head-a" {
		t.Fatalf("notices after rejection = %+v; want one update_required notice for head-a", notices)
	}
	if notices[0].Level != NoticeWarn {
		t.Errorf("update_required level = %q, want %q (mirrors the WARN log site)", notices[0].Level, NoticeWarn)
	}

	// Let the head's reconnect backoff lapse, then a successful reply clears it.
	tooOld = false
	time.Sleep(3 * fetcher.backoff)
	if _, err := fetcher.fetchOne(context.Background()); err != nil {
		t.Fatalf("fetchOne (recovered): %v", err)
	}
	if mc.requestCalls < 2 {
		t.Fatalf("second fetchOne did not reach the head (requestCalls=%d); the backoff test timing is off", mc.requestCalls)
	}
	if st := d.headStatus.Get(addr); st.UpdateRequired {
		t.Errorf("after a successful request, head status = %+v; want update_required cleared", st)
	}
}
