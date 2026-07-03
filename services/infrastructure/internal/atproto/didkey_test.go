package atproto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func TestBase58RoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		{0x00},
		{0x00, 0x00, 0x01},
		{0xff},
		{0xed, 0x01, 0xde, 0xad, 0xbe, 0xef},
		[]byte("hello world"),
	}
	for _, in := range cases {
		enc := base58Encode(in)
		dec, err := base58Decode(enc)
		if err != nil {
			t.Fatalf("base58Decode(%q) error: %v", enc, err)
		}
		if !bytes.Equal(dec, in) {
			t.Fatalf("round trip mismatch: in=%x enc=%q dec=%x", in, enc, dec)
		}
	}
}

func TestBase58KnownVector(t *testing.T) {
	// "hello world" -> "StV1DL6CwTryKyV" is a widely published base58btc vector.
	if got := base58Encode([]byte("hello world")); got != "StV1DL6CwTryKyV" {
		t.Fatalf("base58Encode(hello world) = %q, want StV1DL6CwTryKyV", got)
	}
}

func TestBase58LeadingZerosPreserved(t *testing.T) {
	in := []byte{0x00, 0x00, 0xab, 0xcd}
	enc := base58Encode(in)
	if !strings.HasPrefix(enc, "11") {
		t.Fatalf("expected two leading '1' for two leading zero bytes, got %q", enc)
	}
	dec, err := base58Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, in) {
		t.Fatalf("leading-zero round trip: got %x want %x", dec, in)
	}
}

func TestBase58RejectsInvalidChar(t *testing.T) {
	// '0', 'O', 'I', 'l' are excluded from the Bitcoin alphabet.
	if _, err := base58Decode("abc0def"); err == nil {
		t.Fatal("expected error for invalid base58 character '0'")
	}
}

func TestEd25519DIDKeyRoundTrip(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	did, err := EncodeEd25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(did, "did:key:z6Mk") {
		t.Fatalf("ed25519 did:key should begin did:key:z6Mk, got %q", did)
	}
	back, err := DecodeEd25519DIDKey(did)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, pub) {
		t.Fatalf("decoded key mismatch:\n got %x\nwant %x", back, pub)
	}
}

// TestEd25519DIDKeyWellKnownVector uses a published did:key example from the
// W3C did:key specification and asserts the decoder accepts it and the encoder
// reproduces it byte-for-byte.
func TestEd25519DIDKeyWellKnownVector(t *testing.T) {
	const known = "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"
	pub, err := DecodeEd25519DIDKey(known)
	if err != nil {
		t.Fatalf("decode well-known did:key: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("decoded key length = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	reencoded, err := EncodeEd25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if reencoded != known {
		t.Fatalf("re-encode mismatch:\n got %q\nwant %q", reencoded, known)
	}
}

func TestEncodeEd25519DIDKeyRejectsWrongLength(t *testing.T) {
	if _, err := EncodeEd25519DIDKey(ed25519.PublicKey(make([]byte, 31))); err == nil {
		t.Fatal("expected error for 31-byte key")
	}
}

func TestDecodeEd25519DIDKeyStrict(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	valid, _ := EncodeEd25519DIDKey(pub)

	t.Run("missing prefix", func(t *testing.T) {
		if _, err := DecodeEd25519DIDKey(strings.TrimPrefix(valid, "did:key:z")); err == nil {
			t.Fatal("expected error without did:key:z prefix")
		}
	})
	t.Run("wrong multibase tag", func(t *testing.T) {
		// Swap the multibase 'z' for something else.
		if _, err := DecodeEd25519DIDKey("did:key:Q" + strings.TrimPrefix(valid, "did:key:z")); err == nil {
			t.Fatal("expected error for non-z multibase tag")
		}
	})
	t.Run("wrong multicodec", func(t *testing.T) {
		// A did:key:z over a non-ed25519 multicodec prefix (0x00 0x00 + 32 bytes).
		bad := "did:key:z" + base58Encode(append([]byte{0x00, 0x00}, make([]byte, 32)...))
		if _, err := DecodeEd25519DIDKey(bad); err == nil {
			t.Fatal("expected error for non-ed25519 multicodec")
		}
	})
	t.Run("wrong length", func(t *testing.T) {
		bad := "did:key:z" + base58Encode(append([]byte{0xed, 0x01}, make([]byte, 8)...))
		if _, err := DecodeEd25519DIDKey(bad); err == nil {
			t.Fatal("expected error for short key body")
		}
	})
}
