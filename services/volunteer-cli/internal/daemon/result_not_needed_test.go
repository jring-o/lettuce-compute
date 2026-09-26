package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A result the head no longer needs — its work unit was finalized by other
// machines' results while this copy ran, or after this copy's deadline — is
// refused with FailedPrecondition "work unit already finalized; result is too
// late to accept". That refusal is final. It used to be handled as a failed
// submission: an ERROR line, the result persisted and resent once by the retry
// worker, then dropped with a WARN, with no history entry and no duration sample
// for the leaf's estimate although the unit ran to a clean exit.
//
// These tests read history.jsonl raw and use the duration tracker directly, so
// they run unchanged against the code before and after the fix.

const unitFinalizedRefusal = "work unit already finalized; result is too late to accept"

// newSubmitTestDaemon is a daemon with a fresh data directory, a duration
// tracker, and every log line captured at Debug.
func newSubmitTestDaemon(t *testing.T) (*Daemon, *mockClient, *bytes.Buffer) {
	t.Helper()
	mc := &mockClient{}
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &testLimiter{}, scheduler)
	d.cfg.DataDir = t.TempDir()
	d.durations = LoadDurationTracker(d.cfg.DataDir)
	var logs bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return d, mc, &logs
}

// finishCleanly hands handleSlotResult a unit of leaf-1 that ran for 120 s and
// exited 0.
func finishCleanly(t *testing.T, d *Daemon, unitID string) {
	t.Helper()
	conn := d.serverByName("default")
	if conn == nil {
		t.Fatal("test daemon has no 'default' server")
	}
	d.handleSlotResult(context.Background(), SlotResult{
		WU:   &runtime.WorkUnit{ID: unitID, LeafID: "leaf-1"},
		Conn: conn,
		Result: &runtime.ExecutionResult{
			ExitCode:   0,
			OutputData: []byte(`{"ok":true}`),
			Metrics:    runtime.ExecutionMetrics{WallClockSeconds: 120},
		},
	})
}

