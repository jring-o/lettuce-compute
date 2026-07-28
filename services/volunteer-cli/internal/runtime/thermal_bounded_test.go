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

// A genuinely hot CPU must keep work frozen for as long as it stays hot — the
// ceiling must NOT release it. An earlier version of this code force-released on
// any cause, which contradicted the whole point of the feature; and because the
// CPU check never consulted the suppression map, it produced a freeze/unfreeze
// cycle at the ceiling interval rather than the single release it logged.
func TestThermalMonitor_HotCPUIsNeverForceReleased(t *testing.T) {
	withMockCPUTemp(t, 90) // permanently above the 85C pause / 75C resume band
	withMockExecutor(t, notFoundForAll)
	withMockSensors(t, nil)

	clock := newFakeClock()
	cfg := defaultThermalConfig()
	cfg.MaxThrottleMinutes = 30

	ch := make(chan bool, 16)
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
		return len(signals) >= 1 && signals[0]
	}, "thermal throttle never activated at 90C")

	// Well past the ceiling, and past several multiples of it — the old code
	// would have cycled release/re-pause once per ceiling interval.
	for i := 0; i < 4; i++ {
		clock.advance(31 * time.Minute)
		time.Sleep(40 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for i, s := range signals[1:] {
		if !s {
			t.Fatalf("throttle released at signal %d while the CPU was still at 90C: a genuinely hot processor must keep work suspended for as long as it stays hot", i+1)
		}
	}
	if len(signals) != 1 {
		t.Errorf("got %d signals %v, want exactly 1 (the initial pause) — extra signals mean the throttle is cycling", len(signals), signals)
	}
}

// The ceiling DOES apply when only a non-CPU sensor is holding us: suspending
// work cannot cool a drive someone else is using, so freezing forever helps
// nothing.
func TestThermalMonitor_NonCPUSensorIsForceReleasedAtCeiling(t *testing.T) {
	withMockCPUTemp(t, 60) // CPU fine
	withMockExecutor(t, notFoundForAll)
	withMockSensors(t, []Sensor{
		{Zone: "thermal_zone2", Kind: "nvme", Class: SensorOther, TempC: 99, CriticalC: 95},
	})

	clock := newFakeClock()
	cfg := defaultThermalConfig()
	cfg.MaxThrottleMinutes = 30

	ch := make(chan bool, 16)
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
		return len(signals) >= 1 && signals[0]
	}, "a drive past its own critical point never paused work")

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
	}, "still frozen 31 minutes into a 30-minute ceiling on a sensor suspending work cannot cool")
}

// After a force-release the sensor is ignored for a cooldown — it must not
// re-trip on the very next tick, and it must NOT be silenced permanently.
func TestThermalMonitor_ForceReleasedSensorIsSuppressedThenRecovers(t *testing.T) {
	withMockCPUTemp(t, 60)
	withMockExecutor(t, notFoundForAll)
	withMockSensors(t, []Sensor{
		{Zone: "thermal_zone2", Kind: "nvme", Class: SensorOther, TempC: 99, CriticalC: 95},
	})

	clock := newFakeClock()
	cfg := defaultThermalConfig()
	cfg.MaxThrottleMinutes = 30

	ch := make(chan bool, 16)
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
	}, "never paused")

	clock.advance(31 * time.Minute) // force-release
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(signals) >= 2 && !signals[1]
	}, "never force-released")

	// Inside the cooldown the same still-hot sensor must not re-pause.
	clock.advance(10 * time.Minute)
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	n := len(signals)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("got %d signals, want 2: the sensor re-tripped inside its cooldown", n)
	}

	// Past the cooldown it must be able to protect the machine again —
	// suppression is quiet, not permanent.
	clock.advance(suppressionCooldown + time.Minute)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(signals) >= 3 && signals[2]
	}, "sensor stayed suppressed after its cooldown expired: one force-release must not disable it for the daemon's lifetime")
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
