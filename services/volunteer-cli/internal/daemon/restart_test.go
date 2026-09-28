package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// countingEngine is the grant-recording engine with a count of the
// containers created for each work unit.
type countingEngine struct {
	*grantRecordingEngine
	createsMu sync.Mutex
	creates   map[string]int
}

func (e *countingEngine) ContainerCreate(ctx context.Context, cfg *runtime.ContainerConfig) (string, error) {
	e.createsMu.Lock()
	e.creates[cfg.Labels[runtime.WorkUnitIDLabel]]++
	e.createsMu.Unlock()
	return e.grantRecordingEngine.ContainerCreate(ctx, cfg)
}

func (e *countingEngine) createsOf(id string) int {
	e.createsMu.Lock()
	defer e.createsMu.Unlock()
	return e.creates[id]
}

// TestRestartTask_RerunsTheUnitWithTheCurrentGrant: a GREP task (2–4 cores)
// starts alone on four cores and is given all four. The volunteer then sets
// GREP to 2 cores and restarts the task: its container is stopped, the unit
// goes back to the buffer and starts again at once in a new container with 2
// cores and LETTUCE_CPU_LIMIT=2, its work dir — and the checkpoint the leaf
// wrote there — kept. The head is asked to run-start it again (the call is
// idempotent there), nothing is given back, and when the re-run finishes its
// result is submitted once. Before this there was no restart: a running task
// kept the settings it started with until it finished.
func TestRestartTask_RerunsTheUnitWithTheCurrentGrant(t *testing.T) {
	d, recording, _ := grantHost(t, "")
	engine := &countingEngine{grantRecordingEngine: recording, creates: map[string]int{}}
	cr := runtime.NewContainerRuntimeWithClient(t.TempDir(), quietLogger(), engine)
	d.runtimeRegistry.Register(cr)
	d.wireRuntimeLimits(nil, &testLimiter{})
	client := &mockClient{}
	pushContainerUnit(t, d, cr, "grep-1", "leaf-grep", client)
	workDir := d.prefetchQueue.Items()[0].Prep.WorkDir
	marker := filepath.Join(workDir, "checkpoint", "checkpoint.dat")
	if err := os.WriteFile(marker, []byte("step 40"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.fillSlots(ctx)
	waitCreated(t, recording, 1)
	recording.mu.Lock()
	firstQuota := recording.quotas["grep-1"]
	recording.mu.Unlock()
	if firstQuota != 400000 {
		t.Fatalf("setup: GREP started with quota %d, want 400000 (all four free cores)", firstQuota)
	}

	d.cfg.Servers[0].LeafPreferences = leafPrefsYAML(t, "cores:\n  grep: 2\n")
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if err = d.RestartTask("grep-1"); !errors.Is(err, ErrTaskStarting) {
			break
		}
	}
	if err != nil {
		t.Fatalf("RestartTask: %v", err)
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	res, err := d.slotManager.WaitForCompletion(waitCtx)
	if err != nil {
		t.Fatalf("waiting for the stopped run: %v", err)
	}
	if !res.Restart {
		t.Fatalf("the stopped run's result is not marked for a restart (err %v)", res.Err)
	}
	d.handleSlotResult(ctx, res)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the work dir's checkpoint is gone after the restart (%v); a leaf that checkpoints must continue from it", err)
	}
	d.fillSlots(ctx)
	for deadline := time.Now().Add(10 * time.Second); engine.createsOf("grep-1") < 2 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if n := engine.createsOf("grep-1"); n != 2 {
		t.Fatalf("containers created for the unit = %d, want 2 (the first run and the restart)", n)
	}
	recording.mu.Lock()
	quota, limit := recording.quotas["grep-1"], recording.limits["grep-1"]
	recording.mu.Unlock()
	if quota != 200000 || limit != "2" {
		t.Errorf("restarted task created with quota %d, told LETTUCE_CPU_LIMIT=%q; want 200000 and \"2\" (the cores set before the restart)", quota, limit)
	}

	close(recording.release)
	res, err = d.slotManager.WaitForCompletion(waitCtx)
	if err != nil {
		t.Fatalf("waiting for the re-run: %v", err)
	}
	d.handleSlotResult(ctx, res)
	if got := client.getSubmitCalls(); got != 1 {
		t.Errorf("results submitted = %d, want 1 (the re-run's)", got)
	}
	if got := client.getAbandonCalls(); got != 0 {
		t.Errorf("units given back = %d, want 0: a restart keeps the unit", got)
	}
	if got := client.getStartWorkCalls(); got != 2 {
		t.Errorf("run-start calls = %d, want 2 (the head treats the second as the same start)", got)
	}
}

// TestRestartTask_UnknownTaskIsNotFound: restarting a task that is not
// running says so.
func TestRestartTask_UnknownTaskIsNotFound(t *testing.T) {
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	d.slotManager = NewSlotManager(2, d.logger)
	if err := d.RestartTask("no-such-task"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("RestartTask of an unknown task = %v, want ErrTaskNotFound", err)
	}
}
