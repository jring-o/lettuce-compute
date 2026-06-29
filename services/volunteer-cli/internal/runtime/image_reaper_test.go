package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"testing"
)

func reaperTestRuntime(t *testing.T, mock *MockDockerClient) *ContainerRuntime {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewContainerRuntimeWithClient(t.TempDir(), logger, mock)
}

// idResolver builds an ImageIDFn from a ref->id map.
func idResolver(m map[string]string) func(context.Context, string) (string, error) {
	return func(_ context.Context, ref string) (string, error) {
		if id, ok := m[ref]; ok {
			return id, nil
		}
		return "", fmt.Errorf("not found: %s", ref)
	}
}

func TestReapStaleImages_RemovesSupersededCopies(t *testing.T) {
	const repo = "lbry.science/beyblade"
	var removed []string

	mock := &MockDockerClient{
		ImageIDFn: idResolver(map[string]string{
			repo + ":v2-progress": "sha256:current",
		}),
		ImageListFn: func(_ context.Context) ([]ImageSummary, error) {
			return []ImageSummary{
				{ID: "sha256:current", RepoTags: []string{repo + ":v2-progress"}, Size: 472 << 20},
				// Superseded copy left by a re-pushed tag: untagged but repo-digest named.
				{ID: "sha256:stale1", RepoTags: []string{"<none>:<none>"}, RepoDigests: []string{repo + "@sha256:old1"}, Size: 471 << 20},
				// Old :latest the leaf no longer points at.
				{ID: "sha256:stale2", RepoTags: []string{repo + ":latest"}, Size: 471 << 20},
				// Unrelated repos must never be touched.
				{ID: "sha256:pg", RepoTags: []string{"docker.io/library/postgres:16"}, Size: 458 << 20},
				{ID: "sha256:extract2", RepoTags: []string{"ghcr.io/jring-o/extract2-lettuce:1.2"}, Size: 48 << 30},
			}, nil
		},
		ImageRemoveFn: func(_ context.Context, id string) error { removed = append(removed, id); return nil },
	}

	cr := reaperTestRuntime(t, mock)
	cr.SetWantedImages(func() []string { return []string{repo + ":v2-progress"} })
	cr.reapStaleImages(context.Background(), repo+":v2-progress")

	sort.Strings(removed)
	want := []string{"sha256:stale1", "sha256:stale2"}
	if fmt.Sprint(removed) != fmt.Sprint(want) {
		t.Fatalf("removed = %v, want %v (current image, postgres and extract2 must be kept)", removed, want)
	}
}

// Two active leaves sharing one repository under different tags (grep-cpu :1.2 and
// grep-gpu :1.3-gpu) must never reap each other — only the truly-superseded copy.
func TestReapStaleImages_KeepsOtherActiveLeafSharingRepo(t *testing.T) {
	const repo = "ghcr.io/jring-o/extract2-lettuce"
	var removed []string

	mock := &MockDockerClient{
		ImageIDFn: idResolver(map[string]string{
			repo + ":1.2":     "sha256:cpu",
			repo + ":1.3-gpu": "sha256:gpu",
		}),
		ImageListFn: func(_ context.Context) ([]ImageSummary, error) {
			return []ImageSummary{
				{ID: "sha256:cpu", RepoTags: []string{repo + ":1.2"}, Size: 48 << 30},
				{ID: "sha256:gpu", RepoTags: []string{repo + ":1.3-gpu"}, Size: 49 << 30},
				{ID: "sha256:oldcpu", RepoTags: []string{"<none>:<none>"}, RepoDigests: []string{repo + "@sha256:oldcpu"}, Size: 48 << 30},
			}, nil
		},
		ImageRemoveFn: func(_ context.Context, id string) error { removed = append(removed, id); return nil },
	}

	cr := reaperTestRuntime(t, mock)
	cr.SetWantedImages(func() []string { return []string{repo + ":1.2", repo + ":1.3-gpu"} })
	cr.reapStaleImages(context.Background(), repo+":1.2")

	if fmt.Sprint(removed) != fmt.Sprint([]string{"sha256:oldcpu"}) {
		t.Fatalf("removed = %v, want only [sha256:oldcpu] (the in-use :1.3-gpu image must be kept)", removed)
	}
}

