package server

import (
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Layer-1 server-side defaults (mirror config.HeadConfig.Effective* so a
// volunteerService built without SetHeadConfig still behaves sanely). The
// authoritative, operator-tunable values flow in through SetHeadConfig.
const (
	defaultMaxBatchPerRequest      = 8
	defaultMinRetryDelaySeconds    = 30
	defaultMaxRetryDelaySeconds    = 900
	defaultRetryDelayJitterPct     = 0.20
	defaultTargetRequestRatePerSec = 500.0
	defaultLeaseSeconds            = 900
	// defaultTargetAssignLatency is the FindNextAssignable/reserve duration that
	// maps to latency saturation 1.0. SIMULATOR-CALIBRATED.
	defaultTargetAssignLatency = 50 * time.Millisecond
)

// defaultLoadEstimatorConfig returns the estimator config built from the
// package defaults above.
func defaultLoadEstimatorConfig() loadEstimatorConfig {
	return loadEstimatorConfig{
		targetRequestRatePerSec: defaultTargetRequestRatePerSec,
		targetAssignLatency:     defaultTargetAssignLatency,
		minDelaySeconds:         defaultMinRetryDelaySeconds,
		maxDelaySeconds:         defaultMaxRetryDelaySeconds,
		jitterPct:               defaultRetryDelayJitterPct,
	}
}

// poolSaturation returns a closure reporting AcquiredConns/MaxConns for the
// pool, or nil when pool is nil (so the estimator treats poolSat as 0). This is
// the coarse, secondary load signal (see loadEstimator doc).
func poolSaturation(pool *pgxpool.Pool) func() float64 {
	if pool == nil {
		return nil
	}
	return func() float64 {
		st := pool.Stat()
		maxConns := st.MaxConns()
		if maxConns <= 0 {
			return 0
		}
		return float64(st.AcquiredConns()) / float64(maxConns)
	}
}

// loadEstimatorConfig holds the tunables that shape the load signal and the
// server-directed retry delay derived from it. Zero values are filled with
// sensible defaults by newLoadEstimator's caller (via HeadConfig.Effective*).
type loadEstimatorConfig struct {
	// targetRequestRatePerSec maps RequestWorkUnit calls/sec to the rate
	// saturation signal: rateSat = clamp(recentReqPerSec/target, 0, 1).
	// SIMULATOR-CALIBRATED, not a trusted default.
	targetRequestRatePerSec float64
	// targetAssignLatency is the FindNextAssignable/reserve duration that maps to
	// latency saturation 1.0. SIMULATOR-CALIBRATED.
	targetAssignLatency time.Duration

	// minDelay/maxDelay bound the computed retry delay (seconds). maxDelay must be
	// strictly below the 30-min stale-volunteer threshold (enforced in config).
	minDelaySeconds int
	maxDelaySeconds int
	// jitterPct is the ± uniform jitter fraction stamped server-side.
	jitterPct float64
}

// rateBuckets is the number of 1-second buckets in the sliding request-rate
// counter (an 8-second window).
const rateBuckets = 8

// loadEWMAAlpha smooths the raw load input over ~5s at ~1 sample/sec.
const loadEWMAAlpha = 0.3

// maxLoadDeltaPerSec slew-limits how fast the shared smoothed load may move
// toward the raw load, breaking the rate→delay→rate feedback loop.
const maxLoadDeltaPerSec = 0.1

// loadEstimator tracks a head's real-time self-load and turns it into a
// server-directed retry delay. It is safe for concurrent use: RequestWorkUnit
// handlers increment the rate counter and record assign latency from many
// goroutines, and currentLoad is read once per request.
//
// Signals (rawLoad = max of the three):
//   - request rate (primary): a lock-free-ish sliding 1s-bucket counter.
//   - FindNextAssignable/reserve latency EWMA (primary): the real per-assignment
//     DB cost.
//   - DB pool saturation (coarse secondary): AcquiredConns/MaxConns; documented
//     as a safety net only — at fleet scale the binding constraint is request
//     rate and SKIP LOCKED contention, not steady-state pool occupancy.
//
// Damping: the rawLoad is EWMA-smoothed, then the shared smoothed load is
// slew-limited (≤ maxLoadDeltaPerSec) so the handed-out delay cannot step-change.
type loadEstimator struct {
	cfg loadEstimatorConfig

	// now is the clock seam (injected in tests). Defaults to time.Now.
	now func() time.Time
	// rng draws the jitter. Guarded by mu.
	rng *rand.Rand

	// poolSat returns the current DB pool saturation in [0,1]. May be nil (treated
	// as 0) when no pool is wired (unit tests).
	poolSat func() float64

	mu sync.Mutex
	// buckets[i] counts requests in the 1-second window ending at bucketEpoch[i].
	buckets     [rateBuckets]int
	bucketEpoch [rateBuckets]int64 // unix seconds owning each bucket

	ewmaAssignLatency float64 // seconds; EWMA of assign-query duration
	haveLatency       bool

	smoothedLoad   float64   // EWMA-smoothed rawLoad
	slewedLoad     float64   // slew-limited shared load (the delay's input)
	lastLoadSample time.Time // last time the load was advanced (for slew limiting)
	haveLoad       bool
}

// newLoadEstimator builds an estimator. poolSat may be nil.
func newLoadEstimator(cfg loadEstimatorConfig, poolSat func() float64) *loadEstimator {
	return &loadEstimator{
		cfg:     cfg,
		now:     time.Now,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		poolSat: poolSat,
	}
}

// recordRequest increments the sliding request-rate counter for the current
// 1-second bucket.
func (e *loadEstimator) recordRequest() {
	now := e.now()
	sec := now.Unix()
	idx := int(sec % rateBuckets)
	e.mu.Lock()
	if e.bucketEpoch[idx] != sec {
		// Stale bucket (its second has rolled over): reset it before counting.
		e.bucketEpoch[idx] = sec
		e.buckets[idx] = 0
	}
	e.buckets[idx]++
	e.mu.Unlock()
}

// recordAssignLatency folds an assignment-query duration into the latency EWMA.
func (e *loadEstimator) recordAssignLatency(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := d.Seconds()
	if !e.haveLatency {
		e.ewmaAssignLatency = s
		e.haveLatency = true
		return
	}
	e.ewmaAssignLatency = loadEWMAAlpha*s + (1-loadEWMAAlpha)*e.ewmaAssignLatency
}

// recentReqPerSec returns the average requests/sec over the live buckets
// (buckets whose owning second is within the last rateBuckets seconds).
func (e *loadEstimator) recentReqPerSec(nowSec int64) float64 {
	total := 0
	for i := 0; i < rateBuckets; i++ {
		if nowSec-e.bucketEpoch[i] < rateBuckets {
			total += e.buckets[i]
		}
	}
	return float64(total) / float64(rateBuckets)
}

// currentLoad advances the smoothing/slew state and returns the current slewed
// load in [0,1]. Called once per RequestWorkUnit.
func (e *loadEstimator) currentLoad() float64 {
	now := e.now()

	var poolSat float64
	if e.poolSat != nil {
		poolSat = clamp01(e.poolSat())
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	rateSat := 0.0
	if e.cfg.targetRequestRatePerSec > 0 {
		rateSat = clamp01(e.recentReqPerSec(now.Unix()) / e.cfg.targetRequestRatePerSec)
	}
	latSat := 0.0
	if e.haveLatency && e.cfg.targetAssignLatency > 0 {
		latSat = clamp01(e.ewmaAssignLatency / e.cfg.targetAssignLatency.Seconds())
	}

	rawLoad := math.Max(rateSat, math.Max(latSat, poolSat))

	// EWMA-smooth the input.
	if !e.haveLoad {
		e.smoothedLoad = rawLoad
		e.slewedLoad = rawLoad
		e.lastLoadSample = now
		e.haveLoad = true
		return e.slewedLoad
	}
	e.smoothedLoad = loadEWMAAlpha*rawLoad + (1-loadEWMAAlpha)*e.smoothedLoad

	// Slew-limit the shared load toward the smoothed value.
	elapsed := now.Sub(e.lastLoadSample).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	maxStep := maxLoadDeltaPerSec * elapsed
	delta := e.smoothedLoad - e.slewedLoad
	if delta > maxStep {
		delta = maxStep
	} else if delta < -maxStep {
		delta = -maxStep
	}
	e.slewedLoad = clamp01(e.slewedLoad + delta)
	e.lastLoadSample = now
	return e.slewedLoad
}

// computeRetryDelaySeconds maps a load in [0,1] to a jittered server-directed
// retry delay in seconds:
//
//	base    = minDelay + (maxDelay-minDelay)*load^2   // convex
//	stamped = round(base * (1 + uniform(-jitter, +jitter)))
//	stamped = clamp(stamped, 1, maxDelay)
//
// Exposed (not a method on the locked state) so it is trivially unit-testable;
// jitter is drawn from the estimator's rng under its lock.
func (e *loadEstimator) computeRetryDelaySeconds(load float64) int32 {
	load = clamp01(load)
	minD := float64(e.cfg.minDelaySeconds)
	maxD := float64(e.cfg.maxDelaySeconds)
	base := minD + (maxD-minD)*load*load

	e.mu.Lock()
	jitterFactor := 1 + e.cfg.jitterPct*(2*e.rng.Float64()-1)
	e.mu.Unlock()

	stamped := math.Round(base * jitterFactor)
	if stamped < 1 {
		stamped = 1
	}
	if stamped > maxD {
		stamped = maxD
	}
	return int32(stamped)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
