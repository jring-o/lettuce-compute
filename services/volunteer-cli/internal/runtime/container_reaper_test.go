package runtime

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"
)

func lbl(wu string) map[string]string { return map[string]string{WorkUnitIDLabel: wu} }

// The stranded-container reaper removes the volunteer's own inactive work-unit
// containers (crash/dirty-shutdown leftovers, which pin the leaf image) while
// sparing running, paused, and still-owned containers.
func TestReapStrandedContainers_RemovesInactiveOwnLeftovers(t *testing.T) {
	var removed []string
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, label string) ([]ContainerSummary, error) {
			if label != WorkUnitIDLabel {
				t.Fatalf("listed with label %q, want %q", label, WorkUnitIDLabel)
			}
			return []ContainerSummary{
				{ID: "run1", State: "running", Labels: lbl("wu-running")},      // active — keep
				{ID: "paused1", State: "paused", Labels: lbl("wu-paused")},     // suspended slot — keep
				{ID: "exited1", State: "exited", Labels: lbl("wu-exited")},     // crash leftover — remove
				{ID: "created-old", State: "created", Labels: lbl("wu-old")},   // never-started leftover — remove
				{ID: "created-owned", State: "created", Labels: lbl("wu-own")}, // just-resumed unit — keep (owned)
				{ID: "dead1", State: "dead", Labels: lbl("wu-dead")},           // dead leftover — remove
			}, nil
		},
		ContainerRemoveFn: func(_ context.Context, id string) error { removed = append(removed, id); return nil },
	}
	cr := reaperTestRuntime(t, mock)
	cr.ReapStrandedContainers(context.Background(), map[string]bool{"wu-own": true})

	sort.Strings(removed)
	want := []string{"created-old", "dead1", "exited1"}
	if fmt.Sprint(removed) != fmt.Sprint(want) {
		t.Fatalf("removed = %v, want %v (running/paused and the owned 'created' container must be kept)", removed, want)
	}
}

// A failed list call removes nothing.
func TestReapStrandedContainers_ListErrorIsNoop(t *testing.T) {
	removeCalled := false
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ string) ([]ContainerSummary, error) {
			return nil, fmt.Errorf("daemon unreachable")
		},
		ContainerRemoveFn: func(_ context.Context, _ string) error { removeCalled = true; return nil },
	}
	cr := reaperTestRuntime(t, mock)
	cr.ReapStrandedContainers(context.Background(), nil)
	if removeCalled {
		t.Fatal("no container should be removed when the list call fails")
	}
}

// An error removing one stranded container must not abort the sweep.
func TestReapStrandedContainers_BestEffortOnRemoveError(t *testing.T) {
	var attempted []string
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ string) ([]ContainerSummary, error) {
			return []ContainerSummary{
				{ID: "busy", State: "exited", Labels: lbl("wu1")},
				{ID: "ok", State: "exited", Labels: lbl("wu2")},
			}, nil
		},
		ContainerRemoveFn: func(_ context.Context, id string) error {
			attempted = append(attempted, id)
			if id == "busy" {
				return fmt.Errorf("removal in progress")
			}
			return nil
		},
	}
	cr := reaperTestRuntime(t, mock)
	cr.ReapStrandedContainers(context.Background(), nil)
	sort.Strings(attempted)
	if fmt.Sprint(attempted) != fmt.Sprint([]string{"busy", "ok"}) {
		t.Fatalf("attempted = %v, want both attempted (one error must not abort the sweep)", attempted)
	}
}

// When the image reaper cannot remove a superseded copy (a stopped container
// still pins it), it surfaces a one-line INFO summary — the per-image reason is
// DEBUG-only, so without this an operator at INFO sees the image persist with no
// explanation.
func TestReapStaleImages_LogsSkippedSummaryAtInfo(t *testing.T) {
	const repo = "lbry.science/beyblade"
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mock := &MockDockerClient{
		ImageIDFn: idResolver(map[string]string{repo + ":v3-checkpoint": "sha256:current"}),
		ImageListFn: func(_ context.Context) ([]ImageSummary, error) {
			return []ImageSummary{
				{ID: "sha256:current", RepoTags: []string{repo + ":v3-checkpoint"}},
				{ID: "sha256:pinned", RepoTags: []string{repo + ":latest"}}, // stale, but pinned below
			}, nil
		},
		ImageRemoveFn: func(_ context.Context, _ string) error {
			return fmt.Errorf("image is being used by stopped container")
		},
	}
	cr := NewContainerRuntimeWithClient(t.TempDir(), logger, mock)
	cr.SetWantedImages(func() []string { return []string{repo + ":v3-checkpoint"} })
	cr.ReapStaleImages(context.Background())

	if !strings.Contains(buf.String(), "left superseded image copies cached") {
		t.Fatalf("expected an INFO summary about non-reclaimable images, got:\n%s", buf.String())
	}
}
