package standing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/reliability"
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
//
// Bind parameters: $1 volunteer id, $2/$3 the good/bad step (exactly one is 1),
// $4 the decay half-life in seconds, $5 MinSample, $6 ProbationRate, $7 BenchRate,
// $8 OKRate, $9 BenchFor in seconds. A matched-nothing UPDATE (absent volunteer or
// non-AUTO row) surfaces as pgx.ErrNoRows and is reported as Applied=false, never
// an error.
func (r *PgxRecorder) RecordAdjudicated(ctx context.Context, volunteerID types.ID, agreed bool) (*AdjudicationOutcome, error) {
	// One fold's +1 step lands on exactly one accumulator (the reliability.RecordOutcome
	// good/bad-increment shape); the other side only decays.
	goodInc, badInc := 1.0, 0.0
	if !agreed {
		goodInc, badInc = 0.0, 1.0
	}

	row := r.db.QueryRow(ctx, adjudicatedSQL,
		volunteerID,                          // $1
		goodInc,                              // $2
		badInc,                               // $3
		float64(reliability.HalfLifeSeconds), // $4
		r.cfg.MinSample,                      // $5
		r.cfg.ProbationRate,                  // $6
		r.cfg.BenchRate,                      // $7
		r.cfg.OKRate,                         // $8
		r.cfg.BenchFor.Seconds(),             // $9
	)

	out := &AdjudicationOutcome{Applied: true}
	if err := row.Scan(&out.OldStanding, &out.NewStanding, &out.BenchedUntil, &out.Rate, &out.Sample); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No AUTO row matched: the volunteer is absent or operator-owned. The machine
			// never touches such a row and this is a normal no-op, not an error.
			return &AdjudicationOutcome{Applied: false}, nil
		}
		return nil, apierror.Internal("failed to record adjudicated outcome", err)
	}
	return out, nil
}

// effectiveStandingExpr is the SQL twin of volunteer.EffectiveStanding — the ONE
// resolution rule for the standing enforcement sees (BG-24b). The backpressure
// machine keys its transitions on a volunteer's EFFECTIVE standing (so an expired
// bench re-enters through the PROBATION hysteresis exit rather than jumping straight
// to OK), so it needs that rule inline in its fold UPDATE. This is a deliberate
// second copy of the rule, not a shared import: the workunit package holds its own
// unexported copy for dispatch/counting and the two packages do not depend on each
// other. Each copy is pinned to volunteer.EffectiveStanding by a golden parity test
// so they can never drift. v is the volunteers table alias in scope.
func effectiveStandingExpr(v string) string {
	return `(CASE
		WHEN ` + v + `.standing = 'BENCHED'
			AND (` + v + `.benched_until IS NULL OR ` + v + `.benched_until > NOW())
			THEN 'BENCHED'
		WHEN ` + v + `.standing IN ('PROBATION', 'BENCHED') THEN 'PROBATION'
		ELSE 'OK'
	END)`
}

// adjudicatedSQL is the single atomic fold-and-transition UPDATE, built once from
// shared fragments so the decay formula is written exactly once and interpolated
// everywhere it is needed.
//
// EPQ safety: every correctness input (the decayed accumulators, the sample, the
// rate, the effective-standing gate, and each transition arm) is a SET/CASE
// expression over the target row v's OWN columns. Under READ COMMITTED, when a
// concurrent fold has updated the row, Postgres locks it, re-fetches the latest
// committed version, and re-evaluates those expressions against it — so two folds
// on one row serialize and neither loses its step. The only join is the `prev` CTE,
// which exists SOLELY to carry the pre-update stored standing into RETURNING for
// the caller's log: it feeds no SET expression and no correctness predicate (the
// sole WHERE correctness predicate, standing_source = 'AUTO', reads v directly, so
// an operator taking the row over mid-flight is honored on EPQ re-check), it joins
// on the immutable primary key, and if a concurrent fold commits between the CTE's
// snapshot and the locked update, prev.old_standing may be one fold stale — which
// is harmless, since OldStanding is logging-only.
var adjudicatedSQL = buildAdjudicatedSQL()

