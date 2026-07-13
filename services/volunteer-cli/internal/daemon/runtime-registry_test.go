package daemon

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// These tests use mockRuntime from daemon_test.go (same package).

func TestRuntimeRegistry_SelectNative(t *testing.T) {
	reg := NewRuntimeRegistry()
	reg.Register(&mockRuntime{name: "native", canHandle: true})

	wu := &runtime.WorkUnit{
		Runtime: "native",
		ExecutionSpec: runtime.ExecutionSpec{
			Binaries: map[string]string{"linux_amd64": "http://example.com/bin"},
		},
	}

	rt, err := reg.SelectRuntime(wu)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt.Name() != "native" {
		t.Errorf("got runtime %q, want %q", rt.Name(), "native")
	}
}

func TestRuntimeRegistry_SelectContainer(t *testing.T) {
	reg := NewRuntimeRegistry()
	reg.Register(&mockRuntime{name: "native", canHandle: true})
	reg.Register(&mockRuntime{name: "container", canHandle: true})

	wu := &runtime.WorkUnit{
		Runtime:       "container",
		ExecutionSpec: runtime.ExecutionSpec{Image: "test-image:latest"},
	}

	rt, err := reg.SelectRuntime(wu)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt.Name() != "container" {
		t.Errorf("got runtime %q, want %q", rt.Name(), "container")
	}
}

func TestRuntimeRegistry_NotFound(t *testing.T) {
	reg := NewRuntimeRegistry()
	reg.Register(&mockRuntime{name: "native", canHandle: true})

	wu := &runtime.WorkUnit{
		Runtime:       "container",
		ExecutionSpec: runtime.ExecutionSpec{Image: "test-image:latest"},
	}

	_, err := reg.SelectRuntime(wu)
	if err == nil {
		t.Fatal("expected error for missing runtime")
	}
}

func TestRuntimeRegistry_CannotHandle(t *testing.T) {
	reg := NewRuntimeRegistry()
	reg.Register(&mockRuntime{name: "native", canHandle: false})

	wu := &runtime.WorkUnit{
		Runtime: "native",
		ExecutionSpec: runtime.ExecutionSpec{
			Binaries: map[string]string{"linux_amd64": "http://example.com/bin"},
		},
	}

	_, err := reg.SelectRuntime(wu)
	if err == nil {
		t.Fatal("expected error when runtime cannot handle spec")
	}
}

// TestRuntimeRegistry_EmptyRuntimeRefused is the BG-12 exit test for finding #6:
// an empty runtime is refused UNCONDITIONALLY — even when native is registered it
// must NOT fall back to native. This fails on the pre-fix code, which defaulted an
// empty runtime to native (the "omit the field to get the least-isolated backend"
// bypass).
func TestRuntimeRegistry_EmptyRuntimeRefused(t *testing.T) {
	reg := NewRuntimeRegistry()
	reg.Register(&mockRuntime{name: "native", canHandle: true})

	wu := &runtime.WorkUnit{
		Runtime: "", // empty
		ExecutionSpec: runtime.ExecutionSpec{
			Binaries: map[string]string{"linux_amd64": "http://example.com/bin"},
		},
	}

	if _, err := reg.SelectRuntime(wu); err == nil {
		t.Fatal("expected an empty runtime to be refused, not defaulted to native")
	}
}

// TestRuntimeRegistry_UnknownRuntimeRefused: an unregistered runtime name is
// refused (a malicious head naming a bogus runtime cannot dodge WASM/container).
func TestRuntimeRegistry_UnknownRuntimeRefused(t *testing.T) {
	reg := NewRuntimeRegistry()
	reg.Register(&mockRuntime{name: "wasm", canHandle: true})

	wu := &runtime.WorkUnit{
		Runtime:       "definitely-not-a-runtime",
		ExecutionSpec: runtime.ExecutionSpec{Image: "x"},
	}

	if _, err := reg.SelectRuntime(wu); err == nil {
		t.Fatal("expected an unknown runtime to be refused")
	}
}

func TestRuntimeRegistry_AvailableRuntimes(t *testing.T) {
	reg := NewRuntimeRegistry()
	reg.Register(&mockRuntime{name: "native", canHandle: true})
	reg.Register(&mockRuntime{name: "container", canHandle: true})

	runtimes := reg.AvailableRuntimes()
	if len(runtimes) != 2 {
		t.Fatalf("got %d runtimes, want 2", len(runtimes))
	}

	found := make(map[string]bool)
	for _, name := range runtimes {
		found[name] = true
	}
	if !found["native"] || !found["container"] {
		t.Errorf("expected native and container, got %v", runtimes)
	}
}

func TestRuntimeRegistry_SelectWasm(t *testing.T) {
	reg := NewRuntimeRegistry()
	reg.Register(&mockRuntime{name: "native", canHandle: true})
	reg.Register(&mockRuntime{name: "wasm", canHandle: true})

	wu := &runtime.WorkUnit{
		Runtime: "wasm",
		ExecutionSpec: runtime.ExecutionSpec{
			Binaries: map[string]string{"wasm": "https://example.com/module.wasm"},
		},
	}

	rt, err := reg.SelectRuntime(wu)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt.Name() != "wasm" {
		t.Errorf("got runtime %q, want %q", rt.Name(), "wasm")
	}
}

func TestRuntimeRegistry_WasmAlwaysRegistered(t *testing.T) {
	reg := NewRuntimeRegistry()
	reg.Register(&mockRuntime{name: "native", canHandle: true})
	reg.Register(&mockRuntime{name: "wasm", canHandle: true})

	runtimes := reg.AvailableRuntimes()
	found := make(map[string]bool)
	for _, name := range runtimes {
		found[name] = true
	}
	if !found["wasm"] {
		t.Error("wasm runtime should always be registered")
	}
	if !found["native"] {
		t.Error("native runtime should always be registered")
	}
}

func TestRuntimeRegistry_WasmNotFoundWithoutRegistration(t *testing.T) {
	reg := NewRuntimeRegistry()
	reg.Register(&mockRuntime{name: "native", canHandle: true})

	wu := &runtime.WorkUnit{
		Runtime: "wasm",
		ExecutionSpec: runtime.ExecutionSpec{
			Binaries: map[string]string{"wasm": "https://example.com/module.wasm"},
		},
	}

	_, err := reg.SelectRuntime(wu)
	if err == nil {
		t.Fatal("expected error when wasm runtime is not registered")
	}
}
