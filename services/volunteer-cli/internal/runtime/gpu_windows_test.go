//go:build windows

package runtime

import (
	"context"
	"os/exec"
	"sync"
	"testing"
)

// On Windows, GPU detection must never launch rocm-smi or amd-smi:
// amd-smi.exe ships with AMD's display drivers and requests UAC elevation
// when it starts, so every launch raised an elevation prompt and then
// blocked until the detection timeout. AMD (and every other vendor's)
// adapters come from the registry instead; nvidia-smi alone is still run.
func TestWindowsDetectGPUs_NeverInvokesAMDCommandLineTools(t *testing.T) {
	disableSkipHardwareDetection(t)

	var mu sync.Mutex
	var launched []string
	origCtx := CommandExecutorCtx
	orig := CommandExecutor
	t.Cleanup(func() {
		CommandExecutorCtx = origCtx
		CommandExecutor = orig
	})
	record := func(name string) {
		mu.Lock()
		launched = append(launched, name)
		mu.Unlock()
	}
	CommandExecutorCtx = func(_ context.Context, name string, args ...string) ([]byte, error) {
		record(name)
		return nil, exec.ErrNotFound
	}
	CommandExecutor = func(name string, args ...string) ([]byte, error) {
		record(name)
		return nil, exec.ErrNotFound
	}

	origSource := DisplayAdapterSource
	t.Cleanup(func() { DisplayAdapterSource = origSource })
	DisplayAdapterSource = func() (DisplayAdapterReader, error) {
		return fakeAdapterReader{
			"0000": {
				"DriverDesc":                       "AMD Radeon RX 7800 XT",
				"ProviderName":                     "Advanced Micro Devices, Inc.",
				"HardwareInformation.qwMemorySize": uint64(16 * gib),
			},
		}, nil
	}

	got := DetectGPUs()

	mu.Lock()
	defer mu.Unlock()
	for _, name := range launched {
		if name == "amd-smi" || name == "rocm-smi" {
			t.Fatalf("Windows detection launched %q (all launched: %v); it must read the registry instead", name, launched)
		}
	}
	if len(got) != 1 || got[0].Vendor != "amd" || got[0].VRAMMB != 16*1024 {
		t.Errorf("registry-detected GPUs = %+v, want the one AMD card", dump(got))
	}
}

// A card nvidia-smi reports is not counted a second time from the registry;
// the registry's other vendors still are.
func TestWindowsDetectGPUs_MergesNvidiaSmiWithRegistry(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			return []byte("NVIDIA GeForce RTX 3070, 8192, 8.6\n"), nil
		}
		return nil, exec.ErrNotFound
	})
	DisplayAdapterSource = func() (DisplayAdapterReader, error) {
		return fakeAdapterReader{
			"0000": {
				"DriverDesc":                       "NVIDIA GeForce RTX 3070",
				"ProviderName":                     "NVIDIA",
				"HardwareInformation.qwMemorySize": uint64(8 * gib),
			},
			"0001": {
				"DriverDesc":                       "AMD Radeon RX 7800 XT",
				"ProviderName":                     "Advanced Micro Devices, Inc.",
				"HardwareInformation.qwMemorySize": uint64(16 * gib),
			},
		}, nil
	}

	got := DetectGPUs()
	if len(got) != 2 {
		t.Fatalf("got %d GPUs, want 2 (NVIDIA once, AMD once): %+v", len(got), dump(got))
	}
	vendors := map[string]int{}
	for _, g := range got {
		vendors[g.Vendor]++
		if g.Vendor == "nvidia" && g.ComputeCapability != "8.6" {
			t.Errorf("NVIDIA entry = %+v, want nvidia-smi's entry with compute capability", *g)
		}
	}
	if vendors["nvidia"] != 1 || vendors["amd"] != 1 {
		t.Errorf("vendor counts = %v, want one nvidia and one amd", vendors)
	}
}

// The real registry reader must work on this machine without elevation and
// without panicking; what it finds depends on the hardware, so only the
// contract is checked.
func TestWindowsDisplayAdapterSource_ReadsWithoutElevation(t *testing.T) {
	reader, err := platformDisplayAdapterSource()
	if err != nil {
		t.Skipf("display adapter class key not readable here: %v", err)
	}
	defer reader.(interface{ Close() error }).Close()
	for _, g := range parseDisplayAdapters(reader) {
		if g.Vendor == "" || g.Model == "" || g.VRAMMB <= 0 {
			t.Errorf("malformed adapter from the live registry: %+v", *g)
		}
	}
}
