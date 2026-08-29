package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Persistent visualization bundles for result replay.
//
// The runtime extracts a leaf's visualization bundle into the unit's work
// directory, and the slot removes that directory on every normal completion
// (the start-up sweep removes any leftovers). An index entry that pointed
// into the work directory was therefore dead before it was written, and
// replay of a completed result could never work. SaveResult now keeps its
// own extracted copy under {dataDir}/results/viz/<bundle key>/.

// seedVizTarball writes a gzipped tar of files to the place EnsureVizBundle
// would have cached it for this URL/checksum, and returns the matching source.
func seedVizTarball(t *testing.T, dataDir, url, checksum string, files map[string]string) VizBundleSource {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gw.Close()
	path := runtime.VizTarballPath(dataDir, runtime.VizBundleKey(url, checksum))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return VizBundleSource{URL: url, Checksum: checksum}
}

// The replay path must outlive the work directory.
func TestSaveResult_VizBundlePathSurvivesWorkDirCleanup(t *testing.T) {
	dataDir := t.TempDir()
	viz := seedVizTarball(t, dataDir, "https://head.example.org/viz/a.tar.gz", "", map[string]string{
		"index.html": "<html>a</html>",
		"app.js":     "console.log(1)",
	})

	// What the runtime does at Prepare: extract into the work dir.
	workDir := filepath.Join(dataDir, "work", uuidWU001)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workViz, err := runtime.ExtractVizBundle(runtime.VizTarballPath(dataDir, runtime.VizBundleKey(viz.URL, "")), workDir)
	if err != nil {
		t.Fatalf("ExtractVizBundle: %v", err)
	}

	if err := SaveResult(dataDir, uuidWU001, "Leaf A", "leaf-a", "head", []byte(`{"v":1}`), viz, 0); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}

	// What the slot does on completion, and the start-up sweep after a crash.
	if err := os.RemoveAll(workDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workViz); !os.IsNotExist(err) {
		t.Fatalf("fixture: work-dir bundle should be gone, stat err = %v", err)
	}

	entries, err := ListResults(dataDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListResults = %v, %v; want one entry", entries, err)
	}
	e := entries[0]
	if strings.HasPrefix(e.VizBundlePath, workDir) {
		t.Fatalf("viz_bundle_path %q points into the work directory, which no longer exists", e.VizBundlePath)
	}
	if !strings.HasPrefix(e.VizBundlePath, VizBundlesDir(dataDir)) {
		t.Errorf("viz_bundle_path %q is not under %s", e.VizBundlePath, VizBundlesDir(dataDir))
	}
	for _, name := range []string{"index.html", "app.js"} {
		if _, err := os.Stat(filepath.Join(e.VizBundlePath, name)); err != nil {
			t.Errorf("persistent copy lacks %s: %v", name, err)
		}
	}
}

// Two results of the same leaf share one extracted copy, keyed by the
// bundle's checksum when the spec carries one.
func TestSaveResult_SharesOneBundleCopyPerKey(t *testing.T) {
	dataDir := t.TempDir()
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	viz := seedVizTarball(t, dataDir, "https://head.example.org/viz/b.tar.gz", sum, map[string]string{"index.html": "<html>b</html>"})

	for _, id := range []string{uuidWU001, uuidWU002} {
		if err := SaveResult(dataDir, id, "Leaf B", "leaf-b", "head", []byte(`{}`), viz, 0); err != nil {
			t.Fatalf("SaveResult(%s): %v", id, err)
		}
	}
	entries, _ := ListResults(dataDir)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	want := filepath.Join(VizBundlesDir(dataDir), sum)
	for _, e := range entries {
		if e.VizBundleKey != sum || e.VizBundlePath != want {
			t.Errorf("entry %s: key %q path %q; want key %q path %q", e.WorkUnitID, e.VizBundleKey, e.VizBundlePath, sum, want)
		}
	}
	dirs, _ := os.ReadDir(VizBundlesDir(dataDir))
	if len(dirs) != 1 {
		t.Errorf("got %d bundle copies, want exactly 1 shared copy", len(dirs))
	}
}

// A bundle wrapped in a single top-level directory (the layout assemble.sh
// produces) is persisted with viz_bundle_path pointing at the wrapper that
// holds index.html.
func TestSaveResult_WrappedBundleRootResolved(t *testing.T) {
	dataDir := t.TempDir()
	viz := seedVizTarball(t, dataDir, "https://head.example.org/viz/wrapped.tar.gz", "", map[string]string{
		"dist/index.html": "<html>wrapped</html>",
	})
	if err := SaveResult(dataDir, uuidWU001, "Leaf", "leaf", "head", []byte(`{}`), viz, 0); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}
	entries, _ := ListResults(dataDir)
	want := filepath.Join(VizBundlesDir(dataDir), runtime.VizBundleKey(viz.URL, ""), "dist")
	if entries[0].VizBundlePath != want {
		t.Errorf("viz_bundle_path = %q, want the wrapper %q", entries[0].VizBundlePath, want)
	}
	if _, err := os.Stat(filepath.Join(entries[0].VizBundlePath, "index.html")); err != nil {
		t.Errorf("index.html missing at the recorded root: %v", err)
	}
}

