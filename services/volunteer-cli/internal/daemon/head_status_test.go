package daemon

import (
	"context"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestHeadStatusTracker_SetClearAndGet(t *testing.T) {
	tr := NewHeadStatusTracker()

	if got := tr.Get("h1:443"); got != (HeadStatus{}) {
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
	if got := tr.Get("h2:443"); got != (HeadStatus{}) {
		t.Errorf("contact-only head = %+v, want the zero status", got)
	}
}

func TestHeadStatusTracker_NilAndEmptyAddressAreNoops(t *testing.T) {
	var tr *HeadStatusTracker
	tr.SetVersion("h1:443", "v1")
	tr.MarkUpdateRequired("h1:443")
	tr.MarkContactOK("h1:443")
	if got := tr.Get("h1:443"); got != (HeadStatus{}) {
		t.Errorf("nil tracker Get = %+v, want zero", got)
	}

	live := NewHeadStatusTracker()
	live.MarkUpdateRequired("")
	if got := live.Get(""); got.UpdateRequired {
		t.Errorf("an empty address must not be tracked, got %+v", got)
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
