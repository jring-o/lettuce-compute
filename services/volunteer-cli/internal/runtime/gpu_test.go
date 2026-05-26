package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func withMockExecutor(t *testing.T, mock func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	disableSkipHardwareDetection(t)
	orig := CommandExecutor
	origCtx := CommandExecutorCtx
	t.Cleanup(func() {
		CommandExecutor = orig
		CommandExecutorCtx = origCtx
	})
	CommandExecutor = mock
	// Bridge the context-aware variant to the same mock so legacy tests that
	// only override CommandExecutor continue to capture detection-path calls
	// (which now go through CommandExecutorCtx).
	CommandExecutorCtx = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return mock(name, args...)
	}
}

// disableSkipHardwareDetection clears the SkipHardwareDetectionEnv var for the
// duration of a test that needs DetectGPUs to run its real (mocked-CLI)
// pipeline rather than short-circuit to nil.
func disableSkipHardwareDetection(t *testing.T) {
	t.Helper()
	prev, had := os.LookupEnv(SkipHardwareDetectionEnv)
	os.Unsetenv(SkipHardwareDetectionEnv)
	t.Cleanup(func() {
		if had {
			os.Setenv(SkipHardwareDetectionEnv, prev)
		} else {
			os.Unsetenv(SkipHardwareDetectionEnv)
		}
	})
}

// notFoundForAll returns exec.ErrNotFound for every command.
func notFoundForAll(name string, args ...string) ([]byte, error) {
	return nil, exec.ErrNotFound
}

func TestParseNvidiaSmiCSV(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantLen int
		want    *GpuDetectionResult // first result
	}{
		{
			name:    "single GPU",
			output:  "NVIDIA GeForce RTX 3080, 10240, 8.6\n",
			wantLen: 1,
			want: &GpuDetectionResult{
				Model: "NVIDIA GeForce RTX 3080", Vendor: "nvidia",
				VRAMMB: 10240, ComputeCapability: "8.6",
			},
		},
		{
			name:    "multi GPU",
			output:  "NVIDIA GeForce RTX 3080, 10240, 8.6\nNVIDIA GeForce RTX 3070, 8192, 8.6\n",
			wantLen: 2,
			want: &GpuDetectionResult{
				Model: "NVIDIA GeForce RTX 3080", Vendor: "nvidia",
				VRAMMB: 10240, ComputeCapability: "8.6",
			},
		},
		{
			name:    "empty output",
			output:  "",
			wantLen: 0,
		},
		{
			name:    "malformed line too few fields",
			output:  "garbage without commas\n",
			wantLen: 0,
		},
		{
			name:    "GPU with 0 VRAM skipped",
			output:  "NVIDIA Tesla T4, 0, 7.5\n",
			wantLen: 0,
		},
		{
			name:    "GPU with invalid VRAM skipped",
			output:  "NVIDIA Tesla T4, abc, 7.5\n",
			wantLen: 0,
		},
		{
			name:    "mixed valid and invalid",
			output:  "NVIDIA GeForce RTX 3080, 10240, 8.6\nbad line\nNVIDIA A100, 40960, 8.0\n",
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := parseNvidiaSmiCSV(tt.output)
			if len(results) != tt.wantLen {
				t.Fatalf("got %d results, want %d", len(results), tt.wantLen)
			}
			if tt.want != nil && len(results) > 0 {
				assertGPUEqual(t, results[0], tt.want)
			}
		})
	}
}

func TestParseRocmSmiCSV(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantLen int
		want    *GpuDetectionResult
	}{
		{
			name: "single GPU",
			output: "device,Card Series,VRAM Total Memory (B)\n" +
				"card0,AMD Instinct MI210,68719476736\n",
			wantLen: 1,
			want:    &GpuDetectionResult{Model: "AMD Instinct MI210", Vendor: "amd", VRAMMB: 65536},
		},
		{
			name: "multi GPU",
			output: "device,Card Series,VRAM Total Memory (B)\n" +
				"card0,AMD Instinct MI210,68719476736\n" +
				"card1,AMD Instinct MI250,137438953472\n",
			wantLen: 2,
		},
		{
			name:    "header only",
			output:  "device,Card Series,VRAM Total Memory (B)\n",
			wantLen: 0,
		},
		{
			name:    "empty",
			output:  "",
			wantLen: 0,
		},
		{
			name: "0 VRAM skipped",
			output: "device,Card Series,VRAM Total Memory (B)\n" +
				"card0,AMD GPU,0\n",
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := parseRocmSmiCSV(tt.output)
			if len(results) != tt.wantLen {
				t.Fatalf("got %d results, want %d", len(results), tt.wantLen)
			}
			if tt.want != nil && len(results) > 0 {
				assertGPUEqual(t, results[0], tt.want)
			}
		})
	}
}

