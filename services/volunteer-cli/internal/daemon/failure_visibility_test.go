package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Regression tests for TB-10's second half — the failure was invisible.
//
// A native leaf's process exited non-zero within ~200 ms, seventeen times. The
// abandon that reached the head said only "non-zero exit code 2"; the process's
// own stdout/stderr had been captured to <work_dir>/execution.log and was then
// deleted with the work dir moments later. Neither the volunteer nor the head's
// operator had anything to diagnose from, and the root cause (TB-11) took a
// database session and a purpose-built repro to find.

// TestAbandonReasonCarriesTheFailingProcessOutput: the head's copy of the
// failure must include what the process actually said, not just its exit code.
func TestAbandonReasonCarriesTheFailingProcessOutput(t *testing.T) {
	d := newSlotTestDaemon()
	mc := &mockClient{}
	d.handleSlotResult(context.Background(), SlotResult{
		WU:             &runtime.WorkUnit{ID: "wu-x", LeafID: "leaf-1"},
		Conn:           handleSlotResultTestConn(mc),
		Result:         &runtime.ExecutionResult{ExitCode: 2},
		FailureLogTail: "runtime: out of memory: cannot allocate 8192-byte block",
	})

	if mc.lastAbandonReq == nil {
		t.Fatal("no abandon request recorded")
	}
	reason := mc.lastAbandonReq.Reason
	if !strings.Contains(reason, "exit code 2") {
		t.Errorf("abandon reason lost the exit code: %q", reason)
	}
	if !strings.Contains(reason, "cannot allocate") {
		t.Errorf("abandon reason = %q — it does not carry the failing process's output, so an operator whose artifact is broken still has nothing to diagnose from", reason)
	}
}

// TestAbandonReasonCarriesOutputForARuntimeFailureToo: the same applies when the
// runtime itself errored rather than the process exiting non-zero.
func TestAbandonReasonCarriesOutputForARuntimeFailureToo(t *testing.T) {
	d := newSlotTestDaemon()
	mc := &mockClient{}
	d.handleSlotResult(context.Background(), SlotResult{
		WU:             &runtime.WorkUnit{ID: "wu-x", LeafID: "leaf-1"},
		Conn:           handleSlotResultTestConn(mc),
		Err:            errors.New("execution deadline exceeded"),
		FailureLogTail: "step 4/9 still running",
	})

	reason := mc.lastAbandonReq.Reason
	if !strings.Contains(reason, "deadline exceeded") || !strings.Contains(reason, "step 4/9") {
		t.Errorf("abandon reason = %q, want both the error and the captured output", reason)
	}
}

// TestAbandonReasonUnchangedWhenNothingWasCaptured: a runtime that captures no
// log (or a work dir already gone) must not decorate the reason with an empty
// tail — "; output: " with nothing after it is worse than the bare reason.
func TestAbandonReasonUnchangedWhenNothingWasCaptured(t *testing.T) {
	d := newSlotTestDaemon()
	mc := &mockClient{}
	d.handleSlotResult(context.Background(), SlotResult{
		WU:     &runtime.WorkUnit{ID: "wu-x", LeafID: "leaf-1"},
		Conn:   handleSlotResultTestConn(mc),
		Result: &runtime.ExecutionResult{ExitCode: 2},
	})

	if got := mc.lastAbandonReq.Reason; got != "non-zero exit code 2" {
		t.Errorf("abandon reason = %q, want the bare reason when no output was captured", got)
	}
}

// TestASuccessfulUnitIsNotRecordedAsAFailure guards the breaker's other
// direction: a clean exit must never leave a failure record behind, or `status`
// would report failing leafs on a perfectly healthy volunteer.
func TestASuccessfulUnitIsNotRecordedAsAFailure(t *testing.T) {
	d := newSlotTestDaemon()
	d.leafFailures = newLeafFailureTracker(nil)
	mc := &mockClient{}

	d.handleSlotResult(context.Background(), SlotResult{
		WU:     &runtime.WorkUnit{ID: "wu-x", LeafID: "leaf-1"},
		Conn:   handleSlotResultTestConn(mc),
		Result: &runtime.ExecutionResult{ExitCode: 0, OutputData: []byte("ok")},
	})

	if snap := d.LeafFailureSnapshot(); len(snap) != 0 {
		t.Errorf("a clean run left %d failure records behind: %+v", len(snap), snap)
	}
}

// TestNonZeroExitIsRecordedAgainstTheLeaf ties handleSlotResult to the breaker:
// the failure the head sees must also be the failure the local counter sees, or
// the loop is still unbounded no matter how good the tracker is.
func TestNonZeroExitIsRecordedAgainstTheLeaf(t *testing.T) {
	d := newSlotTestDaemon()
	d.leafFailures = newLeafFailureTracker(nil)
	mc := &mockClient{}

	d.handleSlotResult(context.Background(), SlotResult{
		WU:     &runtime.WorkUnit{ID: "wu-x", LeafID: "leaf-1"},
		Conn:   handleSlotResultTestConn(mc),
		Result: &runtime.ExecutionResult{ExitCode: 2},
	})

	snap := d.LeafFailureSnapshot()
	if len(snap) != 1 || snap[0].LeafID != "leaf-1" || snap[0].Consecutive != 1 {
		t.Fatalf("failure snapshot = %+v, want one record for leaf-1 with 1 consecutive failure", snap)
	}
	if !strings.Contains(snap[0].LastReason, "exit code 2") {
		t.Errorf("recorded reason = %q, want the exit code", snap[0].LastReason)
	}
}