// A digest-pinned pull must reap older digests of the same repo while keeping the
// pinned one — and the reaper resolves the keep image even with no wantedImages.
func TestReapStaleImages_DigestRefNoWantedFunc(t *testing.T) {
	const repo = "lbry.science/beyblade"
	pulled := repo + "@sha256:newdigest"
	var removed []string

	mock := &MockDockerClient{
		ImageIDFn: idResolver(map[string]string{pulled: "sha256:new"}),
		ImageListFn: func(_ context.Context) ([]ImageSummary, error) {
			return []ImageSummary{
				{ID: "sha256:new", RepoDigests: []string{repo + "@sha256:newdigest"}, Size: 472 << 20},
				{ID: "sha256:old", RepoDigests: []string{repo + "@sha256:olddigest"}, Size: 471 << 20},
			}, nil
		},
		ImageRemoveFn: func(_ context.Context, id string) error { removed = append(removed, id); return nil },
	}

	cr := reaperTestRuntime(t, mock) // no SetWantedImages
	cr.reapStaleImages(context.Background(), pulled)

	if fmt.Sprint(removed) != fmt.Sprint([]string{"sha256:old"}) {
		t.Fatalf("removed = %v, want [sha256:old]", removed)
	}
}

// An in-use / undeletable image (non-force remove errors) must be skipped, not
// fatal, and must not stop the reaper from removing the others.
func TestReapStaleImages_SkipsUndeletable(t *testing.T) {
	const repo = "lbry.science/beyblade"
	var attempted []string

	mock := &MockDockerClient{
		ImageIDFn: idResolver(map[string]string{repo + ":v2-progress": "sha256:current"}),
		ImageListFn: func(_ context.Context) ([]ImageSummary, error) {
			return []ImageSummary{
				{ID: "sha256:current", RepoTags: []string{repo + ":v2-progress"}},
				{ID: "sha256:busy", RepoTags: []string{repo + ":latest"}},
				{ID: "sha256:free", RepoDigests: []string{repo + "@sha256:old"}},
			}, nil
		},
		ImageRemoveFn: func(_ context.Context, id string) error {
			attempted = append(attempted, id)
			if id == "sha256:busy" {
				return fmt.Errorf("image is being used by running container")
			}
			return nil
		},
	}

	cr := reaperTestRuntime(t, mock)
	cr.SetWantedImages(func() []string { return []string{repo + ":v2-progress"} })
	cr.reapStaleImages(context.Background(), repo+":v2-progress")

	// Both stale images were attempted; the busy one erroring did not abort the sweep.
	sort.Strings(attempted)
	if fmt.Sprint(attempted) != fmt.Sprint([]string{"sha256:busy", "sha256:free"}) {
		t.Fatalf("attempted removals = %v, want both stale images attempted", attempted)
	}
}

// If the just-pulled image can't be resolved, the reaper refuses to remove
// anything rather than risk deleting the live image.
func TestReapStaleImages_EmptyKeepSetRemovesNothing(t *testing.T) {
	const repo = "lbry.science/beyblade"
	removeCalled := false

	mock := &MockDockerClient{
		ImageIDFn: func(_ context.Context, _ string) (string, error) { return "", fmt.Errorf("unresolved") },
		ImageListFn: func(_ context.Context) ([]ImageSummary, error) {
			return []ImageSummary{{ID: "sha256:whatever", RepoTags: []string{repo + ":latest"}}}, nil
		},
		ImageRemoveFn: func(_ context.Context, _ string) error { removeCalled = true; return nil },
	}

	cr := reaperTestRuntime(t, mock)
	cr.reapStaleImages(context.Background(), repo+":v2-progress")

	if removeCalled {
		t.Fatal("reaper removed an image despite an unresolved keep-set")
	}
}

