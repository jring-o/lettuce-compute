package daemon

// TB-29 regression coverage: Ctrl-C while the daemon is paused races the
// coordinator's balancing ResumeAll against each executor's own cancel path,
// which unpauses and stops its container. Losing that race is cleanup
// succeeding — but it was logged as WARN "failed to resume process", which a
// tester read as engine corruption and filed within minutes of hitting it.
// The runtime half (unpause before the graceful stop) is covered in
// internal/runtime/container_test.go (TestContainerRuntime_CancelDuringPause_
// UnpausesBeforeStop).

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// startBlockedSlot starts one slot whose execution blocks until the returned
// release func is called, and attaches handle for suspend/resume.
func startBlockedSlot(t *testing.T, sm *SlotManager, d *Daemon, handle ProcessHandle) (release func()) {
	t.Helper()
	blockCh := make(chan struct{})
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-tb29", LeafID: "leaf-1"},
		WUResp: &lettucev1.WorkUnitAssignment{},
		Prep:   &runtime.PrepareResult{WorkDir: "/tmp/wu-tb29"},
		Runtime: &mockRuntime{
			canHandle: true,
			executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
				select {
				case <-blockCh:
				case <-ctx.Done():
				}
				return &runtime.ExecutionResult{OutputData: []byte("ok"), ExitCode: 0}, nil
			},
		},
		Conn:      makeTestConn(),
		FetchedAt: time.Now(),
	}
	slotID := <-sm.available
	sm.StartSlot(context.Background(), slotID, item, d)
	time.Sleep(50 * time.Millisecond)
	sm.SetProcessHandle(slotID, handle)
	return func() {
		close(blockCh)
		sm.WaitForCompletion(context.Background())
	}
}

// TestResumeAllAtShutdown_ResumeFailureIsNotAWarn: once the shutdown is marked,
// a resume that finds its process/container gone must log at Info, not WARN.
func TestResumeAllAtShutdown_ResumeFailureIsNotAWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	h := &mockProcessHandle{pid: 4444, resumeErr: fmt.Errorf("no container with ID deadbeef found in database")}
	release := startBlockedSlot(t, sm, d, h)
	defer release()

	sm.SuspendAll()
	sm.SetShuttingDown()
	sm.ResumeAll()

	logs := buf.String()
	if strings.Contains(logs, "failed to resume process") {
		t.Errorf("shutdown resume failure logged as a failure; a container already cleaned up by its executor is a success at shutdown:\n%s", logs)
	}
	if !strings.Contains(logs, "resume at shutdown skipped") {
		t.Errorf("shutdown resume failure should still be visible at Info:\n%s", logs)
	}
}

// TestResumeAll_NormalResumeFailureStillWarns: outside shutdown a failed resume
// leaves a task genuinely frozen — that one stays a WARN.
func TestResumeAll_NormalResumeFailureStillWarns(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sm := NewSlotManager(1, logger)
	d := newSlotTestDaemon()

	h := &mockProcessHandle{pid: 5555, resumeErr: fmt.Errorf("operation not permitted")}
	release := startBlockedSlot(t, sm, d, h)
	defer release()

	sm.SuspendAll()
	sm.ResumeAll()

	if logs := buf.String(); !strings.Contains(logs, "failed to resume process") {
		t.Errorf("a resume failure outside shutdown must stay a WARN:\n%s", logs)
	}
}

// TestWaitForScheduleActive_ShutdownResumeDoesNotWarn drives the real
// shutdown-while-suspended path: the schedule gate suspended the running task,
// the context is cancelled (Ctrl-C), and the gate's balancing ResumeAll runs
// with the shutdown already marked — so a lost race with the executor's own
// cleanup does not WARN. The waitForResume pause path orders its shutdown
// ResumeAll the same way.
func TestWaitForScheduleActive_ShutdownResumeDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := newSlotTestDaemon()
	d.slotManager = NewSlotManager(1, logger)
	// SCHEDULED with no windows and no cron never runs, so the gate is
	// deterministically inactive.
	d.scheduler = resource.NewScheduler(&config.Scheduling{Mode: "SCHEDULED"}, newTestLogger())

	h := &mockProcessHandle{pid: 3333, resumeErr: fmt.Errorf("no container with ID cafef00d found in database")}
	release := startBlockedSlot(t, d.slotManager, d, h)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown already requested, as on Ctrl-C

	if d.waitForScheduleActive(ctx) {
		t.Fatal("waitForScheduleActive = true on a cancelled context")
	}

	logs := buf.String()
	if strings.Contains(logs, "failed to resume process") {
		t.Errorf("the schedule gate's shutdown ResumeAll logged a lost cleanup race as a failure:\n%s", logs)
	}
	if !strings.Contains(logs, "resume at shutdown skipped") {
		t.Errorf("the shutdown resume failure should still be visible at Info:\n%s", logs)
	}
}
