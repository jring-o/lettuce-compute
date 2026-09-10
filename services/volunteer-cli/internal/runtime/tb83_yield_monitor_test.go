package runtime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TB-83 regression tests for the yield monitor: pause when OTHER programs'
// CPU use, averaged over the window, reaches the pause threshold; resume when
// it falls to the resume threshold; never on Lettuce's own load; never on a
// figure that cannot be measured.

// scriptedSampler replays a series of samples, then repeats the last one.
type scriptedSampler struct {
	mu      sync.Mutex
	samples []CPULoadSample
	errs    []error
	calls   int
}

func (s *scriptedSampler) Sample() (CPULoadSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i >= len(s.samples) {
		i = len(s.samples) - 1
	}
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return s.samples[i], err
}

func (s *scriptedSampler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func yieldTestConfig() YieldConfig {
	return YieldConfig{Enabled: true, CPUPausePct: 25, CPUResumePct: 15, WindowSeconds: 30, PollIntervalSeconds: 10}
}

// newYieldTestMonitor polls every 10 ms with a 3-sample window (30/10).
func newYieldTestMonitor(t *testing.T, sampler CPULoadSampler) (*YieldMonitor, chan bool, *recordingSink) {
	t.Helper()
	pauseCh := make(chan bool, 1)
	sink := &recordingSink{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewYieldMonitor(yieldTestConfig(), pauseCh, logger)
	m.SetSampler(sampler)
	m.SetNoticeSink(sink)
	m.SetPollIntervalForTest(10 * time.Millisecond)
	return m, pauseCh, sink
}

func waitSignal(t *testing.T, ch <-chan bool, want bool, timeout time.Duration) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("pause signal = %v, want %v", got, want)
		}
	case <-time.After(timeout):
		t.Fatalf("no %v signal within %s", want, timeout)
	}
}

func expectNoSignal(t *testing.T, ch <-chan bool, d time.Duration) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("unexpected pause signal %v", got)
	case <-time.After(d):
	}
}

func foreign(pct float64) CPULoadSample { return CPULoadSample{MachinePct: pct} }

