package management

import "sync"

// etaRateSmoothing is the weight given to the newest progress-rate sample in the
// exponential moving average. Lower = smoother/slower to react; higher = noisier.
const etaRateSmoothing = 0.4

// etaMinSampleInterval is the least run time, in seconds, between two accepted
// rate samples. Progress is an integer percent that a leaf may advance in steps
// minutes apart, while the status API is polled every few seconds; measuring a
// step against the previous poll rather than against the last accepted sample
// divided a +1 % step by the 3 s poll gap and produced a rate a hundred times
// too fast (TB-58). So the anchor a sample is measured from moves only when a
// sample is accepted, and a sample is accepted only over a window at least this
// long.
const etaMinSampleInterval = 30

// etaSample is the last ACCEPTED progress observation for one work unit — the
// anchor the next rate sample is measured from — plus the smoothed rate derived
// from the sequence of accepted samples.
type etaSample struct {
	progress int     // progress percent at the anchor (0..100)
	elapsed  int     // accrued run-time seconds at the anchor
	emaRate  float64 // smoothed progress rate, percent per second
}

// etaTracker produces a stable estimate of a task's remaining time.
//
// The naive estimate — extrapolate elapsed/progress linearly from t=0 — is volatile:
// a slow start (binary download, input staging, warm-up) permanently drags the
// implied average rate down, so the estimate starts huge and only deflates as real
// progress accrues. Instead this tracker keeps an exponential moving average of the
// RECENT progress rate and blends the resulting estimate with the static per-unit
// estimate (learned from the leaf's completions on this machine, or the CPU
// benchmark before the first), weighting the dynamic estimate more heavily as the
// task nears completion (fraction-done weighting). The number moves smoothly and
// converges rather than lurching downward.
type etaTracker struct {
	mu      sync.Mutex
	samples map[string]etaSample
}

func newETATracker() *etaTracker {
	return &etaTracker{samples: make(map[string]etaSample)}
}

// estimate returns the estimated remaining seconds for a task and whether an
// estimate is available. progressPct is 0..100, elapsedSeconds is accrued run time,
// and estimatedSeconds is the static per-unit total estimate (0 if unknown).
func (e *etaTracker) estimate(wuID string, progressPct, elapsedSeconds int, estimatedSeconds float64) (int, bool) {
	// Static estimate of remaining time, if one is available.
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

	prev, seen := e.samples[wuID]
	emaRate := prev.emaRate
	switch {
	case !seen, progressPct < prev.progress, elapsedSeconds < prev.elapsed:
		// First observation, or the unit went backwards (a restart from an older
		// checkpoint): anchor here, keeping whatever rate is already known.
		e.samples[wuID] = etaSample{progress: progressPct, elapsed: elapsedSeconds, emaRate: emaRate}
	case progressPct > prev.progress && elapsedSeconds-prev.elapsed >= etaMinSampleInterval:
		// Forward progress over a long enough window: fold in a rate sample
		// measured from the anchor, then move the anchor here.
		inst := float64(progressPct-prev.progress) / float64(elapsedSeconds-prev.elapsed)
		if emaRate > 0 {
			emaRate = etaRateSmoothing*inst + (1-etaRateSmoothing)*emaRate
		} else {
			emaRate = inst
		}
		e.samples[wuID] = etaSample{progress: progressPct, elapsed: elapsedSeconds, emaRate: emaRate}
	}
	// Otherwise — no forward progress yet, or too soon since the anchor — the
	// anchor stands, so the next accepted sample spans the whole window.

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
