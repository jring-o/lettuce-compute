package daemon

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTarDirectoryRoundTrip(t *testing.T) {
	// Create a temp directory with files.
	srcDir := t.TempDir()
	checkpointDir := filepath.Join(srcDir, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write some test files.
	if err := os.WriteFile(filepath.Join(checkpointDir, "state.bin"), []byte("binary-state-data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "progress.json"), []byte(`{"step":42}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Tar it.
	blob, err := tarDirectory(checkpointDir)
	if err != nil {
		t.Fatal("tarDirectory failed:", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob")
	}

	// Extract to a new directory.
	dstDir := t.TempDir()
	extractDir := filepath.Join(dstDir, "restored")
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(blob, extractDir); err != nil {
		t.Fatal("extractTar failed:", err)
	}

	// Verify files.
	data, err := os.ReadFile(filepath.Join(extractDir, "state.bin"))
	if err != nil {
		t.Fatal("reading state.bin:", err)
	}
	if string(data) != "binary-state-data" {
		t.Errorf("state.bin content mismatch: got %q", string(data))
	}

	data, err = os.ReadFile(filepath.Join(extractDir, "progress.json"))
	if err != nil {
		t.Fatal("reading progress.json:", err)
	}
	if string(data) != `{"step":42}` {
		t.Errorf("progress.json content mismatch: got %q", string(data))
	}
}

func TestTarDirectoryWithNestedDirs(t *testing.T) {
	srcDir := t.TempDir()
	nested := filepath.Join(srcDir, "subdir", "deep")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "file.txt"), []byte("nested-content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "top.txt"), []byte("top-level"), 0644); err != nil {
		t.Fatal(err)
	}

	blob, err := tarDirectory(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob")
	}

	dstDir := t.TempDir()
	if err := extractTar(blob, dstDir); err != nil {
		t.Fatal(err)
	}

	// Verify nested file.
	data, err := os.ReadFile(filepath.Join(dstDir, "subdir", "deep", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "nested-content" {
		t.Errorf("nested file content mismatch: got %q", string(data))
	}

	// Verify top-level file.
	data, err = os.ReadFile(filepath.Join(dstDir, "top.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "top-level" {
		t.Errorf("top file content mismatch: got %q", string(data))
	}
}

func TestTarDirectoryEmpty(t *testing.T) {
	emptyDir := t.TempDir()

	blob, err := tarDirectory(emptyDir)
	if err != nil {
		t.Fatal(err)
	}
	if blob != nil {
		t.Errorf("expected nil blob for empty dir, got %d bytes", len(blob))
	}
}

func TestTarDirectoryNonexistent(t *testing.T) {
	blob, err := tarDirectory(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatal("expected no error for nonexistent dir, got:", err)
	}
	if blob != nil {
		t.Error("expected nil blob for nonexistent dir")
	}
}

func TestExtractTarPathTraversal(t *testing.T) {
	// Manually test that extractTar rejects ".." in paths.
	// We can't easily create a malicious tar with archive/tar without raw bytes,
	// but we can verify that the check exists by confirming normal paths work.
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "safe.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}

	blob, err := tarDirectory(srcDir)
	if err != nil {
		t.Fatal(err)
	}

	dstDir := t.TempDir()
	if err := extractTar(blob, dstDir); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dstDir, "safe.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ok" {
		t.Errorf("expected 'ok', got %q", string(data))
	}
}

func TestExtractTarPathTraversal_Malicious(t *testing.T) {
	// Create a tar blob with a ".." path traversal entry.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Write a malicious entry.
	hdr := &tar.Header{
		Name: "../../../etc/evil.txt",
		Mode: 0644,
		Size: int64(len("malicious")),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("malicious")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dstDir := t.TempDir()
	err := extractTar(buf.Bytes(), dstDir)
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
	if !strings.Contains(err.Error(), "path traversal") {
		t.Errorf("expected 'path traversal' in error, got: %v", err)
	}
}

// buildCheckpointTar constructs a tar blob (no gzip — extractTar takes raw
// tar bytes, not tar.gz) with one entry whose payload is `entrySize` zero
// bytes. Used by the F2 cap tests below to fabricate oversized checkpoints
// without allocating the full payload up front for many entries.
func buildCheckpointTar(t *testing.T, entries []struct {
	name string
	size int64
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	const chunk = 1 << 20
	zeros := make([]byte, chunk)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0644, Size: e.size}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		remaining := e.size
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
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractTar_PerEntryCap verifies that a single oversized entry is
// rejected and the partially-written file is removed.
func TestExtractTar_PerEntryCap(t *testing.T) {
	blob := buildCheckpointTar(t, []struct {
		name string
		size int64
	}{
		{name: "huge.bin", size: int64(maxCheckpointEntry) + 1}, // 100MB + 1
	})

	dstDir := t.TempDir()
	err := extractTar(blob, dstDir)
	if err == nil {
		t.Fatal("expected per-entry cap error, got nil")
	}
	if !strings.Contains(err.Error(), "per-entry") {
		t.Errorf("error should mention per-entry cap: %v", err)
	}

	// The partial file the extractor opened must have been cleaned up.
	if _, statErr := os.Stat(filepath.Join(dstDir, "huge.bin")); !os.IsNotExist(statErr) {
		t.Errorf("expected huge.bin to be cleaned up after cap breach, stat err: %v", statErr)
	}
}

// TestExtractTar_TotalCap verifies the bundle-wide cap trips when entries
// individually fit but their sum exceeds the total cap, and that all
// partially-written files are wiped.
func TestExtractTar_TotalCap(t *testing.T) {
	// 6 * 100 MB = 600 MB > 500 MB total cap. Each entry sits exactly at
	// the per-entry cap so the per-entry check (strict `>`) doesn't fire.
	specs := make([]struct {
		name string
		size int64
	}, 6)
	for i := range specs {
		specs[i] = struct {
			name string
			size int64
		}{
			name: fmt.Sprintf("part-%02d.bin", i),
			size: int64(maxCheckpointEntry),
		}
	}
	blob := buildCheckpointTar(t, specs)

	dstDir := t.TempDir()
	err := extractTar(blob, dstDir)
	if err == nil {
		t.Fatal("expected total cap error, got nil")
	}
	if !strings.Contains(err.Error(), "total extraction limit") {
		t.Errorf("error should mention total extraction limit: %v", err)
	}

	// All files that the extractor opened must have been cleaned up.
	for _, e := range specs {
		path := filepath.Join(dstDir, e.name)
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to be cleaned up after cap breach, stat err: %v", e.name, statErr)
		}
	}
}

// TestExtractTar_SmallPositiveControl ensures the caps don't reject a
// legitimate small checkpoint. Mirrors the F2 dashboard positive control.
func TestExtractTar_SmallPositiveControl(t *testing.T) {
	srcDir := t.TempDir()
	checkpoint := filepath.Join(srcDir, "ck")
	if err := os.MkdirAll(checkpoint, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpoint, "state.bin"), []byte("small-state"), 0644); err != nil {
		t.Fatal(err)
	}
	blob, err := tarDirectory(checkpoint)
	if err != nil {
		t.Fatal(err)
	}

	dstDir := t.TempDir()
	if err := extractTar(blob, dstDir); err != nil {
		t.Fatalf("small checkpoint should extract: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dstDir, "state.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "small-state" {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestTarDirectoryNotADir(t *testing.T) {
	// When the checkpoint path exists but is a file (not a directory),
	// tarDirectory should return an error.
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	blob, err := tarDirectory(filePath)
	if err == nil {
		t.Fatal("expected error for non-directory path, got nil")
	}
	if blob != nil {
		t.Error("expected nil blob on error")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected 'not a directory' in error, got: %v", err)
	}
}

// TestTarDirectory_SymlinkNotFollowed is the BG-15c guard: the checkpoint archiver
// must not follow a symlink planted in the leaf-controlled checkpoint dir. Before the
// explicit refusal this was only INCIDENTALLY safe — a followed symlink whose target
// was larger than its Size:0 tar header aborted the whole archive with "write too
// long". Now the symlink is skipped: the target's bytes never enter the head-uploaded
// blob, and an otherwise-valid checkpoint still archives.
func TestTarDirectory_SymlinkNotFollowed(t *testing.T) {
	root := t.TempDir()

	// A secret OUTSIDE the checkpoint dir that a symlink target would leak.
	secret := filepath.Join(root, "identity.key")
	secretBytes := []byte("PRIVATE-SIGNING-KEY-MUST-NOT-LEAK-INTO-CHECKPOINT")
	if err := os.WriteFile(secret, secretBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	ckpt := filepath.Join(root, "checkpoint")
	if err := os.MkdirAll(ckpt, 0o755); err != nil {
		t.Fatal(err)
	}
	// A legitimate regular checkpoint file.
	if err := os.WriteFile(filepath.Join(ckpt, "state.dat"), []byte("real-checkpoint"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A malicious symlink pointing at the secret.
	if err := os.Symlink(secret, filepath.Join(ckpt, "leak")); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}

	blob, err := tarDirectory(ckpt)
	if err != nil {
		t.Fatalf("tarDirectory aborted on a symlinked entry (want skip): %v", err)
	}
	if bytes.Contains(blob, secretBytes) {
		t.Fatal("SECURITY: the symlink target's secret bytes were archived into the checkpoint blob")
	}

	// The archive still carries the legitimate file and NOT the symlink entry.
	tr := tar.NewReader(bytes.NewReader(blob))
	names := map[string]bool{}
	for {
		hdr, e := tr.Next()
		if e != nil {
			break
		}
		names[hdr.Name] = true
	}
	if !names["state.dat"] {
		t.Errorf("legitimate checkpoint file missing from archive: %v", names)
	}
	if names["leak"] {
		t.Errorf("symlink entry was archived: %v", names)
	}
}
