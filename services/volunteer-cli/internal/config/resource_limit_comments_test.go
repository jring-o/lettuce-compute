package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The comments every save writes into config.yaml are what a CLI volunteer
// reads while editing it, so they must describe the limits as the client
// applies them. They used to call the resource limits "Per-task resource
// ceilings" and max_cpu_cores the cores "a single work unit may use", although
// CPU, memory and disk are totals for everything running (at most
// max_cpu_cores tasks run at once); call max_concurrent_tasks "THIS is the
// workload throttle", although the CPU budget also caps how many run; and call
// max_bandwidth_mbps a "Bandwidth cap" while it did not limit image pulls.
func TestSavedCommentsDescribeResourceLimitsAsTheyAreApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c := Defaults()
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	out := string(data)

	for _, stale := range []string{
		"Per-task resource ceilings",
		"a single work unit may use",
		"THIS is the workload throttle",
		"Bandwidth cap",
		"freeze ALL work when the GPU reaches this.",
	} {
		if strings.Contains(out, stale) {
			t.Errorf("saved config still says %q", stale)
		}
	}

	for key, want := range map[string]string{
		"resource_limits":      "totals for ALL running work together, not per task",
		"max_cpu_cores":        "at most this many tasks run at once whatever max_concurrent_tasks says",
		"max_memory_mb":        "in total: a unit starts only if its declared memory fits beside what is already running",
		"max_concurrent_tasks": "with max_cpu_cores N, at most N tasks run",
		"max_bandwidth_mbps":   "Container image pulls are made by the container engine and are NOT limited",
		"gpu_pause_threshold":  "read with nvidia-smi, rocm-smi on Linux/macOS, or a Linux GPU sensor",
		"thermal":              "Each threshold acts only where its temperature can be read",
	} {
		comment := commentAbove(out, key+":")
		if !strings.Contains(comment, want) {
			t.Errorf("comment above %s = %q, want it to contain %q", key, comment, want)
		}
	}
}

// New configurations pause for a GPU at 87 °C and resume below 77 °C: above the
// temperature a busy card holds by design, so normal GPU work is not paused now
// that the GPU thresholds act.
func TestDefaultGPUThresholds(t *testing.T) {
	th := Defaults().Thermal
	if th.GPUPauseThresholdC != 87 || th.GPUResumeThresholdC != 77 {
		t.Errorf("default GPU thresholds = %d/%d, want 87/77", th.GPUPauseThresholdC, th.GPUResumeThresholdC)
	}
	if err := Defaults().Validate(); err != nil {
		t.Errorf("defaults do not validate: %v", err)
	}
}

// commentAbove returns the comment lines directly above the first line that
// starts (after indentation) with key, joined without their "# ".
func commentAbove(yamlText, key string) string {
	lines := strings.Split(yamlText, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), key) {
			continue
		}
		var comment []string
		for j := i - 1; j >= 0; j-- {
			l := strings.TrimSpace(lines[j])
			if !strings.HasPrefix(l, "#") {
				break
			}
			comment = append([]string{strings.TrimSpace(strings.TrimPrefix(l, "#"))}, comment...)
		}
		return strings.Join(comment, " ")
	}
	return ""
}
