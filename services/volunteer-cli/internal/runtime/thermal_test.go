package runtime

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// withMockCPUTemp overrides CPUTempReader for the duration of the test.
func withMockCPUTemp(t *testing.T, tempC int) {
	t.Helper()
	orig := CPUTempReader
	t.Cleanup(func() { CPUTempReader = orig })
	CPUTempReader = func() int { return tempC }
}

// withMockCPUTempFunc overrides CPUTempReader with a custom function.
func withMockCPUTempFunc(t *testing.T, fn func() int) {
	t.Helper()
	orig := CPUTempReader
	t.Cleanup(func() { CPUTempReader = orig })
	CPUTempReader = fn
}

func defaultThermalConfig() ThermalConfig {
	return ThermalConfig{
		Enabled:             true,
		CPUPauseThresholdC:  85,
		CPUResumeThresholdC: 75,
		GPUPauseThresholdC:  80,
		GPUResumeThresholdC: 70,
		PollIntervalSeconds: 1,
	}
}

func TestThermalMonitor_CPUExceedsThreshold(t *testing.T) {
	withMockCPUTemp(t, 90) // above 85C pause threshold
	withMockExecutor(t, notFoundForAll) // no GPU

	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := defaultThermalConfig()

	monitor := NewThermalMonitor(cfg, pauseCh, logger)
	monitor.SetPollIntervalForTest(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	monitor.Start(ctx)
	<-ctx.Done()
	monitor.Stop()

	// Should have received a pause signal.
	select {
	case paused := <-pauseCh:
		if !paused {
			t.Error("expected pause=true, got false")
		}
	default:
		t.Error("expected pause signal but channel was empty")
	}
}

func TestThermalMonitor_GPUExceedsThreshold(t *testing.T) {
	withMockCPUTemp(t, 60) // CPU cool
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "nvidia-smi" {
			return []byte("85, 90, 4096, 10240, 250.00\n"), nil // GPU at 85C, above 80C threshold
		}
		return nil, exec.ErrNotFound
	})

	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := defaultThermalConfig()

	monitor := NewThermalMonitor(cfg, pauseCh, logger)
	monitor.SetPollIntervalForTest(50 * time.Millisecond)
	monitor.SetGPUCollectors([]*GPUMetricsCollector{
		NewGPUMetricsCollector("nvidia", 0, logger),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	monitor.Start(ctx)
	<-ctx.Done()
	monitor.Stop()

	select {
	case paused := <-pauseCh:
		if !paused {
			t.Error("expected pause=true for GPU exceeding threshold")
		}
	default:
		t.Error("expected pause signal for GPU temperature")
	}
}

func TestThermalMonitor_ResumeWhenCool(t *testing.T) {
	callCount := 0
	withMockCPUTempFunc(t, func() int {
		callCount++
		if callCount <= 2 {
			return 90 // hot first
		}
		return 60 // then cool
	})
	withMockExecutor(t, notFoundForAll) // no GPU

	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := defaultThermalConfig()

	monitor := NewThermalMonitor(cfg, pauseCh, logger)
	monitor.SetPollIntervalForTest(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	monitor.Start(ctx)
	<-ctx.Done()
	monitor.Stop()

	// Drain the channel and verify we got pause then resume.
	var signals []bool
	for {
		select {
		case s := <-pauseCh:
			signals = append(signals, s)
		default:
			goto done
		}
	}
done:
	if len(signals) < 2 {
		t.Fatalf("expected at least 2 signals (pause + resume), got %d: %v", len(signals), signals)
	}
	if !signals[0] {
		t.Error("first signal should be pause (true)")
	}
	if signals[1] {
		t.Error("second signal should be resume (false)")
	}
}

func TestThermalMonitor_Hysteresis(t *testing.T) {
	// Temperature between resume (75) and pause (85) thresholds.
	// After being throttled, should NOT resume at 80C.
	callCount := 0
	withMockCPUTempFunc(t, func() int {
		callCount++
		if callCount <= 2 {
			return 90 // hot, triggers pause
		}
		return 80 // between resume (75) and pause (85) — should stay paused
	})
	withMockExecutor(t, notFoundForAll)

	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := defaultThermalConfig()

	monitor := NewThermalMonitor(cfg, pauseCh, logger)
	monitor.SetPollIntervalForTest(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	monitor.Start(ctx)
	<-ctx.Done()
	monitor.Stop()

	var signals []bool
	for {
		select {
		case s := <-pauseCh:
			signals = append(signals, s)
		default:
			goto done
		}
	}
done:
	// Should only have a pause signal. No resume because temp is above resume threshold.
	if len(signals) != 1 {
		t.Fatalf("expected 1 signal (pause only), got %d: %v", len(signals), signals)
	}
	if !signals[0] {
		t.Error("signal should be pause (true)")
	}
}

func TestThermalMonitor_OneHotOneCool(t *testing.T) {
	// CPU hot, GPU cool. Should stay paused until BOTH are below resume.
	callCount := 0
	withMockCPUTempFunc(t, func() int {
		callCount++
		if callCount <= 2 {
			return 90 // CPU hot
		}
		return 60 // CPU cool
	})
	// GPU always cool (no collectors = temp 0 = unknown = skip check)
	withMockExecutor(t, notFoundForAll)

	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := defaultThermalConfig()

	monitor := NewThermalMonitor(cfg, pauseCh, logger)
	monitor.SetPollIntervalForTest(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	monitor.Start(ctx)
	<-ctx.Done()
	monitor.Stop()

	var signals []bool
	for {
		select {
		case s := <-pauseCh:
			signals = append(signals, s)
		default:
			goto done
		}
	}
done:
	// Should get pause then resume (GPU temp unknown = OK).
	if len(signals) < 2 {
		t.Fatalf("expected at least 2 signals, got %d: %v", len(signals), signals)
	}
	if !signals[0] {
		t.Error("first signal should be pause")
	}
	if signals[1] {
		t.Error("second signal should be resume")
	}
}

func TestThermalMonitor_UnknownTemperature(t *testing.T) {
	withMockCPUTemp(t, 0) // unknown
	withMockExecutor(t, notFoundForAll) // no GPU either

	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := defaultThermalConfig()

	monitor := NewThermalMonitor(cfg, pauseCh, logger)
	monitor.SetPollIntervalForTest(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	monitor.Start(ctx)
	<-ctx.Done()
	monitor.Stop()

	// Unknown temp (0) should skip threshold check. No pause signal.
	select {
	case s := <-pauseCh:
		t.Errorf("expected no signal for unknown temperature, got %v", s)
	default:
		// good
	}
}

func TestThermalMonitor_Disabled(t *testing.T) {
	withMockCPUTemp(t, 100) // very hot, but monitor disabled
	withMockExecutor(t, notFoundForAll)

	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := defaultThermalConfig()
	cfg.Enabled = false

	monitor := NewThermalMonitor(cfg, pauseCh, logger)
	monitor.SetPollIntervalForTest(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	monitor.Start(ctx)
	<-ctx.Done()

	select {
	case s := <-pauseCh:
		t.Errorf("expected no signal when disabled, got %v", s)
	default:
		// good
	}
}

func TestThermalMonitor_StopIdempotent(t *testing.T) {
	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := defaultThermalConfig()
	monitor := NewThermalMonitor(cfg, pauseCh, logger)

	// Should not panic when called twice.
	monitor.Stop()
	monitor.Stop()
}

func TestThermalConfig_DisabledSkipsValidation(t *testing.T) {
	cfg := config.Defaults()
	cfg.Thermal.Enabled = false
	cfg.Thermal.CPUPauseThresholdC = 30
	cfg.Thermal.CPUResumeThresholdC = 100 // invalid if enabled (pause must be > resume)

	if err := cfg.Validate(); err != nil {
		t.Errorf("expected no error when thermal is disabled, got: %v", err)
	}
}

func TestThermalConfig_Validation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*config.Config)
		wantErr string
	}{
		{
			name: "pause <= resume CPU",
			modify: func(c *config.Config) {
				c.Thermal.CPUPauseThresholdC = 75
				c.Thermal.CPUResumeThresholdC = 75
			},
			wantErr: "cpu_pause_threshold",
		},
		{
			name: "pause <= resume GPU",
			modify: func(c *config.Config) {
				c.Thermal.GPUPauseThresholdC = 60
				c.Thermal.GPUResumeThresholdC = 70
			},
			wantErr: "gpu_pause_threshold",
		},
		{
			name: "CPU threshold below range",
			modify: func(c *config.Config) {
				c.Thermal.CPUResumeThresholdC = 20
			},
			wantErr: "cpu_resume_threshold",
		},
		{
			name: "GPU threshold above range",
			modify: func(c *config.Config) {
				c.Thermal.GPUPauseThresholdC = 110
			},
			wantErr: "gpu_pause_threshold",
		},
		{
			name: "poll interval too low",
			modify: func(c *config.Config) {
				c.Thermal.PollIntervalSeconds = 0
			},
			wantErr: "poll_interval_seconds",
		},
		{
			name: "poll interval too high",
			modify: func(c *config.Config) {
				c.Thermal.PollIntervalSeconds = 500
			},
			wantErr: "poll_interval_seconds",
		},
		{
			name:   "valid defaults",
			modify: func(c *config.Config) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			tt.modify(cfg)
			err := cfg.Validate()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected validation error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want to contain %q", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected validation error: %v", err)
				}
			}
		})
	}
}

