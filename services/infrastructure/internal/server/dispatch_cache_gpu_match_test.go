package server

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// TestLeafMatchesCapabilities_GPURequiredViaExecutionConfig reproduces the #30 GPU
// field-split sub-bug: a leaf that declares it needs a GPU only via the natural
// execution_config.gpu_required flag (leaving the parallel
// resource_requirements.gpu_required unset) must not be handed to a GPU-less
// volunteer. The GPU-presence gate historically keyed only on
// resource_requirements.gpu_required, so such a leaf slipped through.
func TestLeafMatchesCapabilities_GPURequiredViaExecutionConfig(t *testing.T) {
	// A roomy volunteer with NO GPU.
	gpuless := workunit.AssignmentOptions{
		MaxCPUCores:       16,
		MaxMemoryMB:       16384,
		MaxDiskMB:         100 * 1024,
		HasGPU:            false,
		AvailableRuntimes: []string{leaf.RuntimeNative},
	}
	// A GPU volunteer (NVIDIA, plenty of VRAM).
	gpuful := workunit.AssignmentOptions{
		MaxCPUCores:       16,
		MaxMemoryMB:       16384,
		MaxDiskMB:         100 * 1024,
		HasGPU:            true,
		MaxGPUVRAMMB:      24000,
		AvailableRuntimes: []string{leaf.RuntimeNative},
		GPUVendors:        []string{leaf.GPUTypeNvidia},
	}

	// Leaf needs a GPU, declared ONLY on execution_config (gpu_type left at the
	// default ANY); resource_requirements.gpu_required is unset.
	gpuLeaf := &leaf.Leaf{
		ExecutionConfig: leaf.ExecutionConfig{
			Runtime:     leaf.RuntimeNative,
			GPURequired: true,
			MaxMemoryMB: 4096,
		},
		ResourceRequirements: leaf.ResourceRequirements{},
	}

	if leafMatchesCapabilities(gpuLeaf, gpuless) {
		t.Errorf("a GPU-required leaf (execution_config.gpu_required=true) must NOT match a GPU-less volunteer")
	}
	if !leafMatchesCapabilities(gpuLeaf, gpuful) {
		t.Errorf("a GPU-required leaf must match a volunteer that has a GPU with ample VRAM")
	}

	// Regression guard: a leaf that needs no GPU still matches a GPU-less volunteer.
	cpuLeaf := &leaf.Leaf{
		ExecutionConfig: leaf.ExecutionConfig{
			Runtime:     leaf.RuntimeNative,
			MaxMemoryMB: 4096,
		},
	}
	if !leafMatchesCapabilities(cpuLeaf, gpuless) {
		t.Errorf("a CPU-only leaf must match a GPU-less volunteer")
	}
}
