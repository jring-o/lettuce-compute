package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// currentTaskByWU returns the active slot task for a given work-unit id, if any.
func currentTaskByWU(d *Daemon, id string) (CurrentTask, bool) {
	for _, task := range d.slotManager.GetCurrentTasks(0, func(string) float64 { return 0 }) {
		if task.WorkUnitID == id {
			return task, true
		}
	}
	return CurrentTask{}, false
}

// TestResumedReexec_SuspendedByScheduleGate reproduces an alpha-tester finding
// (QuaXeros, 2026-06-30, ArchPod v0.8.12): a work unit resumed from a previous
// session via the RE-EXECUTION path keeps computing through a scheduled OFF
// window. The prior session's process was killed on shutdown (no live orphan to
// re-adopt), so on restart the preserved work directory is re-executed.
//
// Root cause: unlike the orphan/PID-reattach path — which wires the slot's
// process handle SYNCHRONOUSLY in resumePersistedTasks (SetProcessHandle, see
// TestResumedOrphan_SuspendedByScheduleGate, which passes) — the re-exec path
// sets the handle ASYNCHRONOUSLY: StartSlot marks the slot active immediately and
// spawns runSlot, but the handle only appears when the runtime invokes
// prep.PIDCallback partway through Execute (slot.go ~345). The schedule gate's
// SuspendAll (waitForScheduleActive) is a one-shot at gate entry that only
// suspends slots whose handle is already set (slot.go ~596); it therefore SKIPS
// the just-resumed slot (active, handle still nil) and parks the main loop in
// WaitUntilActive. When the handle finally appears, nothing re-suspends it, so
// the re-executed process runs unfrozen for the whole off-schedule window —
// exactly what waitForScheduleActive's own doc comment says it exists to prevent.
//
// On current code this test FAILS at the assertions below — that failure IS the
// reproduction. The fix must suspend a slot whose handle is registered while the
// schedule is inactive (e.g. a pending-suspend flag honored by the PIDCallback
// handle-registration path, or a schedule re-check when the handle is set).
func TestResumedReexec_SuspendedByScheduleGate(t *testing.T) {
	spinFile := filepath.Join(t.TempDir(), "counter")
	releasePID := make(chan struct{})

	// A native runtime whose Execute re-executes the work as a real, suspendable
	// child process and registers the slot's process handle only AFTER the test
	// releases it — modelling the real timing where the spawn/handle lands after
	// the schedule gate has already run its one-shot SuspendAll.
	rt := &mockRuntime{
		canHandle: true,
		name:      "native",
		executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), spinChildEnv+"="+spinFile)
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			defer func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}()
			select {
			case <-releasePID:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if prep.PIDCallback != nil {
				prep.PIDCallback(cmd.Process.Pid)
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	mc := &mockClient{} // StartWork defaults to {Ok: true}
	d := newTestDaemon(mc, rt)
	d.cfg.DataDir = t.TempDir()
	d.slotManager = NewSlotManager(2, d.logger)
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(rt)

	// Persist a task with PID 0 so resumePersistedTasks takes the RE-EXEC branch
	// (not the orphan/PID-reattach branch, which sets the handle synchronously).
	srv := d.multiClient.Servers()[0]
	pt := PersistedTask{
		WorkUnitID:        "wu-reexec",
		LeafID:            "leaf-1",
		ServerGRPCAddress: srv.Config.GRPCAddress,
		ServerName:        srv.Name,
		VolunteerID:       srv.VolunteerID,
		RuntimeName:       "native",
		WorkDir:           t.TempDir(),
		PID:               0,
		StartedAt:         time.Now().Add(-time.Minute),
	}
	if err := SaveActiveState(d.cfg.DataDir, []PersistedTask{pt}); err != nil {
		t.Fatalf("SaveActiveState: %v", err)
	}

	// Schedule is INACTIVE right now: WHEN_IDLE with the machine reported not idle.
	sched := resource.NewScheduler(&config.Scheduling{Mode: "WHEN_IDLE", IdleThresholdMins: 60}, d.logger)
	sched.SetIdleFunc(func() (int, error) { return 0, nil }) // 0s idle < threshold -> ShouldRun()=false
	d.scheduler = sched
	if d.scheduler.ShouldRun() {
		t.Fatal("test setup: scheduler should be inactive")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Resume via re-exec: StartSlot marks the slot active and spawns the runtime,
	// which starts the child spinning.
	d.resumePersistedTasks(ctx)

	sc := &spinChild{t: t, spinFile: spinFile}
	if !sc.advanced(2 * time.Second) {
		t.Fatalf("re-executed task never started running; cannot run reproduction")
	}

	// Enter the schedule gate while inactive. It runs SuspendAll once (the slot is
	// active but its handle is still nil, so it is skipped) and then parks.
	gateCtx, gateCancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- d.waitForScheduleActive(gateCtx) }()

	// Give the gate time to run its one-shot SuspendAll, THEN reveal the handle —
	// modelling the real race (handle registered after SuspendAll).
	time.Sleep(150 * time.Millisecond)
	close(releasePID)

	// THE FIX: while parked at the inactive schedule gate, the resumed re-exec task
	// must be frozen — both marked suspended and actually not advancing.
	deadline := time.Now().Add(2 * time.Second)
	var marked bool
	for time.Now().Before(deadline) {
		if task, ok := currentTaskByWU(d, "wu-reexec"); ok && task.Suspended {
			marked = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !marked {
		t.Errorf("BUG: resumed re-exec task NOT marked suspended while schedule is inactive")
	}
	if !sc.frozen(300 * time.Millisecond) {
		t.Errorf("BUG: resumed re-exec task kept running through the off-schedule window")
	}

	gateCancel()
	<-done
}
