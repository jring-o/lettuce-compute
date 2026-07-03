package atproto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCanonicalKeyAuthorizationGoldenVector(t *testing.T) {
	got := CanonicalKeyAuthorizationBytes(
		"did:plc:abc123",
		"did:key:z6MkTEST",
		"workstation",
		"2026-07-03T12:00:00Z",
	)
	const want = `{"createdAt":"2026-07-03T12:00:00Z","did":"did:plc:abc123","label":"workstation","operationalKey":"did:key:z6MkTEST"}`
	if string(got) != want {
		t.Fatalf("canonical bytes mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestCanonicalKeyAuthorizationOmitsEmptyLabel(t *testing.T) {
	got := CanonicalKeyAuthorizationBytes("did:plc:abc", "did:key:z6MkX", "", "2026-01-01T00:00:00Z")
	const want = `{"createdAt":"2026-01-01T00:00:00Z","did":"did:plc:abc","operationalKey":"did:key:z6MkX"}`
	if string(got) != want {
		t.Fatalf("canonical bytes with empty label:\n got %s\nwant %s", got, want)
	}
}

func TestBytesEnvelopeRoundTrip(t *testing.T) {
	original := Bytes{0x00, 0x01, 0xfe, 0xff, 0x2a}

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	const wantJSON = `{"$bytes":"AAH+/yo="}`
	if string(encoded) != wantJSON {
		t.Fatalf("marshal mismatch:\n got %s\nwant %s", encoded, wantJSON)
	}

	var decoded Bytes
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, original) {
		t.Fatalf("round trip: got %x want %x", decoded, original)
	}
}

func TestBytesUnmarshalVariants(t *testing.T) {
	t.Run("bare base64 string", func(t *testing.T) {
		var b Bytes
		if err := json.Unmarshal([]byte(`"AAH+/yo="`), &b); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, []byte{0x00, 0x01, 0xfe, 0xff, 0x2a}) {
			t.Fatalf("bare base64 decode: got %x", b)
		}
	})
	t.Run("null becomes nil", func(t *testing.T) {
		b := Bytes{0x01}
		if err := json.Unmarshal([]byte(`null`), &b); err != nil {
			t.Fatal(err)
		}
		if b != nil {
			t.Fatalf("null should decode to nil, got %x", b)
		}
	})
	t.Run("bad base64 errors", func(t *testing.T) {
		var b Bytes
		if err := json.Unmarshal([]byte(`{"$bytes":"not base64!!"}`), &b); err == nil {
			t.Fatal("expected error for invalid base64 in envelope")
		}
	})
}

func TestKeyAuthorizationRecordUnmarshalIgnoresUnknown(t *testing.T) {
	const raw = `{
		"$type": "compute.lettuce.keyAuthorization",
		"did": "did:plc:abc",
		"operationalKey": "did:key:z6MkX",
		"keySignature": {"$bytes":"AAH+/yo="},
		"label": "laptop",
		"createdAt": "2026-07-03T12:00:00Z",
		"somethingNew": 42
	}`
	var rec KeyAuthorizationRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("unmarshal with unknown fields: %v", err)
	}
	if rec.DID != "did:plc:abc" || rec.Label != "laptop" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if !bytes.Equal(rec.KeySignature, []byte{0x00, 0x01, 0xfe, 0xff, 0x2a}) {
		t.Fatalf("keySignature not decoded: %x", rec.KeySignature)
	}
}

// signedRecord builds a valid KeyAuthorizationRecord for the given key so each
// verification test can start from a passing record and mutate one field.
func signedRecord(t *testing.T, did, label, createdAt, expiresAt string, priv ed25519.PrivateKey) *KeyAuthorizationRecord {
	t.Helper()
	pub := priv.Public().(ed25519.PublicKey)
	opKey, err := EncodeEd25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	canonical := CanonicalKeyAuthorizationBytes(did, opKey, label, createdAt)
	sig := ed25519.Sign(priv, canonical)
	return &KeyAuthorizationRecord{
		DID:            did,
		OperationalKey: opKey,
		KeySignature:   sig,
		Label:          label,
		CreatedAt:      createdAt,
		ExpiresAt:      expiresAt,
	}
}

