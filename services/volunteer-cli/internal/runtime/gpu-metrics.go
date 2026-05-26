package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// GPUMetricsCollector collects GPU metrics from the host during container execution.
type GPUMetricsCollector struct {
	vendor    string // "nvidia" or "amd"
	deviceIdx int
	logger    *slog.Logger
}

// NewGPUMetricsCollector creates a collector for the given GPU vendor and device index.
func NewGPUMetricsCollector(vendor string, deviceIdx int, logger *slog.Logger) *GPUMetricsCollector {
	return &GPUMetricsCollector{
		vendor:    vendor,
		deviceIdx: deviceIdx,
		logger:    logger,
	}
}

// GPUMetricsSnapshot represents a single point-in-time GPU measurement.
type GPUMetricsSnapshot struct {
	TemperatureC   int
	UtilizationPct int
	VRAMUsedMB     int
	VRAMTotalMB    int
	PowerDrawWatts float64
}

// GPUExecutionMetrics aggregates GPU metrics over an execution period.
type GPUExecutionMetrics struct {
	GPUSeconds     float64 // sum of (utilization_pct/100 * interval) over all samples
	GPUModel       string
	PeakVRAMMB     int
	AvgUtilization float64
}

// Collect takes a single snapshot of GPU metrics.
func (c *GPUMetricsCollector) Collect() (*GPUMetricsSnapshot, error) {
	switch c.vendor {
	case "nvidia":
		return c.collectNVIDIA()
	case "amd":
		return c.collectAMD()
	default:
		return nil, fmt.Errorf("unsupported GPU vendor: %s", c.vendor)
	}
}

func (c *GPUMetricsCollector) collectNVIDIA() (*GPUMetricsSnapshot, error) {
	out, err := CommandExecutor("nvidia-smi",
		"--query-gpu=temperature.gpu,utilization.gpu,memory.used,memory.total,power.draw",
		"--format=csv,noheader,nounits",
		"--id="+strconv.Itoa(c.deviceIdx))
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi metrics: %w", err)
	}
	return parseNvidiaMetrics(string(out))
}

// parseNvidiaMetrics parses nvidia-smi metrics output.
// Format: "temp, util_pct, vram_used_mb, vram_total_mb, power_watts"
func parseNvidiaMetrics(output string) (*GPUMetricsSnapshot, error) {
	line := strings.TrimSpace(output)
	fields := strings.SplitN(line, ",", 5)
	if len(fields) < 5 {
		return nil, fmt.Errorf("expected 5 fields from nvidia-smi, got %d", len(fields))
	}

	snap := &GPUMetricsSnapshot{}

	if v, err := strconv.Atoi(strings.TrimSpace(fields[0])); err == nil {
		snap.TemperatureC = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(fields[1])); err == nil {
		snap.UtilizationPct = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(fields[2])); err == nil {
		snap.VRAMUsedMB = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(fields[3])); err == nil {
		snap.VRAMTotalMB = v
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64); err == nil {
		snap.PowerDrawWatts = v
	}

	return snap, nil
}

func (c *GPUMetricsCollector) collectAMD() (*GPUMetricsSnapshot, error) {
	out, err := CommandExecutor("rocm-smi",
		"-d", strconv.Itoa(c.deviceIdx),
		"--showtemp", "--showuse", "--showmemuse", "--showpower", "--csv")
	if err != nil {
		return nil, fmt.Errorf("rocm-smi metrics: %w", err)
	}
	return parseRocmMetrics(string(out))
}

// parseRocmMetrics parses rocm-smi metrics CSV output.
func parseRocmMetrics(output string) (*GPUMetricsSnapshot, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("expected at least 2 lines from rocm-smi, got %d", len(lines))
	}

	header := strings.Split(lines[0], ",")
	data := strings.Split(lines[1], ",")

	snap := &GPUMetricsSnapshot{}
	for i, col := range header {
		if i >= len(data) {
			break
		}
		col = strings.TrimSpace(strings.ToLower(col))
		val := strings.TrimSpace(data[i])

		switch {
		case strings.Contains(col, "temperature") || strings.Contains(col, "temp"):
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				snap.TemperatureC = int(v)
			}
		case strings.Contains(col, "gpu use") || strings.Contains(col, "utilization"):
			val = strings.TrimSuffix(val, "%")
			if v, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
				snap.UtilizationPct = int(v)
			}
		case strings.Contains(col, "vram") && strings.Contains(col, "used"):
			if v, err := strconv.ParseInt(val, 10, 64); err == nil {
				snap.VRAMUsedMB = int(v / (1024 * 1024))
			}
		case strings.Contains(col, "power"):
			val = strings.TrimSuffix(val, "W")
			if v, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
				snap.PowerDrawWatts = v
			}
		}
	}

	return snap, nil
}

// CollectDuringExecution polls GPU metrics at the given interval until ctx is cancelled.
// Returns aggregated metrics.
func (c *GPUMetricsCollector) CollectDuringExecution(ctx context.Context, interval time.Duration) *GPUExecutionMetrics {
	result := &GPUExecutionMetrics{}

	if c.vendor == "nvidia" {
		out, err := CommandExecutor("nvidia-smi",
			"--query-gpu=name",
			"--format=csv,noheader",
			"--id="+strconv.Itoa(c.deviceIdx))
		if err == nil {
			result.GPUModel = strings.TrimSpace(string(out))
		}
	}

	var totalUtil float64
	var sampleCount int

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if sampleCount > 0 {
				result.AvgUtilization = totalUtil / float64(sampleCount)
			}
			return result
		case <-ticker.C:
			snap, err := c.Collect()
			if err != nil {
				c.logger.Debug("GPU metrics collection failed", "error", err)
				continue
			}

			sampleCount++
			totalUtil += float64(snap.UtilizationPct)
			result.GPUSeconds += (float64(snap.UtilizationPct) / 100.0) * interval.Seconds()

			if snap.VRAMUsedMB > result.PeakVRAMMB {
				result.PeakVRAMMB = snap.VRAMUsedMB
			}
		}
	}
}
