package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Yielding to other programs (TB-83).
//
// Lettuce pauses for a schedule, for heat and for the volunteer's own hand,
// and until this monitor it never paused for the one thing every volunteer
// from the incumbent's community asks for by name: other programs needing the
// CPU. A volunteer sharing a machine with anything interactive, or with
// another volunteer client, had only static levers — a smaller core count, a
// schedule — and none that reacted to the machine getting busy.
//
// The monitor is the thermal monitor's shape with a different question: not
// "is the CPU hot" but "are OTHER programs using more than N % of it". The
// measurement is the whole machine's busy share minus Lettuce's own — the
// daemon, every native task tree, every running container — averaged over a
// window so a brief spike neither pauses nor resumes anything, with a lower
// resume threshold for hysteresis. Off by default; nothing changes for a
// volunteer who has not turned it on.
//
// Two lessons carried over from the thermal pause (TQ-29): a pause held on a
// reading that cannot be trusted is worse than no pause, so when Lettuce's own
// share cannot be measured the monitor says so once and never pauses on the
// raw total; and a paused daemon is never silent for hours — it re-announces
// itself every few minutes.

// YieldConfig configures the yield monitor; the daemon builds it from
// config.YieldConfig.
type YieldConfig struct {
	Enabled             bool
	CPUPausePct         int // pause when foreign load reaches this (default 25)
	CPUResumePct        int // resume when it falls to this (default 15)
	WindowSeconds       int // moving-average window (default 30)
	PollIntervalSeconds int // sampling interval within it (default 5)
}

// yieldBusyCode is the notice raised while work is paused for other
// programs' CPU use and resolved when it resumes. Emitted at Info: this is
// the volunteer's own setting doing what they asked, not a problem.
const yieldBusyCode = "yield_busy"

// yieldUnavailableCode is the notice raised once when the monitor is on but
// cannot measure the load it is meant to judge, so it never pauses.
const yieldUnavailableCode = "yield_unavailable"

// yieldLogInterval is how often an ongoing yield pause re-announces itself.
const yieldLogInterval = 5 * time.Minute

// YieldSnapshot is the monitor's state as `status`, the management API and
// the app report it.
type YieldSnapshot struct {
	// Enabled mirrors the configuration.
	Enabled bool
	// Measurable is false while sampling is failing: nothing pauses, and
	// Unavailable says why.
	Measurable  bool
	Unavailable string
	// ForeignPct is the current windowed average of other programs' CPU use
	// (0 until a full window has been sampled).
	ForeignPct float64
	// HaveWindow is true once a full window of samples has been taken, so a
	// ForeignPct of 0 can be told from "not measured yet".
	HaveWindow bool
	PausePct   int
	ResumePct  int
	// Paused is true while this monitor holds the daemon paused; PausedSince
	// is when it began.
	Paused      bool
	PausedSince time.Time
}

// YieldMonitor watches other programs' CPU use and signals pause/resume to
// the daemon via a channel, with hysteresis between the two thresholds.
type YieldMonitor struct {
	config       YieldConfig
	logger       *slog.Logger
	pauseCh      chan<- bool
	sampler      CPULoadSampler
	pollOverride time.Duration    // for testing; 0 = use config
	nowFn        func() time.Time // for testing; nil = time.Now
	notices      NoticeSink       // optional; nil discards notices

	mu      sync.Mutex
	stopCh  chan struct{}
	stopped bool
	snap    YieldSnapshot
}

// NewYieldMonitor creates a yield monitor. SetSampler must be called before
// Start; without a sampler the monitor reports itself unmeasurable and never
// pauses.
func NewYieldMonitor(cfg YieldConfig, pauseCh chan<- bool, logger *slog.Logger) *YieldMonitor {
	return &YieldMonitor{
		config:  cfg,
		logger:  logger,
		pauseCh: pauseCh,
		stopCh:  make(chan struct{}),
		snap: YieldSnapshot{
			Enabled:   cfg.Enabled,
			PausePct:  cfg.CPUPausePct,
			ResumePct: cfg.CPUResumePct,
		},
	}
}

// SetSampler sets the load source. Must be called before Start.
func (y *YieldMonitor) SetSampler(s CPULoadSampler) { y.sampler = s }

// SetNoticeSink routes the monitor's notices to the given sink. Must be
// called before Start.
func (y *YieldMonitor) SetNoticeSink(sink NoticeSink) { y.notices = sink }

