package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TB-62 regression coverage: desktop-v2.0.0's attach and `init --server` before
// v0.12.0 stored a typed "https://host" verbatim as the gRPC target — a shape
// gRPC resolves to nothing — and PR #197 (TB-51) normalised only the paths
// that STORE a head. An existing entry survived the update unrepaired and
// failed "name resolver error" on every start until it was detached and
// re-added. Load now repairs such entries itself.

// writeServersConfig writes a config holding exactly the given servers block.
func writeServersConfig(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := strings.Join(append([]string{"servers:"}, lines...), "\n") + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTB62_LoadRepairsDesktopV200StoredURL feeds Load the EXACT entry the
// desktop app's 2.0.0 Add Server dialog wrote: the raw URL as gRPC address AND
// as name, no HTTP address, an explicit trust decision.
func TestTB62_LoadRepairsDesktopV200StoredURL(t *testing.T) {
	path := writeServersConfig(t,
		"  - grpc_address: https://lbry.science",
		"    name: https://lbry.science",
		"    trusted_runtimes:",
		"      - CONTAINER",
	)

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(loaded.Servers))
	}
	s := loaded.Servers[0]
	if s.GRPCAddress != "lbry.science:443" {
		t.Errorf("grpc_address = %q, want %q (the stored URL was never repaired)", s.GRPCAddress, "lbry.science:443")
	}
	if s.HTTPAddress != "https://lbry.science" {
		t.Errorf("http_address = %q, want %q", s.HTTPAddress, "https://lbry.science")
	}
	if s.Name != "lbry.science" {
		t.Errorf("name = %q, want %q", s.Name, "lbry.science")
	}
	if s.Insecure {
		t.Error("an https:// entry must not become insecure")
	}
	if !s.TrustsRuntime("CONTAINER") {
		t.Errorf("trust lost in the repair; trusted = %v", s.EffectiveTrustedRuntimes())
	}
	repairs := loaded.ServerAddressRepairs()
	if len(repairs) != 1 {
		t.Fatalf("ServerAddressRepairs = %v, want exactly one line for the start-up log", repairs)
	}
	for _, want := range []string{`"https://lbry.science"`, "lbry.science:443"} {
		if !strings.Contains(repairs[0], want) {
			t.Errorf("repair line %q does not name %s", repairs[0], want)
		}
	}
}

// TestTB62_LoadRepairsLegacyInitStoredURL covers the shape `init --server
// https://host` wrote before v0.12.0: name "https" (the text before the last
// colon) and HTTP address "https://https". Pins and weight ride through.
func TestTB62_LoadRepairsLegacyInitStoredURL(t *testing.T) {
	path := writeServersConfig(t,
		"  - grpc_address: https://lbry.science",
		"    http_address: https://https",
		"    name: https",
		"    weight: 40",
		"    pinned_leaf_ids:",
		"      - leaf-a",
		"    trusted_runtimes: []",
	)

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(loaded.Servers))
	}
	s := loaded.Servers[0]
	if s.GRPCAddress != "lbry.science:443" || s.HTTPAddress != "https://lbry.science" || s.Name != "lbry.science" {
		t.Errorf("entry = %s / %s / %q, want lbry.science:443 / https://lbry.science / \"lbry.science\"", s.GRPCAddress, s.HTTPAddress, s.Name)
	}
	if s.Weight != 40 {
		t.Errorf("weight = %d, want 40 kept", s.Weight)
	}
	if len(s.PinnedLeafIDs) != 1 || s.PinnedLeafIDs[0] != "leaf-a" {
		t.Errorf("pins = %v, want [leaf-a] kept", s.PinnedLeafIDs)
	}
	if s.TrustedRuntimes == nil || len(s.TrustedRuntimes) != 0 {
		t.Errorf("trusted_runtimes = %v, want the explicit WASM-only decision kept", s.TrustedRuntimes)
	}
}

// TestTB62_LoadRepairsHTTPSchemeAsInsecure: an http:// URL names a plain-text
// head, so the repaired entry carries the port the scheme implies and the
// insecure flag, exactly as ParseHeadAddress gives a fresh attach.
func TestTB62_LoadRepairsHTTPSchemeAsInsecure(t *testing.T) {
	path := writeServersConfig(t,
		"  - grpc_address: http://localhost:9090/",
		"    name: http://localhost:9090/",
	)

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := loaded.Servers[0]
	if s.GRPCAddress != "localhost:9090" || s.HTTPAddress != "http://localhost:9090" || s.Name != "localhost" {
		t.Errorf("entry = %s / %s / %q, want localhost:9090 / http://localhost:9090 / \"localhost\"", s.GRPCAddress, s.HTTPAddress, s.Name)
	}
	if !s.Insecure {
		t.Error("an http:// entry must be repaired as insecure, or the dial would attempt TLS against a plain-text head")
	}
}

// TestTB62_RepairKeepsAVolunteerChosenName: only the scheme-derived junk is
// renamed; a name the volunteer typed stays.
func TestTB62_RepairKeepsAVolunteerChosenName(t *testing.T) {
	path := writeServersConfig(t,
		"  - grpc_address: https://lbry.science",
		"    name: Beyblade head",
	)

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := loaded.Servers[0]
	if s.GRPCAddress != "lbry.science:443" {
		t.Errorf("grpc_address = %q, want lbry.science:443", s.GRPCAddress)
	}
	if s.Name != "Beyblade head" {
		t.Errorf("name = %q, want the volunteer's own name kept", s.Name)
	}
}

