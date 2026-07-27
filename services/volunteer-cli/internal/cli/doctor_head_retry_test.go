package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TB-12 regression coverage: doctor failed a head on a single attempt.
//
// The daemon's connect path retries and routinely succeeds on attempt 2 against
// a head whose first RPC stalls, so doctor reported the blocking failure "no
// configured head is reachable" on machines that were fetching, computing and
// submitting work at that moment. A diagnostic that cries wolf on a healthy
// setup trains testers to ignore it — and this one did, masking TB-11 for a
// week.
//
// probeServerStatus is the retry itself, so these drive it directly with a fake
// probe rather than a network.
//
// testProbePolicy shrinks the DURATIONS so the tests are fast but keeps the
// SHIPPED attempt count, because the attempt count is the thing TB-12 was about:
// a test pinned to a locally-invented 2 would keep passing if the shipped policy
// regressed to a single try.
var testProbePolicy = headProbePolicy{
	attempts:   defaultHeadProbePolicy.attempts,
	firstProbe: 500 * time.Millisecond,
	perAttempt: time.Second,
	backoff:    time.Millisecond,
}

func deadlineErr() error {
	return status.Error(codes.DeadlineExceeded, "context deadline exceeded")
}

// TestProbeServerStatusSucceedsAfterFirstAttemptTimesOut is the filed symptom:
// the exact sequence the tester's machine produces on every run — first attempt
// times out, second answers — must come back reachable, not down.
func TestProbeServerStatusSucceedsAfterFirstAttemptTimesOut(t *testing.T) {
	calls := 0
	probe := func(ctx context.Context) (*lettucev1.GetServerStatusResponse, error) {
		calls++
		if calls == 1 {
			return nil, deadlineErr()
		}
		return &lettucev1.GetServerStatusResponse{Version: "v0.10.1"}, nil
	}

	st, attempts, err := probeServerStatus(context.Background(), testProbePolicy, probe)
	if err != nil {
		t.Fatalf("probeServerStatus returned error after a retryable first failure: %v", err)
	}
	if st.GetVersion() != "v0.10.1" {
		t.Errorf("version = %q, want the second attempt's response", st.GetVersion())
	}
	if attempts != 2 {
		t.Errorf("reported attempt %d, want 2 — the caller needs this to tell the volunteer the first connection stalled", attempts)
	}
	if calls != 2 {
		t.Errorf("probe called %d times, want exactly 2", calls)
	}
}

// TestProbeServerStatusStopsOnFirstSuccess: a healthy head must cost exactly one
// RPC. The retry must not double every doctor run.
func TestProbeServerStatusStopsOnFirstSuccess(t *testing.T) {
	calls := 0
	probe := func(ctx context.Context) (*lettucev1.GetServerStatusResponse, error) {
		calls++
		return &lettucev1.GetServerStatusResponse{Version: "v0.10.1"}, nil
	}

	_, attempts, err := probeServerStatus(context.Background(), testProbePolicy, probe)
	if err != nil {
		t.Fatalf("probeServerStatus: %v", err)
	}
	if calls != 1 || attempts != 1 {
		t.Errorf("probe called %d times reporting attempt %d, want exactly 1 of each", calls, attempts)
	}
}

// TestProbeServerStatusReportsGenuinelyDownHead: the retry must not paper over a
// head that is actually down. Every attempt is spent and the last error is
// returned, so checkHeads still raises its blocking failure.
func TestProbeServerStatusReportsGenuinelyDownHead(t *testing.T) {
	wantErr := errors.New("connection refused")
	calls := 0
	probe := func(ctx context.Context) (*lettucev1.GetServerStatusResponse, error) {
		calls++
		return nil, wantErr
	}

	st, attempts, err := probeServerStatus(context.Background(), testProbePolicy, probe)
	if err == nil {
		t.Fatal("probeServerStatus succeeded against a head that never answered")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want the underlying failure preserved for the report", err)
	}
	if st != nil {
		t.Error("a failed probe must not return a response")
	}
	if calls != testProbePolicy.attempts || attempts != testProbePolicy.attempts {
		t.Errorf("probe called %d times reporting %d, want %d", calls, attempts, testProbePolicy.attempts)
	}
}

