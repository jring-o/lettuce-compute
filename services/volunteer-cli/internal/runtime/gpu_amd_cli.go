//go:build !windows

package runtime

import (
	"errors"
	"log/slog"
	"os/exec"
)

// detectAMDGPUs uses rocm-smi (falling back to amd-smi) to detect AMD GPUs.
//
// This file is deliberately excluded from Windows builds. On Windows,
// C:\Windows\System32\amd-smi.exe ships with AMD's display drivers and
// requests UAC elevation when launched: every start of the volunteer daemon
// raised a "DiskPart is requesting your permission" prompt per detection and
// then blocked until the per-command timeout killed the call. Windows reads
// display adapters from the registry instead (gpu_windows.go), which needs no
// subprocess and no elevation — the same class of fix as the earlier removal
// of wmic from CPU detection (client/hardware_windows.go).
//
// Each external call is bounded by DetectionCommandTimeout via
// runDetectionCommand. On any failure (missing binary, non-zero exit, or
// timeout — e.g. the ~3min amd-smi hang observed on hosts without a working
// ROCm driver) we treat the vendor as "no GPUs detected" rather than
// propagating the error.
func detectAMDGPUs() ([]*GpuDetectionResult, error) {
	usedRocm := false
	out, err := runDetectionCommand("rocm-smi",
		"--showid", "--showproductname", "--showmeminfo", "vram", "--csv")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			// Try amd-smi as fallback.
			out, err = runDetectionCommand("amd-smi", "list", "--csv")
			if err != nil {
				if errors.Is(err, exec.ErrNotFound) {
					return nil, nil
				}
				slog.Warn("amd-smi command failed", "error", err)
				return nil, nil
			}
		} else {
			slog.Warn("rocm-smi command failed", "error", err)
			return nil, nil
		}
	} else {
		usedRocm = true
	}

	results := parseRocmSmiCSV(string(out))

	// Fetch GFX versions for compute capability (only available via rocm-smi).
	if usedRocm {
		gfxOut, err := runDetectionCommand("rocm-smi", "--showgfxversion", "--csv")
		if err == nil {
			applyGfxVersions(results, string(gfxOut))
		}
	}

	return results, nil
}