// TestTB62_WellFormedEntriesAreLeftExactlyAsStored guards the other side: the
// gRPC address keys the per-head host-id store, so an entry that could have
// dialled — including a case variant, a bare host and an IPv6 literal — must
// not be rewritten, and nothing is reported. A URL ParseHeadAddress refuses is
// left alone too, for start to report the dial failure as before.
func TestTB62_WellFormedEntriesAreLeftExactlyAsStored(t *testing.T) {
	entries := []string{"lbry.science:443", "Lbry.Science:443", "lbry.science", "[::1]:8443", "https://"}
	var lines []string
	for _, e := range entries {
		lines = append(lines, "  - grpc_address: \""+e+"\"", "    name: kept-"+strings.Map(func(r rune) rune {
			if r == ':' || r == '/' || r == '[' || r == ']' {
				return '_'
			}
			return r
		}, e))
	}
	path := writeServersConfig(t, lines...)

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Servers) != len(entries) {
		t.Fatalf("servers = %d, want %d untouched entries", len(loaded.Servers), len(entries))
	}
	for i, e := range entries {
		if got := loaded.Servers[i].GRPCAddress; got != e {
			t.Errorf("entry %d: grpc_address = %q, want %q left as stored", i, got, e)
		}
		if got := loaded.Servers[i].HTTPAddress; got != "" {
			t.Errorf("entry %d: http_address = %q, want empty (nothing derived for an untouched entry)", i, got)
		}
	}
	if repairs := loaded.ServerAddressRepairs(); len(repairs) != 0 {
		t.Errorf("ServerAddressRepairs = %v, want none", repairs)
	}
}

// TestTB62_RepairedAndCleanEntriesMerge: the "add it again" workaround without
// the detach leaves a 2.0.0 URL entry beside a 2.0.1 host:port entry for the
// same head. They must collapse into ONE entry keyed on the dialable address,
// whichever order the file holds them in: the clean entry's connection fields
// win (it is the one that reached the head), pins are unioned, and trust is
// merged under the PB-28 rule (the intersection of two explicit decisions).
func TestTB62_RepairedAndCleanEntriesMerge(t *testing.T) {
	repairedEntry := []string{
		"  - grpc_address: https://lbry.science",
		"    name: https://lbry.science",
		"    pinned_leaf_ids: [leaf-old]",
		"    trusted_runtimes: [CONTAINER, NATIVE]",
	}
	cleanEntry := []string{
		"  - grpc_address: lbry.science:443",
		"    http_address: https://lbry.science",
		"    name: lbry",
		"    weight: 60",
		"    pinned_leaf_ids: [leaf-new]",
		"    trusted_runtimes: [CONTAINER]",
	}
	orders := map[string][]string{
		"repaired first": append(append([]string{}, repairedEntry...), cleanEntry...),
		"clean first":    append(append([]string{}, cleanEntry...), repairedEntry...),
	}
	for name, lines := range orders {
		t.Run(name, func(t *testing.T) {
			loaded, err := Load(writeServersConfig(t, lines...))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(loaded.Servers) != 1 {
				t.Fatalf("servers = %d, want the two spellings merged into 1; have %+v", len(loaded.Servers), loaded.Servers)
			}
			s := loaded.Servers[0]
			if s.GRPCAddress != "lbry.science:443" {
				t.Errorf("grpc_address = %q, want lbry.science:443", s.GRPCAddress)
			}
			if s.Name != "lbry" || s.Weight != 60 {
				t.Errorf("name/weight = %q/%d, want the clean entry's \"lbry\"/60", s.Name, s.Weight)
			}
			pins := strings.Join(s.PinnedLeafIDs, ",")
			if !strings.Contains(pins, "leaf-old") || !strings.Contains(pins, "leaf-new") {
				t.Errorf("pins = %v, want the union of both entries' pins", s.PinnedLeafIDs)
			}
			if !s.TrustsRuntime("CONTAINER") || s.TrustsRuntime("NATIVE") {
				t.Errorf("trusted = %v, want the intersection [CONTAINER] (PB-28: merging never widens trust)", s.EffectiveTrustedRuntimes())
			}
		})
	}
}

// TestTB62_RepairPersistsOnSaveAndIsIdempotent: the next Save writes the
// repaired entry, and loading that file again repairs nothing and reports
// nothing.
func TestTB62_RepairPersistsOnSaveAndIsIdempotent(t *testing.T) {
	path := writeServersConfig(t,
		"  - grpc_address: https://lbry.science",
		"    http_address: https://https",
		"    name: https",
	)

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.ServerAddressRepairs()) != 1 {
		t.Fatalf("first load reported %v, want one repair", loaded.ServerAddressRepairs())
	}
	if err := loaded.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "grpc_address: https://lbry.science") || !strings.Contains(string(raw), "grpc_address: lbry.science:443") {
		t.Errorf("saved file still carries the URL as the gRPC address:\n%s", raw)
	}

	again, err := Load(path)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if repairs := again.ServerAddressRepairs(); len(repairs) != 0 {
		t.Errorf("second load reported %v, want nothing to repair", repairs)
	}
	if again.Servers[0].GRPCAddress != "lbry.science:443" || again.Servers[0].Name != "lbry.science" {
		t.Errorf("second load = %+v, want the repaired entry unchanged", again.Servers[0])
	}
}
