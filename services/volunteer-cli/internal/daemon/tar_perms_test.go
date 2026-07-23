package daemon

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

// PB-29 regression coverage (unit half; the real-engine test in
// checkpoint_realengine_test.go proves it against a live hardened container):
// extractTar restores a head-downloaded checkpoint for a unit reassigned
// ACROSS volunteers, and the hardened container runs as nobody (65534:65534).
// Restored entries must come out sandbox-writable — dirs 0o777, files 0o666,
// the same contract makeSandboxWritable applies to the fresh bind dirs
// (PB-23) — or the resumed leaf gets EACCES overwriting its own restored
// state in place. POSIX-only: Windows file modes are ACL-governed and the
// container bind crosses a VM share that masks them.
func TestExtractTar_RestoredEntriesSandboxWritable(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX file modes are not enforceable on Windows; covered in the podman VM")
	}

	// A checkpoint shaped like a real one: a top-level state file, a subdir, and
	// a nested file — all carrying the restrictive modes a leaf typically wrote
	// them with on the ORIGINAL volunteer.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range []struct {
		name     string
		typeflag byte
		mode     int64
		body     string
	}{
		{name: "checkpoint.dat", typeflag: tar.TypeReg, mode: 0o644, body: "seq=7"},
		{name: "state", typeflag: tar.TypeDir, mode: 0o755},
		{name: "state/nested.dat", typeflag: tar.TypeReg, mode: 0o600, body: "inner"},
	} {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: e.mode, Size: int64(len(e.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := extractTar(buf.Bytes(), dst); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	for _, want := range []struct {
		rel  string
		perm os.FileMode
	}{
		{"checkpoint.dat", 0o666},
		{"state", 0o777},
		{"state/nested.dat", 0o666},
	} {
		info, err := os.Stat(filepath.Join(dst, want.rel))
		if err != nil {
			t.Fatalf("stat %s: %v", want.rel, err)
		}
		if got := info.Mode().Perm(); got != want.perm {
			t.Errorf("%s restored %04o, want sandbox-writable %04o (PB-29)", want.rel, got, want.perm)
		}
	}
}
