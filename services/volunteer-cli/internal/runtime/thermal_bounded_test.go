package runtime

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TB-17, second half: a thermal throttle had no ceiling. The only exit was the
// reading falling below the resume threshold, so when the heat was not this
// client's to clear — a stuck sensor, a machine kept hot by the volunteer's own
// other work — the daemon waited forever. Observed: 2 h 28 min frozen, with a
// single log line at the start and nothing until it cleared.

// fakeClock is a manually advanced clock so the ceiling can be exercised without
// real waiting.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1800000000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// withMockSensors overrides SensorReader for the duration of the test.
func withMockSensors(t *testing.T, sensors []Sensor) {
	t.Helper()
	orig := SensorReader
	t.Cleanup(func() { SensorReader = orig })
	SensorReader = func() []Sensor { return sensors }
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal(msg)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// collectSignals drains the pause channel into a slice.
func collectSignals(ch chan bool, into *[]bool, mu *sync.Mutex, done <-chan struct{}) {
	for {
		select {
		case v := <-ch:
			mu.Lock()
			*into = append(*into, v)
			mu.Unlock()
		case <-done:
			return
		}
	}
}

// A CPU sensor stuck above the resume threshold must not freeze work forever.
func TestThermalMonitor_ForceReleasesAfterMaxThrottle(t *testing.T) {
	withMockCPUTemp(t, 90) // permanently above the 85C pause / 75C resume band
	withMockExecutor(t, notFoundForAll)

	clock := newFakeClock()
	cfg := defaultThermalConfig()
	cfg.MaxThrottleMinutes = 30

	ch := make(chan bool, 8)
	var mu sync.Mutex
	var signals []bool
	done := make(chan struct{})
	go collectSignals(ch, &signals, &mu, done)
	defer close(done)

	m := NewThermalMonitor(cfg, ch, testLogger())
	m.SetClockForTest(clock.now)
	m.SetPollIntervalForTest(5 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	// Wait for the pause.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(signals) >= 1 && signals[0]
	}, "thermal throttle never activated at 90C")

	// Hold past the ceiling. The temperature never drops.
	clock.advance(31 * time.Minute)

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range signals[1:] {
			if !s {
				return true
			}
		}
		return false
	}, "still frozen 31 minutes into a 30-minute ceiling: a reading that never clears while suspended freezes the volunteer indefinitely")
}

// The ceiling must not cut a throttle short while it is still working.
func TestThermalMonitor_DoesNotReleaseBeforeCeiling(t *testing.T) {
	withMockCPUTemp(t, 90)
	withMockExecutor(t, notFoundForAll)

	clock := newFakeClock()
	cfg := defaultThermalConfig()
	cfg.MaxThrottleMinutes = 30

	ch := make(chan bool, 8)
	var mu sync.Mutex
	var signals []bool
	done := make(chan struct{})
	go collectSignals(ch, &signals, &mu, done)
	defer close(done)

	m := NewThermalMonitor(cfg, ch, testLogger())
	m.SetClockForTest(clock.now)
	m.SetPollIntervalForTest(5 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(signals) >= 1
	}, "thermal throttle never activated")

	clock.advance(20 * time.Minute) // inside the ceiling
	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, s := range signals[1:] {
		if !s {
			t.Fatal("throttle released 20 minutes into a 30-minute ceiling while the CPU was still at 90C")
		}
	}
}

// A negative ceiling is the explicit "wait indefinitely" opt-out.
func TestThermalMonitor_NegativeCeilingWaitsIndefinitely(t *testing.T) {
	withMockCPUTemp(t, 90)
	withMockExecutor(t, notFoundForAll)

	clock := newFakeClock()
	cfg := defaultThermalConfig()
	cfg.MaxThrottleMinutes = -1

	ch := make(chan bool, 8)
	var mu sync.Mutex
	var signals []bool
	done := make(chan struct{})
	go collectSignals(ch, &signals, &mu, done)
	defer close(done)

	m := NewThermalMonitor(cfg, ch, testLogger())
	m.SetClockForTest(clock.now)
	m.SetPollIntervalForTest(5 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(signals) >= 1
	}, "thermal throttle never activated")

	clock.advance(48 * time.Hour)
	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, s := range signals[1:] {
		if !s {
			t.Fatal("throttle force-released despite max_throttle_minutes being negative (wait indefinitely)")
		}
	}
}

// A non-CPU sensor below its own declared danger point must never pause work —
// the monitor-level counterpart of the sensor tests.
func TestThermalMonitor_NormalDriveTemperatureDoesNotPause(t *testing.T) {
	withMockCPUTemp(t, 62) // CPU is fine
	withMockExecutor(t, notFoundForAll)
	withMockSensors(t, []Sensor{
		{Zone: "thermal_zone2", Kind: "nvme", Class: SensorOther, TempC: 86, CriticalC: 95},
	})

	ch := make(chan bool, 4)
	m := NewThermalMonitor(defaultThermalConfig(), ch, testLogger())
	m.SetPollIntervalForTest(5 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	select {
	case v := <-ch:
		if v {
			t.Fatal("an SSD at 86C, inside its own 95C limit, froze all work while the CPU sat at 62C — this is the reported freeze")
		}
	case <-time.After(120 * time.Millisecond):
		// No signal: correct.
	}
}

// The same drive past its own declared limit is a real overheat.
func TestThermalMonitor_DrivePastItsOwnCriticalPauses(t *testing.T) {
	withMockCPUTemp(t, 62)
	withMockExecutor(t, notFoundForAll)
	withMockSensors(t, []Sensor{
		{Zone: "thermal_zone2", Kind: "nvme", Class: SensorOther, TempC: 96, CriticalC: 95},
	})

	ch := make(chan bool, 4)
	m := NewThermalMonitor(defaultThermalConfig(), ch, testLogger())
	m.SetPollIntervalForTest(5 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	select {
	case v := <-ch:
		if !v {
			t.Fatal("expected a pause signal")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("a drive past its own declared critical point did not pause work")
	}
}
