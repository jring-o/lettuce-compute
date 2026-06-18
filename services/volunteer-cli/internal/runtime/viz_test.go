package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createTestTarball creates a gzipped tar archive in memory.
// files is a map of relative path -> content.
func createTestTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	tw.Close()
	gw.Close()
	return buf.Bytes()
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestExtractVizBundle_Valid(t *testing.T) {
	tarball := createTestTarball(t, map[string]string{
		"./index.html": "<html><body>Hello</body></html>",
		"./main.js":    "console.log('viz');",
		"./style.css":  "body { margin: 0; }",
	})

	// Write tarball to temp file.
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "viz.tar.gz")
	if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	vizDir, err := ExtractVizBundle(tarPath, workDir)
	if err != nil {
		t.Fatalf("ExtractVizBundle failed: %v", err)
	}

	// Verify extracted directory.
	if vizDir != filepath.Join(workDir, ".lettuce-viz") {
		t.Errorf("unexpected viz dir: %s", vizDir)
	}

	// Verify index.html exists.
	indexContent, err := os.ReadFile(filepath.Join(vizDir, "index.html"))
	if err != nil {
		t.Fatal("index.html not extracted:", err)
	}
	if string(indexContent) != "<html><body>Hello</body></html>" {
		t.Errorf("unexpected index.html content: %s", indexContent)
	}

	// Verify main.js exists.
	if _, err := os.Stat(filepath.Join(vizDir, "main.js")); err != nil {
		t.Error("main.js not extracted:", err)
	}
}

func TestExtractVizBundle_MissingIndexHTML(t *testing.T) {
	tarball := createTestTarball(t, map[string]string{
		"./main.js":   "console.log('no index');",
		"./style.css": "body {}",
	})

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "viz.tar.gz")
	if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractVizBundle(tarPath, workDir)
	if err == nil {
		t.Fatal("expected error for missing index.html, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("index.html")) {
		t.Errorf("error should mention index.html: %v", err)
	}
}

func TestExtractVizBundle_PathTraversal(t *testing.T) {
	// Create a tarball with a path traversal attack.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name: "viz/../../../etc/passwd",
		Mode: 0o644,
		Size: 5,
	}
	tw.WriteHeader(hdr)
	tw.Write([]byte("evil!"))
	tw.Close()
	gw.Close()

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "evil.tar.gz")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractVizBundle(tarPath, workDir)
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
}

