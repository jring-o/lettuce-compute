package management

import "sync"

// etaRateSmoothing is the weight given to the newest progress-rate sample in the
// exponential moving average. Lower = smoother/slower to react; higher = noisier.
const etaRateSmoothing = 0.4

// etaSample is the last progress observation for one work unit, plus the smoothed
// rate derived from the sequence of observations.
type etaSample struct {
	progress int     // last progress percent observed (0..100)
	elapsed  int     // accrued run-time seconds at that observation
	emaRate  float64 // smoothed progress rate, percent per second
}

// etaTracker produces a stable estimate of a task's remaining time.
//
// The naive estimate — extrapolate elapsed/progress linearly from t=0 — is volatile:
// a slow start (binary download, input staging, warm-up) permanently drags the
// implied average rate down, so the estimate starts huge and only deflates as real
// progress accrues. Instead this tracker keeps an exponential moving average of the
// RECENT progress rate and blends the resulting estimate with the static benchmark
// estimate, weighting the dynamic estimate more heavily as the task nears completion
// (fraction-done weighting). The number moves smoothly and converges rather than
// lurching downward.
type etaTracker struct {
	mu      sync.Mutex
	samples map[string]etaSample
}

func newETATracker() *etaTracker {
	return &etaTracker{samples: make(map[string]etaSample)}
}

// estimate returns the estimated remaining seconds for a task and whether an
// estimate is available. progressPct is 0..100, elapsedSeconds is accrued run time,
// and estimatedSeconds is the benchmark-based total estimate (0 if unknown).
func (e *etaTracker) estimate(wuID string, progressPct, elapsedSeconds int, estimatedSeconds float64) (int, bool) {
	// Static (benchmark) estimate of remaining time, if one is available.
	staticRemaining := -1.0
	if estimatedSeconds > 0 {
		staticRemaining = estimatedSeconds - float64(elapsedSeconds)
		if staticRemaining < 0 {
			staticRemaining = 0
		}
	}

	// Without usable live progress, fall back to the static estimate (or nothing).
	if progressPct <= 0 || progressPct >= 100 || elapsedSeconds <= 0 {
		if staticRemaining >= 0 {
			return int(staticRemaining), true
		}
		return 0, false
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	emaRate := 0.0
	if prev, seen := e.samples[wuID]; seen {
		emaRate = prev.emaRate
		// Only fold in a fresh rate sample on forward progress over elapsed run time.
		if progressPct > prev.progress && elapsedSeconds > prev.elapsed {
			inst := float64(progressPct-prev.progress) / float64(elapsedSeconds-prev.elapsed)
			if prev.emaRate > 0 {
				emaRate = etaRateSmoothing*inst + (1-etaRateSmoothing)*prev.emaRate
			} else {
				emaRate = inst
			}
		}
	}
	e.samples[wuID] = etaSample{progress: progressPct, elapsed: elapsedSeconds, emaRate: emaRate}

	// Dynamic estimate: from the smoothed recent rate once we have one, otherwise the
	// average rate since t=0 as a bootstrap.
	var dynamicRemaining float64
	if emaRate > 0 {
		dynamicRemaining = float64(100-progressPct) / emaRate
	} else {
		dynamicRemaining = float64(elapsedSeconds) / float64(progressPct) * float64(100-progressPct)
	}

	// Blend with the static estimate, trusting the dynamic estimate more as progress
	// advances. With no static estimate available, use the dynamic estimate alone.
	if staticRemaining < 0 {
		return int(dynamicRemaining), true
	}
	w := float64(progressPct) / 100.0
	blended := (1-w)*staticRemaining + w*dynamicRemaining
	return int(blended), true
}

// retain drops samples for work units no longer active, bounding memory over a long
// run as work units complete and new ones start.
func (e *etaTracker) retain(active map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id := range e.samples {
		if !active[id] {
			delete(e.samples, id)
		}
	}
}
