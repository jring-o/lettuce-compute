package atproto

import "testing"

func TestCanonicalBytesGoldenVector(t *testing.T) {
	got, err := CanonicalBytes("did:plc:abc123", "did:key:z6MkTEST", "workstation", "2026-07-03T12:00:00Z")
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	const want = `{"createdAt":"2026-07-03T12:00:00Z","did":"did:plc:abc123","label":"workstation","operationalKey":"did:key:z6MkTEST"}`
	if string(got) != want {
		t.Fatalf("canonical bytes mismatch:\n got  %s\n want %s", got, want)
	}
}

func TestCanonicalBytesOmitsEmptyLabel(t *testing.T) {
	got, err := CanonicalBytes("did:plc:abc123", "did:key:z6MkTEST", "", "2026-07-03T12:00:00Z")
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	const want = `{"createdAt":"2026-07-03T12:00:00Z","did":"did:plc:abc123","operationalKey":"did:key:z6MkTEST"}`
	if string(got) != want {
		t.Fatalf("canonical bytes mismatch:\n got  %s\n want %s", got, want)
	}
}
