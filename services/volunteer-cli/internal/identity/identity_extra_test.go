package identity

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadKeyPairInvalidPublicKeySize(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "identity.key")
	pubPath := filepath.Join(dir, "identity.pub")

	// Write a valid-size private key (64 bytes) but invalid-size public key.
	validPriv := make([]byte, ed25519.PrivateKeySize)
	os.WriteFile(privPath, validPriv, 0600)
	os.WriteFile(pubPath, []byte("short"), 0644)

	_, _, err := LoadKeyPair(privPath, pubPath)
	if err == nil {
		t.Error("expected error for invalid public key size")
	}
}

func TestLoadKeyPairMissingPrivateKey(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "identity.key")
	pubPath := filepath.Join(dir, "identity.pub")

	// Only write the public key file.
	pub := make([]byte, ed25519.PublicKeySize)
	os.WriteFile(pubPath, pub, 0644)

	_, _, err := LoadKeyPair(privPath, pubPath)
	if err == nil {
		t.Error("expected error when private key file is missing")
	}
}

func TestLoadKeyPairMissingPublicKey(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "identity.key")
	pubPath := filepath.Join(dir, "identity.pub")

	// Only write the private key file.
	priv := make([]byte, ed25519.PrivateKeySize)
	os.WriteFile(privPath, priv, 0600)

	_, _, err := LoadKeyPair(privPath, pubPath)
	if err == nil {
		t.Error("expected error when public key file is missing")
	}
}

func TestKeyPairExistsPartialFiles(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "identity.key")
	pubPath := filepath.Join(dir, "identity.pub")

	// Only private key exists.
	os.WriteFile(privPath, []byte("data"), 0600)
	if KeyPairExists(privPath, pubPath) {
		t.Error("KeyPairExists should return false when only private key exists")
	}

	// Remove private, add public.
	os.Remove(privPath)
	os.WriteFile(pubPath, []byte("data"), 0644)
	if KeyPairExists(privPath, pubPath) {
		t.Error("KeyPairExists should return false when only public key exists")
	}
}

func TestSaveKeyPairCreatesNestedDirectories(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "deep", "nested", "identity.key")
	pubPath := filepath.Join(dir, "deep", "nested", "identity.pub")

	pub, priv, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	if err := SaveKeyPair(privPath, pubPath, priv, pub); err != nil {
		t.Fatalf("SaveKeyPair() error: %v", err)
	}

	if _, err := os.Stat(privPath); err != nil {
		t.Errorf("private key file not created: %v", err)
	}
	if _, err := os.Stat(pubPath); err != nil {
		t.Errorf("public key file not created: %v", err)
	}
}

func TestGenerateProducesUniqueKeys(t *testing.T) {
	pub1, priv1, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	pub2, priv2, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	if pub1.Equal(pub2) {
		t.Error("two generated public keys should not be equal")
	}
	if priv1.Equal(priv2) {
		t.Error("two generated private keys should not be equal")
	}
}

func TestGenerateSignVerify(t *testing.T) {
	pub, priv, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	message := []byte("hello lettuce")
	sig := ed25519.Sign(priv, message)

	if !ed25519.Verify(pub, message, sig) {
		t.Error("signature verification failed for generated keypair")
	}
}