func TestApplyGfxVersions(t *testing.T) {
	results := []*GpuDetectionResult{
		{Model: "MI210", Vendor: "amd", VRAMMB: 65536},
		{Model: "MI250", Vendor: "amd", VRAMMB: 131072},
	}

	gfxOutput := "device,GFX Version\ncard0,gfx90a\ncard1,gfx940\n"
	applyGfxVersions(results, gfxOutput)

	if results[0].ComputeCapability != "gfx90a" {
		t.Errorf("GPU 0 ComputeCapability = %q, want %q", results[0].ComputeCapability, "gfx90a")
	}
	if results[1].ComputeCapability != "gfx940" {
		t.Errorf("GPU 1 ComputeCapability = %q, want %q", results[1].ComputeCapability, "gfx940")
	}
}

func TestDetectGPUsNVIDIAOnly(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			return []byte("NVIDIA GeForce RTX 3080, 10240, 8.6\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	results := DetectGPUs()
	if len(results) != 1 {
		t.Fatalf("got %d GPUs, want 1", len(results))
	}
	if results[0].Vendor != "nvidia" {
		t.Errorf("Vendor = %q, want %q", results[0].Vendor, "nvidia")
	}
}

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

func TestDetectGPUsNothingFound(t *testing.T) {
	withMockExecutor(t, notFoundForAll)

	results := DetectGPUs()
	if len(results) != 0 {
		t.Errorf("got %d GPUs, want 0", len(results))
	}
}

func TestDetectGPUsNvidiaSmiFailsNonFatal(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			return nil, fmt.Errorf("segfault")
		}
		return nil, exec.ErrNotFound
	})

	// Should not panic, just return no GPUs.
	results := DetectGPUs()
	if len(results) != 0 {
		t.Errorf("got %d GPUs, want 0", len(results))
	}
}

// TestDetectGPUsSlowVendorBoundedByAggregateTimeout verifies that when one
// vendor CLI hangs (e.g. amd-smi on a broken-driver host) DetectGPUs still
// returns within the aggregate cap and the other vendors' results are
// preserved. This is the regression test for the volunteer-CLI hang where
// `amd-smi list --csv` took ~208 seconds and blocked Register.
func TestDetectGPUsSlowVendorBoundedByAggregateTimeout(t *testing.T) {
	disableSkipHardwareDetection(t)
	hangFor := 30 * time.Second // far beyond DetectHardwareTimeout

	origCtx := CommandExecutorCtx
	t.Cleanup(func() { CommandExecutorCtx = origCtx })
	CommandExecutorCtx = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		// nvidia-smi responds instantly; rocm-smi/amd-smi/system_profiler hang.
		if name == "nvidia-smi" {
			return []byte("NVIDIA A100, 40960, 8.0\n"), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(hangFor):
			return nil, nil
		}
	}

	cap := DetectHardwareTimeout
	margin := 3 * time.Second

	start := time.Now()
	results := DetectGPUs()
	elapsed := time.Since(start)

	if elapsed > cap+margin {
		t.Fatalf("DetectGPUs took %v, want <= %v (cap %v + margin %v)", elapsed, cap+margin, cap, margin)
	}
	// nvidia-smi's instant result must survive even though sibling vendors hung.
	gotNvidia := false
	for _, r := range results {
		if r.Vendor == "nvidia" {
			gotNvidia = true
			break
		}
	}
	if !gotNvidia {
		t.Errorf("nvidia GPU missing from results = %+v; fast vendor result should not be dropped when others hang", results)
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

func assertGPUEqual(t *testing.T, got, want *GpuDetectionResult) {
	t.Helper()
	if got.Model != want.Model {
		t.Errorf("Model = %q, want %q", got.Model, want.Model)
	}
	if got.Vendor != want.Vendor {
		t.Errorf("Vendor = %q, want %q", got.Vendor, want.Vendor)
	}
	if got.VRAMMB != want.VRAMMB {
		t.Errorf("VRAMMB = %d, want %d", got.VRAMMB, want.VRAMMB)
	}
	if want.ComputeCapability != "" && got.ComputeCapability != want.ComputeCapability {
		t.Errorf("ComputeCapability = %q, want %q", got.ComputeCapability, want.ComputeCapability)
	}
}
