package runtime

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ThermalConfig configures the thermal monitoring thresholds.
type ThermalConfig struct {
	Enabled             bool
	CPUPauseThresholdC  int // default 85
	CPUResumeThresholdC int // default 75
	GPUPauseThresholdC  int // default 80
	GPUResumeThresholdC int // default 70
	PollIntervalSeconds int // default 10
}

// CPUTempReader reads the current CPU temperature in degrees Celsius.
// Returns 0 if the temperature cannot be determined.
// Override in tests for mocking.
var CPUTempReader = readCPUTemperature

// ThermalMonitor watches CPU and GPU temperatures and signals pause/resume
// to the daemon via a channel. It implements hysteresis with separate
// pause and resume thresholds to prevent rapid cycling.
type ThermalMonitor struct {
	config        ThermalConfig
	logger        *slog.Logger
	pauseCh       chan<- bool
	gpuCollectors []*GPUMetricsCollector
	pollOverride  time.Duration // for testing; 0 = use config

	mu      sync.Mutex
	stopCh  chan struct{}
	stopped bool
}

// NewThermalMonitor creates a new thermal monitor.
func NewThermalMonitor(cfg ThermalConfig, pauseCh chan<- bool, logger *slog.Logger) *ThermalMonitor {
	return &ThermalMonitor{
		config:  cfg,
		logger:  logger,
		pauseCh: pauseCh,
		stopCh:  make(chan struct{}),
	}
}

// SetGPUCollectors sets the GPU metrics collectors for temperature monitoring.
func (t *ThermalMonitor) SetGPUCollectors(collectors []*GPUMetricsCollector) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gpuCollectors = collectors
}

// SetPollIntervalForTest overrides the poll interval (for testing only).
func (t *ThermalMonitor) SetPollIntervalForTest(d time.Duration) {
	t.pollOverride = d
}

// Start begins temperature monitoring in a goroutine.
func (t *ThermalMonitor) Start(ctx context.Context) {
	if !t.config.Enabled {
		return
	}

	interval := time.Duration(t.config.PollIntervalSeconds) * time.Second
	if t.pollOverride > 0 {
		interval = t.pollOverride
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}

	go t.run(ctx, interval)
}

func (t *ThermalMonitor) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	throttled := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stopCh:
			return
		case <-ticker.C:
			cpuTemp := CPUTempReader()
			gpuTemp := t.readGPUTemperature()

			if !throttled {
				shouldPause := false
				if cpuTemp > 0 && cpuTemp >= t.config.CPUPauseThresholdC {
					shouldPause = true
				}
				if gpuTemp > 0 && gpuTemp >= t.config.GPUPauseThresholdC {
					shouldPause = true
				}

				if shouldPause {
					throttled = true
					t.logger.Warn("thermal throttle activated",
						"cpu_temp", cpuTemp,
						"cpu_threshold", t.config.CPUPauseThresholdC,
						"gpu_temp", gpuTemp,
						"gpu_threshold", t.config.GPUPauseThresholdC,
					)
					select {
					case t.pauseCh <- true:
					default:
					}
				}
			} else {
				cpuOK := cpuTemp == 0 || cpuTemp < t.config.CPUResumeThresholdC
				gpuOK := gpuTemp == 0 || gpuTemp < t.config.GPUResumeThresholdC

				if cpuOK && gpuOK {
					throttled = false
					t.logger.Info("thermal throttle released",
						"cpu_temp", cpuTemp,
						"gpu_temp", gpuTemp,
					)
					select {
					case t.pauseCh <- false:
					default:
					}
				}
			}
		}
	}
}

// readGPUTemperature reads the highest GPU temperature from all collectors.
func (t *ThermalMonitor) readGPUTemperature() int {
	t.mu.Lock()
	collectors := t.gpuCollectors
	t.mu.Unlock()

	maxTemp := 0
	for _, c := range collectors {
		snap, err := c.Collect()
		if err != nil {
			continue
		}
		if snap.TemperatureC > maxTemp {
			maxTemp = snap.TemperatureC
		}
	}
	return maxTemp
}

// Stop signals the monitor to stop.
func (t *ThermalMonitor) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.stopped {
		t.stopped = true
		close(t.stopCh)
	}
}
