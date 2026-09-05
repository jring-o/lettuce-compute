package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-74 regressions, daemon half: a container unit's slot persists the
// container's id, and the relaunch adopts a paused or running container of
// the unit instead of re-executing it beside the frozen twin. A container
// that is gone or exited is removed and the unit re-executed from its
// preserved work dir, as before.

// tb74Engine is a mock engine whose one container of the unit is in the given
// state, recording the calls the tests assert on. ContainerWait blocks until
// release is closed, so the adopted unit stays active for the assertions.
type tb74Engine struct {
	runtime.DockerClient // nil: any call not implemented below panics, none should happen
	listed               []runtime.ContainerSummary
	mu                   sync.Mutex
	created              int
	unpaused             []string
	paused               []string
	removed              []string
	release              chan struct{}
}

func newTB74Engine(t *testing.T, listed []runtime.ContainerSummary) *tb74Engine {
	t.Helper()
	return &tb74Engine{listed: listed, release: make(chan struct{})}
}

func (e *tb74Engine) ContainerList(_ context.Context, _ string) ([]runtime.ContainerSummary, error) {
	return e.listed, nil
}

func (e *tb74Engine) ContainerCreate(_ context.Context, _ *runtime.ContainerConfig) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.created++
	return "c-new", nil
}

func (e *tb74Engine) ContainerStart(_ context.Context, _ string) error { return nil }

