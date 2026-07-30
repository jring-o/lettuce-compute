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

// TestBuildRuntimeRegistry_NativeGate is the BG-12 exit test under PER-HEAD TRUST: native
// is built ONLY when at least one attached head is trusted to run it
// (ServerConfig.TrustedRuntimes contains NATIVE — chosen at attach). No trusted head
// builds wasm but NOT native, so native is never advertised and a native (or empty) work
// unit is refused downstream. The test would fail on a build that registered native
// unconditionally or gated it on anything but per-head trust. (The retired global keys
// available_runtimes / allow_native_runtime no longer exist to key on — TB-25/TQ-22 —
// and config.Load pins a nil trusted_runtimes to WASM-only, covered by
// TestMigrateServerRuntimeTrust in the config package.)
func TestBuildRuntimeRegistry_NativeGate(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Native OFF: no attached head is trusted for NATIVE (a WASM-only server). wasm still builds.
	off := config.Defaults()
	off.DataDir = t.TempDir()
	off.Servers = []config.ServerConfig{{GRPCAddress: "h1:443"}} // nil TrustedRuntimes => WASM only
	regOff, _ := buildRuntimeRegistry(off, logger)
	if regOff.GetRuntime("native") != nil {
		t.Error("native must NOT be registered when no attached head is trusted for NATIVE")
	}
	if regOff.GetRuntime("wasm") == nil {
		t.Error("wasm must always be registered")
	}

	// Native ON: a head is explicitly trusted for NATIVE (the attach-time opt-in).
	on := config.Defaults()
	on.DataDir = t.TempDir()
	on.Servers = []config.ServerConfig{{GRPCAddress: "h1:443", TrustedRuntimes: []string{"NATIVE"}}}
	regOn, _ := buildRuntimeRegistry(on, logger)
	if regOn.GetRuntime("native") == nil {
		t.Error("native must be registered when a head is trusted for NATIVE")
	}
}

// TestAdvertisedForServer_IntersectsCapabilityAndTrust proves the per-head advertise gate:
// a head hears a runtime only when the machine can run it (it's in the registry) AND the
// volunteer trusts that head for it. WASM is always trusted; NATIVE only when opted in.
func TestAdvertisedForServer_IntersectsCapabilityAndTrust(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// A machine capable of wasm + native (no container backend registered).
	reg := daemon.NewRuntimeRegistry()
	reg.Register(runtime.NewWasmRuntime(t.TempDir(), logger))
	reg.Register(runtime.NewNativeRuntime(t.TempDir(), logger))

	// Head trusted only for WASM (default) hears only WASM, even though the machine can run native.
	wasmOnly := config.ServerConfig{GRPCAddress: "h1:443"}
	if got, want := advertisedForServer(reg, wasmOnly), []string{"WASM"}; !reflect.DeepEqual(got, want) {
		t.Errorf("wasm-only head: advertisedForServer = %v, want %v", got, want)
	}

	// Head trusted for NATIVE hears NATIVE + WASM (sorted).
	nativeTrusted := config.ServerConfig{GRPCAddress: "h2:443", TrustedRuntimes: []string{"NATIVE"}}
	if got, want := advertisedForServer(reg, nativeTrusted), []string{"NATIVE", "WASM"}; !reflect.DeepEqual(got, want) {
		t.Errorf("native-trusted head: advertisedForServer = %v, want %v", got, want)
	}

	// Head trusted for CONTAINER hears only WASM here, because the machine has no container
	// runtime registered — trust cannot conjure a capability the machine lacks.
	containerTrusted := config.ServerConfig{GRPCAddress: "h3:443", TrustedRuntimes: []string{"CONTAINER"}}
	if got, want := advertisedForServer(reg, containerTrusted), []string{"WASM"}; !reflect.DeepEqual(got, want) {
		t.Errorf("container-trusted head on backend-less machine: advertisedForServer = %v, want %v", got, want)
	}
}
