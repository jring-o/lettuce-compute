package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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
	MaxThrottleMinutes  int // default 30; 0 disables the ceiling
}

// SensorClass is the threshold family a temperature sensor belongs to. A CPU and
// an NVMe controller do not share a danger point, and treating them as if they
// did is what TB-17 was.
type SensorClass int

const (
	// SensorOther is anything we cannot positively identify — storage, wireless,
	// chipset, battery, ambient, or an unrecognised vendor string. Judged only
	// against its own kernel-declared critical trip point, never against the CPU
	// thresholds.
	SensorOther SensorClass = iota
	SensorCPU
	SensorGPU
)

func (c SensorClass) String() string {
	switch c {
	case SensorCPU:
		return "cpu"
	case SensorGPU:
		return "gpu"
	default:
		return "other"
	}
}

// Sensor is one temperature reading with enough context to judge it.
type Sensor struct {
	Zone      string      // sysfs name, e.g. "thermal_zone2"
	Kind      string      // the zone's own type string, e.g. "nvme"
	Class     SensorClass // which threshold family applies
	TempC     int
	CriticalC int // vendor/kernel-declared danger point; 0 when undeclared
}

// CPUTempReader reads the current CPU temperature in degrees Celsius.
// Returns 0 if the temperature cannot be determined.
// Override in tests for mocking.
var CPUTempReader = readCPUTemperature

// SensorReader returns every temperature sensor this machine exposes,
// classified. Used for the non-CPU/GPU overheat check and by `doctor` to show a
// volunteer which sensor the client is actually watching. Override in tests.
var SensorReader = readSensors

// criticalOverheats returns sensors outside the CPU and GPU families that have
// reached their OWN declared danger point.
//
// A sensor with no declared critical trip can never pause work: we have no
// defensible threshold for a component we cannot identify, and inventing one is
// what froze a volunteer for two and a half hours. A sensor that does declare
// one is honoured, because that number came from the part itself.
func criticalOverheats(sensors []Sensor, resumeMarginC int) (pausing []Sensor, stillHot []Sensor) {
	for _, s := range sensors {
		if s.Class != SensorOther || s.CriticalC <= 0 {
			continue
		}
		if s.TempC >= s.CriticalC {
			pausing = append(pausing, s)
		}
		if s.TempC >= s.CriticalC-resumeMarginC {
			stillHot = append(stillHot, s)
		}
	}
	return pausing, stillHot
}

// criticalResumeMarginC is the hysteresis band below a sensor's declared
// critical point, mirroring the 10-degree CPU pause/resume gap.
const criticalResumeMarginC = 10

// defaultMaxThrottleMinutes bounds how long work stays frozen on one continuous
// throttle before the client forces a re-evaluation.
//
// Without a ceiling a throttle is a livelock whenever the heat is not ours to
// clear — a stuck sensor, a misclassified part, or a machine kept hot by the
// volunteer's own other work. Observed: 2 h 28 min frozen with no log output and
// no path back. Forcing a resume is safe in the sense that matters: every CPU
// throttles itself in hardware long before damage, so this layer is a courtesy
// to the volunteer's machine, not its protection.
const defaultMaxThrottleMinutes = 30

// throttleLogInterval is how often an ongoing throttle re-announces itself, so a
// frozen daemon is never silent for hours.
const throttleLogInterval = 5 * time.Minute

// ThermalMonitor watches CPU and GPU temperatures and signals pause/resume
// to the daemon via a channel. It implements hysteresis with separate
// pause and resume thresholds to prevent rapid cycling.
type ThermalMonitor struct {
	config        ThermalConfig
	logger        *slog.Logger
	pauseCh       chan<- bool
	gpuCollectors []*GPUMetricsCollector
	pollOverride  time.Duration    // for testing; 0 = use config
	nowFn         func() time.Time // for testing; nil = time.Now

	mu      sync.Mutex
	stopCh  chan struct{}
	stopped bool
}

