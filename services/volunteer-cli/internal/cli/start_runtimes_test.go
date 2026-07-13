package cli

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// advertisedRuntimes reports the UPPERCASE enum names for exactly what's
// registered (registry Name()s are lowercase), sorted for stable output. A
// registry without a container runtime — e.g. a box with no Docker/Podman —
// therefore advertises only NATIVE/WASM, never CONTAINER.
func TestAdvertisedRuntimes_UppercasesSortsAndReflectsRegistry(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	reg := daemon.NewRuntimeRegistry()
	reg.Register(runtime.NewWasmRuntime(t.TempDir(), logger))   // registered out of order
	reg.Register(runtime.NewNativeRuntime(t.TempDir(), logger)) // ...to prove sorting

	got := advertisedRuntimes(reg)
	want := []string{"NATIVE", "WASM"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("advertisedRuntimes() = %v, want %v (uppercase, sorted, container absent)", got, want)
	}
}

// TestBuildRuntimeRegistry_NativeGate is the BG-12 exit test (a / a'): native is
// registered ONLY when allow_native_runtime is set. A default config — where an
// upgraded pre-release config also lands, since AllowNativeRuntime's zero value is
// false — registers wasm but NOT native, so native is never advertised and a
// native (or empty) work unit is refused downstream. The test would fail on a build
// that registered native unconditionally or gated it on available_runtimes.
func TestBuildRuntimeRegistry_NativeGate(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Native OFF (the default, and where an upgraded pre-release config lands).
	off := config.Defaults()
	off.DataDir = t.TempDir()
	if off.AllowNativeRuntime {
		t.Fatal("precondition: Defaults() must have AllowNativeRuntime=false")
	}
	regOff, _, _ := buildRuntimeRegistry(off, logger)
	if regOff.GetRuntime("native") != nil {
		t.Error("native must NOT be registered when allow_native_runtime is false")
	}
	if regOff.GetRuntime("wasm") == nil {
		t.Error("wasm must always be registered")
	}

	// Native ON only with the explicit opt-in.
	on := config.Defaults()
	on.DataDir = t.TempDir()
	on.AllowNativeRuntime = true
	regOn, _, _ := buildRuntimeRegistry(on, logger)
	if regOn.GetRuntime("native") == nil {
		t.Error("native must be registered when allow_native_runtime is true")
	}
}
