package client

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lettuce-compute/volunteer-cli/internal/netlimit"
)

// Replies over gRPC's 4 MiB default receive limit. A head accepts and sends
// messages of up to 128 MiB, and lets a leaf keep checkpoints of up to 100 MB,
// but the client kept gRPC's default: a checkpoint over 4 MiB could be saved
// and never restored, and a work reply over 4 MiB failed with the code a head
// uses to shed load. These tests run the client built the production way (New)
// against an in-process server with the head's own message limits.

// headMaxMsgSize is the head's transport ceiling (its gRPC server options).
const headMaxMsgSize = 128 * 1024 * 1024

// largeReplyHead answers with payloads of the sizes a test sets, and reports
// how far away each call's deadline was when it arrived.
type largeReplyHead struct {
	lettucev1.UnimplementedVolunteerServiceServer
	checkpoint []byte // GetCheckpoint's blob
	units      int    // RequestWorkUnit's assignment count
	inline     []byte // each assignment's inline input
	saved      chan int
	deadlines  chan rpcDeadline
}

type rpcDeadline struct {
	method string
	left   time.Duration
}

func (h *largeReplyHead) note(ctx context.Context, method string) {
	if h.deadlines == nil {
		return
	}
	var left time.Duration
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	h.deadlines <- rpcDeadline{method: method, left: left}
}

func (h *largeReplyHead) GetCheckpoint(ctx context.Context, _ *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
	h.note(ctx, "GetCheckpoint")
	return &lettucev1.GetCheckpointResponse{HasCheckpoint: true, CheckpointData: h.checkpoint, CheckpointSequence: 3}, nil
}

func (h *largeReplyHead) RequestWorkUnit(ctx context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
	h.note(ctx, "RequestWorkUnit")
	resp := &lettucev1.RequestWorkUnitResponse{}
	for i := 0; i < h.units; i++ {
		resp.Assignments = append(resp.Assignments, &lettucev1.WorkUnitAssignment{
			WorkUnitId: fmt.Sprintf("wu-%d", i),
			LeafId:     "leaf-1",
			InputData:  h.inline,
		})
	}
	return resp, nil
}

func (h *largeReplyHead) SaveCheckpoint(_ context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
	h.saved <- len(req.GetCheckpointData())
	return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
}

func (h *largeReplyHead) GetHeadInfo(ctx context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
	h.note(ctx, "GetHeadInfo")
	return &lettucev1.GetHeadInfoResponse{}, nil
}

func (h *largeReplyHead) GetServerStatus(ctx context.Context, _ *lettucev1.GetServerStatusRequest) (*lettucev1.GetServerStatusResponse, error) {
	h.note(ctx, "GetServerStatus")
	return &lettucev1.GetServerStatusResponse{}, nil
}

func (h *largeReplyHead) StartWork(ctx context.Context, _ *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error) {
	h.note(ctx, "StartWork")
	return &lettucev1.StartWorkResponse{Ok: true}, nil
}

func (h *largeReplyHead) AbandonWorkUnit(ctx context.Context, _ *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error) {
	h.note(ctx, "AbandonWorkUnit")
	return &lettucev1.AbandonWorkUnitResponse{}, nil
}

func (h *largeReplyHead) GetMyContribution(ctx context.Context, _ *lettucev1.GetMyContributionRequest) (*lettucev1.GetMyContributionResponse, error) {
	h.note(ctx, "GetMyContribution")
	return &lettucev1.GetMyContributionResponse{}, nil
}

