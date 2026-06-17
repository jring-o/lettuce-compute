package cli

import (
	"io"
	"log/slog"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TestDedupeServersByAddress verifies the daemon opens one connection per head
// address: duplicate cfg.Servers entries for the same gRPC address collapse to a
// single entry, preferring a head-level (no LeafID) entry over a leaf-scoped one
// so the surviving connection serves all the head's leafs. Prevents the
// double-connection / double-RPC-rate symptom from the v0.4.0 alpha test.
func TestDedupeServersByAddress(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("two entries for one head collapse to one", func(t *testing.T) {
		in := []config.ServerConfig{
			{GRPCAddress: "lbry.science:443", Name: "lbry", LeafPreferences: config.LeafPreferences{Mode: "BLOCKLIST"}},
			{GRPCAddress: "lbry.science:443", Name: "lbry", LeafPreferences: config.LeafPreferences{Mode: "ALL"}},
		}
		got := dedupeServersByAddress(in, discard)
		if len(got) != 1 {
			t.Fatalf("got %d entries, want 1", len(got))
		}
	})

	t.Run("head-level entry preferred over leaf-scoped", func(t *testing.T) {
		in := []config.ServerConfig{
			{GRPCAddress: "lbry.science:443", LeafID: "85baa67f"}, // leaf-scoped first
			{GRPCAddress: "lbry.science:443", LeafID: ""},         // head-level second
		}
		got := dedupeServersByAddress(in, discard)
		if len(got) != 1 {
			t.Fatalf("got %d entries, want 1", len(got))
		}
		if got[0].LeafID != "" {
			t.Errorf("kept LeafID=%q, want head-level entry (empty LeafID)", got[0].LeafID)
		}
	})

	t.Run("distinct heads preserved", func(t *testing.T) {
		in := []config.ServerConfig{
			{GRPCAddress: "lbry.science:443"},
			{GRPCAddress: "infra.scios.tech:443"},
		}
		got := dedupeServersByAddress(in, discard)
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2 (distinct heads must not be collapsed)", len(got))
		}
	})
}

func TestDefaultConfigIncludesWasm(t *testing.T) {
	cfg := config.Defaults()

	found := false
	for _, rt := range cfg.AvailableRuntimes {
		if rt == "WASM" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("default AvailableRuntimes = %v, should include WASM", cfg.AvailableRuntimes)
	}
}

func TestWasmAddedToConfigIfMissing(t *testing.T) {
	// Simulate an old config that only has NATIVE.
	runtimes := []string{"NATIVE"}

	if !containsRuntime(runtimes, "WASM") {
		runtimes = append(runtimes, "WASM")
	}

	found := false
	for _, rt := range runtimes {
		if rt == "WASM" {
			found = true
			break
		}
	}
	if !found {
		t.Error("WASM should be added to AvailableRuntimes if missing")
	}
}

func TestWasmNoDuplicateWhenAlreadyPresent(t *testing.T) {
	runtimes := []string{"NATIVE", "WASM"}

	if !containsRuntime(runtimes, "WASM") {
		runtimes = append(runtimes, "WASM")
	}

	count := 0
	for _, rt := range runtimes {
		if rt == "WASM" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("WASM appears %d times, should appear exactly once", count)
	}
}

func TestContainerConfigPreservesWasm(t *testing.T) {
	// Simulate config with NATIVE + CONTAINER but no WASM.
	runtimes := []string{"NATIVE", "CONTAINER"}

	if !containsRuntime(runtimes, "WASM") {
		runtimes = append(runtimes, "WASM")
	}

	if len(runtimes) != 3 {
		t.Errorf("expected 3 runtimes, got %d: %v", len(runtimes), runtimes)
	}
	if !containsRuntime(runtimes, "NATIVE") || !containsRuntime(runtimes, "CONTAINER") || !containsRuntime(runtimes, "WASM") {
		t.Errorf("expected NATIVE, CONTAINER, WASM, got %v", runtimes)
	}
}
