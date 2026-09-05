package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// TB-74 regressions: a quit that suspended a container unit left the container
// paused, and the relaunch — knowing only a PID of 0 — ran the unit again in a
// fresh container beside the frozen twin, which the stranded-container reaper
// never touched. The runtime half: a persisted container is adopted instead
// of re-created, a leftover twin is removed before a new container is created,
// and the reaper removes un-owned containers whatever their state.

// tb74Recorder captures the engine calls the tests assert on.
type tb74Recorder struct {
	mu       sync.Mutex
	created  int
	removed  []string
	unpaused []string
	waited   []string
}

func (r *tb74Recorder) mock(listed []ContainerSummary) *MockDockerClient {
	return &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ string) ([]ContainerSummary, error) { return listed, nil },
		ContainerCreateFn: func(_ context.Context, _ *ContainerConfig) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.created++
			return "c-new", nil
		},
		ContainerRemoveFn: func(_ context.Context, id string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.removed = append(r.removed, id)
			return nil
		},
		ContainerUnpauseFn: func(_ context.Context, id string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.unpaused = append(r.unpaused, id)
			return nil
		},
		ContainerWaitFn: func(_ context.Context, id string) (int64, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.waited = append(r.waited, id)
			return 0, nil
		},
	}
}

// tb74WorkDir is a preserved work dir with the three bind directories and a
// finished output file, as a quit-and-relaunch leaves it.
func tb74WorkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"input", "output", "checkpoint"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "output", "output.dat"), []byte("adopted-result"), 0o644); err != nil {
		t.Fatalf("write output: %v", err)
	}
	return dir
}

func tb74Unit() *WorkUnit {
	return &WorkUnit{ID: "wu-frozen", LeafID: "leaf-grep", ExecutionSpec: ExecutionSpec{Image: "grep:2.1", MaxMemoryMB: 512}}
}

// Execute with a persisted container supervises that container — no
// ContainerCreate — reads its output from the preserved work dir, hands the
// slot its id for pause/unpause, and removes the unit's other container (the
// twin) first and the adopted one when it finishes.
func TestTB74_ExecuteAdoptsPersistedContainerWithoutCreating(t *testing.T) {
	rec := &tb74Recorder{}
	mock := rec.mock([]ContainerSummary{
		{ID: "c-frozen", State: "running", Labels: lbl("wu-frozen")},
		{ID: "c-twin", State: "paused", Labels: lbl("wu-frozen")},
		{ID: "c-other", State: "paused", Labels: lbl("wu-other")},
	})
	cr, _ := newTestContainerRuntime(t, mock)

	var handed string
	prep := &PrepareResult{
		WorkDir:             tb74WorkDir(t),
		OrphanContainerID:   "c-frozen",
		ContainerIDCallback: func(id string) { handed = id },
	}
	result, err := cr.Execute(context.Background(), tb74Unit(), prep)
	if err != nil {
		t.Fatalf("Execute (adopt): %v", err)
	}

	if rec.created != 0 {
		t.Errorf("ContainerCreate called %d times for a unit whose container was persisted, want 0 — that is the second container beside the frozen twin", rec.created)
	}
	if fmt.Sprint(rec.waited) != fmt.Sprint([]string{"c-frozen"}) {
		t.Errorf("waited on %v, want [c-frozen]", rec.waited)
	}
	if string(result.OutputData) != "adopted-result" || result.ExitCode != 0 {
		t.Errorf("result = %q exit %d, want the preserved work dir's output and exit 0", result.OutputData, result.ExitCode)
	}
	if handed != "c-frozen" {
		t.Errorf("ContainerIDCallback got %q, want c-frozen (the slot must be able to pause it again)", handed)
	}
	sort.Strings(rec.removed)
	if fmt.Sprint(rec.removed) != fmt.Sprint([]string{"c-frozen", "c-twin"}) {
		t.Errorf("removed = %v, want [c-frozen c-twin]: the twin before supervising, the adopted one when done, never another unit's", rec.removed)
	}
}

// A fresh Execute (no persisted container) removes any container this
// volunteer still holds for the unit before creating the new one, so the unit
// never has two.
func TestTB74_ExecuteRemovesLeftoverTwinBeforeCreate(t *testing.T) {
	rec := &tb74Recorder{}
	mock := rec.mock([]ContainerSummary{
		{ID: "c-leftover", State: "paused", Labels: lbl("wu-frozen")},
		{ID: "c-other", State: "paused", Labels: lbl("wu-other")},
	})
	var createdAfterRemove bool
	mock.ContainerCreateFn = func(_ context.Context, _ *ContainerConfig) (string, error) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.created++
		for _, id := range rec.removed {
			if id == "c-leftover" {
				createdAfterRemove = true
			}
		}
		return "c-new", nil
	}
	cr, _ := newTestContainerRuntime(t, mock)

	if _, err := cr.Execute(context.Background(), tb74Unit(), &PrepareResult{WorkDir: tb74WorkDir(t)}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rec.created != 1 {
		t.Fatalf("ContainerCreate called %d times, want 1", rec.created)
	}
	if !createdAfterRemove {
		t.Errorf("the leftover container of this unit was not removed before the new one was created; removed = %v", rec.removed)
	}
	for _, id := range rec.removed {
		if id == "c-other" {
			t.Errorf("another unit's container was removed: %v", rec.removed)
		}
	}
}

