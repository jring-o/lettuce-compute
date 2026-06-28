package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Generate creates a new Ed25519 keypair.
func Generate() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating ed25519 keypair: %w", err)
	}
	return pub, priv, nil
}

// SaveKeyPair writes the private key (0600) and public key (0644) as raw bytes.
func SaveKeyPair(privPath, pubPath string, priv ed25519.PrivateKey, pub ed25519.PublicKey) error {
	if err := os.MkdirAll(filepath.Dir(privPath), 0700); err != nil {
		return fmt.Errorf("creating directory for private key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(pubPath), 0700); err != nil {
		return fmt.Errorf("creating directory for public key: %w", err)
	}
	if err := os.WriteFile(privPath, []byte(priv), 0600); err != nil {
		return fmt.Errorf("writing private key: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(pub), 0644); err != nil {
		return fmt.Errorf("writing public key: %w", err)
	}
	return nil
}

// LoadKeyPair reads the keypair from disk.
func LoadKeyPair(privPath, pubPath string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	privBytes, err := os.ReadFile(privPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading private key: %w", err)
	}
	pubBytes, err := os.ReadFile(pubPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading public key: %w", err)
	}
	if len(privBytes) != ed25519.PrivateKeySize {
		return nil, nil, fmt.Errorf("invalid private key size: got %d, want %d", len(privBytes), ed25519.PrivateKeySize)
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		return nil, nil, fmt.Errorf("invalid public key size: got %d, want %d", len(pubBytes), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(pubBytes), ed25519.PrivateKey(privBytes), nil
}

// PublicKeyToBase64URL encodes a public key as base64url without padding.
func PublicKeyToBase64URL(pub ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(pub)
}

// PublicKeyFromBase64URL decodes a base64url-encoded public key.
func PublicKeyFromBase64URL(encoded string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decoding base64url public key: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid public key size: got %d, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// LoadFailureRemedy returns a one-line, actionable remedy for a LoadKeyPair
// failure when the key files are PRESENT on disk but won't load — the data-dir
// relocation failure mode (TODO #25), e.g. a data dir copied to another user with
// the wrong ownership, or a partial/corrupt copy. It distinguishes a
// permission/ownership problem from a corrupt copy and, critically, NEVER tells
// the caller to run `init`: the files exist, so the account already has an
// identity, and re-running init would mint a NEW key and abandon the account's
// accrued credit. Callers should use this only after KeyPairExists is true; if
// the files are simply absent, `init` is the correct remedy.
//
// privPath/pubPath are named verbatim so the message points at the exact files.
func LoadFailureRemedy(loadErr error, privPath, pubPath string) string {
	if errors.Is(loadErr, fs.ErrPermission) {
		return fmt.Sprintf(
			"the key files exist but the current user cannot read them — the data dir was likely moved to another user "+
				"without fixing ownership. Give the running user ownership and lock down the private key: "+
				"`chown $(id -un) %s %s && chmod 600 %s` (the daemon must run as the owner of these files). "+
				"Do NOT run `init` — it creates a new identity and abandons this account's credit.",
			privPath, pubPath, privPath)
	}
	return fmt.Sprintf(
		"the key files exist but won't load (likely a partial or corrupt copy) — re-copy %s and %s from the original "+
			"data dir, then `chmod 600 %s`. Do NOT run `init` — it creates a new identity and abandons this account's credit.",
		privPath, pubPath, privPath)
}

// KeyPairExists returns true if both key files exist.
func KeyPairExists(privPath, pubPath string) bool {
	if _, err := os.Stat(privPath); err != nil {
		return false
	}
	if _, err := os.Stat(pubPath); err != nil {
		return false
	}
	return true
}
