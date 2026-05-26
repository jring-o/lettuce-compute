package identity

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGenerate(t *testing.T) {
	pub, priv, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Errorf("public key size = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	if len(priv) != ed25519.PrivateKeySize {
		t.Errorf("private key size = %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
}

func TestSaveLoadKeyPair(t *testing.T) {
	pub, priv, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	dir := t.TempDir()
	privPath := filepath.Join(dir, "identity.key")
	pubPath := filepath.Join(dir, "identity.pub")

	if err := SaveKeyPair(privPath, pubPath, priv, pub); err != nil {
		t.Fatalf("SaveKeyPair() error: %v", err)
	}

	loadedPub, loadedPriv, err := LoadKeyPair(privPath, pubPath)
	if err != nil {
		t.Fatalf("LoadKeyPair() error: %v", err)
	}

	if !pub.Equal(loadedPub) {
		t.Error("loaded public key does not match original")
	}
	if !priv.Equal(loadedPriv) {
		t.Error("loaded private key does not match original")
	}
}

func TestSaveKeyPairPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permissions not enforced on Windows")
	}

	pub, priv, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	dir := t.TempDir()
	privPath := filepath.Join(dir, "identity.key")
	pubPath := filepath.Join(dir, "identity.pub")

	if err := SaveKeyPair(privPath, pubPath, priv, pub); err != nil {
		t.Fatalf("SaveKeyPair() error: %v", err)
	}

	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("Stat private key: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("private key permissions = %o, want 0600", perm)
	}
}

func TestBase64URLRoundTrip(t *testing.T) {
	pub, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	encoded := PublicKeyToBase64URL(pub)
	if encoded == "" {
		t.Fatal("PublicKeyToBase64URL returned empty string")
	}

	decoded, err := PublicKeyFromBase64URL(encoded)
	if err != nil {
		t.Fatalf("PublicKeyFromBase64URL() error: %v", err)
	}

	if !pub.Equal(decoded) {
		t.Error("round-tripped public key does not match original")
	}
}

func TestPublicKeyFromBase64URLInvalid(t *testing.T) {
	_, err := PublicKeyFromBase64URL("not-valid-base64url!!!")
	if err == nil {
		t.Error("expected error for invalid base64url, got nil")
	}

	// Valid base64url but wrong size.
	_, err = PublicKeyFromBase64URL("AAAA")
	if err == nil {
		t.Error("expected error for wrong-size key, got nil")
	}
}

func TestKeyPairExists(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "identity.key")
	pubPath := filepath.Join(dir, "identity.pub")

	if KeyPairExists(privPath, pubPath) {
		t.Error("KeyPairExists should return false when files don't exist")
	}

	pub, priv, _ := Generate()
	_ = SaveKeyPair(privPath, pubPath, priv, pub)

	if !KeyPairExists(privPath, pubPath) {
		t.Error("KeyPairExists should return true after saving")
	}
}

func TestLoadKeyPairInvalidSize(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "identity.key")
	pubPath := filepath.Join(dir, "identity.pub")

	os.WriteFile(privPath, []byte("short"), 0600)
	os.WriteFile(pubPath, []byte("short"), 0644)

	_, _, err := LoadKeyPair(privPath, pubPath)
	if err == nil {
		t.Error("expected error for invalid key sizes, got nil")
	}
}