// Every container this volunteer creates names its data dir, so a volunteer
// sharing the engine never mistakes it for its own.
func TestTB74_CreatedContainerCarriesDataDirLabel(t *testing.T) {
	rec := &tb74Recorder{}
	mock := rec.mock(nil)
	cr, dataDir := newTestContainerRuntime(t, mock)
	if _, err := cr.Execute(context.Background(), tb74Unit(), &PrepareResult{WorkDir: tb74WorkDir(t)}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := mock.LastCreateConfig.Labels[DataDirLabel]; got != dataDir {
		t.Errorf("created container's %s label = %q, want the runtime's data dir %q", DataDirLabel, got, dataDir)
	}
}

// ResumeWorkUnitContainer: paused is unpaused and adopted, running is adopted
// as is, anything else — or another volunteer's — is not.
func TestTB74_ResumeWorkUnitContainerStates(t *testing.T) {
	cases := []struct {
		name       string
		listed     []ContainerSummary
		unpauseErr error
		want       bool
		unpauses   int
	}{
		{name: "paused is unpaused and adopted", listed: []ContainerSummary{{ID: "c-1", State: "paused", Labels: lbl("wu-1")}}, want: true, unpauses: 1},
		{name: "running is adopted as is", listed: []ContainerSummary{{ID: "c-1", State: "running", Labels: lbl("wu-1")}}, want: true},
		{name: "paused but the unpause fails", listed: []ContainerSummary{{ID: "c-1", State: "paused", Labels: lbl("wu-1")}}, unpauseErr: fmt.Errorf("state improper"), want: false, unpauses: 1},
		{name: "exited is not adopted (may be the tail of our own stop)", listed: []ContainerSummary{{ID: "c-1", State: "exited", Labels: lbl("wu-1")}}, want: false},
		{name: "created is not adopted", listed: []ContainerSummary{{ID: "c-1", State: "created", Labels: lbl("wu-1")}}, want: false},
		{name: "gone (removed by hand)", listed: nil, want: false},
		{name: "another volunteer's container of the same unit", listed: []ContainerSummary{{ID: "c-1", State: "paused", Labels: map[string]string{WorkUnitIDLabel: "wu-1", DataDirLabel: "/elsewhere"}}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unpauses := 0
			mock := &MockDockerClient{
				ContainerListFn:    func(_ context.Context, _ string) ([]ContainerSummary, error) { return tc.listed, nil },
				ContainerUnpauseFn: func(_ context.Context, _ string) error { unpauses++; return tc.unpauseErr },
			}
			cr := reaperTestRuntime(t, mock)
			if got := cr.ResumeWorkUnitContainer(context.Background(), "wu-1", "c-1"); got != tc.want {
				t.Errorf("ResumeWorkUnitContainer = %v, want %v", got, tc.want)
			}
			if unpauses != tc.unpauses {
				t.Errorf("ContainerUnpause called %d times, want %d", unpauses, tc.unpauses)
			}
		})
	}
}

// The tester's leftovers: paused containers of units no slot owns, from a
// build that stamped no data-dir label. The reaper removes them; a container
// another volunteer created on the same engine is left alone.
func TestTB74_ReaperRemovesPausedLeftoversAndSparesAnotherVolunteers(t *testing.T) {
	var removed []string
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ string) ([]ContainerSummary, error) {
			return []ContainerSummary{
				{ID: "frozen-bb", State: "paused", Labels: lbl("wu-bb")},
				{ID: "frozen-grep", State: "paused", Labels: lbl("wu-grep")},
				{ID: "theirs", State: "paused", Labels: map[string]string{WorkUnitIDLabel: "wu-theirs", DataDirLabel: "/other/volunteer"}},
			}, nil
		},
		ContainerRemoveFn: func(_ context.Context, id string) error { removed = append(removed, id); return nil },
	}
	cr := reaperTestRuntime(t, mock)
	cr.ReapStrandedContainers(context.Background(), nil)

	sort.Strings(removed)
	if fmt.Sprint(removed) != fmt.Sprint([]string{"frozen-bb", "frozen-grep"}) {
		t.Fatalf("removed = %v, want [frozen-bb frozen-grep] (paused leftovers go; another volunteer's stays)", removed)
	}
}
