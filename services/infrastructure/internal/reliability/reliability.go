// Package reliability implements the per-MACHINE measured-reliability signal that backs
// the adaptive work quota (TODO #54): a host's in-flight work BUFFER is grounded in
// OBSERVED THROUGHPUT (validated units grow it, wasted units shrink it) instead of claimed
// hardware. It is the "smarter" generalization of the flat #53 send-interval floor, keyed
// on the #19 per-machine host id.
//
// The signal is a single DECAYING running score (it reuses RAC's decay math, internal/
// credit/rac.go): a decaying NET-GOOD count, deliberately NOT a single-unit verdict — one
// slow unit is not a liar, so a momentary failure barely moves a long good record. The
// score maps to an in-flight budget in [floor, cap]. Sybil / anti-cheat is explicitly out
// of scope (a throttled host can mint a fresh key); this is a fairness/liveness shaping
// signal, never a ban trigger.
package reliability

import (
	"math"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// HalfLifeSeconds is the reliability-score decay half-life (7 days). It deliberately
// matches credit.HalfLifeSeconds so the reliability signal decays on the same timescale as
// RAC — a host keeps its earned buffer across a head restart (the score is persisted and
// barely decays over minutes) and an inactive host drifts back toward the floor over days.
const HalfLifeSeconds = 604800

// Default ramp tunables. These are the in-memory shaping constants; the [floor, cap] bound
// itself is operator-configured (floor via LETTUCE_HEAD_RELIABILITY_QUOTA_FLOOR, cap = the
// existing max_inflight_per_volunteer), so a warmed reliable host reaches exactly the flat
// cap. Above the flat cap, a host whose machine is big enough to have a higher Ceiling
// earns one more copy per net-good point past the ramp (ScaledBudget).
const (
	// DefaultGoodStep is added to the (decayed) score per validated copy.
	DefaultGoodStep = 1.0
	// DefaultBadStep is subtracted per timeout / abandon / disagreement. It is LARGER than
	// the good step (asymmetric) so a host that wastes work is squeezed toward the floor
	// faster than it earned its way up, while a single bad event among many goods barely
	// dents the budget (decay + the running net keep it stable — the false-positive guard).
	DefaultBadStep = 2.0
	// DefaultRampUnits is the net-good score at which a host reaches the full cap: ~5
	// validated units take an honest host floor -> cap, independent of the cap's magnitude.
	DefaultRampUnits = 5.0
)

// DecayedScore applies exponential decay to a stored score over elapsedSeconds, using the
// reliability half-life. elapsedSeconds <= 0 returns the score unchanged. This is the same
// decay factor RAC uses (exp(-elapsed*ln2/halflife)); the caller decides whether to add a
// new good/bad step afterward.
func DecayedScore(score, elapsedSeconds float64) float64 {
	if elapsedSeconds <= 0 {
		return score
	}
	return score * math.Exp(-elapsedSeconds*math.Ln2/HalfLifeSeconds)
}

// Budget maps a current (already-decayed) reliability score to an in-flight buffer size in
// [floor, cap]:
//
//	budget = floor + (cap-floor) * clamp(0, 1, score/rampUnits)
//
// A score of 0 (a brand-new host, or one decayed/penalized back down) yields the floor; a
// score at or above rampUnits yields the full cap. Degenerate inputs (cap <= floor,
// rampUnits <= 0) collapse to the floor. The result is always clamped to [floor, cap].
func Budget(score float64, floor, cap int, rampUnits float64) int {
	if cap <= floor {
		return floor
	}
	if rampUnits <= 0 || score <= 0 {
		return floor
	}
	frac := score / rampUnits
	if frac > 1 {
		frac = 1
	}
	b := floor + int(math.Round(float64(cap-floor)*frac))
	if b < floor {
		return floor
	}
	if b > cap {
		return cap
	}
	return b
}

// Ceiling is the most copies one machine may hold at once, however good its record: the
// flat cap, or perUnit copies for each advertised CPU core and each GPU when that is more.
// A machine's running copies are bounded by its cores (each task books at least one), so
// with perUnit >= 2 the ceiling leaves room for a waiting copy behind every running one,
// and a machine small enough that the product is below the flat cap keeps the flat cap
// exactly. perUnit <= 0 turns the scaling off (the flat cap for every machine), and an
// unbounded flat cap (<= 0) stays unbounded.
func Ceiling(flatCap, perUnit, cpuCores, gpus int) int {
	if flatCap <= 0 || perUnit <= 0 {
		return flatCap
	}
	if cpuCores < 0 {
		cpuCores = 0
	}
	if gpus < 0 {
		gpus = 0
	}
	if scaled := perUnit * (cpuCores + gpus); scaled > flatCap {
		return scaled
	}
	return flatCap
}

// ScaledBudget is Budget with headroom above the flat cap for a machine whose Ceiling is
// higher. Up to the flat cap it is exactly Budget: floor -> cap over rampUnits. Past the
// ramp, each further net-good point of score adds one copy, up to the ceiling:
//
//	budget = min(ceiling, cap + floor(score - rampUnits))
//
// So a big machine reaches its ceiling only by returning work that validates (the score is
// the decayed count of validated copies, less the penalties), and a client that merely
// claims many cores gains nothing above the flat cap until its results do. A ceiling at or
// below the flat cap leaves Budget unchanged.
func ScaledBudget(score float64, floor, cap, ceiling int, rampUnits float64) int {
	b := Budget(score, floor, cap, rampUnits)
	if ceiling <= cap || rampUnits <= 0 || score <= rampUnits {
		return b
	}
	scaled := ceiling
	if extra := score - rampUnits; extra < float64(ceiling-cap) {
		scaled = cap + int(extra)
	}
	if scaled < b {
		return b
	}
	return scaled
}

// BudgetInput is one host's current (read-time-decayed) reliability score, returned by
// ListBudgetInputs for the off-hot-path budget refresher.
type BudgetInput struct {
	HostID types.ID
	Score  float64
}
