package cli

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

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
