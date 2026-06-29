package runtime

import (
	"fmt"
	"testing"
)

func TestUsesContainerdSnapshotter(t *testing.T) {
	cases := []struct {
		name   string
		status [][2]string
		want   bool
	}{
		{"containerd snapshotter", [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}}, true},
		{"classic overlay2", [][2]string{{"Backing Filesystem", "extfs"}, {"Supports d_type", "true"}}, false},
		{"nil status", nil, false},
		{"empty status", [][2]string{}, false},
	}
	for _, tc := range cases {
		if got := usesContainerdSnapshotter(tc.status); got != tc.want {
			t.Errorf("%s: usesContainerdSnapshotter = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// On a classic graphdriver host the image store is the single DockerRootDir.
func TestBuildEngineInfo_GraphDriverSinglePath(t *testing.T) {
	defer withPathExists(func(string) bool { return true })()

	ei := buildEngineInfo("/var/lib/docker", [][2]string{{"Backing Filesystem", "extfs"}})
	if ei.Snapshotter {
		t.Fatal("graphdriver host must not be flagged as snapshotter")
	}
	if got := fmt.Sprint(ei.ImageStorePaths); got != fmt.Sprint([]string{"/var/lib/docker"}) {
		t.Fatalf("ImageStorePaths = %v, want [/var/lib/docker]", ei.ImageStorePaths)
	}
}

// Under the containerd snapshotter the containerd content root is added when it
// exists — that is the filesystem the blobs and overlayfs snapshots actually
// land on, which DockerRootDir does not cover.
func TestBuildEngineInfo_SnapshotterAddsExistingContainerdRoot(t *testing.T) {
	// Only the system containerd root exists; the bundled <root>/containerd does not.
	defer withPathExists(func(p string) bool { return p == "/var/lib/containerd" })()

	ei := buildEngineInfo("/var/lib/docker", [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}})
	if !ei.Snapshotter {
		t.Fatal("snapshotter host must be flagged")
	}
	want := []string{"/var/lib/docker", "/var/lib/containerd"}
	if fmt.Sprint(ei.ImageStorePaths) != fmt.Sprint(want) {
		t.Fatalf("ImageStorePaths = %v, want %v", ei.ImageStorePaths, want)
	}
}

// A candidate containerd root that does not exist must NOT be added, so the gate
// can never falsely block on a path with no filesystem behind it; resolution
// degrades to DockerRootDir-only.
func TestBuildEngineInfo_SnapshotterSkipsMissingCandidates(t *testing.T) {
	defer withPathExists(func(string) bool { return false })()

	ei := buildEngineInfo("/var/lib/docker", [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}})
	if !ei.Snapshotter {
		t.Fatal("snapshotter host must be flagged even when no candidate root exists")
	}
	if got := fmt.Sprint(ei.ImageStorePaths); got != fmt.Sprint([]string{"/var/lib/docker"}) {
		t.Fatalf("ImageStorePaths = %v, want only [/var/lib/docker] when candidates are absent", ei.ImageStorePaths)
	}
}

// withPathExists swaps the package pathExistsFunc seam for a test and returns a
// restore func.
func withPathExists(fn func(string) bool) func() {
	orig := pathExistsFunc
	pathExistsFunc = fn
	return func() { pathExistsFunc = orig }
}
