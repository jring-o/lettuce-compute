package cli

import (
	"strings"
	"testing"
)

// Regression tests for TB-43: classifyLeaf reports one blocking reason — the
// first case that bites — and ordered the settings gates (memory, disk,
// cores) before GPU presence. So a GPU-less machine was told to raise its
// disk allowance for GPU leaves it can never run; a real tester paid the
// remedy (config change + restart) only to have the same rows flip to "needs
// a GPU". The unfixable-hardware gates must be judged first.

// The tester's screenshots, 2026-08-04/05: allowance 15,360 MB, the
// extract2-student-crowd-*-gpu leaves declaring 20,000 MB disk and a GPU.
func tb43GPULeaf() leafRequirements {
	return leafRequirements{
		name: "extract2-student-crowd-v1-gpu", needsContainer: true,
		diskMB: 20000, cpuCores: 1,
		needsGPU: true, specGPURequired: true, rrGPURequired: true,
	}
}

func tb43GPUlessCaps() volunteerCaps {
	return volunteerCaps{
		maxMemoryMB: 16384, containerUsable: true, hasGPU: false,
		maxDiskMB: 15 * 1024, maxCPUCores: 8,
	}
}

// TestTB43_GPUPresenceReportedBeforeDiskRemedy is the filed shape byte for
// byte: pre-fix this returned blocked="disk" with the paste-me allowance
// remedy; the machine can raise max_disk_gb to 1 TB and still never run the
// leaf.
func TestTB43_GPUPresenceReportedBeforeDiskRemedy(t *testing.T) {
	le, blocked := classifyLeaf(tb43GPULeaf(), tb43GPUlessCaps(), trustingHead)
	if blocked != "gpu" {
		t.Fatalf("blocked = %q (reason: %q), want \"gpu\" — a settings remedy for a leaf this machine can never run is the TB-43 trap", blocked, le.reason)
	}
	if strings.Contains(le.reason, "max_disk_gb") {
		t.Errorf("the reason hands out a disk remedy on a GPU-impossible leaf: %q", le.reason)
	}
}

// TestTB43_UnfixableGPUGatesAllPrecedeSettingsGates: vendor, compute
// capability and a too-small card are as unfixable as absence — each must be
// reported before any settings remedy.
func TestTB43_UnfixableGPUGatesAllPrecedeSettingsGates(t *testing.T) {
	req := tb43GPULeaf()
	req.gpuType = "NVIDIA"
	amd := gpuCaps(8192, 50)
	amd.maxDiskMB = 15 * 1024 // disk would also block
	amd.gpuVendors = []string{"AMD"}
	if le, blocked := classifyLeaf(req, amd, trustingHead); blocked != "gpu" || !strings.Contains(le.reason, "NVIDIA") {
		t.Errorf("wrong-vendor leaf: blocked=%q reason=%q, want the vendor named before any settings remedy", blocked, le.reason)
	}

	req = tb43GPULeaf()
	req.gpuComputeCapability = "8.6"
	cc := gpuCaps(8192, 50)
	cc.maxDiskMB = 15 * 1024
	cc.gpuComputeCapabilities = []string{"7.5"}
	if le, blocked := classifyLeaf(req, cc, trustingHead); blocked != "gpu" || !strings.Contains(le.reason, "8.6") {
		t.Errorf("wrong-compute-capability leaf: blocked=%q reason=%q, want the capability named before any settings remedy", blocked, le.reason)
	}

	req = tb43GPULeaf()
	req.gpuVRAMMB = 24000 // more than the whole 8 GB card
	small := gpuCaps(8192, 50)
	small.maxDiskMB = 15 * 1024
	le, blocked := classifyLeaf(req, small, trustingHead)
	if blocked != "vram" || !strings.Contains(le.reason, "too small") {
		t.Errorf("too-small-card leaf: blocked=%q reason=%q, want the card named before any settings remedy", blocked, le.reason)
	}
	if strings.Contains(le.reason, "max_disk_gb") {
		t.Errorf("too-small-card reason hands out a disk remedy: %q", le.reason)
	}
}

// TestTB43_SettingsGatesStillFireWhenTheHardwareFits: the reorder must not
// swallow the fixable reasons — a machine whose hardware qualifies still gets
// the disk remedy, and a fixable VRAM percentage still gets its remedy.
func TestTB43_SettingsGatesStillFireWhenTheHardwareFits(t *testing.T) {
	req := tb43GPULeaf()
	fits := gpuCaps(8192, 50)
	fits.maxDiskMB = 15 * 1024
	if le, blocked := classifyLeaf(req, fits, trustingHead); blocked != "disk" || !strings.Contains(le.reason, "max_disk_gb") {
		t.Errorf("GPU machine, big-disk leaf: blocked=%q reason=%q, want the disk remedy", blocked, le.reason)
	}

	req = tb43GPULeaf()
	req.diskMB = 1024
	req.gpuVRAMMB = 4096 // fits the 8 GB card, not the 50% allowance
	if le, blocked := classifyLeaf(req, gpuCaps(8192, 25), trustingHead); blocked != "vram" || !strings.Contains(le.reason, "max_gpu_vram_pct") {
		t.Errorf("fixable VRAM shortfall: blocked=%q reason=%q, want the percentage remedy", blocked, le.reason)
	}
}
