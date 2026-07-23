package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PB-28 regression coverage (duplicate-entry merge path): when Load merges the
// legacy one-entry-per-leaf duplicates into a single head entry (PB-16), an
// explicit trusted_runtimes decision — the empty "none" included — must never
// be dropped just because it rides on the entry whose OTHER fields lose the
// merge. Before the fix, both merge branches kept exactly one entry's trust
// field, so a hand-edited `trusted_runtimes: []` on the non-surviving
// duplicate vanished, the survivor stayed nil (legacy), and the trust
// migration re-seeded it from available_runtimes — granting CONTAINER in the
// same Load that discarded the explicit "none".

func loadTrustMergeConfig(t *testing.T, lines ...string) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := strings.Join(append(lines, ""), "\n")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// Replace branch: the kept entry is a bare leaf pin carrying the explicit
// "none"; the later head-level entry (no trusted_runtimes key) takes over the
// connection fields. The explicit empty must survive the takeover.
func TestExplicitTrustNone_SurvivesDuplicateMerge_HeadEntryLast(t *testing.T) {
	cfg := loadTrustMergeConfig(t,
		"available_runtimes: [WASM, CONTAINER]",
		"servers:",
		"  - grpc_address: head1.example.com:443",
		"    leaf_id: leaf-1",
		"    trusted_runtimes: []",
		"  - grpc_address: head1.example.com:443",
		"    name: head1.example.com",
	)

	if len(cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(cfg.Servers))
	}
	srv := cfg.Servers[0]
	if srv.TrustsRuntime("CONTAINER") {
		t.Errorf("explicit trusted_runtimes: [] on the merged-away duplicate was dropped and "+
			"re-seeded to %v (PB-28)", srv.EffectiveTrustedRuntimes())
	}
	if srv.TrustedRuntimes == nil {
		t.Error("merged trust is nil; the explicit empty must survive the merge explicitly")
	}
	if len(srv.PinnedLeafIDs) != 1 || srv.PinnedLeafIDs[0] != "leaf-1" {
		t.Errorf("pins = %v, want [leaf-1]", srv.PinnedLeafIDs)
	}
}

// Union branch: the head-level entry (no trusted_runtimes key) is kept and the
// later leaf-pin duplicate carries the explicit "none". The explicit empty
// must survive being on the dropped entry.
func TestExplicitTrustNone_SurvivesDuplicateMerge_HeadEntryFirst(t *testing.T) {
	cfg := loadTrustMergeConfig(t,
		"available_runtimes: [WASM, CONTAINER]",
		"servers:",
		"  - grpc_address: head1.example.com:443",
		"    name: head1.example.com",
		"  - grpc_address: head1.example.com:443",
		"    leaf_id: leaf-1",
		"    trusted_runtimes: []",
	)

	if len(cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(cfg.Servers))
	}
	srv := cfg.Servers[0]
	if srv.TrustsRuntime("CONTAINER") {
		t.Errorf("explicit trusted_runtimes: [] on the dropped duplicate was lost and "+
			"re-seeded to %v (PB-28)", srv.EffectiveTrustedRuntimes())
	}
	if srv.TrustedRuntimes == nil {
		t.Error("merged trust is nil; the explicit empty must survive the merge explicitly")
	}
}

// When BOTH entries carry an explicit decision, the merge keeps the
// intersection — the most restrictive reading, so merging duplicates can only
// ever narrow trust, never widen it.
func TestDuplicateMerge_ExplicitTrustIntersects(t *testing.T) {
	cfg := loadTrustMergeConfig(t,
		"available_runtimes: [WASM, CONTAINER]",
		"servers:",
		"  - grpc_address: head1.example.com:443",
		"    name: head1.example.com",
		"    trusted_runtimes: [CONTAINER, NATIVE]",
		"  - grpc_address: head1.example.com:443",
		"    leaf_id: leaf-1",
		"    trusted_runtimes: [CONTAINER]",
	)

	if len(cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(cfg.Servers))
	}
	srv := cfg.Servers[0]
	if srv.TrustsRuntime("NATIVE") {
		t.Errorf("merge widened trust to %v; two explicit decisions must intersect",
			srv.EffectiveTrustedRuntimes())
	}
	if !srv.TrustsRuntime("CONTAINER") {
		t.Errorf("runtime trusted by BOTH entries was lost; trusted = %v",
			srv.EffectiveTrustedRuntimes())
	}
}

// Control: two legacy duplicates with no trusted_runtimes key anywhere must
// still take the migration seed, exactly as a single legacy entry would.
func TestDuplicateMerge_BothLegacyStillMigrates(t *testing.T) {
	cfg := loadTrustMergeConfig(t,
		"available_runtimes: [WASM, CONTAINER]",
		"servers:",
		"  - grpc_address: head1.example.com:443",
		"    name: head1.example.com",
		"  - grpc_address: head1.example.com:443",
		"    leaf_id: leaf-1",
	)

	if len(cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(cfg.Servers))
	}
	srv := cfg.Servers[0]
	if !srv.TrustsRuntime("CONTAINER") {
		t.Errorf("legacy duplicates (no trusted_runtimes key) must still migrate from "+
			"available_runtimes; trusted = %v", srv.EffectiveTrustedRuntimes())
	}
}
