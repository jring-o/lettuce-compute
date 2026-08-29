package runtime

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// GpuDetectionResult holds detected GPU information.
type GpuDetectionResult struct {
	Model             string
	Vendor            string // "nvidia", "amd", "intel", "apple"
	VRAMMB            int32
	ComputeCapability string
}

// CommandExecutor runs an external command and returns its output.
// Override in tests for mocking. The default is set per-platform
// (cmd_default.go / cmd_windows.go) to suppress console popups on Windows.
//
// This variable is the legacy seam. New code paths should prefer
// CommandExecutorCtx so the caller controls the timeout — see
// DetectionCommandTimeout for the value used by hardware detection.
var CommandExecutor = defaultCommandExecutor

// CommandExecutorCtx runs an external command with an explicit context.
// Detection code uses this with a short per-command timeout
// (DetectionCommandTimeout) so a hung CLI like a broken amd-smi cannot block
// volunteer registration. Override in tests for mocking.
var CommandExecutorCtx = defaultCommandExecutorCtx

// runDetectionCommand invokes CommandExecutorCtx with the standard per-command
// detection timeout. Returns ("", true) when the command failed for any reason
// (not installed, timed out, non-zero exit) so callers can degrade to "tool
// unavailable" instead of propagating the error.
func runDetectionCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DetectionCommandTimeout)
	defer cancel()
	return CommandExecutorCtx(ctx, name, args...)
}

// gpuDetector is one vendor- or platform-specific GPU probe. The set that runs
// is chosen per platform (platformGPUDetectors): the vendor command-line tools
// are safe to launch on Linux and macOS, but on Windows AMD's amd-smi requests
// UAC elevation the moment it starts — so Windows enumerates display adapters
// from the registry instead and launches only nvidia-smi.
type gpuDetector struct {
	label string
	fn    func() ([]*GpuDetectionResult, error)
}

// DetectGPUs discovers all available GPUs across vendors.
//
// The platform's sub-detections run in parallel under an aggregate timeout
// (DetectHardwareTimeout) so a single hung CLI cannot serialize the others
// and so the total wall time is bounded even if every sub-call hits its own
// per-command timeout. Any sub-detection that errors or times out is treated
// as "no GPUs of that vendor" and logged — never propagated.
func DetectGPUs() []*GpuDetectionResult {
	if SkipHardwareDetection() {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), DetectHardwareTimeout)
	defer cancel()

	var (
		mu      sync.Mutex
		results []*GpuDetectionResult
		wg      sync.WaitGroup
	)

	collect := func(label string, fn func() ([]*GpuDetectionResult, error)) {
		defer wg.Done()
		done := make(chan struct{})
		var (
			detected []*GpuDetectionResult
			err      error
		)
		go func() {
			defer close(done)
			detected, err = fn()
		}()
		select {
		case <-done:
		case <-ctx.Done():
			slog.Warn("GPU detection timed out, skipping", "vendor", label)
			return
		}
		if err != nil {
			slog.Warn("GPU detection failed", "vendor", label, "error", err)
		}
		if len(detected) == 0 {
			return
		}
		mu.Lock()
		results = append(results, detected...)
		mu.Unlock()
	}

	detectors := platformGPUDetectors()
	wg.Add(len(detectors))
	for _, d := range detectors {
		go collect(d.label, d.fn)
	}
	wg.Wait()

	return results
}

// detectNVIDIAGPUs uses nvidia-smi to detect NVIDIA GPUs. nvidia-smi ships
// with NVIDIA's driver on every platform, runs unelevated, and is the only
// source of CUDA compute capability, so it is used everywhere.
func detectNVIDIAGPUs() ([]*GpuDetectionResult, error) {
	out, err := runDetectionCommand("nvidia-smi",
		"--query-gpu=name,memory.total,compute_cap",
		"--format=csv,noheader,nounits")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, nil
		}
		slog.Warn("nvidia-smi command failed", "error", err)
		return nil, nil
	}

	return parseNvidiaSmiCSV(string(out)), nil
}

// parseNvidiaSmiCSV parses nvidia-smi CSV output.
// Expected format per line: "GPU Name, VRAM MB, compute_cap"
func parseNvidiaSmiCSV(output string) []*GpuDetectionResult {
	var results []*GpuDetectionResult
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, ",", 3)
		if len(fields) < 3 {
			slog.Warn("malformed nvidia-smi output line", "line", line)
			continue
		}

		model := strings.TrimSpace(fields[0])
		vramStr := strings.TrimSpace(fields[1])
		cc := strings.TrimSpace(fields[2])

		vram, err := strconv.ParseInt(vramStr, 10, 32)
		if err != nil || vram <= 0 {
			slog.Warn("invalid VRAM from nvidia-smi, skipping GPU", "value", vramStr)
			continue
		}

		results = append(results, &GpuDetectionResult{
			Model:             model,
			Vendor:            "nvidia",
			VRAMMB:            int32(vram),
			ComputeCapability: cc,
		})
	}
	return results
}

// parseRocmSmiCSV parses rocm-smi --csv output (also the shape of
// amd-smi list --csv). The command itself is only launched on non-Windows
// platforms — see gpu_amd_cli.go.
func parseRocmSmiCSV(output string) []*GpuDetectionResult {
	var results []*GpuDetectionResult
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return nil
	}

	header := strings.Split(lines[0], ",")
	nameIdx := -1
	vramIdx := -1
	for i, col := range header {
		col = strings.TrimSpace(strings.ToLower(col))
		if nameIdx == -1 && (strings.Contains(col, "card series") || strings.Contains(col, "name") || strings.Contains(col, "product")) {
			nameIdx = i
		}
		if strings.Contains(col, "vram total") {
			vramIdx = i
		}
	}

	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")

		model := "AMD GPU"
		if nameIdx >= 0 && nameIdx < len(fields) {
			m := strings.TrimSpace(fields[nameIdx])
			if m != "" {
				model = m
			}
		}

		var vramMB int32
		if vramIdx >= 0 && vramIdx < len(fields) {
			vramStr := strings.TrimSpace(fields[vramIdx])
			vramBytes, err := strconv.ParseInt(vramStr, 10, 64)
			if err == nil && vramBytes > 0 {
				vramMB = int32(vramBytes / (1024 * 1024))
			}
		}

		if vramMB <= 0 {
			slog.Warn("skipping AMD GPU with 0 VRAM", "model", model)
			continue
		}

		results = append(results, &GpuDetectionResult{
			Model:  model,
			Vendor: "amd",
			VRAMMB: vramMB,
		})
	}
	return results
}

// applyGfxVersions merges GFX versions from rocm-smi --showgfxversion into results.
func applyGfxVersions(results []*GpuDetectionResult, output string) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return
	}

	header := strings.Split(lines[0], ",")
	gfxIdx := -1
	for i, col := range header {
		if strings.Contains(strings.ToLower(strings.TrimSpace(col)), "gfx") {
			gfxIdx = i
			break
		}
	}
	if gfxIdx < 0 {
		return
	}

	for i, line := range lines[1:] {
		if i >= len(results) {
			break
		}
		fields := strings.Split(strings.TrimSpace(line), ",")
		if gfxIdx < len(fields) {
			results[i].ComputeCapability = strings.TrimSpace(fields[gfxIdx])
		}
	}
}
