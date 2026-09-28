package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Tests for the CPU time limit that read the setting as a volunteer writes it
// and use only entry points that existed before the limit did, so they
// compile against the earlier client and fail there on what it does (the
// rest are in cpu_time_test.go).

// TestCPUTimeLimit_BufferHoldsTheHoursAtTheLimitsPace: a unit is booked at
// the time it runs, and at 50 % a slot runs half the time, so two hours of
// buffer hold one hour of running. Pre-fix the setting did not exist: a
// volunteer's max_cpu_time_pct was ignored and the buffer held the full
// running time, twice the hours at the limit's pace. The setting is read from
// the config file as a volunteer writes it.
func TestCPUTimeLimit_BufferHoldsTheHoursAtTheLimitsPace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "work_buffer_hours: 2\nmax_running_tasks: 4\nresource_limits:\n  max_cpu_cores: 4\n  max_memory_mb: 8192\n  max_disk_gb: 10\n  max_cpu_time_pct: 50\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := newBufferTestDaemon(t, 2, 4, 1)
	d.cfg = cfg
	if got := d.bufferTargetSeconds(); got != 14400 {
		t.Errorf("buffer target at 50 %% CPU time = %g s, want 14400 (2 h of wall clock for 4 tasks running half the time)", got)
	}
}

// TestSuspendedTaskStaysSuspendedThroughAnAutomaticPause: a task the
// volunteer suspended stays suspended when an automatic pause ends. The CPU
// time limit resumes work every few seconds; before it, the end of a thermal
// or schedule pause already resumed such a task, and with the limit it would
// have been resumed within seconds of the volunteer suspending it.
func TestSuspendedTaskStaysSuspendedThroughAnAutomaticPause(t *testing.T) {
	sm := NewSlotManager(2, newTestLogger())
	d := newSlotTestDaemon()
	block := make(chan struct{})
	defer close(block)
	item := &PreFetchItem{
		WU:   &runtime.WorkUnit{ID: "wu-mine"},
		Prep: &runtime.PrepareResult{WorkDir: "/tmp/wu-mine"},
		Runtime: &mockRuntime{canHandle: true, executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			select {
			case <-block:
			case <-ctx.Done():
			}
			return &runtime.ExecutionResult{OutputData: []byte("ok")}, nil
		}},
		Conn:      makeTestConn(),
		FetchedAt: time.Now(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slotID := <-sm.available
	sm.StartSlot(ctx, slotID, item, d)
	time.Sleep(50 * time.Millisecond)
	handle := &mockProcessHandle{pid: 3001}
	sm.SetProcessHandle(slotID, handle)

	if err := sm.SuspendSlot("wu-mine"); err != nil {
		t.Fatalf("SuspendSlot: %v", err)
	}
	sm.SuspendAll()
	sm.ResumeAll()
	if handle.resumeCalls != 0 {
		t.Errorf("the end of an automatic pause resumed a task the volunteer suspended (%d resume calls)", handle.resumeCalls)
	}
	for _, task := range sm.GetCurrentTasks(nil) {
		if task.WorkUnitID == "wu-mine" && !task.Suspended {
			t.Error("the task the volunteer suspended is reported running")
		}
	}
	if err := sm.ResumeSlot("wu-mine"); err != nil || handle.resumeCalls != 1 {
		t.Errorf("the volunteer's resume: err=%v, resume calls=%d; want nil and 1", err, handle.resumeCalls)
	}
}