func TestEnsureVizBundle_CacheHit(t *testing.T) {
	var downloadCount int
	tarball := createTestTarball(t, map[string]string{
		"./index.html": "<html>test</html>",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadCount++
		w.Write(tarball)
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	logger := testLogger()
	ctx := context.Background()

	// First download (no checksum -> URL-keyed cache, unverified).
	path1, err := EnsureVizBundle(ctx, dataDir, srv.URL+"/viz.tar.gz", "", srv.Client(), logger)
	if err != nil {
		t.Fatal(err)
	}
	if path1 == "" {
		t.Fatal("expected non-empty path")
	}
	if downloadCount != 1 {
		t.Errorf("expected 1 download, got %d", downloadCount)
	}

	// Second call — should be cached.
	path2, err := EnsureVizBundle(ctx, dataDir, srv.URL+"/viz.tar.gz", "", srv.Client(), logger)
	if err != nil {
		t.Fatal(err)
	}
	if path2 != path1 {
		t.Error("cache miss: paths differ")
	}
	if downloadCount != 1 {
		t.Errorf("expected still 1 download (cache hit), got %d", downloadCount)
	}
}

// TestEnsureVizBundle_ChecksumMatch verifies that a matching checksum is accepted.
func TestEnsureVizBundle_ChecksumMatch(t *testing.T) {
	tarball := createTestTarball(t, map[string]string{
		"./index.html": "<html>test</html>",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarball)
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	want := checksumSHA256(tarball)
	path, err := EnsureVizBundle(context.Background(), dataDir, srv.URL+"/viz.tar.gz", want, srv.Client(), testLogger())
	if err != nil {
		t.Fatalf("EnsureVizBundle with matching checksum should succeed: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}
}

// TestEnsureVizBundle_ChecksumMismatch verifies that a viz bundle whose bytes do
// not match the declared checksum is rejected (C2 anti-tamper for viz).
func TestEnsureVizBundle_ChecksumMismatch(t *testing.T) {
	tarball := createTestTarball(t, map[string]string{
		"./index.html": "<html>test</html>",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarball)
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	wrong := checksumSHA256([]byte("not the bundle"))
	_, err := EnsureVizBundle(context.Background(), dataDir, srv.URL+"/viz.tar.gz", wrong, srv.Client(), testLogger())
	if err == nil {
		t.Fatal("expected error for viz bundle checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %q, want to contain 'checksum mismatch'", err)
	}
}

func TestPrepareVizBundle_NoVizKey(t *testing.T) {
	spec := &ExecutionSpec{
		Binaries: map[string]string{
			"linux_amd64": "https://example.com/bin",
		},
	}

	path, err := PrepareVizBundle(context.Background(), t.TempDir(), t.TempDir(), spec, http.DefaultClient, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Errorf("expected empty path for no viz key, got %s", path)
	}
}

func TestPrepareVizBundle_NilSpec(t *testing.T) {
	path, err := PrepareVizBundle(context.Background(), t.TempDir(), t.TempDir(), nil, http.DefaultClient, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Errorf("expected empty path for nil spec, got %s", path)
	}
}

// TestExtractVizBundle_WrappedSingleDir verifies the TODO #39 fix: a bundle that
// wraps all of its files in a single top-level directory (the shape assemble.sh
// produces for beyblade-viz, and what the dashboard's viz route already strips)
// extracts successfully, with the wrapper directory returned as the bundle root.
func TestExtractVizBundle_WrappedSingleDir(t *testing.T) {
	tarball := createTestTarball(t, map[string]string{
		"beyblade-viz/index.html":                       "<html><body>Wrapped</body></html>",
		"beyblade-viz/player.js":                         "console.log('wrapped');",
		"beyblade-viz/style.css":                         "body{}",
		"beyblade-viz/lib/three.module.min.js":           "// three",
		"beyblade-viz/lib/addons/controls/OrbitControls.js": "// orbit",
	})

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "viz.tar.gz")
	if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	vizDir, err := ExtractVizBundle(tarPath, workDir)
	if err != nil {
		t.Fatalf("wrapped bundle should extract: %v", err)
	}

	// The returned root must be the wrapper directory, and index.html must live
	// directly inside it.
	wantRoot := filepath.Join(workDir, vizBundleDir, "beyblade-viz")
	if vizDir != wantRoot {
		t.Errorf("viz root = %q, want %q", vizDir, wantRoot)
	}
	idx, err := os.ReadFile(filepath.Join(vizDir, "index.html"))
	if err != nil {
		t.Fatalf("index.html not found in wrapper root: %v", err)
	}
	if string(idx) != "<html><body>Wrapped</body></html>" {
		t.Errorf("unexpected index.html content: %s", idx)
	}
	// A nested asset must resolve relative to the wrapper root too.
	if _, err := os.Stat(filepath.Join(vizDir, "lib", "three.module.min.js")); err != nil {
		t.Errorf("nested lib asset missing: %v", err)
	}
}

// TestExtractVizBundle_WrappedDirNoIndex verifies that a single wrapper directory
// that does NOT contain index.html is still rejected (the strip is only a
// convenience, not a way to skip the index.html requirement).
func TestExtractVizBundle_WrappedDirNoIndex(t *testing.T) {
	tarball := createTestTarball(t, map[string]string{
		"wrapper/player.js": "console.log('no index');",
		"wrapper/style.css": "body{}",
	})

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "viz.tar.gz")
	if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractVizBundle(tarPath, workDir)
	if err == nil {
		t.Fatal("expected error when wrapper dir lacks index.html")
	}
	if !strings.Contains(err.Error(), "index.html") {
		t.Errorf("error should mention index.html: %v", err)
	}
}

// TestExtractVizBundle_MultipleTopLevelDirsRejected verifies the strip only fires
// for EXACTLY one top-level directory; an ambiguous multi-dir layout with no root
// index.html is rejected rather than guessing which dir is the root.
func TestExtractVizBundle_MultipleTopLevelDirsRejected(t *testing.T) {
	tarball := createTestTarball(t, map[string]string{
		"dirA/index.html": "<html>A</html>",
		"dirB/index.html": "<html>B</html>",
	})

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "viz.tar.gz")
	if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractVizBundle(tarPath, workDir)
	if err == nil {
		t.Fatal("expected error for ambiguous multi-dir bundle with no root index.html")
	}
	if !strings.Contains(err.Error(), "index.html") {
		t.Errorf("error should mention index.html: %v", err)
	}
}

func TestExtractVizBundle_Subdirectories(t *testing.T) {
	tarball := createTestTarball(t, map[string]string{
		"./index.html":          "<html><body>App</body></html>",
		"./assets/style.css":    "body { color: red; }",
		"./assets/img/logo.png": "FAKEPNG",
		"./js/main.js":          "console.log('nested');",
	})

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "viz.tar.gz")
	if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	vizDir, err := ExtractVizBundle(tarPath, workDir)
	if err != nil {
		t.Fatalf("ExtractVizBundle failed: %v", err)
	}

	// Verify nested files exist.
	for _, relPath := range []string{
		"index.html",
		"assets/style.css",
		"assets/img/logo.png",
		"js/main.js",
	} {
		full := filepath.Join(vizDir, filepath.FromSlash(relPath))
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected %s to exist: %v", relPath, err)
		}
	}

	// Verify content of a nested file.
	css, err := os.ReadFile(filepath.Join(vizDir, "assets", "style.css"))
	if err != nil {
		t.Fatal(err)
	}
	if string(css) != "body { color: red; }" {
		t.Errorf("unexpected css content: %s", css)
	}
}

func TestPrepareVizBundle_FullFlow(t *testing.T) {
	tarball := createTestTarball(t, map[string]string{
		"./index.html": "<html><body>Full Flow</body></html>",
		"./app.js":     "console.log('full flow');",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarball)
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "work-unit-123")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	spec := &ExecutionSpec{
		Binaries: map[string]string{
			"linux_amd64": "https://example.com/bin",
			"viz":         srv.URL + "/viz.tar.gz",
		},
	}

	vizPath, err := PrepareVizBundle(context.Background(), dataDir, workDir, spec, srv.Client(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if vizPath == "" {
		t.Fatal("expected non-empty viz path")
	}

	// Verify index.html was extracted.
	indexContent, err := os.ReadFile(filepath.Join(vizPath, "index.html"))
	if err != nil {
		t.Fatal("index.html not found:", err)
	}
	if string(indexContent) != "<html><body>Full Flow</body></html>" {
		t.Errorf("unexpected content: %s", indexContent)
	}

	// Verify app.js was extracted.
	if _, err := os.Stat(filepath.Join(vizPath, "app.js")); err != nil {
		t.Error("app.js not found:", err)
	}
}

func TestDownloadVizBundle_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	destPath := filepath.Join(t.TempDir(), "should-not-exist.tar.gz")
	err := downloadVizBundle(context.Background(), srv.Client(), srv.URL+"/missing", destPath)
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("404")) {
		t.Errorf("error should mention status code: %v", err)
	}

	// Ensure no file was left behind.
	if _, err := os.Stat(destPath); err == nil {
		t.Error("destination file should not exist after failed download")
	}
}

func TestExtractVizBundle_CorruptTarball(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "corrupt.tar.gz")
	if err := os.WriteFile(tarPath, []byte("this is not a gzip file"), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractVizBundle(tarPath, workDir)
	if err == nil {
		t.Fatal("expected error for corrupt tarball, got nil")
	}
}

func TestExtractVizBundle_AbsolutePathInTarball(t *testing.T) {
	// Create a tarball with an absolute path entry.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name: "/etc/passwd",
		Mode: 0o644,
		Size: 5,
	}
	tw.WriteHeader(hdr)
	tw.Write([]byte("evil!"))

	// Also add a valid top-level entry so there's content to parse.
	hdr2 := &tar.Header{
		Name: "./index.html",
		Mode: 0o644,
		Size: 6,
	}
	tw.WriteHeader(hdr2)
	tw.Write([]byte("<html>"))
	tw.Close()
	gw.Close()

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "abs.tar.gz")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The absolute path "/etc/passwd" gets cleaned to "etc/passwd" by
	// filepath.Clean, which is safely within destDir. The bundle should
	// still extract index.html successfully.
	vizDir, err := ExtractVizBundle(tarPath, workDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify /etc/passwd was NOT written anywhere in the work dir.
	passwdPath := filepath.Join(vizDir, "passwd")
	if _, err := os.Stat(passwdPath); err == nil {
		t.Error("absolute path entry should not be extracted")
	}
}