func (e *tb74Engine) ContainerWait(ctx context.Context, _ string) (int64, error) {
	select {
	case <-e.release:
		return 0, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (e *tb74Engine) ContainerLogs(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (e *tb74Engine) ContainerInspect(_ context.Context, _ string) (*runtime.ContainerStats, error) {
	return &runtime.ContainerStats{}, nil
}

func (e *tb74Engine) ContainerStop(_ context.Context, _ string, _ time.Duration) error { return nil }

func (e *tb74Engine) ContainerRemove(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed = append(e.removed, id)
	return nil
}

func (e *tb74Engine) ContainerPause(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.paused = append(e.paused, id)
	return nil
}

func (e *tb74Engine) ContainerUnpause(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.unpaused = append(e.unpaused, id)
	return nil
}

func (e *tb74Engine) ImageDeclaredVolumes(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

// tb74Daemon is a daemon with a container runtime on the mock engine and one
// persisted container unit — the tester's active-tasks.json after a quit.
func tb74Daemon(t *testing.T, engine *tb74Engine) (*Daemon, string) {
	t.Helper()
	d := newSlotTestDaemon()
	d.cfg.DataDir = t.TempDir()
	d.slotManager = NewSlotManager(1, quietLogger()) // Run creates it; the resumer is called directly here
	d.runtimeRegistry.Register(runtime.NewContainerRuntimeWithClient(d.cfg.DataDir, quietLogger(), engine))

	workDir := t.TempDir()
	for _, sub := range []string{"input", "output", "checkpoint"} {
		if err := os.MkdirAll(filepath.Join(workDir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, "output", "output.dat"), []byte("adopted-result"), 0o644); err != nil {
		t.Fatalf("write output: %v", err)
	}
	if err := SaveActiveState(d.cfg.DataDir, []PersistedTask{{
		WorkUnitID:    "wu-frozen",
		LeafID:        "leaf-grep",
		ServerName:    "default",
		VolunteerID:   "test-slot-vol",
		RuntimeName:   "container",
		WorkDir:       workDir,
		ExecutionSpec: runtime.ExecutionSpec{Image: "grep:2.1", MaxMemoryMB: 512},
		StartedAt:     time.Now().Add(-time.Hour),
		ContainerID:   "c-frozen", // what the quit persisted (TB-74); PID stays 0
	}}); err != nil {
		t.Fatalf("save active state: %v", err)
	}
	return d, workDir
}

// The tester's relaunch: the persisted container is paused. The daemon
// unpauses it, supervises it in a slot without creating a second container,
// can pause it again (the handle is attached), persists its id again, and
// collects its result when it finishes.
func TestTB74_ResumeAdoptsPausedContainerInsteadOfReexecuting(t *testing.T) {
	engine := newTB74Engine(t, []runtime.ContainerSummary{
		{ID: "c-frozen", State: "paused", Labels: map[string]string{runtime.WorkUnitIDLabel: "wu-frozen"}},
	})
	d, _ := tb74Daemon(t, engine)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.resumePersistedTasks(ctx)

	engine.mu.Lock()
	unpaused, created := engine.unpaused, engine.created
	engine.mu.Unlock()
	if fmt.Sprint(unpaused) != fmt.Sprint([]string{"c-frozen"}) {
		t.Fatalf("unpaused = %v, want [c-frozen]", unpaused)
	}
	if created != 0 {
		t.Fatalf("ContainerCreate called %d times on relaunch, want 0 — the unit was re-executed beside its frozen container", created)
	}

	// The slot holds the unit and would persist the same container id at the
	// next quit.
	tasks := d.slotManager.GetActivePersistableTasks()
	if len(tasks) != 1 || tasks[0].WorkUnitID != "wu-frozen" || tasks[0].ContainerID != "c-frozen" {
		t.Fatalf("active persistable tasks = %+v, want the adopted unit with container_id c-frozen", tasks)
	}

	// A schedule pause or the next quit pauses the adopted container.
	d.slotManager.SuspendAll()
	engine.mu.Lock()
	paused := engine.paused
	engine.mu.Unlock()
	if fmt.Sprint(paused) != fmt.Sprint([]string{"c-frozen"}) {
		t.Fatalf("SuspendAll paused %v, want [c-frozen] (no handle on the adopted slot)", paused)
	}
	d.slotManager.ResumeAll()

	// It finishes: the result comes from the preserved work dir.
	close(engine.release)
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	result, err := d.slotManager.WaitForCompletion(waitCtx)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if result.Err != nil || result.Result == nil || string(result.Result.OutputData) != "adopted-result" {
		t.Fatalf("adopted unit's result = %+v (err %v), want the preserved output", result.Result, result.Err)
	}
	engine.mu.Lock()
	removed := engine.removed
	engine.mu.Unlock()
	if fmt.Sprint(removed) != fmt.Sprint([]string{"c-frozen"}) {
		t.Errorf("removed = %v, want [c-frozen] once it finished", removed)
	}
}

// A persisted container that is gone (the tester removed it by hand) or
// exited is not adopted: the unit is re-executed from its preserved work dir
// in a new container, and an exited leftover is removed before that.
func TestTB74_ResumeReexecutesWhenPersistedContainerIsNotResumable(t *testing.T) {
	cases := []struct {
		name          string
		listed        []runtime.ContainerSummary
		wantRemovedID string
	}{
		{name: "gone", listed: nil},
		{name: "exited", listed: []runtime.ContainerSummary{{ID: "c-frozen", State: "exited", Labels: map[string]string{runtime.WorkUnitIDLabel: "wu-frozen"}}}, wantRemovedID: "c-frozen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := newTB74Engine(t, tc.listed)
			close(engine.release) // the new container finishes at once
			d, _ := tb74Daemon(t, engine)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			d.resumePersistedTasks(ctx)
			waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
			defer waitCancel()
			if _, err := d.slotManager.WaitForCompletion(waitCtx); err != nil {
				t.Fatalf("WaitForCompletion: %v", err)
			}

			engine.mu.Lock()
			defer engine.mu.Unlock()
			if len(engine.unpaused) != 0 {
				t.Errorf("unpaused %v, want nothing (not resumable)", engine.unpaused)
			}
			if engine.created != 1 {
				t.Errorf("ContainerCreate called %d times, want 1 (re-executed)", engine.created)
			}
			if tc.wantRemovedID != "" {
				found := false
				for _, id := range engine.removed {
					if id == tc.wantRemovedID {
						found = true
					}
				}
				if !found {
					t.Errorf("the exited leftover was not removed before re-execution; removed = %v", engine.removed)
				}
			}
		})
	}
}

// A slot whose unit runs in a container persists the container's id (and no
// PID), so the next launch can find it.
func TestTB74_PersistedTaskCarriesContainerID(t *testing.T) {
	sm := NewSlotManager(1, newTestLogger())
	d := newSlotTestDaemon()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blocked := make(chan struct{})
	item := &PreFetchItem{
		WU:     &runtime.WorkUnit{ID: "wu-container", LeafID: "leaf-1", Runtime: "container"},
		Prep:   &runtime.PrepareResult{WorkDir: t.TempDir()},
		Conn:   &ServerConnection{Name: "test", VolunteerID: "vol-1", Client: &mockClient{}},
		Runtime: &mockRuntime{canHandle: true, executeFn: func(ctx context.Context, _ *runtime.WorkUnit, _ *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
			<-blocked
			return &runtime.ExecutionResult{}, nil
		}},
		FetchedAt: time.Now(),
	}
	slotID := <-sm.available
	if err := sm.StartSlot(ctx, slotID, item, d); err != nil {
		t.Fatalf("StartSlot: %v", err)
	}
	defer close(blocked)

	sm.SetProcessHandle(slotID, NewContainerProcessHandle(&tb74Engine{}, "c-abc"))
	tasks := sm.GetActivePersistableTasks()
	if len(tasks) != 1 {
		t.Fatalf("got %d persistable tasks, want 1", len(tasks))
	}
	if tasks[0].ContainerID != "c-abc" || tasks[0].PID != 0 {
		t.Errorf("persisted container_id = %q pid = %d, want c-abc and 0", tasks[0].ContainerID, tasks[0].PID)
	}
}
