package attestation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// chownChmodRemediation is the exact host-side fix line every refusal must
// carry. It is duplicated here (rather than referencing the production const) so
// the test pins the operator-facing string independently of the implementation.
const chownChmodRemediation = "sudo chown 10001:10001 keys/signing.key && chmod 600 keys/signing.key"

// writeKeyFixture writes a valid PEM Ed25519 private key at path, then forces the
// exact mode. os.WriteFile's mode argument is masked by umask, so an explicit
// os.Chmod is required to land the mode under test precisely.
func writeKeyFixture(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: priv}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), mode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
}

func TestLoadSigningKey_RefusesGroupOrOtherReadableKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission enforcement is a no-op on Windows")
	}
	for _, mode := range []os.FileMode{0644, 0640, 0604} {
		mode := mode
		t.Run(fmt.Sprintf("mode-%04o", mode), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "signing.key")
			writeKeyFixture(t, path, mode)

			_, err := LoadSigningKey(path, false)
			if err == nil {
				t.Fatalf("mode %04o: expected refusal, got nil", mode)
			}
			msg := err.Error()
			if !strings.Contains(msg, "insecure permissions") {
				t.Errorf("mode %04o: error %q missing %q", mode, msg, "insecure permissions")
			}
			if !strings.Contains(msg, chownChmodRemediation) {
				t.Errorf("mode %04o: error %q missing remediation %q", mode, msg, chownChmodRemediation)
			}
		})
	}
}

func TestLoadSigningKey_Loads0600Key(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission enforcement is a no-op on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "signing.key")
	writeKeyFixture(t, path, 0600)

	key, err := LoadSigningKey(path, false)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Errorf("key length = %d, want %d", len(key), ed25519.PrivateKeySize)
	}
}

func TestLoadSigningKey_RefusesWrongOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission enforcement is a no-op on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "signing.key")
	writeKeyFixture(t, path, 0600)

	// Override the euid seam so the file's real owner never matches. Restored on
	// return so other tests see the true euid.
	orig := processEUID
	processEUID = func() int { return orig() + 1 }
	defer func() { processEUID = orig }()

	_, err := LoadSigningKey(path, false)
	if err == nil {
		t.Fatal("expected refusal for wrong owner, got nil")
	}
	if msg := err.Error(); !strings.Contains(msg, "not owned by") {
		t.Errorf("error %q missing %q", msg, "not owned by")
	}
}

func TestLoadSigningKey_RefusesNonRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission enforcement is a no-op on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.key")
	writeKeyFixture(t, target, 0600)

	link := filepath.Join(dir, "signing.key")
	if err := os.Symlink(target, link); err != nil {
		if os.IsPermission(err) {
			t.Skipf("cannot create symlink in this environment: %v", err)
		}
		t.Fatalf("Symlink: %v", err)
	}

	_, err := LoadSigningKey(link, false)
	if err == nil {
		t.Fatal("expected refusal for non-regular (symlink) key file, got nil")
	}
	if msg := err.Error(); !strings.Contains(msg, "regular file") {
		t.Errorf("error %q should explain the key must be a regular file", msg)
	}
}

func TestLoadSigningKey_AutogenPathCompliant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission enforcement is a no-op on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "signing.key")

	key, err := LoadSigningKey(path, true)
	if err != nil {
		t.Fatalf("LoadSigningKey autogen: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Errorf("key length = %d, want %d", len(key), ed25519.PrivateKeySize)
	}

	// The freshly generated key must satisfy the same check LoadSigningKey
	// enforces on load.
	if err := checkKeyFilePermissions(path); err != nil {
		t.Errorf("autogen key failed its own permission check: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("autogen key mode = %04o, want 0600", perm)
	}
}
