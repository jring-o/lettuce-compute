package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
)

// spinChild is a real, long-lived child process used to verify OS-level
// suspend/resume. It records liveness by writing an increasing counter to a file
// (see runSpinChild in test_main_test.go), so the parent can observe whether the
// process is making progress or has been frozen.
type spinChild struct {
	t        *testing.T
	cmd      *exec.Cmd
	pid      int
	spinFile string
}

func startSpinChild(t *testing.T) *spinChild {
	t.Helper()
	spinFile := filepath.Join(t.TempDir(), "counter")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), spinChildEnv+"="+spinFile)
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn spin child: %v", err)
	}
	sc := &spinChild{t: t, cmd: cmd, pid: cmd.Process.Pid, spinFile: spinFile}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	if !sc.advanced(2 * time.Second) {
		t.Fatalf("spin child never advanced its counter; cannot run reproduction")
	}
	return sc
}

func (sc *spinChild) read() (int, bool) {
	b, err := os.ReadFile(sc.spinFile)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return n, true
}

// advanced reports whether the counter strictly increases within timeout.
func (sc *spinChild) advanced(timeout time.Duration) bool {
	start, _ := sc.read()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
		if n, ok := sc.read(); ok && n > start {
			return true
		}
	}
	return false
}

// frozen reports whether the counter fails to advance across the window.
func (sc *spinChild) frozen(window time.Duration) bool {
	a, _ := sc.read()
	time.Sleep(window)
	b, _ := sc.read()
	return a == b && a != 0
}

