package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// base62Alphabet contains the characters used for base62 encoding.
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ApiKey represents a stored API key record. The plaintext key is never stored.
type ApiKey struct {
	ID         types.ID   `json:"id"`
	UserID     types.ID   `json:"user_id"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	KeyHash    []byte     `json:"key_hash"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// GenerateKey creates a new API key. It returns the plaintext key (shown once),
// the visible prefix for identification, and the SHA-256 hash for storage.
func GenerateKey() (plaintextKey string, keyPrefix string, keyHash []byte, err error) {
	// Generate 32 cryptographically random bytes.
	randomBytes := make([]byte, 32)
	if _, err = rand.Read(randomBytes); err != nil {
		return "", "", nil, err
	}

	// Encode as base62.
	encoded := base62Encode(randomBytes)

	// Full plaintext key with lk_ prefix.
	plaintextKey = "lk_" + encoded

	// Visible prefix: lk_ + first 9 chars of the base62-encoded portion (12 chars total).
	keyPrefix = "lk_" + encoded[:9]

	// SHA-256 hash of the full plaintext key (including lk_ prefix).
	hash := sha256.Sum256([]byte(plaintextKey))
	keyHash = hash[:]

	return plaintextKey, keyPrefix, keyHash, nil
}

// HashKey computes the SHA-256 hash of a plaintext API key.
// Used during authentication to look up the key by hash.
func HashKey(plaintextKey string) []byte {
	hash := sha256.Sum256([]byte(plaintextKey))
	return hash[:]
}

// base62Encode encodes a byte slice as a base62 string.
func base62Encode(data []byte) string {
	n := new(big.Int).SetBytes(data)
	base := big.NewInt(62)
	zero := big.NewInt(0)
	mod := new(big.Int)

	var result []byte
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		result = append(result, base62Alphabet[mod.Int64()])
	}

	// Reverse the result.
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	if len(result) == 0 {
		return "0"
	}
	return string(result)
}