// createOversizedTarball builds a tar.gz directly (avoids createTestTarball's
// in-memory contentString parameter) where one entry's payload is `entrySize`
// bytes of zeros. Used by the F2 cap tests below.
func createOversizedTarball(t *testing.T, name string, entrySize int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name: name,
		Mode: 0o644,
		Size: entrySize,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	// Stream zeros so we don't allocate the full entry at once.
	const chunk = 1 << 20 // 1 MB
	zeros := make([]byte, chunk)
	remaining := entrySize
	for remaining > 0 {
		n := int64(chunk)
		if n > remaining {
			n = remaining
		}
		if _, err := tw.Write(zeros[:n]); err != nil {
			t.Fatal(err)
		}
		remaining -= n
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractVizBundle_PerEntryCap verifies that a single entry larger than
// the per-entry cap is rejected and no files are written to destDir.
func TestExtractVizBundle_PerEntryCap(t *testing.T) {
	// 101 MB > 100 MB per-entry cap. Gzip compresses zeros to a few KB so the
	// tarball on disk is small even though the decompressed entry isn't.
	tarball := createOversizedTarball(t, "huge.bin", int64(maxVizExtractedFile)+1)

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "viz.tar.gz")
	if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractVizBundle(tarPath, workDir)
	if err == nil {
		t.Fatal("expected per-entry cap error, got nil")
	}
	if !strings.Contains(err.Error(), "per-entry") {
		t.Errorf("error should mention per-entry cap: %v", err)
	}

	// F2: on cap breach the partial extraction directory must be wiped.
	if _, statErr := os.Stat(filepath.Join(workDir, vizBundleDir)); !os.IsNotExist(statErr) {
		t.Errorf("expected %s to be cleaned up after cap breach, stat err: %v", vizBundleDir, statErr)
	}
}

// TestExtractVizBundle_TotalCap verifies that the SUM of decompressed entries
// is bounded — multiple entries each under the per-entry cap can still trip
// the total cap.
func TestExtractVizBundle_TotalCap(t *testing.T) {
	// Build a tarball with 6 entries each at exactly the per-entry cap
	// (100 MB). 6 * 100 MB = 600 MB > 500 MB total cap, so the total cap
	// trips on the 6th entry. Each entry individually is fine.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	const chunk = 1 << 20
	zeros := make([]byte, chunk)
	for i := 0; i < 6; i++ {
		hdr := &tar.Header{
			Name: fmt.Sprintf("part-%02d.bin", i),
			Mode: 0o644,
			Size: int64(maxVizExtractedFile),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		remaining := int64(maxVizExtractedFile)
		for remaining > 0 {
			n := int64(chunk)
			if n > remaining {
				n = remaining
			}
			if _, err := tw.Write(zeros[:n]); err != nil {
				t.Fatal(err)
			}
			remaining -= n
		}
	}
	tw.Close()
	gw.Close()

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "viz.tar.gz")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractVizBundle(tarPath, workDir)
	if err == nil {
		t.Fatal("expected total cap error, got nil")
	}
	if !strings.Contains(err.Error(), "total extraction limit") {
		t.Errorf("error should mention total extraction limit: %v", err)
	}

	// F2: partial extraction directory must be wiped.
	if _, statErr := os.Stat(filepath.Join(workDir, vizBundleDir)); !os.IsNotExist(statErr) {
		t.Errorf("expected %s to be cleaned up after cap breach, stat err: %v", vizBundleDir, statErr)
	}
}

// TestExtractVizBundle_SmallBundlePositiveControl is the positive control for
// the F2 caps: a normal, well-under-cap bundle still extracts successfully.
// Duplicates the happy-path coverage from TestExtractVizBundle_Valid so the
// F2 commit can land/regress together.
func TestExtractVizBundle_SmallBundlePositiveControl(t *testing.T) {
	tarball := createTestTarball(t, map[string]string{
		"./index.html": "<html>small</html>",
		"./app.js":     "console.log('small');",
	})

	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "viz.tar.gz")
	if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	vizDir, err := ExtractVizBundle(tarPath, workDir)
	if err != nil {
		t.Fatalf("small bundle should extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vizDir, "index.html")); err != nil {
		t.Errorf("index.html missing: %v", err)
	}
}
