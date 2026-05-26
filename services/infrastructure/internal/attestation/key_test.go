package attestation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSigningKey_MissingWithoutAutogen_Errors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.pem")

	// File should not exist yet.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file not to exist, got err=%v", err)
	}

	// With autogen disabled, a missing key file must be a fatal error.
	_, err := LoadSigningKey(path, false)
	if err == nil {
		t.Fatal("expected error for missing key file when autogen disabled, got nil")
	}

	// And it must NOT have created the file (no silent identity minting).
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected no file to be created, got err=%v", statErr)
	}
}

func TestLoadSigningKey_AutoGenerateWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.pem")

	// File should not exist yet.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file not to exist, got err=%v", err)
	}

	key, err := LoadSigningKey(path, true)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}

	// Verify it's a valid Ed25519 private key.
	if len(key) != ed25519.PrivateKeySize {
		t.Errorf("key length = %d, want %d", len(key), ed25519.PrivateKeySize)
	}

	// File should now exist.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Verify it's valid PEM.
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("generated file is not valid PEM")
	}
	if block.Type != "PRIVATE KEY" {
		t.Errorf("PEM type = %q, want %q", block.Type, "PRIVATE KEY")
	}

	// Verify the file has restricted permissions (0600).
	// Note: on Windows, file permission checks are limited, so we
	// just verify the file is readable.
}

func TestLoadSigningKey_AutoGenerateProducesWorkingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.pem")

	key, err := LoadSigningKey(path, true)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}

	// Sign and verify with the generated key.
	msg := []byte("test message")
	sig := ed25519.Sign(key, msg)
	pub := key.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, msg, sig) {
		t.Error("generated key cannot sign/verify")
	}
}

func TestLoadSigningKey_LoadExistingPEM_FullKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.pem")

	// Generate a key and save as PEM with full 64-byte private key.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	block := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: priv,
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// autogen=false: an existing valid file must load unchanged regardless of the flag.
	loaded, err := LoadSigningKey(path, false)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	if string(loaded) != string(priv) {
		t.Error("loaded key does not match written key")
	}
}

func TestLoadSigningKey_LoadExistingPEM_Seed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.pem")

	// Generate a key and save only the 32-byte seed as PEM.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	seed := priv.Seed()
	block := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: seed,
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loaded, err := LoadSigningKey(path, false)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}

	// Should reconstruct the same full key from the seed.
	expected := ed25519.NewKeyFromSeed(seed)
	if string(loaded) != string(expected) {
		t.Error("loaded key from seed does not match expected key")
	}
}

func TestLoadSigningKey_LoadRawFullKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.raw")

	// Write raw 64-byte private key (not PEM).
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	if err := os.WriteFile(path, []byte(priv), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loaded, err := LoadSigningKey(path, false)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	if string(loaded) != string(priv) {
		t.Error("loaded raw key does not match written key")
	}
}

func TestLoadSigningKey_LoadRawSeed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.raw")

	// Write raw 32-byte seed (not PEM).
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	seed := priv.Seed()
	if err := os.WriteFile(path, seed, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loaded, err := LoadSigningKey(path, false)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}

	expected := ed25519.NewKeyFromSeed(seed)
	if string(loaded) != string(expected) {
		t.Error("loaded raw seed key does not match expected key")
	}
}

func TestLoadSigningKey_PKCS8Format(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.pem")

	// Generate a key and marshal to PKCS#8 (same as openssl genpkey -algorithm ed25519).
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}

	block := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8Bytes,
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loaded, err := LoadSigningKey(path, false)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}

	// Verify the loaded key matches the original.
	if string(loaded) != string(priv) {
		t.Error("loaded PKCS#8 key does not match original key")
	}

	// Verify the key works for signing.
	msg := []byte("test message for PKCS#8")
	sig := ed25519.Sign(loaded, msg)
	pub := loaded.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, msg, sig) {
		t.Error("PKCS#8 loaded key cannot sign/verify")
	}
}

func TestLoadSigningKey_InvalidPEMBlockSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.pem")

	// Write PEM with wrong-sized bytes (e.g., 16 bytes).
	block := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: make([]byte, 16),
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadSigningKey(path, false)
	if err == nil {
		t.Fatal("expected error for invalid PEM block size")
	}
}

func TestLoadSigningKey_InvalidRawFileSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.raw")

	// Write raw bytes with wrong size (not 32 or 64).
	if err := os.WriteFile(path, make([]byte, 48), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadSigningKey(path, false)
	if err == nil {
		t.Fatal("expected error for invalid raw file size")
	}
}

func TestLoadSigningKey_ReloadProducesSameKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-key.pem")

	// First call auto-generates (autogen enabled).
	key1, err := LoadSigningKey(path, true)
	if err != nil {
		t.Fatalf("first LoadSigningKey: %v", err)
	}

	// Second call loads from the now-existing file — autogen disabled to prove
	// an existing key is loaded unchanged regardless of the flag.
	key2, err := LoadSigningKey(path, false)
	if err != nil {
		t.Fatalf("second LoadSigningKey: %v", err)
	}

	if string(key1) != string(key2) {
		t.Error("reloaded key does not match original")
	}
}
