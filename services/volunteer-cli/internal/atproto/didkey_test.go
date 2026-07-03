package atproto

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

// wellKnownEd25519DIDKey is the W3C did:key test vector for an Ed25519 key.
const wellKnownEd25519DIDKey = "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"

func TestDecodeEncodeWellKnownVector(t *testing.T) {
	pub, err := DecodeEd25519DIDKey(wellKnownEd25519DIDKey)
	if err != nil {
		t.Fatalf("decoding well-known did:key: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("decoded key length = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	if got := EncodeEd25519DIDKey(pub); got != wellKnownEd25519DIDKey {
		t.Fatalf("re-encoded did:key = %q, want %q", got, wellKnownEd25519DIDKey)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	// Deterministic key so the test is stable.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	encoded := EncodeEd25519DIDKey(pub)
	if encoded[:len(didKeyPrefix)] != didKeyPrefix {
		t.Fatalf("encoded did:key %q missing prefix %q", encoded, didKeyPrefix)
	}

	decoded, err := DecodeEd25519DIDKey(encoded)
	if err != nil {
		t.Fatalf("round-trip decode: %v", err)
	}
	if !bytes.Equal(decoded, pub) {
		t.Fatalf("round-trip key mismatch:\n got  %x\n want %x", decoded, pub)
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"wrong scheme":    "did:web:example.com",
		"missing z":       "did:key:6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK",
		"bad base58 char": "did:key:z0OIl",
		"empty":           "",
		"prefix only":     "did:key:z",
	}
	for name, input := range cases {
		if _, err := DecodeEd25519DIDKey(input); err == nil {
			t.Errorf("%s: expected error decoding %q, got nil", name, input)
		}
	}
}

func TestBase58RoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		{0x00},
		{0x00, 0x00, 0x01},
		{0xed, 0x01, 0xde, 0xad, 0xbe, 0xef},
		[]byte("hello world"),
	}
	for _, in := range cases {
		out, err := base58Decode(base58Encode(in))
		if err != nil {
			t.Fatalf("decode(encode(%x)): %v", in, err)
		}
		if !bytes.Equal(out, in) {
			t.Fatalf("base58 round-trip mismatch: got %x, want %x", out, in)
		}
	}
}
