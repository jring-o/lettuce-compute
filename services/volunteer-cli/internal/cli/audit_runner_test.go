package cli

import (
	"context"
	"log/slog"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeAuditClient is a scripted stand-in for the two AuditService RPCs, driven by
// per-call response/error slices (index by call count; past the end it returns a
// benign default). It records requests and call counts for assertions.
type fakeAuditClient struct {
	claimResps []*lettucev1.ClaimAuditJobResponse
	claimErrs  []error
	claimCalls int

	submitErrs  []error
	submitReqs  []*lettucev1.SubmitAuditResultRequest
	submitCalls int
}

func (f *fakeAuditClient) ClaimAuditJob(_ context.Context, _ *lettucev1.ClaimAuditJobRequest) (*lettucev1.ClaimAuditJobResponse, error) {
	i := f.claimCalls
	f.claimCalls++
	if i < len(f.claimErrs) && f.claimErrs[i] != nil {
		return nil, f.claimErrs[i]
	}
	if i < len(f.claimResps) {
		return f.claimResps[i], nil
	}
	// Default: empty queue (no job).
	return &lettucev1.ClaimAuditJobResponse{}, nil
}

func (f *fakeAuditClient) SubmitAuditResult(_ context.Context, req *lettucev1.SubmitAuditResultRequest) (*lettucev1.SubmitAuditResultResponse, error) {
	i := f.submitCalls
	f.submitCalls++
	f.submitReqs = append(f.submitReqs, req)
	if i < len(f.submitErrs) && f.submitErrs[i] != nil {
		return nil, f.submitErrs[i]
	}
	return &lettucev1.SubmitAuditResultResponse{Accepted: true}, nil
}

func TestAuditExecDeadline(t *testing.T) {
	now := time.Unix(1_000_000, 0)

	tests := []struct {
		name   string
		lease  int64
		wantOK bool
		wantDL time.Time
	}{
		{name: "unset lease", lease: 0, wantOK: false},
		{name: "negative lease", lease: -5, wantOK: false},
		{
			name:   "lease in the past",
			lease:  now.Add(-time.Minute).Unix(),
			wantOK: false,
		},
		{
			// Lease is in the future but the 30s safety margin eats the whole window.
			name:   "lease inside safety margin",
			lease:  now.Add(20 * time.Second).Unix(),
			wantOK: false,
		},
		{
			name:   "ample lease",
			lease:  now.Add(10 * time.Minute).Unix(),
			wantOK: true,
			wantDL: now.Add(10*time.Minute - submitSafetyMargin),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDL, gotOK := auditExecDeadline(tt.lease, now)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if tt.wantOK && !gotDL.Equal(tt.wantDL) {
				t.Errorf("deadline = %v, want %v", gotDL, tt.wantDL)
			}
		})
	}
}

// A FailedPrecondition submit means the job is already settled: report it as done and
// never retry (spec F-L2).
func TestSubmitAuditResultFailedPreconditionIsDone(t *testing.T) {
	f := &fakeAuditClient{
		submitErrs: []error{status.Error(codes.FailedPrecondition, "not claimed by you")},
	}
	req := &lettucev1.SubmitAuditResultRequest{AuditId: "a1"}

	got := submitAuditResultWithRetry(context.Background(), f, req, quietLogger())
	if got != outcomeAlreadySettled {
		t.Errorf("outcome = %q, want %q", got, outcomeAlreadySettled)
	}
	if f.submitCalls != 1 {
		t.Errorf("submit called %d times, want exactly 1 (no retry on FailedPrecondition)", f.submitCalls)
	}
}

func TestSubmitAuditResultSuccessOutcomes(t *testing.T) {
	t.Run("output bytes accepted", func(t *testing.T) {
		f := &fakeAuditClient{}
		req := &lettucev1.SubmitAuditResultRequest{AuditId: "a1", OutputData: []byte("x")}
		if got := submitAuditResultWithRetry(context.Background(), f, req, quietLogger()); got != outcomeSubmitted {
			t.Errorf("outcome = %q, want %q", got, outcomeSubmitted)
		}
	})
	t.Run("execution failure accepted", func(t *testing.T) {
		f := &fakeAuditClient{}
		req := &lettucev1.SubmitAuditResultRequest{AuditId: "a1", ExecutionFailed: true, ErrorMessage: "boom"}
		if got := submitAuditResultWithRetry(context.Background(), f, req, quietLogger()); got != outcomeExecutionFailed {
			t.Errorf("outcome = %q, want %q", got, outcomeExecutionFailed)
		}
	})
}

