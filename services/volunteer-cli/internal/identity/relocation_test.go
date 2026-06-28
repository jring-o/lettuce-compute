package identity

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadFailureRemedy_PermissionError covers the headline TODO #25 case: the
// key files were carried to another user but the running user can't read the
// private key (wrong owner/mode after chown). The remedy must name the ownership
// fix and must not advise running init (which would mint a new identity).
func TestLoadFailureRemedy_PermissionError(t *testing.T) {
	err := fmt.Errorf("reading private key: %w", fs.ErrPermission)
	remedy := LoadFailureRemedy(err, "/data/identity.key", "/data/identity.pub")
	for _, want := range []string{"chown", "chmod 600", "/data/identity.key", "/data/identity.pub"} {
		if !strings.Contains(remedy, want) {
			t.Errorf("remedy missing %q; got: %s", want, remedy)
		}
	}
	if strings.Contains(remedy, "lettuce-volunteer init") {
		t.Errorf("remedy must not advise running init; got: %s", remedy)
	}
}

// TestLoadFailureRemedy_CorruptCopy covers the partial/corrupt-copy case: the
// files exist but fail the size check. The remedy must suggest re-copying and
// must not advise init.
func TestLoadFailureRemedy_CorruptCopy(t *testing.T) {
	err := fmt.Errorf("invalid private key size: got 9, want 64")
	remedy := LoadFailureRemedy(err, "/data/identity.key", "/data/identity.pub")
	if !strings.Contains(remedy, "re-copy") {
		t.Errorf("remedy should suggest re-copying the key files; got: %s", remedy)
	}
	if strings.Contains(remedy, "lettuce-volunteer init") {
		t.Errorf("remedy must not advise running init; got: %s", remedy)
	}
}

// TestLoadKeyPair_RelocationPreservesIdentity is the TODO #25 definition-of-done
// guard: carrying identity.key/.pub to a new location (another user's data dir)
// yields the SAME identity. The key is raw bytes, independent of username or path,
// so a clean copy round-trips to an identical keypair — only a permission or
// corruption problem (covered above) breaks the load.
func TestLoadKeyPair_RelocationPreservesIdentity(t *testing.T) {
	src := t.TempDir()
	srcPriv := filepath.Join(src, "identity.key")
	srcPub := filepath.Join(src, "identity.pub")
	pub, priv, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if err := SaveKeyPair(srcPriv, srcPub, priv, pub); err != nil {
		t.Fatalf("SaveKeyPair() error: %v", err)
	}

	// Simulate a relocation: copy the raw key bytes into a different data dir.
	dst := t.TempDir()
	dstPriv := filepath.Join(dst, "identity.key")
	dstPub := filepath.Join(dst, "identity.pub")
	copyFile(t, srcPriv, dstPriv, 0o600)
	copyFile(t, srcPub, dstPub, 0o644)

	loadedPub, loadedPriv, err := LoadKeyPair(dstPriv, dstPub)
	if err != nil {
		t.Fatalf("LoadKeyPair after relocation: %v", err)
	}
	if !pub.Equal(loadedPub) || !priv.Equal(loadedPriv) {
		t.Error("relocated keypair does not match the original — identity should be preserved")
	}
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}