// The startup/across-wanted reaper (ReapStaleImages) reclaims orphaned sibling
// tags WITHOUT a fresh pull — the v0.8.11 field case where a leaf moved to a new
// tag while the volunteer still had the old image cached, so the pull-triggered
// reaper never ran. It must clean every wanted repo, keep all wanted images, and
// never touch an unrelated repository.
func TestReapStaleImages_StartupAcrossWantedRepos(t *testing.T) {
	const beyblade = "lbry.science/beyblade"
	const grep = "ghcr.io/jring-o/extract2-lettuce"
	var removed []string

	mock := &MockDockerClient{
		ImageIDFn: idResolver(map[string]string{
			beyblade + ":v3-checkpoint": "sha256:bb-current",
			grep + ":1.2":              "sha256:grep-current",
		}),
		ImageListFn: func(_ context.Context) ([]ImageSummary, error) {
			return []ImageSummary{
				{ID: "sha256:bb-current", RepoTags: []string{beyblade + ":v3-checkpoint"}, Size: 471 << 20},
				// Orphan sibling tag the leaf moved off of — still tagged, no pull fired.
				{ID: "sha256:bb-stale", RepoTags: []string{beyblade + ":v2-progress"}, Size: 471 << 20},
				{ID: "sha256:grep-current", RepoTags: []string{grep + ":1.2"}, Size: 48 << 30},
				// Unrelated repo must never be touched.
				{ID: "sha256:other", RepoTags: []string{"docker.io/library/postgres:16"}, Size: 1 << 30},
			}, nil
		},
		ImageRemoveFn: func(_ context.Context, id string) error { removed = append(removed, id); return nil },
	}

	cr := reaperTestRuntime(t, mock)
	cr.SetWantedImages(func() []string { return []string{beyblade + ":v3-checkpoint", grep + ":1.2"} })
	cr.ReapStaleImages(context.Background())

	if fmt.Sprint(removed) != fmt.Sprint([]string{"sha256:bb-stale"}) {
		t.Fatalf("removed = %v, want [sha256:bb-stale] (current images and the unrelated repo must be kept)", removed)
	}
}

// Without a wanted-image callback the startup reaper is a no-op and must not even
// list images — a native-only volunteer or one with no enabled container leaf
// removes nothing.
func TestReapStaleImages_StartupNoWantedFuncIsNoop(t *testing.T) {
	mock := &MockDockerClient{
		ImageListFn: func(_ context.Context) ([]ImageSummary, error) {
			t.Fatal("ReapStaleImages must not list images when no wanted-image callback is set")
			return nil, nil
		},
	}
	cr := reaperTestRuntime(t, mock)
	cr.ReapStaleImages(context.Background())
}

// If no wanted image resolves (none pulled yet / engine unreachable) the startup
// reaper refuses to remove anything, exactly like the per-pull path.
func TestReapStaleImages_StartupEmptyKeepSetRemovesNothing(t *testing.T) {
	const repo = "lbry.science/beyblade"
	removeCalled := false
	mock := &MockDockerClient{
		ImageIDFn: func(_ context.Context, _ string) (string, error) { return "", fmt.Errorf("unresolved") },
		ImageListFn: func(_ context.Context) ([]ImageSummary, error) {
			return []ImageSummary{{ID: "sha256:x", RepoTags: []string{repo + ":latest"}}}, nil
		},
		ImageRemoveFn: func(_ context.Context, _ string) error { removeCalled = true; return nil },
	}
	cr := reaperTestRuntime(t, mock)
	cr.SetWantedImages(func() []string { return []string{repo + ":v3-checkpoint"} })
	cr.ReapStaleImages(context.Background())
	if removeCalled {
		t.Fatal("startup reaper removed an image despite an unresolved keep-set")
	}
}

func TestRepoFromImageRef(t *testing.T) {
	cases := map[string]string{
		"lbry.science/beyblade:v2-progress":        "lbry.science/beyblade",
		"lbry.science/beyblade@sha256:abc":         "lbry.science/beyblade",
		"lbry.science/beyblade:tag@sha256:abc":     "lbry.science/beyblade",
		"registry:5000/team/img:tag":               "registry:5000/team/img",
		"ghcr.io/jring-o/extract2-lettuce:1.3-gpu": "ghcr.io/jring-o/extract2-lettuce",
		"lbry.science/beyblade":                    "lbry.science/beyblade",
		"":                                         "",
	}
	for in, want := range cases {
		if got := repoFromImageRef(in); got != want {
			t.Errorf("repoFromImageRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// A repo must not match a longer sibling sharing its prefix.
func TestImageInRepo_NoSiblingPrefixMatch(t *testing.T) {
	const repo = "lbry.science/beyblade"
	sibling := ImageSummary{RepoTags: []string{"lbry.science/beyblade-native:latest"}}
	if imageInRepo(sibling, repo) {
		t.Error("beyblade-native must not be treated as part of the beyblade repo")
	}
	member := ImageSummary{RepoDigests: []string{"lbry.science/beyblade@sha256:x"}}
	if !imageInRepo(member, repo) {
		t.Error("a repo-digest-named image must be recognized as part of the repo")
	}
}
