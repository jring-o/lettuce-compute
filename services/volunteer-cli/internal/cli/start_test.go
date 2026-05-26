package cli

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

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
