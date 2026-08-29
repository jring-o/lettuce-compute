//go:build !windows

package runtime

import (
	"os/exec"
	"testing"
)

// AMD detection through rocm-smi / amd-smi exists only on non-Windows builds:
// on Windows amd-smi requests UAC elevation when launched, so the Windows
// build enumerates display adapters from the registry instead (see
// gpu_amd_cli.go and gpu_windows.go).

func TestDetectGPUsAMDOnly(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "rocm-smi" {
			if len(args) > 0 && args[0] == "--showgfxversion" {
				return []byte("device,GFX Version\ncard0,gfx1030\n"), nil
			}
			return []byte("device,Card Series,VRAM Total Memory (B)\ncard0,AMD RX 6800,17179869184\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	results := DetectGPUs()
	if len(results) != 1 {
		t.Fatalf("got %d GPUs, want 1", len(results))
	}
	if results[0].Vendor != "amd" {
		t.Errorf("Vendor = %q, want %q", results[0].Vendor, "amd")
	}
	if results[0].ComputeCapability != "gfx1030" {
		t.Errorf("ComputeCapability = %q, want %q", results[0].ComputeCapability, "gfx1030")
	}
}

func TestDetectGPUsBothVendors(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		switch name {
		case "nvidia-smi":
			return []byte("NVIDIA A100, 40960, 8.0\n"), nil
		case "rocm-smi":
			if len(args) > 0 && args[0] == "--showgfxversion" {
				return []byte("device,GFX Version\ncard0,gfx90a\n"), nil
			}
			return []byte("device,Card Series,VRAM Total Memory (B)\ncard0,AMD MI210,68719476736\n"), nil
		default:
			return nil, exec.ErrNotFound
		}
	})

	results := DetectGPUs()
	if len(results) != 2 {
		t.Fatalf("got %d GPUs, want 2", len(results))
	}

	vendors := map[string]bool{}
	for _, r := range results {
		vendors[r.Vendor] = true
	}
	if !vendors["nvidia"] || !vendors["amd"] {
		t.Errorf("expected both nvidia and amd, got vendors: %v", vendors)
	}
}

func TestDetectAMDFallbackToAmdSmi(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		switch name {
		case "rocm-smi":
			return nil, exec.ErrNotFound
		case "amd-smi":
			return []byte("device,Name,VRAM Total Memory (B)\n0,AMD RX 7900,25769803776\n"), nil
		default:
			return nil, exec.ErrNotFound
		}
	})

	results := DetectGPUs()
	if len(results) != 1 {
		t.Fatalf("got %d GPUs, want 1", len(results))
	}
	if results[0].Model != "AMD RX 7900" {
		t.Errorf("Model = %q, want %q", results[0].Model, "AMD RX 7900")
	}
}
