package identity

import (
	"os"
	"path/filepath"
	"testing"
)

// A brand-new store over a missing file reports no id and no error, then round-trips
// a Set/Get for a single head.
func TestHostIDStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-ids.json")
	s := NewHostIDStore(path)

	got, err := s.Get("head-a:443")
	if err != nil {
		t.Fatalf("Get on missing file: %v", err)
	}
	if got != "" {
		t.Errorf("Get on missing file = %q, want empty", got)
	}

	if err := s.Set("head-a:443", "id-aaa"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err = s.Get("head-a:443")
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if got != "id-aaa" {
		t.Errorf("Get after Set = %q, want id-aaa", got)
	}
}

// Two heads keep independent ids: the whole point of per-head storage (audit F-2) is
// that one head's id never clobbers another's.
func TestHostIDStore_TwoHeadsIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-ids.json")
	s := NewHostIDStore(path)

	if err := s.Set("head-a:443", "id-aaa"); err != nil {
		t.Fatalf("Set head-a: %v", err)
	}
	if err := s.Set("head-b:443", "id-bbb"); err != nil {
		t.Fatalf("Set head-b: %v", err)
	}

	// A fresh store over the same file sees both (persistence, not in-memory state).
	reloaded := NewHostIDStore(path)
	if got, _ := reloaded.Get("head-a:443"); got != "id-aaa" {
		t.Errorf("head-a id = %q, want id-aaa", got)
	}
	if got, _ := reloaded.Get("head-b:443"); got != "id-bbb" {
		t.Errorf("head-b id = %q, want id-bbb", got)
	}

	// Overwriting one head leaves the other untouched.
	if err := reloaded.Set("head-a:443", "id-aaa2"); err != nil {
		t.Fatalf("overwrite head-a: %v", err)
	}
	if got, _ := reloaded.Get("head-a:443"); got != "id-aaa2" {
		t.Errorf("head-a id after overwrite = %q, want id-aaa2", got)
	}
	if got, _ := reloaded.Get("head-b:443"); got != "id-bbb" {
		t.Errorf("head-b id after head-a overwrite = %q, want id-bbb (untouched)", got)
	}
}

// Delete removes one head's id and leaves others; deleting an absent key is a no-op.
func TestHostIDStore_Delete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-ids.json")
	s := NewHostIDStore(path)

	if err := s.Set("head-a:443", "id-aaa"); err != nil {
		t.Fatalf("Set head-a: %v", err)
	}
	if err := s.Set("head-b:443", "id-bbb"); err != nil {
		t.Fatalf("Set head-b: %v", err)
	}

	if err := s.Delete("head-a:443"); err != nil {
		t.Fatalf("Delete head-a: %v", err)
	}
	if got, _ := s.Get("head-a:443"); got != "" {
		t.Errorf("head-a id after Delete = %q, want empty", got)
	}
	if got, _ := s.Get("head-b:443"); got != "id-bbb" {
		t.Errorf("head-b id after head-a Delete = %q, want id-bbb", got)
	}

	// Deleting an absent key is not an error.
	if err := s.Delete("head-c:443"); err != nil {
		t.Errorf("Delete absent key: %v", err)
	}
}

// Set with an empty id deletes the entry rather than persisting a blank value. This
// mirrors the register flow adopting an EMPTY response host id (unknown/revoked echo,
// or at-cap): the stored id is discarded and the machine runs host-less.
func TestHostIDStore_EmptySetDeletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-ids.json")
	s := NewHostIDStore(path)

	if err := s.Set("head-a:443", "id-aaa"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("head-a:443", ""); err != nil {
		t.Fatalf("Set empty: %v", err)
	}
	if got, _ := s.Get("head-a:443"); got != "" {
		t.Errorf("id after empty Set = %q, want empty", got)
	}
}

// A malformed backing file surfaces an error rather than being silently treated as
// empty (which would clobber other heads' ids on the next Set).
func TestHostIDStore_MalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-ids.json")
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatalf("seed malformed file: %v", err)
	}
	s := NewHostIDStore(path)
	if _, err := s.Get("head-a:443"); err == nil {
		t.Error("Get on malformed file: expected error, got nil")
	}
}
