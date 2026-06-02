package server

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// fakeClock is a controllable monotonic clock for the load estimator tests.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

// newTestEstimator builds an estimator with a fake clock and a fixed rng seed so
// jitter is deterministic. poolSat may be nil.
func newTestEstimator(cfg loadEstimatorConfig, clk *fakeClock, poolSat func() float64) *loadEstimator {
	e := newLoadEstimator(cfg, poolSat)
	e.now = clk.now
	e.rng = rand.New(rand.NewSource(1))
	return e
}

func baseCfg() loadEstimatorConfig {
	return loadEstimatorConfig{
		targetRequestRatePerSec: 100,
		targetAssignLatency:     100 * time.Millisecond,
		minDelaySeconds:         30,
		maxDelaySeconds:         900,
		jitterPct:               0.20,
	}
}

func TestLoadEstimator_RetryDelayCurveEndpoints(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	// Zero jitter to isolate the load^2 curve.
	cfg := baseCfg()
	cfg.jitterPct = 0
	e := newTestEstimator(cfg, clk, nil)

	tests := []struct {
		name string
		load float64
		want int32
	}{
		{"zero load -> minDelay", 0, 30},
		{"full load -> maxDelay", 1, 900},
		// load=0.5 -> base = 30 + 870*0.25 = 247.5 -> round 248
		{"half load convex", 0.5, 248},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := e.computeRetryDelaySeconds(tt.load)
			if got != tt.want {
				t.Fatalf("computeRetryDelaySeconds(%v) = %d, want %d", tt.load, got, tt.want)
			}
		})
	}
}

func TestLoadEstimator_JitterWithinBoundsAndFloor(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := baseCfg()
	e := newTestEstimator(cfg, clk, nil)

	// At zero load, base = minDelay = 30; ±20% jitter => [24, 36], always >= 1.
	for i := 0; i < 500; i++ {
		got := e.computeRetryDelaySeconds(0)
		if got < 24 || got > 36 {
			t.Fatalf("delay %d out of expected ±20%% band [24,36] for base 30", got)
		}
		if got < 1 {
			t.Fatalf("delay %d below 1s floor", got)
		}
	}
}

func TestLoadEstimator_JitterFloorsAtOne(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := baseCfg()
	cfg.minDelaySeconds = 1
	cfg.jitterPct = 0.9 // could push below 1 without the floor
	e := newTestEstimator(cfg, clk, nil)
	for i := 0; i < 500; i++ {
		if got := e.computeRetryDelaySeconds(0); got < 1 {
			t.Fatalf("delay %d below 1s floor", got)
		}
	}
}

func TestLoadEstimator_ClampToMaxDelay(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := baseCfg()
	cfg.jitterPct = 0.20
	e := newTestEstimator(cfg, clk, nil)
	for i := 0; i < 500; i++ {
		if got := e.computeRetryDelaySeconds(1); got > int32(cfg.maxDelaySeconds) {
			t.Fatalf("delay %d exceeds maxDelay %d", got, cfg.maxDelaySeconds)
		}
	}
}

func TestLoadEstimator_RateSaturationDrivesLoad(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := baseCfg()
	cfg.targetRequestRatePerSec = 8 // 8 req/s over the 8s window == load 1 from rate
	e := newTestEstimator(cfg, clk, nil)

	// First sample seeds the load directly (no slew on the first reading).
	// Fill all 8 buckets with 8 requests each -> 64 reqs / 8s = 8 req/s -> rateSat 1.
	// Read on the same second as the final fill so no bucket has yet rolled out of
	// the window.
	for sec := 0; sec < rateBuckets; sec++ {
		for r := 0; r < 8; r++ {
			e.recordRequest()
		}
		if sec < rateBuckets-1 {
			clk.advance(time.Second)
		}
	}
	got := e.currentLoad()
	if got < 0.99 {
		t.Fatalf("expected near-saturated load from request rate, got %v", got)
	}
}

func TestLoadEstimator_LatencyEWMADrivesLoad(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := baseCfg()
	cfg.targetRequestRatePerSec = 1_000_000 // make rate signal negligible
	cfg.targetAssignLatency = 100 * time.Millisecond
	e := newTestEstimator(cfg, clk, nil)

	// Feed several at-or-above-target latencies so the EWMA approaches 1.0.
	for i := 0; i < 20; i++ {
		e.recordAssignLatency(150 * time.Millisecond)
	}
	got := e.currentLoad()
	if got <= 0 {
		t.Fatalf("expected positive load from latency EWMA, got %v", got)
	}
}

func TestLoadEstimator_PoolSaturationFallback(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := baseCfg()
	cfg.targetRequestRatePerSec = 1_000_000 // rate negligible
	poolSat := 0.7
	e := newTestEstimator(cfg, clk, func() float64 { return poolSat })

	// No requests, no latency: only the pool signal is non-zero.
	got := e.currentLoad()
	if math.Abs(got-0.7) > 1e-9 {
		t.Fatalf("first load reading should equal pool saturation 0.7, got %v", got)
	}
}

func TestLoadEstimator_SlewLimitCannotStepChange(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := baseCfg()
	cfg.targetRequestRatePerSec = 1_000_000
	poolSat := 0.0
	e := newTestEstimator(cfg, clk, func() float64 { return poolSat })

	// First reading seeds load at 0.
	if got := e.currentLoad(); got != 0 {
		t.Fatalf("expected initial load 0, got %v", got)
	}

	// Jump the raw signal to 1.0 and advance just 1 second: the slew limit
	// (0.1/s) bounds the per-step movement well below the raw jump.
	poolSat = 1.0
	clk.advance(time.Second)
	got := e.currentLoad()
	if got > 0.1+1e-9 {
		t.Fatalf("slew limit violated: load moved to %v in 1s (cap 0.1)", got)
	}
	if got <= 0 {
		t.Fatalf("expected load to move upward, got %v", got)
	}
}

func TestLoadEstimator_SlewConvergesOverTime(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := baseCfg()
	cfg.targetRequestRatePerSec = 1_000_000
	poolSat := 0.0
	e := newTestEstimator(cfg, clk, func() float64 { return poolSat })
	_ = e.currentLoad() // seed at 0

	poolSat = 1.0
	// Advance enough wall-clock and re-sample repeatedly; load should climb toward 1.
	for i := 0; i < 200; i++ {
		clk.advance(time.Second)
		e.currentLoad()
	}
	if got := e.currentLoad(); got < 0.9 {
		t.Fatalf("expected load to converge toward 1.0 over time, got %v", got)
	}
}

func TestLoadEstimator_RateBucketsExpire(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	cfg := baseCfg()
	cfg.targetRequestRatePerSec = 1
	e := newTestEstimator(cfg, clk, nil)

	// Burst in one second, then let the window roll fully past it.
	for r := 0; r < 8; r++ {
		e.recordRequest()
	}
	// Advance well beyond the 8-bucket window so the burst no longer counts.
	clk.advance((rateBuckets + 2) * time.Second)
	if rps := e.recentReqPerSec(clk.now().Unix()); rps != 0 {
		t.Fatalf("expected stale buckets to expire to 0 rps, got %v", rps)
	}
}