// SetPollIntervalForTest overrides the poll interval (for testing only).
func (y *YieldMonitor) SetPollIntervalForTest(d time.Duration) { y.pollOverride = d }

// SetClockForTest overrides the monitor's clock (for testing only).
func (y *YieldMonitor) SetClockForTest(fn func() time.Time) { y.nowFn = fn }

// Snapshot reports the monitor's current state.
func (y *YieldMonitor) Snapshot() YieldSnapshot {
	y.mu.Lock()
	defer y.mu.Unlock()
	return y.snap
}

func (y *YieldMonitor) update(fn func(s *YieldSnapshot)) {
	y.mu.Lock()
	fn(&y.snap)
	y.mu.Unlock()
}

// Start begins load monitoring in a goroutine. Disabled monitors do nothing.
func (y *YieldMonitor) Start(ctx context.Context) {
	if !y.config.Enabled {
		return
	}
	interval := time.Duration(y.config.PollIntervalSeconds) * time.Second
	if y.pollOverride > 0 {
		interval = y.pollOverride
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	window := time.Duration(y.config.WindowSeconds) * time.Second
	if y.pollOverride > 0 && y.config.PollIntervalSeconds > 0 {
		// Keep the window-to-poll ratio under a test override.
		window = interval * time.Duration(y.config.WindowSeconds) / time.Duration(y.config.PollIntervalSeconds)
	}
	samples := int(window / interval)
	if samples < 1 {
		samples = 1
	}

	if y.sampler == nil {
		y.markUnavailable(errors.New("no load sampler on this platform"))
	} else {
		y.logger.Info("yield monitor started: pausing when other programs use the CPU",
			"pause_above_pct", y.config.CPUPausePct,
			"resume_below_pct", y.config.CPUResumePct,
			"window", window.String(),
			"poll_interval", interval.String(),
		)
		y.update(func(s *YieldSnapshot) { s.Measurable = true })
	}
	go y.run(ctx, interval, samples)
}

// markUnavailable records, once, that the load cannot be measured; the
// monitor will not pause on a figure it cannot trust.
func (y *YieldMonitor) markUnavailable(err error) {
	y.update(func(s *YieldSnapshot) {
		s.Measurable = false
		s.Unavailable = err.Error()
		s.HaveWindow = false
		s.ForeignPct = 0
	})
	y.logger.Warn("yield monitor cannot measure other programs' CPU use; it will not pause work until it can",
		"error", err.Error(),
		"note", "Lettuce's own share of the machine must be known before the rest can be blamed on other programs",
	)
	if y.notices != nil {
		y.notices.Notify(NoticeLevelWarn, yieldUnavailableCode,
			fmt.Sprintf("Yielding to other programs is on, but Lettuce cannot measure their CPU use on this machine right now (%s). Work will not be paused for other programs until it can.", err.Error()),
			"", "")
	}
}

func (y *YieldMonitor) run(ctx context.Context, interval time.Duration, windowSize int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	window := make([]float64, 0, windowSize)
	paused := false
	var pausedSince, lastLog time.Time
	// failures counts consecutive samples that could not be taken while
	// paused; a full window of them releases the pause, because a pause held
	// on a reading nobody can take is a hung daemon.
	failures := 0
	unavailable := y.sampler == nil

	for {
		select {
		case <-ctx.Done():
			return
		case <-y.stopCh:
			return
		case <-ticker.C:
			if y.sampler == nil {
				continue
			}
			sample, err := y.sampler.Sample()
			if errors.Is(err, ErrNoBaseline) {
				continue
			}
			if err != nil {
				window = window[:0]
				if !unavailable {
					unavailable = true
					y.markUnavailable(err)
				}
				if paused {
					failures++
					if failures >= windowSize {
						paused = false
						y.logger.Warn("yield pause released: other programs' CPU use can no longer be measured",
							"paused_for", y.now().Sub(pausedSince).Round(time.Second).String())
						y.setPaused(false, time.Time{})
						y.resolveBusyNotice()
						y.notify(NoticeLevelInfo, fmt.Sprintf("Computing resumed after %s: other programs' CPU use can no longer be measured, so the pause was released.",
							y.now().Sub(pausedSince).Round(time.Second).String()))
						y.signal(ctx, false)
					}
				}
				continue
			}
			failures = 0
			if unavailable {
				unavailable = false
				y.logger.Info("yield monitor can measure other programs' CPU use again")
				y.update(func(s *YieldSnapshot) { s.Measurable = true; s.Unavailable = "" })
				if y.notices != nil {
					y.notices.Resolve(yieldUnavailableCode, "", "")
				}
			}

			foreign := sample.ForeignPct()
			if len(window) == windowSize {
				window = append(window[:0], window[1:]...)
			}
			window = append(window, foreign)
			if len(window) < windowSize {
				continue
			}
			avg := average(window)
			y.update(func(s *YieldSnapshot) { s.ForeignPct = avg; s.HaveWindow = true })

			if !paused {
				if avg >= float64(y.config.CPUPausePct) {
					paused = true
					pausedSince = y.now()
					lastLog = pausedSince
					y.logger.Info("yield pause: other programs are using the CPU; computing paused",
						"foreign_cpu_pct", round1(avg),
						"machine_cpu_pct", round1(sample.MachinePct),
						"own_cpu_pct", round1(sample.OwnPct),
						"pause_above_pct", y.config.CPUPausePct,
						"resume_below_pct", y.config.CPUResumePct,
					)
					y.setPaused(true, pausedSince)
					y.notify(NoticeLevelInfo, fmt.Sprintf("Computing paused: other programs are using %.0f%% of the CPU (pause above %d%%, resume below %d%%). Work resumes on its own when they need less.",
						avg, y.config.CPUPausePct, y.config.CPUResumePct))
					y.signal(ctx, true)
				}
				continue
			}

			if avg <= float64(y.config.CPUResumePct) {
				paused = false
				pausedFor := y.now().Sub(pausedSince).Round(time.Second).String()
				y.logger.Info("yield pause released: other programs' CPU use fell; computing resumed",
					"foreign_cpu_pct", round1(avg),
					"paused_for", pausedFor,
				)
				y.setPaused(false, time.Time{})
				y.resolveBusyNotice()
				y.notify(NoticeLevelInfo, fmt.Sprintf("Computing resumed after %s: other programs' CPU use fell to %.0f%%.", pausedFor, avg))
				y.signal(ctx, false)
				continue
			}

			if y.now().Sub(lastLog) >= yieldLogInterval {
				lastLog = y.now()
				y.logger.Info("still paused for other programs' CPU use",
					"foreign_cpu_pct", round1(avg),
					"resume_below_pct", y.config.CPUResumePct,
					"paused_for", y.now().Sub(pausedSince).Round(time.Second).String(),
				)
			}
		}
	}
}

func (y *YieldMonitor) setPaused(paused bool, since time.Time) {
	y.update(func(s *YieldSnapshot) { s.Paused = paused; s.PausedSince = since })
}

func (y *YieldMonitor) notify(level, message string) {
	if y.notices == nil {
		return
	}
	y.notices.Notify(level, yieldBusyCode, message, "", "")
}

func (y *YieldMonitor) resolveBusyNotice() {
	if y.notices == nil {
		return
	}
	y.notices.Resolve(yieldBusyCode, "", "")
}

func average(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func round1(x float64) float64 {
	return float64(int(x*10+0.5)) / 10
}

func (y *YieldMonitor) now() time.Time {
	if y.nowFn != nil {
		return y.nowFn()
	}
	return time.Now()
}

// signal delivers a transition (true = pause, false = resume) to the daemon,
// blocking until it is received or the monitor is shutting down — the same
// undroppable send the thermal monitor uses (a lost pause leaves work running
// against the volunteer's wish; a lost resume leaves it paused for good).
func (y *YieldMonitor) signal(ctx context.Context, pause bool) {
	select {
	case y.pauseCh <- pause:
	case <-ctx.Done():
	case <-y.stopCh:
	}
}

// Stop signals the monitor to stop.
func (y *YieldMonitor) Stop() {
	y.mu.Lock()
	defer y.mu.Unlock()
	if !y.stopped {
		y.stopped = true
		close(y.stopCh)
	}
}

// DescribeYieldPause is the one sentence `status` and the app show while
// work is paused for other programs: the measured share and both thresholds.
func DescribeYieldPause(s YieldSnapshot) string {
	return fmt.Sprintf("other programs are using %.0f%% of the CPU (pause above %d%%, resume below %d%%)",
		s.ForeignPct, s.PausePct, s.ResumePct)
}
