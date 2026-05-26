package runtime

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestParseNvidiaMetrics(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr bool
		want    *GPUMetricsSnapshot
	}{
		{
			name:   "valid output",
			output: "65, 80, 4096, 10240, 250.00\n",
			want: &GPUMetricsSnapshot{
				TemperatureC:   65,
				UtilizationPct: 80,
				VRAMUsedMB:     4096,
				VRAMTotalMB:    10240,
				PowerDrawWatts: 250.0,
			},
		},
		{
			name:    "too few fields",
			output:  "65, 80, 4096\n",
			wantErr: true,
		},
		{
			name:    "empty output",
			output:  "",
			wantErr: true,
		},
		{
			name:   "fields with extra whitespace",
			output: " 72 , 95 , 8192 , 16384 , 300.50 ",
			want: &GPUMetricsSnapshot{
				TemperatureC:   72,
				UtilizationPct: 95,
				VRAMUsedMB:     8192,
				VRAMTotalMB:    16384,
				PowerDrawWatts: 300.5,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNvidiaMetrics(tt.output)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.TemperatureC != tt.want.TemperatureC {
				t.Errorf("TemperatureC = %d, want %d", got.TemperatureC, tt.want.TemperatureC)
			}
			if got.UtilizationPct != tt.want.UtilizationPct {
				t.Errorf("UtilizationPct = %d, want %d", got.UtilizationPct, tt.want.UtilizationPct)
			}
			if got.VRAMUsedMB != tt.want.VRAMUsedMB {
				t.Errorf("VRAMUsedMB = %d, want %d", got.VRAMUsedMB, tt.want.VRAMUsedMB)
			}
			if got.VRAMTotalMB != tt.want.VRAMTotalMB {
				t.Errorf("VRAMTotalMB = %d, want %d", got.VRAMTotalMB, tt.want.VRAMTotalMB)
			}
			if got.PowerDrawWatts != tt.want.PowerDrawWatts {
				t.Errorf("PowerDrawWatts = %f, want %f", got.PowerDrawWatts, tt.want.PowerDrawWatts)
			}
		})
	}
}

func TestParseRocmMetrics(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr bool
		want    *GPUMetricsSnapshot
	}{
		{
			name: "valid output",
			output: "device,Temperature (Sensor edge) (C),GPU use (%),VRAM Total Used Memory (B),Average Graphics Package Power (W)\n" +
				"card0,65,80%,4294967296,250.0W\n",
			want: &GPUMetricsSnapshot{
				TemperatureC:   65,
				UtilizationPct: 80,
				VRAMUsedMB:     4096,
				PowerDrawWatts: 250.0,
			},
		},
		{
			name:    "header only",
			output:  "device,Temperature\n",
			wantErr: true,
		},
		{
			name:    "empty",
			output:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRocmMetrics(tt.output)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.TemperatureC != tt.want.TemperatureC {
				t.Errorf("TemperatureC = %d, want %d", got.TemperatureC, tt.want.TemperatureC)
			}
			if got.UtilizationPct != tt.want.UtilizationPct {
				t.Errorf("UtilizationPct = %d, want %d", got.UtilizationPct, tt.want.UtilizationPct)
			}
			if got.VRAMUsedMB != tt.want.VRAMUsedMB {
				t.Errorf("VRAMUsedMB = %d, want %d", got.VRAMUsedMB, tt.want.VRAMUsedMB)
			}
			if got.PowerDrawWatts != tt.want.PowerDrawWatts {
				t.Errorf("PowerDrawWatts = %f, want %f", got.PowerDrawWatts, tt.want.PowerDrawWatts)
			}
		})
	}
}

func TestGPUMetricsCollector_CollectNVIDIA(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			return []byte("65, 80, 4096, 10240, 250.00\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	collector := NewGPUMetricsCollector("nvidia", 0, logger)

	snap, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if snap.TemperatureC != 65 {
		t.Errorf("TemperatureC = %d, want 65", snap.TemperatureC)
	}
	if snap.UtilizationPct != 80 {
		t.Errorf("UtilizationPct = %d, want 80", snap.UtilizationPct)
	}
}

func TestGPUMetricsCollector_CollectNVIDIANotFound(t *testing.T) {
	withMockExecutor(t, notFoundForAll)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	collector := NewGPUMetricsCollector("nvidia", 0, logger)

	_, err := collector.Collect()
	if err == nil {
		t.Fatal("expected error when nvidia-smi not found")
	}
}

func TestGPUMetricsCollector_CollectAMD(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "rocm-smi" {
			return []byte("device,Temperature (Sensor edge) (C),GPU use (%),VRAM Total Used Memory (B),Average Graphics Package Power (W)\n" +
				"card0,72,90%,8589934592,300.0W\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	collector := NewGPUMetricsCollector("amd", 0, logger)

	snap, err := collector.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if snap.TemperatureC != 72 {
		t.Errorf("TemperatureC = %d, want 72", snap.TemperatureC)
	}
}

func TestGPUMetricsCollector_CollectUnsupportedVendor(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	collector := NewGPUMetricsCollector("intel", 0, logger)

	_, err := collector.Collect()
	if err == nil {
		t.Fatal("expected error for unsupported vendor")
	}
}

func TestGPUMetricsCollector_CollectDuringExecution(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			for _, a := range args {
				if strings.Contains(a, "name") {
					return []byte("NVIDIA RTX 3080\n"), nil
				}
			}
			return []byte("65, 80, 4096, 10240, 250.00\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	collector := NewGPUMetricsCollector("nvidia", 0, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	metrics := collector.CollectDuringExecution(ctx, 50*time.Millisecond)

	if metrics.GPUModel != "NVIDIA RTX 3080" {
		t.Errorf("GPUModel = %q, want %q", metrics.GPUModel, "NVIDIA RTX 3080")
	}
	if metrics.GPUSeconds <= 0 {
		t.Error("GPUSeconds should be > 0")
	}
	if metrics.PeakVRAMMB != 4096 {
		t.Errorf("PeakVRAMMB = %d, want 4096", metrics.PeakVRAMMB)
	}
	if metrics.AvgUtilization != 80 {
		t.Errorf("AvgUtilization = %f, want 80", metrics.AvgUtilization)
	}
}

func TestGPUMetricsCollector_CollectDuringExecutionAllFail(t *testing.T) {
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			for _, a := range args {
				if strings.Contains(a, "name") {
					return []byte("NVIDIA RTX 3080\n"), nil
				}
			}
			// Metrics query fails.
			return nil, exec.ErrNotFound
		}
		return nil, exec.ErrNotFound
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	collector := NewGPUMetricsCollector("nvidia", 0, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	metrics := collector.CollectDuringExecution(ctx, 50*time.Millisecond)

	// Should return zero metrics when all collections fail.
	if metrics.GPUSeconds != 0 {
		t.Errorf("GPUSeconds = %f, want 0", metrics.GPUSeconds)
	}
	if metrics.PeakVRAMMB != 0 {
		t.Errorf("PeakVRAMMB = %d, want 0", metrics.PeakVRAMMB)
	}
}
