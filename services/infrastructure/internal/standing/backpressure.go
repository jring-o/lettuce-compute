package standing

import (
	"context"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// BackpressureConfig holds the resolved thresholds of the automatic rejection-rate
// backpressure machine (BG-24 / BG-24b PR-B). A Recorder is only constructed when
// LETTUCE_HEAD_STANDING_BACKPRESSURE_ENABLED is true — "machine off" is expressed as
// a nil Recorder on the consumer, never as a zero config — so every field here is
// always the EFFECTIVE (validated, defaulted) value: 0 < OKRate < ProbationRate <=
// BenchRate <= 1, MinSample > 0, BenchFor > 0.
type BackpressureConfig struct {
	// ProbationRate enters probation: an effectively-OK volunteer whose decayed
	// rejection rate reaches this (at MinSample) moves to PROBATION.
	ProbationRate float64
	// OKRate exits probation: an effectively-PROBATION volunteer whose decayed
	// rejection rate falls to this or below (at MinSample) returns to OK. It sits
	// strictly below ProbationRate — the hysteresis band that stops flapping.
	OKRate float64
	// BenchRate benches: an effectively-PROBATION volunteer whose decayed rejection
	// rate reaches this (at MinSample) moves to BENCHED for BenchFor.
	BenchRate float64
	// MinSample is the minimum decayed sample (good + bad) at which transitions are
	// evaluated at all — below it the accumulators fold but standing never moves,
	// so a newcomer's first unlucky results cannot bench them.
	MinSample float64
	// BenchFor is the auto-bench duration: benched_until = now() + BenchFor. The
	// machine always sets a deadline (only operators bench indefinitely), and an
	// expired bench resolves to PROBATION via volunteer.EffectiveStanding, so
	// re-entry to OK goes through the hysteresis exit.
	BenchFor time.Duration
}

// AdjudicationOutcome reports what one RecordAdjudicated fold did, for the caller's
// structured log. Standing values are the STORED ones (volunteer.Standing*).
type AdjudicationOutcome struct {
	// Applied is false when no row was updated: the volunteer does not exist or its
	// standing_source is 'OPERATOR' (the machine never touches an operator's row,
	// and such rows accumulate no signal while operator-owned).
	Applied bool
	// OldStanding / NewStanding are the stored standing before and after the fold
	// (equal when no transition fired). OldStanding is logging-only.
	OldStanding string
	NewStanding string
	// BenchedUntil is the stored bench deadline after the fold (set on a fresh
	// PROBATION -> BENCHED transition, cleared on a transition to OK, otherwise
	// carried unchanged).
	BenchedUntil *time.Time
	// Rate is the decayed rejection rate bad/(good+bad) AFTER this fold (0 when the
	// sample is zero); Sample is the decayed good+bad it was computed over.
	Rate   float64
	Sample float64
}

// Recorder is the surface the validation engine records adjudicated outcomes on.
// Implementations fold one AGREED (agreed=true) or DISAGREED (agreed=false) result
// into the volunteer's decayed rejection-rate signal and apply any threshold
// standing transition, atomically. Callers are best-effort: they log and continue
// on error (the signal is backpressure shaping, never a validation input).
type Recorder interface {
	RecordAdjudicated(ctx context.Context, volunteerID types.ID, agreed bool) (*AdjudicationOutcome, error)
}

// PgxRecorder implements Recorder over the volunteers table's rejection_* signal
// columns (migration 00015) and standing columns (migration 00014).
//
// The fold is ONE atomic UPDATE (the reliability.RecordOutcome shape): both
// accumulators are decayed in SQL to NOW() on the shared 7-day half-life
// (reliability.HalfLifeSeconds), the adjudicated outcome adds 1 to one of them,
// and the threshold transitions are decided in the same statement from the same
// decayed values, so concurrent folds serialize on the row and never lose a step.
// Decay elapsed time is measured from rejection_updated_at, COALESCEd to NOW() so
// a first-ever fold decays nothing. Transitions key on the volunteer's EFFECTIVE
// standing (the volunteer.EffectiveStanding rule — its SQL twin here is pinned by
// a golden test), so an expired bench re-enters through the PROBATION hysteresis
// exit rather than jumping straight to OK. Rows whose standing_source is not
// 'AUTO' are never updated.
type PgxRecorder struct {
	db  DBTX
	cfg BackpressureConfig
}

// NewPgxRecorder creates a Recorder over db with the given EFFECTIVE thresholds.
// Construct one only when the backpressure machine is enabled; consumers express
// "machine off" as a nil Recorder.
func NewPgxRecorder(db DBTX, cfg BackpressureConfig) *PgxRecorder {
	return &PgxRecorder{db: db, cfg: cfg}
}

// RecordAdjudicated folds one adjudicated result outcome into volunteerID's decayed
// rejection-rate signal and applies any threshold standing transition. See the
// PgxRecorder doc for the atomicity and transition semantics.
func (r *PgxRecorder) RecordAdjudicated(ctx context.Context, volunteerID types.ID, agreed bool) (*AdjudicationOutcome, error) {
	// KEEL STUB — implementation lands with the backpressure machine slice; nothing
	// constructs a PgxRecorder until the enabling knob (default false) is flipped.
	return nil, apierror.Internal("standing backpressure recorder not implemented", nil)
}