// TestSlotReadsTheExecutionLogBeforeCleanupDeletesIt is the ordering regression,
// driven through a real slot.
//
// The slot's deferred block does three things in order: clean up the work dir,
// then send the result to the coordinator, which then decides to abandon. So the
// work dir — and with it execution.log, the only local record of why the leaf
// failed — is already gone by the time anyone wants to read it. The capture has
// to happen inside the slot, ahead of the cleanup; anywhere later reads an empty
// directory. This test runs a slot whose runtime writes a log, exits non-zero,
// and deletes the work dir on Cleanup exactly as the real runtimes do.
func TestSlotReadsTheExecutionLogBeforeCleanupDeletesIt(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, runtime.ExecutionLogName),
		[]byte("panic: bad input\ngoroutine 1 [running]:\n"), 0o600); err != nil {
		t.Fatalf("write execution log: %v", err)
	}

	sm := NewSlotManager(1, newTestLogger())
	d := newSlotTestDaemon()
	d.SetSlotManagerForTest(sm)

	rt := &mockRuntime{
		canHandle: true,
		executeFn: func(context.Context, *runtime.WorkUnit, *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			return &runtime.ExecutionResult{ExitCode: 2}, nil
		},
		cleanupFn: func(prep *runtime.PrepareResult) error {
			// What every real runtime's Cleanup does.
			return os.RemoveAll(prep.WorkDir)
		},
	}

	ctx := context.Background()
	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, &PreFetchItem{
		WU:        &runtime.WorkUnit{ID: "wu-log", LeafID: "leaf-1"},
		WUResp:    &lettucev1.WorkUnitAssignment{},
		Prep:      &runtime.PrepareResult{WorkDir: workDir},
		Runtime:   rt,
		Conn:      &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
		FetchedAt: time.Now(),
	}, d)

	result, err := sm.WaitForCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if result.Result == nil || result.Result.ExitCode != 2 {
		t.Fatalf("slot result = %+v, want exit code 2", result.Result)
	}

	if _, statErr := os.Stat(workDir); !os.IsNotExist(statErr) {
		t.Fatalf("the work dir still exists, so this test is not exercising the ordering it claims to")
	}
	if !strings.Contains(result.FailureLogTail, "panic: bad input") {
		t.Errorf("FailureLogTail = %q — the execution log was read after cleanup deleted it, so the failure reaches the head with no explanation", result.FailureLogTail)
	}
	if strings.Contains(result.FailureLogTail, "\n") {
		t.Errorf("FailureLogTail contains a newline; the abandon reason must stay one line: %q", result.FailureLogTail)
	}
}

// TestSlotCapturesNothingForASuccessfulUnit: a unit that exited 0 has nothing to
// explain, so the capture must not run for it — reading and flattening a log on
// every successful unit is pure cost on the hot path.
func TestSlotCapturesNothingForASuccessfulUnit(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, runtime.ExecutionLogName), []byte("all fine\n"), 0o600); err != nil {
		t.Fatalf("write execution log: %v", err)
	}

	sm := NewSlotManager(1, newTestLogger())
	d := newSlotTestDaemon()
	d.SetSlotManagerForTest(sm)

	ctx := context.Background()
	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, &PreFetchItem{
		WU:      &runtime.WorkUnit{ID: "wu-ok", LeafID: "leaf-1"},
		WUResp:  &lettucev1.WorkUnitAssignment{},
		Prep:    &runtime.PrepareResult{WorkDir: workDir},
		Runtime: &mockRuntime{canHandle: true},
		Conn:    &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
	}, d)

	result, err := sm.WaitForCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if result.FailureLogTail != "" {
		t.Errorf("FailureLogTail = %q for a unit that exited 0, want empty", result.FailureLogTail)
	}
}

// TestExecutionLogSummaryIsBounded: the reason crosses the wire into the head's
// log, so a leaf that prints megabytes must not take the log with it.
func TestExecutionLogSummaryIsBounded(t *testing.T) {
	workDir := t.TempDir()
	huge := strings.Repeat("x", 2*1024*1024) + " LAST-LINE-MARKER"
	if err := os.WriteFile(filepath.Join(workDir, runtime.ExecutionLogName), []byte(huge), 0o600); err != nil {
		t.Fatalf("write execution log: %v", err)
	}

	got := runtime.ExecutionLogSummary(workDir, abandonReasonLogTailBytes)
	if len(got) > abandonReasonLogTailBytes+len("…") {
		t.Errorf("summary is %d bytes, want at most %d", len(got), abandonReasonLogTailBytes+len("…"))
	}
	if !strings.Contains(got, "LAST-LINE-MARKER") {
		t.Errorf("summary kept the wrong end of the log: a failing process's LAST words explain it. got %q", got)
	}
}
