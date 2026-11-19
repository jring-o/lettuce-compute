package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestFetcher_TooLargeWorkReplyIsNotReadAsLoadShedding: gRPC reports a reply
// too large to receive as ResourceExhausted, the code a head sheds load with,
// and the fetcher read it that way ("head shed load"), then asked for the same
// batch again after the backoff, to the same refusal. Now it says the reply
// was too large, still backs off locally (so a reply that cannot shrink is not
// asked for again at once), keeps the head available (no reconnect backoff),
// and asks for at most half as many units next time.
func TestFetcher_TooLargeWorkReplyIsNotReadAsLoadShedding(t *testing.T) {
	var asked []int32
	mc := &mockClient{
		requestWorkUnitFn: func(_ context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			asked = append(asked, req.MaxAssignments)
			return nil, status.Error(codes.ResourceExhausted, "grpc: received message larger than max (140000000 vs. 134217728)")
		},
	}
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	d := newFetcherTestDaemon(servers)
	var logs bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	queue := NewPreFetchQueue(minWorkBufferQueueDepth, d.logger)
	f := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	f.batchSizeFn = func(CachedLeafInfo, float64, float64) int32 { return 64 }
	f.rateLimitBackoff = 100 * time.Millisecond
	base := time.Now()
	f.now = func() time.Time { return base }

	leaf := CachedLeafInfo{ID: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"}
	if pushed, stop := f.requestAndBuffer(context.Background(), servers[0], leaf, []string{leaf.ID}, nil, 1); pushed != 0 || !stop {
		t.Fatalf("too-large reply: pushed=%d stop=%v, want nothing pushed and this head done for the round", pushed, stop)
	}

	out := logs.String()
	if strings.Contains(out, "head shed load") {
		t.Errorf("a reply too large to receive was logged as the head shedding load:\n%s", out)
	}
	if !strings.Contains(out, "work reply was too large to receive") {
		t.Errorf("no line says the reply was too large:\n%s", out)
	}

	head := servers[0]
	if !head.Available || head.Backoff != 0 {
		t.Errorf("head marked unavailable (available=%v, backoff=%v); a too-large reply is not a connection fault", head.Available, head.Backoff)
	}
	if delay := head.NextContactAt.Sub(base); delay < 80*time.Millisecond || delay > 120*time.Millisecond {
		t.Errorf("local backoff = %v, want the rate-limit backoff of %v ±20%%", delay, f.rateLimitBackoff)
	}

	f.requestAndBuffer(context.Background(), head, leaf, []string{leaf.ID}, nil, 1)
	if len(asked) != 2 {
		t.Fatalf("%d requests, want 2", len(asked))
	}
	if asked[0] != 64 || asked[1] != 32 {
		t.Errorf("asks = %v, want [64 32]: the next ask after a too-large reply is at most half", asked)
	}

	// The head's own load shedding is unchanged.
	logs.Reset()
	mc.requestWorkUnitFn = func(context.Context, *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
		return nil, status.Error(codes.ResourceExhausted, "dispatch overloaded; back off and retry")
	}
	f.requestAndBuffer(context.Background(), head, leaf, []string{leaf.ID}, nil, 1)
	if !strings.Contains(logs.String(), "head shed load") {
		t.Errorf("the head's load shedding is no longer reported as such:\n%s", logs.String())
	}
}