func historyLines(t *testing.T, d *Daemon) []string {
	t.Helper()
	raw, err := os.ReadFile(HistoryFilePath(d.cfg.DataDir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading history: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func pendingCount(t *testing.T, d *Daemon) int {
	t.Helper()
	pending, err := ListPendingResults(d.cfg.DataDir)
	if err != nil {
		t.Fatalf("listing pending results: %v", err)
	}
	return len(pending)
}

func assertNotNeededEntry(t *testing.T, line, unitID string) {
	t.Helper()
	for _, want := range []string{
		`"work_unit_id":"` + unitID + `"`,
		`"result_accepted":false`,
		`"outcome":"not_needed"`,
		`"server_name":"default"`,
		`"wall_clock_seconds":120`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("history entry lacks %s; got:\n%s", want, line)
		}
	}
}

// The first submission meets the refusal: one SubmitResult call, nothing kept
// for the retry worker, an Info line that says the result was not needed, one
// history entry marked not needed, and one duration sample for the leaf.
func TestResultNotNeeded_SettledAtFirstSubmission(t *testing.T) {
	d, mc, logs := newSubmitTestDaemon(t)
	mc.submitResultFn = func(_ context.Context, _ *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
		return nil, status.Error(codes.FailedPrecondition, unitFinalizedRefusal)
	}

	finishCleanly(t, d, "wu-finalized-1")

	if n := pendingCount(t, d); n != 0 {
		t.Errorf("results kept for resending = %d, want 0: the refusal is final", n)
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("a result the head no longer needs is not a failure, but an ERROR was logged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `"level":"INFO","msg":"result not needed`) {
		t.Errorf("want an Info line saying the result was not needed; log:\n%s", logs.String())
	}
	lines := historyLines(t, d)
	if len(lines) != 1 {
		t.Fatalf("history entries = %d, want 1 (the run finished here); history: %q", len(lines), lines)
	}
	assertNotNeededEntry(t, lines[0], "wu-finalized-1")
	if got := d.durations.Completions("leaf-1"); got != 1 {
		t.Errorf("duration samples for the leaf = %d, want 1: the unit ran to a clean exit", got)
	}

	// The retry worker has nothing to resend.
	d.retryPendingResults(context.Background())
	if calls := mc.getSubmitCalls(); calls != 1 {
		t.Errorf("SubmitResult calls = %d, want 1 (no resend)", calls)
	}
	if lines := historyLines(t, d); len(lines) != 1 {
		t.Errorf("history entries after the retry sweep = %d, want still 1", len(lines))
	}
}

// A result kept after a transient failure and refused as finalized on the
// resend gets the same treatment from the retry worker: dropped, an Info line,
// and a history entry marked not needed. Its duration was learned once, when
// the unit finished.
func TestResultNotNeeded_OnResendAfterATransientFailure(t *testing.T) {
	d, mc, logs := newSubmitTestDaemon(t)
	mc.submitResultFn = func(_ context.Context, _ *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
		return nil, status.Error(codes.Unavailable, "head is restarting")
	}

	finishCleanly(t, d, "wu-finalized-2")
	if n := pendingCount(t, d); n != 1 {
		t.Fatalf("results kept after a transient failure = %d, want 1", n)
	}

	mc.submitResultFn = func(_ context.Context, _ *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
		return nil, status.Error(codes.FailedPrecondition, unitFinalizedRefusal)
	}
	logs.Reset()
	d.retryPendingResults(context.Background())

	if calls := mc.getSubmitCalls(); calls != 2 {
		t.Errorf("SubmitResult calls = %d, want 2 (the first submission and one resend)", calls)
	}
	if n := pendingCount(t, d); n != 0 {
		t.Errorf("results kept after the finalized refusal = %d, want 0", n)
	}
	if strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("the resend's finalized refusal is not a failure, but a WARN was logged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `"level":"INFO","msg":"result not needed`) {
		t.Errorf("want an Info line saying the result was not needed; log:\n%s", logs.String())
	}
	lines := historyLines(t, d)
	if len(lines) != 1 {
		t.Fatalf("history entries = %d, want 1; history: %q", len(lines), lines)
	}
	assertNotNeededEntry(t, lines[0], "wu-finalized-2")
	if got := d.durations.Completions("leaf-1"); got != 1 {
		t.Errorf("duration samples for the leaf = %d, want 1", got)
	}
}

// A transient failure is still kept and resent, and a resend the head accepts
// is recorded as accepted. The duration is learned when the unit finishes,
// not only when the first submission succeeds.
func TestTransientSubmitFailure_StillKeptAndResent(t *testing.T) {
	d, mc, _ := newSubmitTestDaemon(t)
	mc.submitResultFn = func(_ context.Context, _ *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
		return nil, status.Error(codes.Unavailable, "head is restarting")
	}

	finishCleanly(t, d, "wu-transient-1")
	if n := pendingCount(t, d); n != 1 {
		t.Fatalf("results kept after a transient failure = %d, want 1", n)
	}
	if lines := historyLines(t, d); len(lines) != 0 {
		t.Errorf("history entries before the result reached the head = %d, want 0", len(lines))
	}

	mc.submitResultFn = func(_ context.Context, _ *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
		return &lettucev1.SubmitResultResponse{ResultId: "r1", Accepted: true}, nil
	}
	d.retryPendingResults(context.Background())

	if calls := mc.getSubmitCalls(); calls != 2 {
		t.Errorf("SubmitResult calls = %d, want 2", calls)
	}
	if n := pendingCount(t, d); n != 0 {
		t.Errorf("results kept after the head accepted the resend = %d, want 0", n)
	}
	lines := historyLines(t, d)
	if len(lines) != 1 || !strings.Contains(lines[0], `"result_accepted":true`) || strings.Contains(lines[0], `"outcome"`) {
		t.Errorf("want one accepted history entry with no outcome; history: %q", lines)
	}
	if got := d.durations.Completions("leaf-1"); got != 1 {
		t.Errorf("duration samples for the leaf = %d, want 1: the result reached the head on a resend", got)
	}
}

// Any other definitive refusal at the first submission is not kept or resent
// either; it stays a WARN, and it is not recorded as "not needed".
func TestOtherFinalRefusal_NotResentFromTheFirstSubmission(t *testing.T) {
	d, mc, logs := newSubmitTestDaemon(t)
	mc.submitResultFn = func(_ context.Context, _ *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
		return nil, status.Error(codes.FailedPrecondition, "no active assignment for this volunteer and work unit")
	}

	finishCleanly(t, d, "wu-refused-1")

	if n := pendingCount(t, d); n != 0 {
		t.Errorf("results kept for resending = %d, want 0: resending the same bytes gets the same refusal", n)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "no active assignment") {
		t.Errorf("want a WARN naming the head's refusal; log:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "result not needed") {
		t.Errorf("only the finalized refusal means the result was not needed; log:\n%s", logs.String())
	}
	for _, line := range historyLines(t, d) {
		if strings.Contains(line, `"outcome":"not_needed"`) {
			t.Errorf("this refusal must not be recorded as not needed; got:\n%s", line)
		}
	}

	d.retryPendingResults(context.Background())
	if calls := mc.getSubmitCalls(); calls != 1 {
		t.Errorf("SubmitResult calls = %d, want 1 (no resend)", calls)
	}
}