func TestVerifyKeyAuthorizationPass(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	t.Run("no expiry", func(t *testing.T) {
		rec := signedRecord(t, "did:plc:abc", "workstation", "2026-07-03T12:00:00Z", "", priv)
		if err := VerifyKeyAuthorization(rec, "did:plc:abc", pub, now); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})
	t.Run("future expiry", func(t *testing.T) {
		rec := signedRecord(t, "did:plc:abc", "", "2026-07-03T12:00:00Z", "2027-01-01T00:00:00Z", priv)
		if err := VerifyKeyAuthorization(rec, "did:plc:abc", pub, now); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})
}

func TestVerifyKeyAuthorizationFailures(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	t.Run("did mismatch", func(t *testing.T) {
		rec := signedRecord(t, "did:plc:abc", "", "2026-07-03T12:00:00Z", "", priv)
		err := VerifyKeyAuthorization(rec, "did:plc:someone-else", pub, now)
		if !errors.Is(err, ErrDIDMismatch) {
			t.Fatalf("want ErrDIDMismatch, got %v", err)
		}
	})

	t.Run("key mismatch against other expected key", func(t *testing.T) {
		rec := signedRecord(t, "did:plc:abc", "", "2026-07-03T12:00:00Z", "", priv)
		err := VerifyKeyAuthorization(rec, "did:plc:abc", otherPub, now)
		if !errors.Is(err, ErrKeyMismatch) {
			t.Fatalf("want ErrKeyMismatch, got %v", err)
		}
	})

	t.Run("operationalKey undecodable", func(t *testing.T) {
		rec := signedRecord(t, "did:plc:abc", "", "2026-07-03T12:00:00Z", "", priv)
		rec.OperationalKey = "did:key:zNOTVALID"
		err := VerifyKeyAuthorization(rec, "did:plc:abc", pub, now)
		if !errors.Is(err, ErrKeyMismatch) {
			t.Fatalf("want ErrKeyMismatch, got %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		rec := signedRecord(t, "did:plc:abc", "", "2026-07-03T12:00:00Z", "2026-07-03T11:00:00Z", priv)
		err := VerifyKeyAuthorization(rec, "did:plc:abc", pub, now)
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("want ErrExpired, got %v", err)
		}
	})

	t.Run("invalid expiresAt", func(t *testing.T) {
		rec := signedRecord(t, "did:plc:abc", "", "2026-07-03T12:00:00Z", "not-a-date", priv)
		err := VerifyKeyAuthorization(rec, "did:plc:abc", pub, now)
		if !errors.Is(err, ErrInvalidExpiresAt) {
			t.Fatalf("want ErrInvalidExpiresAt, got %v", err)
		}
	})

	t.Run("bad signature", func(t *testing.T) {
		rec := signedRecord(t, "did:plc:abc", "", "2026-07-03T12:00:00Z", "", priv)
		// Tamper with a field after signing so the signature no longer matches.
		rec.Label = "tampered"
		err := VerifyKeyAuthorization(rec, "did:plc:abc", pub, now)
		if !errors.Is(err, ErrBadSignature) {
			t.Fatalf("want ErrBadSignature, got %v", err)
		}
	})
}

func TestParseATURI(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		did, coll, rkey, err := ParseATURI("at://did:plc:abc/compute.lettuce.keyAuthorization/self")
		if err != nil {
			t.Fatal(err)
		}
		if did != "did:plc:abc" || coll != "compute.lettuce.keyAuthorization" || rkey != "self" {
			t.Fatalf("unexpected parse: %q %q %q", did, coll, rkey)
		}
	})

	bad := map[string]string{
		"missing scheme":   "did:plc:abc/coll/rkey",
		"too few parts":    "at://did:plc:abc/coll",
		"too many parts":   "at://did:plc:abc/coll/rkey/extra",
		"authority no did": "at://plc:abc/coll/rkey",
		"empty rkey":       "at://did:plc:abc/coll/",
	}
	for name, uri := range bad {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := ParseATURI(uri); err == nil {
				t.Fatalf("expected error for %q", uri)
			}
		})
	}
}