func TestSubmitAuditResultRetriesTransientThenSucceeds(t *testing.T) {
	old := auditSubmitInitialBackoff
	auditSubmitInitialBackoff = time.Millisecond
	defer func() { auditSubmitInitialBackoff = old }()

	f := &fakeAuditClient{
		submitErrs: []error{
			status.Error(codes.Unavailable, "blip"),
			status.Error(codes.Unavailable, "blip"),
			nil, // third attempt succeeds
		},
	}
	req := &lettucev1.SubmitAuditResultRequest{AuditId: "a1", OutputData: []byte("x")}

	got := submitAuditResultWithRetry(context.Background(), f, req, quietLogger())
	if got != outcomeSubmitted {
		t.Errorf("outcome = %q, want %q", got, outcomeSubmitted)
	}
	if f.submitCalls != 3 {
		t.Errorf("submit called %d times, want 3", f.submitCalls)
	}
}

func TestSubmitAuditResultGivesUpAfterMaxRetries(t *testing.T) {
	old := auditSubmitInitialBackoff
	auditSubmitInitialBackoff = time.Millisecond
	defer func() { auditSubmitInitialBackoff = old }()

	f := &fakeAuditClient{
		submitErrs: []error{
			status.Error(codes.Unavailable, "blip"),
			status.Error(codes.Unavailable, "blip"),
			status.Error(codes.Unavailable, "blip"),
		},
	}
	req := &lettucev1.SubmitAuditResultRequest{AuditId: "a1", OutputData: []byte("x")}

	got := submitAuditResultWithRetry(context.Background(), f, req, quietLogger())
	if got != outcomeSubmitFailed {
		t.Errorf("outcome = %q, want %q", got, outcomeSubmitFailed)
	}
	if f.submitCalls != 3 {
		t.Errorf("submit called %d times, want 3 (maxAttempts)", f.submitCalls)
	}
}

// --once exits cleanly on an empty queue without ever touching the runtime registry.
func TestAuditRunnerOnceEmptyQueueExits(t *testing.T) {
	f := &fakeAuditClient{} // default: every claim returns an empty response
	r := &auditRunner{
		client: f,
		// registry deliberately nil: an empty queue must never reach execution.
		logger:       quietLogger(),
		pollInterval: time.Second,
		once:         true,
	}

	processed, err := r.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if processed != 0 {
		t.Errorf("processed = %d, want 0", processed)
	}
	if f.claimCalls != 1 {
		t.Errorf("claim called %d times, want 1", f.claimCalls)
	}
}

// A claim refused with PermissionDenied (not a registered runner) is terminal.
func TestAuditRunnerPermissionDeniedIsTerminal(t *testing.T) {
	f := &fakeAuditClient{
		claimErrs: []error{status.Error(codes.PermissionDenied, "not a runner")},
	}
	r := &auditRunner{
		client:       f,
		logger:       quietLogger(),
		pollInterval: time.Second,
		once:         true,
	}

	_, err := r.run(context.Background())
	if err == nil {
		t.Fatal("expected error on PermissionDenied claim")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("error did not wrap PermissionDenied: %v", err)
	}
}

// A job whose lease leaves no usable execution window is skipped without execution
// (registry nil proves it is never reached) and without a submit, then --once drains.
func TestAuditRunnerUnusableLeaseSkipped(t *testing.T) {
	f := &fakeAuditClient{
		claimResps: []*lettucev1.ClaimAuditJobResponse{
			{Job: &lettucev1.AuditJob{AuditId: "a1", LeaseExpiresUnix: 0}},
			// subsequent claims fall through to the empty default
		},
	}
	r := &auditRunner{
		client:       f,
		registry:     nil, // must not be dereferenced for a skipped job
		logger:       quietLogger(),
		pollInterval: time.Second,
		once:         true,
	}

	processed, err := r.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1", processed)
	}
	if f.submitCalls != 0 {
		t.Errorf("submit called %d times, want 0 (job skipped, not submitted)", f.submitCalls)
	}
}

// A cancelled context stops the loop promptly rather than sleeping a full poll.
func TestAuditRunnerContextCancelStops(t *testing.T) {
	f := &fakeAuditClient{} // always empty queue
	r := &auditRunner{
		client:       f,
		logger:       quietLogger(),
		pollInterval: time.Hour, // long: the test would hang if cancel were not honored
		once:         false,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	done := make(chan struct{})
	go func() {
		_, _ = r.run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return promptly on a cancelled context")
	}
}

func TestTruncateForWire(t *testing.T) {
	if got := truncateForWire("short", 1024); got != "short" {
		t.Errorf("short string altered: %q", got)
	}
	long := make([]byte, 2048)
	for i := range long {
		long[i] = 'a'
	}
	if got := truncateForWire(string(long), maxErrorMessageBytes); len(got) != maxErrorMessageBytes {
		t.Errorf("len = %d, want %d", len(got), maxErrorMessageBytes)
	}
}