// Pause at the threshold only after a full window, not before: three samples
// of 60 % foreign load, the first two of which must not pause anything.
func TestTB83_PausesAfterFullWindowNotBefore(t *testing.T) {
	sampler := &scriptedSampler{samples: []CPULoadSample{foreign(60)}}
	m, pauseCh, sink := newYieldTestMonitor(t, sampler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitSignal(t, pauseCh, true, time.Second)
	if n := sampler.count(); n < 3 {
		t.Errorf("paused after %d samples, want a full window of 3", n)
	}
	snap := m.Snapshot()
	if !snap.Paused || snap.ForeignPct < 59 || snap.ForeignPct > 61 {
		t.Errorf("snapshot = %+v, want paused at ~60%% foreign", snap)
	}
	notices := sink.snapshot()
	if len(notices) != 1 || notices[0].code != yieldBusyCode || notices[0].level != NoticeLevelInfo {
		t.Fatalf("notices = %+v, want one %s at info", notices, yieldBusyCode)
	}
	for _, want := range []string{"60%", "pause above 25%", "resume below 15%"} {
		if !strings.Contains(notices[0].message, want) {
			t.Errorf("pause notice lacks %q: %q", want, notices[0].message)
		}
	}
}

// Hysteresis: a paused monitor stays paused between the two thresholds and
// resumes only once the windowed average is at or below the resume one.
func TestTB83_ResumesAtLowerThresholdOnly(t *testing.T) {
	sampler := &scriptedSampler{samples: []CPULoadSample{
		foreign(60), foreign(60), foreign(60), // pause
		foreign(20), foreign(20), foreign(20), // between the thresholds: still paused
		foreign(10), foreign(10), foreign(10), // at/below resume: resume
	}}
	m, pauseCh, sink := newYieldTestMonitor(t, sampler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitSignal(t, pauseCh, true, time.Second)
	// The window's average passes through 20 (between thresholds) before it
	// reaches 10; a resume must not arrive until the 10s dominate.
	waitSignal(t, pauseCh, false, time.Second)
	if n := sampler.count(); n < 8 {
		t.Errorf("resumed after %d samples; the average could not have been ≤ 15%% before the eighth", n)
	}
	events := sink.eventLog()
	joined := strings.Join(events, " ")
	if !strings.Contains(joined, "resolve:"+yieldBusyCode) {
		t.Errorf("resume must resolve the pause notice; events %v", events)
	}
	if snap := m.Snapshot(); snap.Paused {
		t.Errorf("snapshot still paused after resume: %+v", snap)
	}
}

// The own-load exclusion the specification names as a test: Lettuce's own
// two units at full quota never trigger the pause.
func TestTB83_OwnLoadAloneNeverPauses(t *testing.T) {
	sampler := &scriptedSampler{samples: []CPULoadSample{{MachinePct: 100, OwnPct: 100}}}
	m, pauseCh, _ := newYieldTestMonitor(t, sampler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	expectNoSignal(t, pauseCh, 150*time.Millisecond)
	if n := sampler.count(); n < 5 {
		t.Fatalf("only %d samples taken; the window never filled", n)
	}
	if snap := m.Snapshot(); snap.Paused || snap.ForeignPct != 0 {
		t.Errorf("snapshot = %+v, want not paused with 0%% foreign", snap)
	}
}

// Unavailable own-measurement never pauses on the raw total: one warning
// notice, no pause, and the notice resolves when measurement recovers.
func TestTB83_UnmeasurableLoadNeverPauses(t *testing.T) {
	boom := errors.New("engine stats unavailable")
	sampler := &scriptedSampler{
		samples: []CPULoadSample{foreign(90), foreign(90), foreign(90), foreign(90), foreign(90), foreign(90), foreign(5)},
		errs:    []error{boom, boom, boom, boom, boom, boom, nil},
	}
	m, pauseCh, sink := newYieldTestMonitor(t, sampler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	expectNoSignal(t, pauseCh, 150*time.Millisecond)
	events := sink.eventLog()
	if got := strings.Count(strings.Join(events, " "), "notify:warn"); got != 1 {
		t.Errorf("unavailable warning raised %d times, want exactly once; events %v", got, events)
	}
	if !strings.Contains(strings.Join(events, " "), "resolve:"+yieldUnavailableCode) {
		t.Errorf("recovery must resolve the unavailable notice; events %v", events)
	}
	if snap := m.Snapshot(); !snap.Measurable {
		t.Errorf("snapshot = %+v, want measurable again after recovery", snap)
	}
}

// A sampler still taking its baseline is skipped silently: no notice, no
// pause, and the window starts from the first real sample.
func TestTB83_BaselineSamplesAreSkippedSilently(t *testing.T) {
	sampler := &scriptedSampler{
		samples: []CPULoadSample{{}, {}, foreign(60)},
		errs:    []error{ErrNoBaseline, ErrNoBaseline, nil},
	}
	m, pauseCh, sink := newYieldTestMonitor(t, sampler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitSignal(t, pauseCh, true, time.Second)
	for _, e := range sink.eventLog() {
		if e == "notify:warn" {
			t.Errorf("a baseline sample must not raise the unavailable warning; events %v", sink.eventLog())
		}
	}
}

// A pause held on a reading nobody can take is a hung daemon: a full window
// of failed samples while paused releases it.
func TestTB83_PauseReleasedWhenMeasurementIsLost(t *testing.T) {
	boom := errors.New("gone")
	sampler := &scriptedSampler{
		samples: []CPULoadSample{foreign(60), foreign(60), foreign(60), {}, {}, {}, {}},
		errs:    []error{nil, nil, nil, boom, boom, boom, boom},
	}
	m, pauseCh, _ := newYieldTestMonitor(t, sampler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitSignal(t, pauseCh, true, time.Second)
	waitSignal(t, pauseCh, false, time.Second)
	if snap := m.Snapshot(); snap.Paused || snap.Measurable {
		t.Errorf("snapshot = %+v, want released and unmeasurable", snap)
	}
}

// A disabled monitor never starts.
func TestTB83_DisabledMonitorDoesNothing(t *testing.T) {
	sampler := &scriptedSampler{samples: []CPULoadSample{foreign(100)}}
	pauseCh := make(chan bool, 1)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := yieldTestConfig()
	cfg.Enabled = false
	m := NewYieldMonitor(cfg, pauseCh, logger)
	m.SetSampler(sampler)
	m.SetPollIntervalForTest(5 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	expectNoSignal(t, pauseCh, 50*time.Millisecond)
	if sampler.count() != 0 {
		t.Errorf("disabled monitor sampled %d times", sampler.count())
	}
	if snap := m.Snapshot(); snap.Enabled {
		t.Errorf("snapshot = %+v, want disabled", snap)
	}
}

// The sentence status and the app show names the share and both thresholds.
func TestTB83_DescribeYieldPause(t *testing.T) {
	got := DescribeYieldPause(YieldSnapshot{ForeignPct: 61.7, PausePct: 25, ResumePct: 15})
	want := "other programs are using 62% of the CPU (pause above 25%, resume below 15%)"
	if got != want {
		t.Errorf("DescribeYieldPause = %q, want %q", got, want)
	}
}
