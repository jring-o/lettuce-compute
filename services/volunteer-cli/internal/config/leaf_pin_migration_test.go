package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PB-16 regression coverage (config half): the old attach flow APPENDED a whole
// duplicate server entry `{grpc_address, http_address, leaf_id, name}` per leaf
// pin — dropping the head entry's --insecure/--trust settings — and the daemon
// then collapsed the duplicate at startup, silently discarding the pin. Load
// now migrates that legacy shape to ONE entry per head with the pins preserved
// on it (PinnedLeafIDs) and the head-level entry's connection fields kept.

// TestLoad_MergesLegacyDuplicateServerEntries feeds Load the EXACT config shape
// the campaign repro produced (head entry with insecure+trust, plus the
// appended duplicate carrying only the leaf pin).
func TestLoad_MergesLegacyDuplicateServerEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := strings.Join([]string{
		"available_runtimes: [WASM]",
		"servers:",
		"  - grpc_address: 127.0.0.1:18081",
		"    http_address: http://127.0.0.1:18080",
		"    name: 127.0.0.1",
		"    insecure: true",
		"    trusted_runtimes:",
		"      - NATIVE",
		"  - grpc_address: 127.0.0.1:18081",
		"    http_address: http://127.0.0.1:18080",
		"    leaf_id: 85baa67f-4b31-4f34-9d1e-30c1a2b3c4d5",
		"    name: 127.0.0.1",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Servers) != 1 {
		t.Fatalf("servers = %d, want 1 (legacy duplicate merged)", len(loaded.Servers))
	}
	srv := loaded.Servers[0]
	if len(srv.PinnedLeafIDs) != 1 || srv.PinnedLeafIDs[0] != "85baa67f-4b31-4f34-9d1e-30c1a2b3c4d5" {
		t.Errorf("pins = %v, want the legacy leaf_id preserved as a pin (PB-16)", srv.PinnedLeafIDs)
	}
	if !srv.Insecure {
		t.Error("head entry's insecure flag lost in the merge")
	}
	if !srv.TrustsRuntime("NATIVE") {
		t.Errorf("head entry's trust lost in the merge; trusted = %v", srv.EffectiveTrustedRuntimes())
	}
	if srv.LeafID != "" {
		t.Errorf("retired leaf_id still set after migration: %q", srv.LeafID)
	}
}

// TestLoad_MergesLeafEntryBeforeHeadEntry covers the reversed order: the bare
// leaf-pin entry appears FIRST in the file. The head-level entry's connection
// fields must still win.
func TestLoad_MergesLeafEntryBeforeHeadEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := strings.Join([]string{
		"servers:",
		"  - grpc_address: head:443",
		"    leaf_id: leaf-a",
		"    name: head",
		"  - grpc_address: head:443",
		"    name: head",
		"    insecure: true",
		"    trusted_runtimes: []",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(loaded.Servers))
	}
	srv := loaded.Servers[0]
	if len(srv.PinnedLeafIDs) != 1 || srv.PinnedLeafIDs[0] != "leaf-a" {
		t.Errorf("pins = %v, want [leaf-a]", srv.PinnedLeafIDs)
	}
	if !srv.Insecure {
		t.Error("head-level entry's fields must win regardless of file order")
	}
	if srv.TrustsRuntime("CONTAINER") {
		t.Errorf("explicit empty trust must survive the merge; trusted = %v", srv.EffectiveTrustedRuntimes())
	}
}

// TestLoad_DistinctHeadsNotMerged: entries for different addresses stay apart.
func TestLoad_DistinctHeadsNotMerged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := strings.Join([]string{
		"servers:",
		"  - grpc_address: head-a:443",
		"    name: head-a",
		"  - grpc_address: head-b:443",
		"    name: head-b",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Servers) != 2 {
		t.Fatalf("servers = %d, want 2 (distinct heads must not be merged)", len(loaded.Servers))
	}
}
