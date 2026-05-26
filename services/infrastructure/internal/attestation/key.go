package attestation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
)

// LoadSigningKey reads an Ed25519 private key from a file.
// The file can be PEM-encoded (PRIVATE KEY) or raw 64-byte seed+key.
//
// The signing key is the platform's external trust anchor: published
// attestations are verified against the corresponding public key. If the key
// file is missing, LoadSigningKey FAILS CLOSED by default and returns an error
// explaining how to generate one — silently minting a fresh signing identity
// would invalidate every previously published attestation.
//
// Auto-generation is a development-only convenience and must be explicitly
// opted into by passing autogen=true. When enabled and the file is missing, a
// new keypair is generated, written to path, and a LOUD warning is logged.
func LoadSigningKey(path string, autogen bool) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if autogen {
				return generateAndSave(path)
			}
			return nil, fmt.Errorf(
				"signing key file %q does not exist: refusing to start without a signing key. "+
					"Generate a persistent Ed25519 key (e.g. `openssl genpkey -algorithm ed25519 -out %s`) "+
					"and set LETTUCE_SIGNING_PRIVATE_KEY_PATH to its path. "+
					"For local development only, set LETTUCE_SIGNING_KEY_AUTOGEN=true to auto-generate an ephemeral key",
				path, path)
		}
		return nil, fmt.Errorf("read signing key: %w", err)
	}

	// Try PEM first.
	block, _ := pem.Decode(data)
	if block != nil {
		if len(block.Bytes) == ed25519.PrivateKeySize {
			return ed25519.PrivateKey(block.Bytes), nil
		}
		if len(block.Bytes) == ed25519.SeedSize {
			return ed25519.NewKeyFromSeed(block.Bytes), nil
		}
		// Try PKCS#8 format (produced by openssl genpkey -algorithm ed25519).
		if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			if key, ok := parsed.(ed25519.PrivateKey); ok {
				return key, nil
			}
			return nil, fmt.Errorf("PKCS#8 key is not Ed25519")
		}
		return nil, fmt.Errorf("PEM block has unexpected size %d (expected %d or %d, or PKCS#8)",
			len(block.Bytes), ed25519.PrivateKeySize, ed25519.SeedSize)
	}

	// Raw bytes.
	if len(data) == ed25519.PrivateKeySize {
		return ed25519.PrivateKey(data), nil
	}
	if len(data) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(data), nil
	}

	return nil, fmt.Errorf("signing key file has unexpected size %d", len(data))
}

func generateAndSave(path string) (ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}

	block := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: priv,
	}

	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		return nil, fmt.Errorf("write signing key: %w", err)
	}

	slog.Warn("generated a NEW ephemeral signing key — attestations signed before this will not verify; "+
		"set LETTUCE_SIGNING_PRIVATE_KEY_PATH to a persistent pre-generated key for production",
		"path", path,
		"public_key", base64.StdEncoding.EncodeToString(pub),
	)

	return priv, nil
}