// daemonForOrphan builds a daemon wired enough to drive resumePersistedTasks against
// a single native orphan whose process is `pid`, and persists that task.
func daemonForOrphan(t *testing.T, pid int) (*Daemon, context.Context, context.CancelFunc) {
	t.Helper()
	mc := &mockClient{} // StartWork defaults to {Ok: true}
	d := newTestDaemon(mc, &mockRuntime{canHandle: true, name: "native"})
	d.cfg.DataDir = t.TempDir()
	d.slotManager = NewSlotManager(2, d.logger)
	d.runtimeRegistry = NewRuntimeRegistry()
	d.runtimeRegistry.Register(&mockRuntime{canHandle: true, name: "native"})

	srv := d.multiClient.Servers()[0]
	pt := PersistedTask{
		WorkUnitID:        "wu-orphan",
		LeafID:            "leaf-1",
		ServerGRPCAddress: srv.Config.GRPCAddress, // empty for the default test conn
		ServerName:        srv.Name,
		VolunteerID:       srv.VolunteerID,
		RuntimeName:       "native",
		WorkDir:           t.TempDir(),
		PID:               pid,
		StartedAt:         time.Now().Add(-time.Minute),
	}
	if err := SaveActiveState(d.cfg.DataDir, []PersistedTask{pt}); err != nil {
		t.Fatalf("SaveActiveState: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return d, ctx, cancel
}

func orphanTask(d *Daemon) (CurrentTask, bool) {
	for _, task := range d.slotManager.GetCurrentTasks(0, func(string) float64 { return 0 }) {
		if task.WorkUnitID == "wu-orphan" {
			return task, true
		}
	}
	return CurrentTask{}, false
}

// TestResumedOrphan_CoveredBySuspendAll reproduces the alpha-tester report that a
// work unit resumed from a previous daemon session (an "orphan" process re-adopted
// by PID) ignores pause signals. It drives the REAL resumePersistedTasks path with a
// REAL child process and asserts that, after resume, SuspendAll both marks the slot
// suspended and actually freezes the underlying OS process.
//
// Lifecycle mirrored: a prior session froze the process (suspend-and-quit), the
// daemon restarts, resumePersistedTasks resumes+adopts it, then a pause arrives and
// SuspendAll must re-freeze it. This guards the handle wiring in resumePersistedTasks
// (SetProcessHandle): without it the slot has no process handle and SuspendAll skips
// the resumed task entirely.
func TestResumedOrphan_CoveredBySuspendAll(t *testing.T) {
	child := startSpinChild(t)

	// Freeze the child — stands in for the previous session's suspend-and-quit.
	priorHandle := NewNativeProcessHandle(child.pid)
	if err := priorHandle.Suspend(); err != nil {
		t.Skipf("cannot suspend a process on this platform/runner: %v", err)
	}
	if !child.frozen(200 * time.Millisecond) {
		t.Fatalf("child did not freeze after Suspend(); platform suspend unreliable here")
	}

	d, ctx, cancel := daemonForOrphan(t, child.pid)
	defer cancel()

	// Resume: should unfreeze, adopt into a slot, and wire a suspendable handle.
	d.resumePersistedTasks(ctx)
	if !child.advanced(2 * time.Second) {
		t.Fatalf("child did not resume running after resumePersistedTasks; orphan not adopted")
	}
	task, ok := orphanTask(d)
	if !ok {
		t.Fatalf("resumed orphan is not an active slot task")
	}
	if task.ProcessID != child.pid {
		t.Errorf("orphan slot ProcessID = %d, want child pid %d (no handle wired)", task.ProcessID, child.pid)
	}

	// A pause arrives: SuspendAll must freeze the resumed orphan.
	d.slotManager.SuspendAll()

	if task, _ := orphanTask(d); !task.Suspended {
		t.Errorf("BUG: resumed orphan NOT marked suspended by SuspendAll (handle missing)")
	}
	if !child.frozen(300 * time.Millisecond) {
		t.Errorf("BUG: resumed orphan kept running after SuspendAll (pause/schedule violated)")
	}

	d.slotManager.ResumeAll()
}

// TestResumedOrphan_SuspendedByScheduleGate covers the real defect behind the report:
// a task resumed from a previous session keeps running through a scheduled OFF window.
// The schedule gate blocks NEW slot-filling but, before the fix, did not freeze a
// slot that was already running from resume — so the resumed task executed for the
// whole off-schedule window. waitForScheduleActive must suspend it while the schedule
// is inactive and resume it when the window reopens.
func TestResumedOrphan_SuspendedByScheduleGate(t *testing.T) {
	child := startSpinChild(t)

	priorHandle := NewNativeProcessHandle(child.pid)
	if err := priorHandle.Suspend(); err != nil {
		t.Skipf("cannot suspend a process on this platform/runner: %v", err)
	}
	if !child.frozen(200 * time.Millisecond) {
		t.Fatalf("child did not freeze after Suspend(); platform suspend unreliable here")
	}

	d, ctx, cancel := daemonForOrphan(t, child.pid)
	defer cancel()

	// Schedule is INACTIVE right now: WHEN_IDLE with the machine reported as not idle.
	sched := resource.NewScheduler(&config.Scheduling{Mode: "WHEN_IDLE", IdleThresholdMins: 60}, d.logger)
	sched.SetIdleFunc(func() (int, error) { return 0, nil }) // 0s idle < threshold -> ShouldRun()=false
	d.scheduler = sched
	if d.scheduler.ShouldRun() {
		t.Fatal("test setup: scheduler should be inactive")
	}

	// Resume the orphan; it starts running again.
	d.resumePersistedTasks(ctx)
	if !child.advanced(2 * time.Second) {
		t.Fatalf("child did not resume running after resumePersistedTasks")
	}

	// Enter the schedule gate while inactive (it blocks until active or ctx ends).
	gateCtx, gateCancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- d.waitForScheduleActive(gateCtx) }()

	// THE FIX: while parked at the inactive schedule gate, the resumed orphan must be
	// frozen — both marked suspended and actually not advancing.
	deadline := time.Now().Add(2 * time.Second)
	var marked bool
	for time.Now().Before(deadline) {
		if task, ok := orphanTask(d); ok && task.Suspended {
			marked = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !marked {
		t.Errorf("BUG: resumed orphan NOT suspended while schedule is inactive")
	}
	if !child.frozen(300 * time.Millisecond) {
		t.Errorf("BUG: resumed orphan kept running through the off-schedule window")
	}

	// End the wait (cancel stands in for ctx shutdown). The gate must resume the
	// process on its way out so the shutdown path can reap it (suspend/resume stays
	// balanced).
	gateCancel()
	if got := <-done; got {
		t.Errorf("waitForScheduleActive returned true after cancellation, want false")
	}
	if !child.advanced(2 * time.Second) {
		t.Errorf("schedule gate left the orphan frozen after returning (unbalanced suspend)")
	}
}