// startHeadLikeServer serves h with the given send limit (the head's is
// headMaxMsgSize) and the head's receive limit.
func startHeadLikeServer(t *testing.T, h lettucev1.VolunteerServiceServer, maxSend int) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer(grpc.MaxRecvMsgSize(headMaxMsgSize), grpc.MaxSendMsgSize(maxSend))
	lettucev1.RegisterVolunteerServiceServer(s, h)
	go s.Serve(lis)
	t.Cleanup(func() { s.Stop(); lis.Close() })

	c, err := New(ClientConfig{ServerURL: lis.Addr().String(), Insecure: true}, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestLargeReplies_CheckpointsAndWorkRepliesOverFourMiB: the reproduction's
// failing rows. A 5 MiB checkpoint, one at the 100 MB default ceiling, and a
// work reply of five 1 MiB inline inputs all arrive whole. Before, each failed
// with "grpc: received message larger than max (… vs. 4194304)".
func TestLargeReplies_CheckpointsAndWorkRepliesOverFourMiB(t *testing.T) {
	head := &largeReplyHead{saved: make(chan int, 1)}
	c := startHeadLikeServer(t, head, headMaxMsgSize)
	ctx := context.Background()

	for _, size := range []int{3 << 20, 5 << 20, 100_000_000} {
		head.checkpoint = make([]byte, size)
		resp, err := c.GetCheckpoint(ctx, &lettucev1.GetCheckpointRequest{WorkUnitId: "wu-ckpt"})
		if err != nil {
			t.Fatalf("GetCheckpoint of %d bytes failed: %v", size, err)
		}
		if got := len(resp.GetCheckpointData()); got != size {
			t.Errorf("GetCheckpoint of %d bytes delivered %d", size, got)
		}
	}
	head.checkpoint = nil

	head.units, head.inline = 5, make([]byte, 1<<20)
	resp, err := c.RequestWorkUnit(ctx, &lettucev1.RequestWorkUnitRequest{MaxAssignments: 5})
	if err != nil {
		t.Fatalf("RequestWorkUnit of 5 × 1 MiB inline failed: %v", err)
	}
	if got := len(resp.GetAssignments()); got != 5 {
		t.Errorf("RequestWorkUnit delivered %d assignments, want 5", got)
	}
	for _, a := range resp.GetAssignments() {
		if len(a.GetInputData()) != 1<<20 {
			t.Errorf("assignment %s carried %d bytes of input, want %d", a.GetWorkUnitId(), len(a.GetInputData()), 1<<20)
		}
	}
	head.inline = nil

	// The upload direction was never limited; it still is not.
	if _, err := c.SaveCheckpoint(ctx, &lettucev1.SaveCheckpointRequest{WorkUnitId: "wu-ckpt", CheckpointData: make([]byte, 5<<20)}); err != nil {
		t.Fatalf("SaveCheckpoint of 5 MiB failed: %v", err)
	}
	if n := <-head.saved; n != 5<<20 {
		t.Errorf("head received %d bytes of checkpoint, want %d", n, 5<<20)
	}
}

// TestLargeReplies_StillBoundedAtTheHeadsCeiling: the raised limit is the
// head's own 128 MiB, not unlimited. A server allowed to send more is refused
// by the client itself at 128 MiB, and the refusal is told apart from load
// shedding.
func TestLargeReplies_StillBoundedAtTheHeadsCeiling(t *testing.T) {
	head := &largeReplyHead{checkpoint: make([]byte, 129<<20)}
	c := startHeadLikeServer(t, head, 2*headMaxMsgSize)

	_, err := c.GetCheckpoint(context.Background(), &lettucev1.GetCheckpointRequest{WorkUnitId: "wu-huge"})
	head.checkpoint = nil
	if err == nil {
		t.Fatal("a 129 MiB checkpoint was accepted; the client's receive limit must stay at the head's 128 MiB")
	}
	if st, _ := status.FromError(err); st.Code() != codes.ResourceExhausted || !strings.Contains(st.Message(), fmt.Sprintf("vs. %d", headMaxMsgSize)) {
		t.Errorf("error = %v, want the client's own ResourceExhausted refusal at %d bytes", err, headMaxMsgSize)
	}
	if !IsMessageTooLargeError(err) {
		t.Errorf("IsMessageTooLargeError(%v) = false", err)
	}

	// The other RPCs keep gRPC's default receive limit: nothing raised it for them.
	if got := maxReplyBytes; got != 4<<20 {
		t.Errorf("maxReplyBytes = %d, want gRPC's 4 MiB default", got)
	}
}

// TestIsMessageTooLargeError tells a size refusal, from either side, from the
// head shedding load.
func TestIsMessageTooLargeError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{status.Error(codes.ResourceExhausted, "grpc: received message larger than max (5243180 vs. 4194304)"), true},
		{status.Error(codes.ResourceExhausted, "grpc: trying to send message larger than max (140000000 vs. 134217728)"), true},
		{status.Error(codes.ResourceExhausted, "grpc: received message after decompression larger than max 4194304"), true},
		{status.Error(codes.ResourceExhausted, "dispatch overloaded; back off and retry"), false},
		{status.Error(codes.ResourceExhausted, "rate limit exceeded"), false},
		{status.Error(codes.Internal, "received message larger than max"), false},
		{fmt.Errorf("plain error"), false},
		{nil, false},
	} {
		if got := IsMessageTooLargeError(tc.err); got != tc.want {
			t.Errorf("IsMessageTooLargeError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestLargeReplies_DeadlineGrowsOnlyForTheTwoLargeReplyRPCs: under a bandwidth
// limit a call's deadline is widened by the time its largest reply takes at
// that rate. GetCheckpoint and RequestWorkUnit accept 128 MiB now, so theirs
// grows with it; every other RPC keeps the deadline sized for a 4 MiB reply, as
// before. Read from the deadline each call arrives at the server with.
func TestLargeReplies_DeadlineGrowsOnlyForTheTwoLargeReplyRPCs(t *testing.T) {
	const mbps = 10 // 1.25 MB/s
	netlimit.SetMbps(mbps)
	t.Cleanup(func() { netlimit.SetMbps(0) })

	head := &largeReplyHead{deadlines: make(chan rpcDeadline, 16)}
	c := startHeadLikeServer(t, head, headMaxMsgSize)
	c.requestTimeout = 30 * time.Second
	ctx := context.Background()

	calls := []func() error{
		func() error { _, err := c.GetCheckpoint(ctx, &lettucev1.GetCheckpointRequest{}); return err },
		func() error { _, err := c.RequestWorkUnit(ctx, &lettucev1.RequestWorkUnitRequest{}); return err },
		func() error { _, err := c.GetHeadInfo(ctx, &lettucev1.GetHeadInfoRequest{}); return err },
		func() error { _, err := c.GetServerStatus(ctx); return err },
		func() error { _, err := c.StartWork(ctx, &lettucev1.StartWorkRequest{}); return err },
		func() error { _, err := c.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{}); return err },
		func() error { _, err := c.GetMyContribution(ctx, &lettucev1.GetMyContributionRequest{}); return err },
	}
	for i, call := range calls {
		if err := call(); err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
	}
	close(head.deadlines)

	small := netlimit.Allowance(30*time.Second, 0, maxReplyBytes)
	large := netlimit.Allowance(30*time.Second, 0, maxLargeReplyBytes)
	for d := range head.deadlines {
		want := small
		if d.method == "GetCheckpoint" || d.method == "RequestWorkUnit" {
			want = large
		}
		// The deadline travels as the time left when the call left the client,
		// in gRPC's rounded timeout encoding; allow for that rounding and for the
		// call's own time in transit.
		if d.left > want+time.Second || d.left < want-5*time.Second {
			t.Errorf("%s arrived with %s left, want about %s (30 s plus the paced time of a %s reply)",
				d.method, d.left.Round(time.Second), want.Round(time.Second), map[bool]string{true: "128 MiB", false: "4 MiB"}[want == large])
		}
	}
	if large < 200*time.Second || small > 40*time.Second {
		t.Fatalf("test precondition: at %d Mbps the 128 MiB allowance (%s) must be far above the 4 MiB one (%s)", mbps, large, small)
	}
}
