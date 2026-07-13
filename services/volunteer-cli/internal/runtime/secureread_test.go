package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestReadRegularNoFollow_RegularFile: a normal output.dat reads back its bytes.
func TestReadRegularNoFollow_RegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.dat")
	want := []byte("result bytes")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readRegularNoFollow(path)
	if err != nil {
		t.Fatalf("readRegularNoFollow: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("read %q, want %q", got, want)
	}
}

// TestReadRegularNoFollow_SymlinkRefused is the core BG-15/BG-15b guard: an
// output.dat that is a symlink to the volunteer's signing key must read NOTHING.
// The reader must return errNotRegularFile and never expose the secret's contents.
func TestReadRegularNoFollow_SymlinkRefused(t *testing.T) {
	dir := t.TempDir()

	secret := filepath.Join(dir, "identity.key")
	secretBytes := []byte("PRIVATE-SIGNING-KEY-MUST-NOT-LEAK")
	if err := os.WriteFile(secret, secretBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "output.dat")
	if err := os.Symlink(secret, link); err != nil {
		// Windows without the developer/symlink privilege can't create symlinks.
		t.Skipf("cannot create symlink on this platform: %v", err)
	}

	got, err := readRegularNoFollow(link)
	if err == nil {
		t.Fatalf("expected refusal reading a symlinked output.dat, got %d bytes", len(got))
	}
	if !errors.Is(err, errNotRegularFile) {
		t.Fatalf("error = %v, want errNotRegularFile", err)
	}
	if string(got) == string(secretBytes) {
		t.Fatal("SECURITY: the symlinked secret's contents were returned")
	}
}

// TestReadRegularNoFollow_DirectoryRefused: a directory in output.dat's place is
// not a regular file and must be refused.
func TestReadRegularNoFollow_DirectoryRefused(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "output.dat")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := readRegularNoFollow(sub); !errors.Is(err, errNotRegularFile) {
		t.Fatalf("error = %v, want errNotRegularFile", err)
	}
}

// TestReadRegularNoFollow_SizeCapped is the BG-16f guard: the shared reader must NOT
// pull an oversized leaf-controlled file wholesale into daemon RAM. A file above the
// cap fails with errOutputTooLarge; a file at/under the cap reads back verbatim. Pre-
// fix the read ended in a bare io.ReadAll with no cap, so the over-cap case returned
// the full contents instead of erroring.
func TestReadRegularNoFollow_SizeCapped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.dat")
	if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cap below the file size: overflow must be an explicit error, not a truncated read.
	if got, err := readRegularNoFollowLimited(path, 10); !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("over-cap read: err = %v (got %d bytes), want errOutputTooLarge", err, len(got))
	}
	// Cap at/above the file size: full contents, no error, no off-by-one truncation.
	got, err := readRegularNoFollowLimited(path, 100)
	if err != nil {
		t.Fatalf("at-cap read: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("at-cap read returned %d bytes, want 100", len(got))
	}
}
