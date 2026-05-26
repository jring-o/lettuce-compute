package apikey

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestGenerateKey_Prefix(t *testing.T) {
	key, _, _, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !strings.HasPrefix(key, "lk_") {
		t.Errorf("key should start with lk_, got %q", key[:10])
	}
}

func TestGenerateKey_PrefixLength(t *testing.T) {
	_, prefix, _, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if len(prefix) != 12 {
		t.Errorf("key prefix length = %d, want 12", len(prefix))
	}
	if !strings.HasPrefix(prefix, "lk_") {
		t.Errorf("key prefix should start with lk_, got %q", prefix)
	}
}

func TestGenerateKey_PrefixMatchesKey(t *testing.T) {
	key, prefix, _, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !strings.HasPrefix(key, prefix) {
		t.Errorf("key %q does not start with prefix %q", key, prefix)
	}
}

func TestGenerateKey_HashLength(t *testing.T) {
	_, _, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if len(hash) != sha256.Size {
		t.Errorf("hash length = %d, want %d", len(hash), sha256.Size)
	}
}

func TestGenerateKey_HashDeterministic(t *testing.T) {
	key, _, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	// Re-compute hash from the key.
	recomputed := sha256.Sum256([]byte(key))
	for i := range hash {
		if hash[i] != recomputed[i] {
			t.Fatalf("hash mismatch at byte %d: got %02x, want %02x", i, hash[i], recomputed[i])
		}
	}
}

func TestGenerateKey_Uniqueness(t *testing.T) {
	key1, _, _, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey 1: %v", err)
	}
	key2, _, _, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey 2: %v", err)
	}
	if key1 == key2 {
		t.Error("two generated keys should be different")
	}
}

func TestGenerateKey_Base62Characters(t *testing.T) {
	key, _, _, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	// Strip the lk_ prefix and check remaining chars are base62.
	encoded := key[3:]
	for i, c := range encoded {
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
			t.Errorf("invalid character %q at position %d in encoded key", c, i)
		}
	}
}

func TestHashKey(t *testing.T) {
	key, _, expectedHash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	got := HashKey(key)
	if len(got) != sha256.Size {
		t.Fatalf("HashKey length = %d, want %d", len(got), sha256.Size)
	}
	for i := range expectedHash {
		if got[i] != expectedHash[i] {
			t.Fatalf("HashKey mismatch at byte %d", i)
		}
	}
}

func TestHashKey_Static(t *testing.T) {
	// Verify HashKey produces a known SHA-256 digest for a fixed input.
	// SHA-256("lk_test123") is deterministic — this guards against
	// accidentally changing the hash algorithm.
	input := "lk_test123"
	expected := sha256.Sum256([]byte(input))
	got := HashKey(input)
	if len(got) != sha256.Size {
		t.Fatalf("HashKey length = %d, want %d", len(got), sha256.Size)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Fatalf("HashKey static mismatch at byte %d: got %02x, want %02x", i, got[i], expected[i])
		}
	}
}

func TestGenerateKey_MinimumLength(t *testing.T) {
	// 32 random bytes base62-encoded should produce at least 40 characters.
	// With the "lk_" prefix the full key should be >= 43 characters.
	key, _, _, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if len(key) < 43 {
		t.Errorf("key length = %d, expected >= 43 (3 prefix + ~40 base62)", len(key))
	}
}

func TestBase62Encode_ZeroInput(t *testing.T) {
	// All-zero bytes encode to "0" via the len(result)==0 branch.
	result := base62Encode([]byte{0, 0, 0})
	if result != "0" {
		t.Errorf("base62Encode(all zeros) = %q, want %q", result, "0")
	}
}

func TestBase62Encode_EmptyInput(t *testing.T) {
	// Empty input should return "0".
	result := base62Encode([]byte{})
	if result != "0" {
		t.Errorf("base62Encode(empty) = %q, want %q", result, "0")
	}
}

func TestBase62Encode_SingleByte(t *testing.T) {
	// base62Encode([]byte{1}) should be "1" (big.Int value 1, mod 62 = 1).
	result := base62Encode([]byte{1})
	if result != "1" {
		t.Errorf("base62Encode([]byte{1}) = %q, want %q", result, "1")
	}

	// base62Encode([]byte{61}) should be "z" (last char in base62 alphabet).
	result = base62Encode([]byte{61})
	if result != "z" {
		t.Errorf("base62Encode([]byte{61}) = %q, want %q", result, "z")
	}

	// base62Encode([]byte{62}) should be "10" (one full cycle).
	result = base62Encode([]byte{62})
	if result != "10" {
		t.Errorf("base62Encode([]byte{62}) = %q, want %q", result, "10")
	}
}