func buildAdjudicatedSQL() string {
	// decayFactor is the shared exponential-decay multiplier applied to BOTH
	// accumulators. Elapsed time is measured from rejection_updated_at, COALESCEd to
	// NOW() so a first-ever fold decays nothing: rejection_updated_at is NULL until the
	// first fold (this is a plain UPDATE, not the reliability upsert, so the column is
	// not seeded on insert). $4 is the half-life in seconds. Written once here and
	// interpolated below so the formula is never hand-repeated.
	const decayFactor = `exp(-EXTRACT(EPOCH FROM (NOW() - COALESCE(v.rejection_updated_at, NOW()))) * ln(2) / $4)`

	// newGood / newBad decay the stored accumulator, then add this fold's step ($2 is
	// 1/0 on the good side, $3 is 0/1 on the bad side). No GREATEST floor is needed: a
	// non-negative accumulator times a positive decay factor plus a non-negative step
	// stays non-negative.
	newGood := `(v.rejection_good * ` + decayFactor + ` + $2)`
	newBad := `(v.rejection_bad * ` + decayFactor + ` + $3)`

	// sample is always >= 1 after a fold (exactly one step is +1), so rate never
	// divides by zero.
	sample := `(` + newGood + ` + ` + newBad + `)`
	rate := `(` + newBad + ` / ` + sample + `)`

	// eff is the volunteer's EFFECTIVE standing BEFORE this fold (bare target columns,
	// re-fetched under EPQ after any lock wait).
	eff := effectiveStandingExpr("v")

	// Transitions are evaluated only at or above MinSample ($5). The three arms are
	// mutually exclusive — config guarantees OKRate < ProbationRate <= BenchRate, so
	// the enter-bench (rate >= BenchRate) and exit-to-OK (rate <= OKRate) arms of a
	// PROBATION row can never both fire — and the ladder never skips a rung: an OK row
	// can only move to PROBATION, never straight to BENCHED.
	gate := `(` + sample + ` >= $5)`
	enterProbation := `(` + eff + ` = 'OK' AND ` + rate + ` >= $6)`
	enterBench := `(` + eff + ` = 'PROBATION' AND ` + rate + ` >= $7)`
	exitToOK := `(` + eff + ` = 'PROBATION' AND ` + rate + ` <= $8)`
	transitioned := `(` + gate + ` AND (` + enterProbation + ` OR ` + enterBench + ` OR ` + exitToOK + `))`

	// The machine-generated reason carries the post-fold decayed rate (as a percentage)
	// and sample; both are the same OLD-column expressions the transition decided on.
	reason := `format('backpressure: decayed rejection rate %s%% over sample %s', ` +
		`round((` + rate + ` * 100)::numeric, 1), round((` + sample + `)::numeric, 1))`

	// RETURNING computes rate and sample from the NEW (post-update) accumulator columns
	// — which the SET clause just wrote to newGood/newBad — NOT from the decay fragments
	// (in RETURNING the fragments would re-decay against the freshly stamped
	// rejection_updated_at and double-count the step). The two are numerically identical.
	returnRate := `v.rejection_bad / (v.rejection_good + v.rejection_bad)`
	returnSample := `(v.rejection_good + v.rejection_bad)`

	return `
WITH prev AS (
	SELECT id, standing AS old_standing
	FROM volunteers
	WHERE id = $1
)
UPDATE volunteers v SET
	rejection_good = ` + newGood + `,
	rejection_bad = ` + newBad + `,
	rejection_updated_at = NOW(),
	standing = CASE
		WHEN ` + gate + ` AND ` + enterProbation + ` THEN 'PROBATION'
		WHEN ` + gate + ` AND ` + enterBench + ` THEN 'BENCHED'
		WHEN ` + gate + ` AND ` + exitToOK + ` THEN 'OK'
		ELSE v.standing
	END,
	benched_until = CASE
		WHEN ` + gate + ` AND ` + enterBench + ` THEN NOW() + make_interval(secs => $9)
		WHEN ` + gate + ` AND ` + enterProbation + ` THEN NULL
		WHEN ` + gate + ` AND ` + exitToOK + ` THEN NULL
		ELSE v.benched_until
	END,
	standing_reason = CASE
		WHEN ` + transitioned + ` THEN ` + reason + `
		ELSE v.standing_reason
	END,
	standing_changed_at = CASE
		WHEN ` + transitioned + ` THEN NOW()
		ELSE v.standing_changed_at
	END
FROM prev
WHERE v.id = prev.id AND v.standing_source = 'AUTO'
RETURNING
	prev.old_standing,
	v.standing,
	v.benched_until,
	` + returnRate + `,
	` + returnSample
}
