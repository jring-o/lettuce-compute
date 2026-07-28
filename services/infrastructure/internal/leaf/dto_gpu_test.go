package leaf

import "testing"

// gpuLeaf mirrors a real leaf that exposed TB-20 in production: the two
// independently-settable VRAM fields hold DIFFERENT values, so a test that reads
// the wrong one fails loudly instead of passing by coincidence. Dispatch gates on
// resource_requirements.min_gpu_vram_mb; execution_config carried a parallel
// min_vram_gb that nothing enforced and that the catalog published.
func gpuLeaf() *Leaf {
	img := "example.test/model:1.0"
	return &Leaf{
		ExecutionConfig: ExecutionConfig{
			Runtime:     RuntimeContainer,
			Image:       &img,
			GPURequired: true,
			GPUType:     GPUTypeNvidia,
			MaxMemoryMB: 6000,
			MaxDiskMB:   20480,
		},
		ResourceRequirements: ResourceRequirements{
			MinCPUCores:  1,
			MinDiskMB:    20480,
			MinGPUVRAMMB: 4096,
			GPURequired:  true,
		},
	}
}

// TestToLeafSummary_VRAMFromResourceRequirements is the TB-20 regression test.
// The list endpoint must publish the VRAM figure dispatch enforces, not a
// separately-authored one: a catalog that advertises a requirement the scheduler
// does not apply sends volunteers shopping for the wrong hardware.
func TestToLeafSummary_VRAMFromResourceRequirements(t *testing.T) {
	lf := gpuLeaf()
	got := ToLeafSummary(lf).ResourceRequirements.GPUMinVRAMMB
	if got != 4096 {
		t.Fatalf("gpu_min_vram_mb = %d, want 4096 (resource_requirements.min_gpu_vram_mb)", got)
	}

	// Changing the enforced field must move the published one. Reading any other
	// source would leave this pinned at 4096.
	lf.ResourceRequirements.MinGPUVRAMMB = 12288
	if got := ToLeafSummary(lf).ResourceRequirements.GPUMinVRAMMB; got != 12288 {
		t.Errorf("after raising min_gpu_vram_mb to 12288, published %d — the catalog is not tracking the enforced field", got)
	}

	// A leaf with no VRAM requirement publishes none (omitempty), rather than a
	// derived zero that reads as "0 MB required".
	lf.ResourceRequirements.MinGPUVRAMMB = 0
	if got := ToLeafSummary(lf).ResourceRequirements.GPUMinVRAMMB; got != 0 {
		t.Errorf("no VRAM requirement published %d, want 0", got)
	}
}

// TestToLeafSummary_GPURequiredFromEitherFlag is the second half of TB-20. Both
// dispatch paths require a GPU when EITHER flag is set (widened by the #30 fix);
// the summary read resource_requirements alone, so a leaf that set only the
// execution_config flag was published as a CPU leaf while reaching no GPU-less
// volunteer.
func TestToLeafSummary_GPURequiredFromEitherFlag(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rr, ec bool
		want   bool
	}{
		{"neither flag", false, false, false},
		{"resource_requirements only", true, false, true},
		{"execution_config only", false, true, true},
		{"both", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lf := gpuLeaf()
			lf.ResourceRequirements.GPURequired = tc.rr
			lf.ExecutionConfig.GPURequired = tc.ec
			if got := ToLeafSummary(lf).ResourceRequirements.GPURequired; got != tc.want {
				t.Errorf("gpu_required = %v, want %v", got, tc.want)
			}
		})
	}
}