// SetClockForTest overrides the monitor's clock (for testing only).
func (t *ThermalMonitor) SetClockForTest(fn func() time.Time) {
	t.nowFn = fn
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
	var throttledSince time.Time
	var lastThrottleLog time.Time
	// suppressed holds sensors that have been force-released past the throttle
	// ceiling, so a stuck reading cannot immediately re-trip the same freeze.
	suppressed := make(map[string]bool)

	maxThrottle := time.Duration(t.config.MaxThrottleMinutes) * time.Minute
	if t.config.MaxThrottleMinutes == 0 {
		maxThrottle = time.Duration(defaultMaxThrottleMinutes) * time.Minute
	}
	if t.config.MaxThrottleMinutes < 0 {
		maxThrottle = 0 // explicitly disabled: wait indefinitely
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stopCh:
			return
		case <-ticker.C:
			cpuTemp := CPUTempReader()
			gpuTemp := t.readGPUTemperature()
			sensors := t.readSensors()
			critPausing, critStillHot := criticalOverheats(sensors, criticalResumeMarginC)
			critPausing = filterSuppressed(critPausing, suppressed)
			critStillHot = filterSuppressed(critStillHot, suppressed)

			if !throttled {
				cpuHot := cpuTemp > 0 && cpuTemp >= t.config.CPUPauseThresholdC
				gpuHot := gpuTemp > 0 && gpuTemp >= t.config.GPUPauseThresholdC
				shouldPause := cpuHot || gpuHot || len(critPausing) > 0

				if shouldPause {
					throttled = true
					throttledSince = t.now()
					lastThrottleLog = throttledSince
					t.logger.Warn("thermal throttle activated",
						"cause", throttleCause(cpuHot, gpuHot, critPausing),
						"cpu_temp", cpuTemp,
						"cpu_threshold", t.config.CPUPauseThresholdC,
						"gpu_temp", gpuTemp,
						"gpu_threshold", t.config.GPUPauseThresholdC,
						"sensors", describeSensors(critPausing),
					)
					t.signal(ctx, true)
				}
				continue
			}

			cpuOK := cpuTemp == 0 || cpuTemp < t.config.CPUResumeThresholdC
			gpuOK := gpuTemp == 0 || gpuTemp < t.config.GPUResumeThresholdC

			if cpuOK && gpuOK && len(critStillHot) == 0 {
				throttled = false
				t.logger.Info("thermal throttle released",
					"cpu_temp", cpuTemp,
					"gpu_temp", gpuTemp,
					"paused_for", t.now().Sub(throttledSince).Round(time.Second).String(),
				)
				t.signal(ctx, false)
				continue
			}

			held := t.now().Sub(throttledSince)

			// Force a release once the ceiling is reached. Whatever is holding us
			// is not clearing, and staying frozen indefinitely on a signal this
			// client cannot influence contributes nothing while looking identical
			// to a hung daemon. Suppress the specific sensor so we do not thrash.
			if maxThrottle > 0 && held >= maxThrottle {
				for _, s := range critStillHot {
					suppressed[s.Zone] = true
				}
				if !cpuOK {
					suppressed["cpu"] = true
				}
				throttled = false
				t.logger.Warn("thermal throttle force-released: still hot after the maximum throttle time",
					"paused_for", held.Round(time.Second).String(),
					"max_throttle", maxThrottle.String(),
					"cpu_temp", cpuTemp,
					"gpu_temp", gpuTemp,
					"sensors", describeSensors(critStillHot),
					"note", "resuming work; this reading is not clearing while suspended, so it is unlikely to be ours. Hardware thermal protection is unaffected. Raise thermal.cpu_pause_threshold or set thermal.enabled=false if this recurs",
				)
				t.signal(ctx, false)
				continue
			}

			// A throttle used to be completely silent for its whole duration —
			// one line at the start and nothing until it cleared, which is why a
			// 2 h 28 min freeze reached support as "everything is working fine".
			if t.now().Sub(lastThrottleLog) >= throttleLogInterval {
				lastThrottleLog = t.now()
				t.logger.Info("still thermally throttled",
					"paused_for", held.Round(time.Second).String(),
					"cpu_temp", cpuTemp,
					"cpu_resume_below", t.config.CPUResumeThresholdC,
					"gpu_temp", gpuTemp,
					"sensors", describeSensors(critStillHot),
				)
			}
		}
	}
}

// filterSuppressed drops sensors that have already force-released a throttle.
func filterSuppressed(sensors []Sensor, suppressed map[string]bool) []Sensor {
	if len(suppressed) == 0 {
		return sensors
	}
	out := sensors[:0:0]
	for _, s := range sensors {
		if !suppressed[s.Zone] {
			out = append(out, s)
		}
	}
	return out
}

// throttleCause names what tripped the throttle, so the log says which part of
// the machine is hot rather than only how hot something is.
func throttleCause(cpuHot, gpuHot bool, crit []Sensor) string {
	var causes []string
	if cpuHot {
		causes = append(causes, "cpu")
	}
	if gpuHot {
		causes = append(causes, "gpu")
	}
	if len(crit) > 0 {
		causes = append(causes, "sensor-critical")
	}
	if len(causes) == 0 {
		return "unknown"
	}
	return strings.Join(causes, "+")
}

// describeSensors renders sensors for a log line: "thermal_zone2(nvme) 96C/95C".
func describeSensors(sensors []Sensor) string {
	if len(sensors) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sensors))
	for _, s := range sensors {
		parts = append(parts, fmt.Sprintf("%s(%s) %dC/%dC", s.Zone, s.Kind, s.TempC, s.CriticalC))
	}
	return strings.Join(parts, ", ")
}

// readSensors reads this machine's sensors, tolerating a nil reader.
func (t *ThermalMonitor) readSensors() []Sensor {
	if SensorReader == nil {
		return nil
	}
	return SensorReader()
}

// now is the monitor's clock, indirected so the throttle ceiling and the
// periodic log can be driven deterministically in tests.
func (t *ThermalMonitor) now() time.Time {
	if t.nowFn != nil {
		return t.nowFn()
	}
	return time.Now()
}

// signal delivers a throttle transition (true = pause, false = resume) to the
// daemon, blocking until the daemon receives it or the monitor is shutting down
// (ctx cancelled / Stop called). The send MUST block rather than drop: the
// caller flips the throttled state on the transition and only signals once per
// transition, so a dropped signal is never retried — a lost pause leaves work
// running hot with thermal protection silently disengaged, and a lost resume
// leaves the daemon paused indefinitely (#61). This mirrors the resource
// monitor's ctx-guarded blocking send (see resource.Monitor.Run); the daemon's
// thermalPauseCh is size-1, so a transient full buffer only delays delivery
// until the daemon drains it.
func (t *ThermalMonitor) signal(ctx context.Context, pause bool) {
	select {
	case t.pauseCh <- pause:
	case <-ctx.Done():
	case <-t.stopCh:
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
