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

// defaultMaxThrottleMinutes bounds how long work stays frozen on a throttle held
// by a sensor OUTSIDE the CPU and GPU families.
//
// It deliberately does NOT bound a CPU or GPU throttle. Backing off a machine
// whose processor is genuinely hot is the whole point of this feature and is
// correct for as long as it stays hot — our own load is the plausible cause, and
// a volunteer's machine matters more than our throughput. The ceiling exists
// only for the case we cannot influence: a drive under someone else's I/O, a
// chipset, or a sensor stuck above its own trip point, where staying frozen
// helps nothing and is indistinguishable from a hung daemon.
const defaultMaxThrottleMinutes = 30

// suppressionCooldown is how long a sensor that held work past the throttle
// ceiling is ignored afterwards. Bounded rather than permanent: a part that
// genuinely overheats again later must still be able to pause work, so this
// buys quiet rather than switching the sensor off for the daemon's lifetime.
const suppressionCooldown = 2 * time.Hour

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
	notices       NoticeSink       // optional; nil discards notices

	mu      sync.Mutex
	stopCh  chan struct{}
	stopped bool
}

// NoticeSink receives volunteer-facing notices — the throttle activated /
// released escalations that the monitor otherwise only logs. The daemon's
// notice ring implements it; declared here (not imported) because this
// package cannot depend on the daemon package that depends on it.
type NoticeSink interface {
	Notify(level, code, message, head, leaf string)
}

// SetNoticeSink routes the monitor's throttle notices to the given sink. Must
// be called before Start.
func (t *ThermalMonitor) SetNoticeSink(sink NoticeSink) {
	t.notices = sink
}

// notify forwards a notice to the sink, if one is set.
func (t *ThermalMonitor) notify(level, message string) {
	if t.notices == nil {
		return
	}
	t.notices.Notify(level, "thermal_throttle", message, "", "")
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
	// suppressed maps a zone to the time its force-release cooldown ends, so a
	// sensor that held work past the ceiling cannot immediately re-trip the same
	// freeze. Time-bounded rather than permanent: a part that genuinely overheats
	// again later must still be able to pause work.
	suppressed := make(map[string]time.Time)

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
			critPausing = filterSuppressed(critPausing, suppressed, t.now())
			critStillHot = filterSuppressed(critStillHot, suppressed, t.now())

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
					t.notify("warn", fmt.Sprintf("Computing paused: thermal throttle activated (cause: %s; CPU %d°C, pause threshold %d°C; GPU %d°C, pause threshold %d°C%s). Work resumes when temperatures fall below the resume thresholds.",
						throttleCause(cpuHot, gpuHot, critPausing), cpuTemp, t.config.CPUPauseThresholdC, gpuTemp, t.config.GPUPauseThresholdC,
						sensorSuffix(critPausing)))
					t.signal(ctx, true)
				}
				continue
			}

			cpuOK := cpuTemp == 0 || cpuTemp < t.config.CPUResumeThresholdC
			gpuOK := gpuTemp == 0 || gpuTemp < t.config.GPUResumeThresholdC

			if cpuOK && gpuOK && len(critStillHot) == 0 {
				throttled = false
				pausedFor := t.now().Sub(throttledSince).Round(time.Second).String()
				t.logger.Info("thermal throttle released",
					"cpu_temp", cpuTemp,
					"gpu_temp", gpuTemp,
					"paused_for", pausedFor,
				)
				t.notify("info", fmt.Sprintf("Thermal throttle released after %s (CPU %d°C, GPU %d°C); computing resumed.",
					pausedFor, cpuTemp, gpuTemp))
				t.signal(ctx, false)
				continue
			}

			held := t.now().Sub(throttledSince)

			// The ceiling applies ONLY when a live CPU/GPU reading is not what is
			// holding us — i.e. reaching here with cpuOK && gpuOK means some other
			// sensor is past its own declared critical point and staying there.
			//
			// A genuinely hot CPU or GPU must keep work suspended for as long as it
			// stays hot, however long that is. That is the entire purpose of the
			// feature, our own load is the plausible cause, and backing off is the
			// correct response to a machine in trouble whatever caused it. An
			// earlier version of this code force-released on ANY cause, which both
			// contradicted that and — because the CPU check never consulted the
			// suppression map — produced a freeze/unfreeze cycle at the ceiling
			// interval rather than the single clean release it logged.
			//
			// What remains is the case the ceiling was actually for: a part whose
			// temperature this client cannot influence (a drive under someone
			// else's I/O, a chipset) or a sensor stuck above its trip point.
			// Freezing forever on that contributes nothing and is
			// indistinguishable from a hung daemon.
			if maxThrottle > 0 && held >= maxThrottle && cpuOK && gpuOK && len(critStillHot) > 0 {
				until := t.now().Add(suppressionCooldown)
				for _, s := range critStillHot {
					suppressed[s.Zone] = until
				}
				throttled = false
				t.logger.Warn("thermal throttle force-released: a non-CPU sensor stayed past its own critical point",
					"paused_for", held.Round(time.Second).String(),
					"max_throttle", maxThrottle.String(),
					"cpu_temp", cpuTemp,
					"gpu_temp", gpuTemp,
					"sensors", describeSensors(critStillHot),
					"suppressed_for", suppressionCooldown.String(),
					"note", "the CPU and GPU are within their thresholds; this reading is not clearing while work is suspended, so suspending is not helping it. Resuming, and ignoring these sensors for the cooldown. Hardware thermal protection is unaffected.",
				)
				t.notify("warn", fmt.Sprintf("Thermal throttle force-released after %s: the CPU (%d°C) and GPU (%d°C) are within their thresholds, but %s stayed past its own critical point and suspending work was not helping it. Computing resumed; those sensors are ignored for %s. Hardware thermal protection is unaffected.",
					held.Round(time.Second).String(), cpuTemp, gpuTemp, describeSensors(critStillHot), suppressionCooldown.String()))
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

// filterSuppressed drops sensors whose force-release cooldown has not expired.
func filterSuppressed(sensors []Sensor, suppressed map[string]time.Time, now time.Time) []Sensor {
	if len(suppressed) == 0 {
		return sensors
	}
	out := sensors[:0:0]
	for _, s := range sensors {
		if until, ok := suppressed[s.Zone]; ok && now.Before(until) {
			continue
		}
		out = append(out, s)
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

// sensorSuffix renders sensors for a notice sentence — "; sensors: ..." — or
// nothing when there are none, so the sentence reads cleanly either way.
func sensorSuffix(sensors []Sensor) string {
	if len(sensors) == 0 {
		return ""
	}
	return "; sensors: " + describeSensors(sensors)
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