// A result whose cached tarball is gone cannot be replayed and is not indexed.
func TestSaveResult_MissingTarballIsAnError(t *testing.T) {
	dataDir := t.TempDir()
	viz := VizBundleSource{URL: "https://head.example.org/viz/missing.tar.gz"}
	if err := SaveResult(dataDir, uuidWU001, "Leaf", "leaf", "head", []byte(`{}`), viz, 0); err == nil {
		t.Fatal("SaveResult succeeded with no cached tarball to persist from")
	}
	if entries, _ := ListResults(dataDir); len(entries) != 0 {
		t.Errorf("an unreplayable result was indexed: %v", entries)
	}
}

// Eviction counts extracted bundles toward the cap, keeps a bundle while any
// remaining entry references it, and removes it once none does.
func TestEvictResults_BundleRemovedOnlyWhenUnreferenced(t *testing.T) {
	dataDir := t.TempDir()
	big := strings.Repeat("a", 1000)
	vizA := seedVizTarball(t, dataDir, "https://head.example.org/viz/a.tar.gz", "", map[string]string{"index.html": big})
	vizB := seedVizTarball(t, dataDir, "https://head.example.org/viz/b.tar.gz", "", map[string]string{"index.html": big})
	keyA := runtime.VizBundleKey(vizA.URL, "")
	keyB := runtime.VizBundleKey(vizB.URL, "")
	bundleDir := func(key string) string { return filepath.Join(VizBundlesDir(dataDir), key) }

	// Three 100-byte results: old and mid share bundle A, new uses bundle B.
	payload := []byte(strings.Repeat("x", 100))
	for _, c := range []struct {
		id  string
		viz VizBundleSource
	}{{uuidOld, vizA}, {uuidMid, vizA}, {uuidNew, vizB}} {
		if err := SaveResult(dataDir, c.id, "Leaf", "leaf", "head", payload, c.viz, 0); err != nil {
			t.Fatalf("SaveResult(%s): %v", c.id, err)
		}
	}
	entries, _ := ListResults(dataDir)
	for i := range entries {
		entries[i].CompletedAt = time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC)
	}
	rewriteResultIndex(dataDir, entries)

	// Total = 300 bytes of results + 1000 (A) + 1000 (B) = 2300. A cap of
	// 2250 evicts only the oldest entry; bundle A is still referenced by mid.
	if err := evictResults(dataDir, 2250); err != nil {
		t.Fatalf("evictResults: %v", err)
	}
	remaining, _ := ListResults(dataDir)
	if len(remaining) != 2 || remaining[0].WorkUnitID != uuidMid || remaining[1].WorkUnitID != uuidNew {
		t.Fatalf("after first eviction remaining = %v, want mid and new", remaining)
	}
	if _, err := os.Stat(bundleDir(keyA)); err != nil {
		t.Errorf("bundle A was removed while entry %s still references it: %v", uuidMid, err)
	}

	// Total is now 2200. A cap of 1150 evicts mid too, and with it bundle A;
	// bundle B stays with the newest entry (which is never evicted).
	if err := evictResults(dataDir, 1150); err != nil {
		t.Fatalf("evictResults: %v", err)
	}
	remaining, _ = ListResults(dataDir)
	if len(remaining) != 1 || remaining[0].WorkUnitID != uuidNew {
		t.Fatalf("after second eviction remaining = %v, want only new", remaining)
	}
	if _, err := os.Stat(bundleDir(keyA)); !os.IsNotExist(err) {
		t.Errorf("bundle A should be removed once nothing references it, stat err = %v", err)
	}
	if _, err := os.Stat(bundleDir(keyB)); err != nil {
		t.Errorf("bundle B (still referenced) was removed: %v", err)
	}

	// An orphaned copy nothing references is swept on the next eviction pass.
	orphan := bundleDir("deadbeef")
	os.MkdirAll(orphan, 0o755)
	os.WriteFile(filepath.Join(orphan, "index.html"), []byte("x"), 0o644)
	if err := evictResults(dataDir, 1<<30); err != nil {
		t.Fatalf("evictResults: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("unreferenced bundle copy was not swept, stat err = %v", err)
	}
	if _, err := os.Stat(bundleDir(keyB)); err != nil {
		t.Errorf("referenced bundle B was swept: %v", err)
	}
}
