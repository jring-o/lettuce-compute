package runtime

import (
	"fmt"
	"log/slog"
	goruntime "runtime"
	"sort"
	"strings"
)

// GPU temperatures for the thermal monitor.
//
// The monitor judges a GPU against gpu_pause_threshold / gpu_resume_threshold
// using two sources: a vendor tool per detected card (nvidia-smi, rocm-smi) and,
// on Linux, the kernel's GPU thermal zones. Until these were wired the monitor
// was built with no GPU source at all, so the GPU thresholds never acted on any
// machine while the settings, the notices and the guide all said they did.

// gpuTempToolAllowed reports whether this client may launch vendor's tool on
// this platform to read a temperature. The same rule GPU detection follows: on
// Windows only nvidia-smi is ever launched, because AMD's amd-smi asks for
// administrator rights the moment it starts (and rocm-smi does not ship there).
func gpuTempToolAllowed(vendor string) bool {
	switch vendor {
	case "nvidia":
		return true
	case "amd":
		return goruntime.GOOS != "windows"
	default:
		return false
	}
}

// GPUThermalCollectors returns one temperature collector per detected card a
// vendor tool on this platform can read. A card is addressed by its position
// among the cards of its own vendor, which is the index nvidia-smi --id and
// rocm-smi -d take: detection lists each vendor's cards in its tool's order.
func GPUThermalCollectors(gpus []*GpuDetectionResult, logger *slog.Logger) []*GPUMetricsCollector {
	seen := make(map[string]int)
	var out []*GPUMetricsCollector
	for _, g := range gpus {
		if g == nil {
			continue
		}
		vendor := strings.ToLower(strings.TrimSpace(g.Vendor))
		idx := seen[vendor]
		seen[vendor]++
		if !gpuTempToolAllowed(vendor) {
			continue
		}
		out = append(out, NewGPUMetricsCollector(vendor, idx, logger))
	}
	return out
}

// DetectGPUThermal says whether this machine's GPU temperature can be read: it
// asks each collector once and looks for GPU thermal zones among sensors. gpus is
// the detection the collectors were built from; it lets the detail name a card
// that no tool here can read. Returns the source ("nvidia-smi", "rocm-smi",
// "sysfs", joined with "+" when there are several, or "none"), whether any
// reading arrived, and one sentence for a human.
func DetectGPUThermal(gpus []*GpuDetectionResult, collectors []*GPUMetricsCollector, sensors []Sensor) (source string, readable bool, detail string) {
	var sources, read []string
	addSource := func(s string) {
		for _, have := range sources {
			if have == s {
				return
			}
		}
		sources = append(sources, s)
	}
	var failed []string
	for _, c := range collectors {
		snap, err := c.Collect()
		if err == nil && snap.TemperatureC > 0 {
			addSource(c.Tool())
			read = append(read, fmt.Sprintf("%s card %d with %s (%d°C now)", strings.ToUpper(c.Vendor()), c.deviceIdx, c.Tool(), snap.TemperatureC))
			continue
		}
		failed = append(failed, fmt.Sprintf("%s card %d (%s reported no temperature)", strings.ToUpper(c.Vendor()), c.deviceIdx, c.Tool()))
	}
	for _, s := range sensors {
		if s.Class == SensorGPU {
			addSource("sysfs")
			read = append(read, fmt.Sprintf("%s (%s, %d°C now)", s.Zone, s.Kind, s.TempC))
		}
	}
	if len(read) > 0 {
		sort.Strings(sources)
		detail = "reading " + strings.Join(read, ", ")
		if len(failed) > 0 {
			detail += "; not readable: " + strings.Join(failed, ", ")
		}
		return strings.Join(sources, "+"), true, detail
	}

	if len(gpus) == 0 {
		return "none", false, "no GPU detected"
	}
	var unsupported []string
	for _, g := range gpus {
		if g == nil {
			continue
		}
		vendor := strings.ToLower(strings.TrimSpace(g.Vendor))
		if gpuTempToolAllowed(vendor) {
			continue
		}
		unsupported = append(unsupported, unreadableGPUReason(vendor, g.Model))
	}
	parts := append(failed, unsupported...)
	if len(parts) == 0 {
		return "none", false, "no GPU detected"
	}
	return "none", false, "no GPU temperature can be read: " + strings.Join(parts, "; ")
}

// unreadableGPUReason says why a detected card's temperature is not read.
func unreadableGPUReason(vendor, model string) string {
	name := strings.TrimSpace(model)
	if name == "" {
		name = strings.ToUpper(vendor) + " GPU"
	}
	switch {
	case vendor == "amd" && goruntime.GOOS == "windows":
		return name + " (AMD's temperature tool asks for administrator rights on Windows, so Lettuce does not start it)"
	case vendor == "apple":
		return name + " (Apple GPUs report no temperature to programs)"
	default:
		return name + " (no temperature tool Lettuce can use for this make of card)"
	}
}