// TestProbeServerStatusGivesEachAttemptItsOwnDeadline is the property that makes
// the retry worth having. A stalled first RPC must not consume the budget of the
// attempt that would have succeeded — if both attempts shared one deadline, the
// retry would be dead on arrival against exactly the symptom it exists to
// tolerate.
func TestProbeServerStatusGivesEachAttemptItsOwnDeadline(t *testing.T) {
	var deadlines []time.Time
	probe := func(ctx context.Context) (*lettucev1.GetServerStatusResponse, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("attempt ran with no deadline")
		}
		deadlines = append(deadlines, d)
		if len(deadlines) == 1 {
			return nil, deadlineErr()
		}
		return &lettucev1.GetServerStatusResponse{}, nil
	}

	if _, _, err := probeServerStatus(context.Background(), testProbePolicy, probe); err != nil {
		t.Fatalf("probeServerStatus: %v", err)
	}
	if len(deadlines) != 2 {
		t.Fatalf("got %d attempts, want 2", len(deadlines))
	}
	if !deadlines[1].After(deadlines[0]) {
		t.Errorf("second attempt's deadline (%v) is not later than the first's (%v) — the attempts are sharing one budget",
			deadlines[1], deadlines[0])
	}
}

// TestProbeServerStatusHonorsCancellation: doctor must remain interruptible
// while it is sleeping between attempts.
func TestProbeServerStatusHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	slow := headProbePolicy{attempts: 3, perAttempt: time.Second, backoff: time.Hour}

	probe := func(context.Context) (*lettucev1.GetServerStatusResponse, error) {
		cancel() // fail, then cancel while the backoff is pending
		return nil, deadlineErr()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := probeServerStatus(ctx, slow, probe); err == nil {
			t.Error("expected an error once the context was cancelled")
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("probeServerStatus ignored cancellation and sat out its backoff")
	}
}

// TestProbeServerStatusEscalatesTheDeadline: the first try is impatient and the
// retry patient, so the reported failure mode — a stalled first RPC whose retry
// answers at once — resolves FASTER than the single long attempt it replaced,
// instead of costing a second long wait.
func TestProbeServerStatusEscalatesTheDeadline(t *testing.T) {
	var budgets []time.Duration
	probe := func(ctx context.Context) (*lettucev1.GetServerStatusResponse, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Fatal("attempt ran with no deadline")
		}
		budgets = append(budgets, time.Until(d))
		if len(budgets) == 1 {
			return nil, deadlineErr()
		}
		return &lettucev1.GetServerStatusResponse{}, nil
	}

	if _, _, err := probeServerStatus(context.Background(), testProbePolicy, probe); err != nil {
		t.Fatalf("probeServerStatus: %v", err)
	}
	if len(budgets) != 2 {
		t.Fatalf("got %d attempts, want 2", len(budgets))
	}
	if budgets[0] >= budgets[1] {
		t.Errorf("first attempt got %v and the retry %v — the first try must be the impatient one", budgets[0], budgets[1])
	}
}

// TestDefaultHeadProbePolicyRetries pins the shipped policy. TB-12 was a
// single-attempt probe; dropping back to one attempt would silently restore it.
func TestDefaultHeadProbePolicyRetries(t *testing.T) {
	if defaultHeadProbePolicy.attempts < 2 {
		t.Errorf("attempts = %d: doctor must not fail a head on one try (TB-12)", defaultHeadProbePolicy.attempts)
	}
	if defaultHeadProbePolicy.backoff <= 0 {
		t.Error("retrying with no backoff re-runs into the same cold-connection stall")
	}
	if defaultHeadProbePolicy.perAttempt <= 0 {
		t.Error("each attempt needs its own positive deadline")
	}
	// The retry must be at least as patient as the first try, or a slow-but-
	// healthy head could be failed on the more impatient attempt.
	if defaultHeadProbePolicy.probeDeadline(1) > defaultHeadProbePolicy.probeDeadline(2) {
		t.Errorf("first attempt (%v) is more patient than the retry (%v)",
			defaultHeadProbePolicy.probeDeadline(1), defaultHeadProbePolicy.probeDeadline(2))
	}
}
