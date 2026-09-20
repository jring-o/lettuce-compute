package runtime

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TB-90 regression tests: SetConfig changes a running monitor's settings.
// Before this change the monitor had no setter — the daemon built it once
// from the config file and ApplyConfig never touched it — so a change saved
// in the app was on disk and in `config get`, and did nothing until the next
// restart, which the app never asked for.

// New thresholds judge the next sample: a steady 20 % foreign load pauses
// nothing at 25/15, pauses once the pause threshold drops to 18, and resumes
// once the resume threshold rises above 20 — all on the one running monitor.
func TestTB90_SetConfigThresholdsApplyWithoutRestart(t *testing.T) {
	sampler := &scriptedSampler{samples: []CPULoadSample{foreign(20)}}
	m, pauseCh, _ := newYieldTestMonitor(t, sampler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	// Ten samples' worth of silence at 20 < 25.
	expectNoSignal(t, pauseCh, 100*time.Millisecond)
	if n := sampler.count(); n < 3 {
		t.Fatalf("only %d samples taken; the window never filled", n)
	}

	cfg := yieldTestConfig()
	cfg.CPUPausePct, cfg.CPUResumePct = 18, 10
	m.SetConfig(cfg)
	waitSignal(t, pauseCh, true, time.Second)
	snap := m.Snapshot()
	if !snap.Paused || snap.PausePct != 18 || snap.ResumePct != 10 {
		t.Errorf("snapshot after lowering the pause threshold = %+v, want paused with 18/10", snap)
	}

	cfg.CPUPausePct, cfg.CPUResumePct = 40, 30
	m.SetConfig(cfg)
	waitSignal(t, pauseCh, false, time.Second)
	snap = m.Snapshot()
	if snap.Paused || snap.PausePct != 40 || snap.ResumePct != 30 {
		t.Errorf("snapshot after raising the resume threshold = %+v, want resumed with 40/30", snap)
	}
}

// Turning the setting off releases the pause the monitor holds, resolves the
// notice and stops sampling; turning it back on samples again and pauses
// after a fresh full window.
func TestTB90_DisablingReleasesPauseAndEnablingSamplesAgain(t *testing.T) {
	sampler := &scriptedSampler{samples: []CPULoadSample{foreign(60)}}
	m, pauseCh, sink := newYieldTestMonitor(t, sampler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()
	waitSignal(t, pauseCh, true, time.Second)

	off := yieldTestConfig()
	off.Enabled = false
	m.SetConfig(off)
	waitSignal(t, pauseCh, false, time.Second)
	snap := m.Snapshot()
	if snap.Paused || snap.Enabled {
		t.Errorf("snapshot after turning the setting off = %+v, want neither paused nor enabled", snap)
	}
	joined := strings.Join(sink.eventLog(), " ")
	if !strings.Contains(joined, "resolve:"+yieldBusyCode) {
		t.Errorf("the busy notice was not resolved when the setting was turned off: %s", joined)
	}
	taken := sampler.count()
	time.Sleep(50 * time.Millisecond)
	if n := sampler.count(); n != taken {
		t.Errorf("a disabled monitor kept sampling: %d samples after it was turned off", n-taken)
	}

	m.SetConfig(yieldTestConfig())
	waitSignal(t, pauseCh, true, time.Second)
	if n := sampler.count(); n < taken+3 {
		t.Errorf("paused after %d new samples, want a fresh full window of 3", n-taken)
	}
	if snap := m.Snapshot(); !snap.Paused || !snap.Enabled || !snap.Measurable {
		t.Errorf("snapshot after turning the setting back on = %+v, want paused, enabled, measurable", snap)
	}
}

// A monitor started with the setting off is idle until SetConfig turns it
// on — the daemon does not need to be restarted for the first enable either.
func TestTB90_EnablingAnIdleMonitorStartsIt(t *testing.T) {
	sampler := &scriptedSampler{samples: []CPULoadSample{foreign(60)}}
	pauseCh := make(chan bool, 1)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := yieldTestConfig()
	cfg.Enabled = false
	m := NewYieldMonitor(cfg, pauseCh, logger)
	m.SetSampler(sampler)
	m.SetPollIntervalForTest(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	expectNoSignal(t, pauseCh, 50*time.Millisecond)
	if n := sampler.count(); n != 0 {
		t.Fatalf("disabled monitor sampled %d times", n)
	}

	m.SetConfig(yieldTestConfig())
	waitSignal(t, pauseCh, true, time.Second)
	if snap := m.Snapshot(); !snap.Paused || !snap.Enabled {
		t.Errorf("snapshot = %+v, want paused and enabled", snap)
	}
}

// A longer window restarts the average: paused on three samples of 60 %,
// the monitor is told to average over six, and the resume — on 10 % samples
// — waits for six of them rather than three.
func TestTB90_ChangedWindowRestartsTheAverage(t *testing.T) {
	sampler := &scriptedSampler{samples: []CPULoadSample{
		foreign(60), foreign(60), foreign(60), // pause on the third
		foreign(10),                           // then quiet for good
	}}
	m, pauseCh, _ := newYieldTestMonitor(t, sampler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()
	waitSignal(t, pauseCh, true, time.Second)

	wider := yieldTestConfig()
	wider.WindowSeconds = 60 // six samples at the test's 10-per-30 ratio
	m.SetConfig(wider)
	waitSignal(t, pauseCh, false, 2*time.Second)
	// Three samples made the pause; the resume needs a full six-sample
	// window taken after the change, whether or not one 10 % sample landed
	// before the change was applied.
	if n := sampler.count(); n < 9 {
		t.Errorf("resumed after %d samples; a six-sample window could not have filled before the ninth", n)
	}
}
