//go:build darwin

package runtime

import (
	"strings"
)

func detectPlatformGPUs() ([]*GpuDetectionResult, error) {
	out, err := runDetectionCommand("system_profiler", "SPDisplaysDataType")
	if err != nil {
		return nil, nil
	}
	return parseSystemProfilerGPUs(string(out)), nil
}

// parseSystemProfilerGPUs parses macOS system_profiler output for Apple Metal GPUs.
func parseSystemProfilerGPUs(output string) []*GpuDetectionResult {
	var results []*GpuDetectionResult
	var currentModel string

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "Chipset Model:") {
			currentModel = strings.TrimSpace(strings.TrimPrefix(trimmed, "Chipset Model:"))
		}

		if strings.HasPrefix(trimmed, "Vendor:") && strings.Contains(strings.ToLower(trimmed), "apple") {
			if currentModel != "" {
				results = append(results, &GpuDetectionResult{
					Model:  currentModel,
					Vendor: "apple",
					VRAMMB: 0, // Unified memory — no discrete VRAM
				})
				currentModel = ""
			}
		}
	}

	return results
}
