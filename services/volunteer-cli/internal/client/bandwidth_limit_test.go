package client

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"

	"github.com/lettuce-compute/volunteer-cli/internal/netlimit"
)

// submitRecorder is a head that accepts every result and reports each payload's
// size as it arrives.
type submitRecorder struct {
	lettucev1.UnimplementedVolunteerServiceServer
	got chan int
}

func (s *submitRecorder) SubmitResult(_ context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
	s.got <- len(req.GetOutputData())
	return &lettucev1.SubmitResultResponse{Accepted: true}, nil
}

// A result upload to a head is held to resource_limits.max_bandwidth_mbps, and
// the per-RPC deadline written for an unpaced link is widened so the paced
// upload is not cut off. The client is built the production way (New), so the
// pacing comes from the credentials it installs, not from the test.
func TestSubmitResult_PacedToTheBandwidthLimitWithoutTimingOut(t *testing.T) {
	const mbps = 2 // 250,000 bytes a second
	netlimit.SetMbps(mbps)
	t.Cleanup(func() { netlimit.SetMbps(0) })

	head := &submitRecorder{got: make(chan int, 1)}
	addr, stop := startMockServer(t, head)
	defer stop()

	// One second is far less than the upload needs at 2 Mbps; the deadline
	// must grow with the payload for the call to succeed.
	c, err := New(ClientConfig{ServerURL: addr, Insecure: true, RequestTimeout: time.Second}, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	output := bytes.Repeat([]byte("r"), 600_000)
	start := time.Now()
	resp, err := c.SubmitResult(context.Background(), &lettucev1.SubmitResultRequest{
		WorkUnitId: "wu-paced",
		OutputData: output,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("SubmitResult under a %d Mbps limit failed after %s: %v (a paced upload must not time out)", mbps, elapsed, err)
	}
	if !resp.GetAccepted() {
		t.Fatal("head did not accept the result")
	}
	if n := <-head.got; n != len(output) {
		t.Fatalf("head received %d bytes of output, want %d", n, len(output))
	}

	// Everything past the bucket's opening burst waits for tokens.
	floor := time.Duration(float64(len(output)-netlimit.Upload.Burst()) / float64(netlimit.BytesPerSecond(mbps)) * float64(time.Second))
	if elapsed < floor {
		t.Errorf("600 kB result uploaded in %s under a %d Mbps limit; the limit allows no less than %s", elapsed, mbps, floor)
	}
}

// With no limit set the deadline is the configured request timeout, exactly as
// before; with one set it grows by the time the largest accepted reply takes.
func TestRPCDeadline_WidenedOnlyUnderALimit(t *testing.T) {
	c := &Client{requestTimeout: 30 * time.Second}

	netlimit.SetMbps(0)
	ctx, cancel := c.rpcCtxSized(context.Background(), &lettucev1.SubmitResultRequest{OutputData: make([]byte, 50_000_000)})
	deadline, _ := ctx.Deadline()
	cancel()
	if got := time.Until(deadline); got > 30*time.Second || got < 29*time.Second {
		t.Errorf("unlimited: deadline %s away, want the configured 30s", got.Round(time.Second))
	}

	netlimit.SetMbps(10) // 1.25 MB/s
	t.Cleanup(func() { netlimit.SetMbps(0) })
	ctx, cancel = c.rpcCtxSized(context.Background(), &lettucev1.SubmitResultRequest{OutputData: make([]byte, 50_000_000)})
	deadline, _ = ctx.Deadline()
	cancel()
	// 50 MB up takes 40 s and a 4 MiB reply about 3.4 s at 10 Mbps; with the
	// margin that is over two minutes on top of the 30 s.
	if got := time.Until(deadline); got < 30*time.Second+2*(40*time.Second+3*time.Second) {
		t.Errorf("10 Mbps: deadline %s away is too short for a 50 MB upload", got.Round(time.Second))
	}
}
