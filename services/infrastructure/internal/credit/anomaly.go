package credit

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AnomalyVerdict is the emission circuit breaker's read of the credit ledger: whether the
// export must freeze, plus the figures the halt response and the operator WARN report.
type AnomalyVerdict struct {
	// Halted is true when the breaker has tripped (armed AND today is anomalously high).
	Halted bool
	// Today is the total credit granted over the last 24h (rolling, DB clock).
	Today float64
	// Baseline is the trailing daily average: the [now()-31d, now()-1d) window sum divided
	// by a FIXED 30-day calendar denominator.
	Baseline float64
	// Armed reports whether the baseline window has enough distinct grant-days for the
	// breaker to be able to trip at all (see anomalyMinDistinctDays); a young or sparse
	// head is never armed.
	Armed bool
}

// AnomalyCheck is the read-side capability the export gates consult to decide whether the
// emission-anomaly circuit breaker has tripped. *AnomalyChecker implements it; tests supply
// a stub. Extracted as an interface so the stats handler can be exercised without a
// database.
type AnomalyCheck interface {
	Check(ctx context.Context) (AnomalyVerdict, error)
}

// anomalyVerdictTTL bounds how stale a served verdict may be. 60s is fine for a circuit
// breaker: a burst that trips the halt keeps tripping until it enters the baseline window,
// and a false positive self-clears within a minute of the trailing average catching up.
const anomalyVerdictTTL = 60 * time.Second

const (
	// anomalyBaselineDays is the FIXED calendar-day denominator for the trailing baseline
	// average — see AnomalyChecker.evaluate for why it is fixed rather than the number of
	// days that actually had grants.
	anomalyBaselineDays = 30.0
	// anomalyMinDistinctDays is the arming threshold: the breaker stays inert until the
	// baseline window has grants on at least this many distinct days, so a young or sparse
	// head cannot trip on one stray grant.
	anomalyMinDistinctDays = 7
)

// AnomalyChecker evaluates the global emission-anomaly condition behind the export circuit
// breaker: it compares the last 24h of granted credit against the trailing 30-day daily
// average and reports whether today's total is anomalously high. A 60-second in-memory
// cache keeps the read off the hot path of every export request; staleness up to
// anomalyVerdictTTL is acceptable for a circuit breaker. Safe for concurrent use.
type AnomalyChecker struct {
	pool   *pgxpool.Pool
	factor float64

	mu       sync.Mutex
	cached   AnomalyVerdict
	cachedAt time.Time
}

// NewAnomalyChecker builds a checker over pool that trips when the last 24h of grants
// exceeds factor times the trailing daily average.
func NewAnomalyChecker(pool *pgxpool.Pool, factor float64) *AnomalyChecker {
	return &AnomalyChecker{pool: pool, factor: factor}
}

// Check returns the current emission-anomaly verdict, serving a cached value while it is
// younger than anomalyVerdictTTL and otherwise re-evaluating against the ledger. An error
// is returned only on a real query failure; the caller (the export gate) fails OPEN on
// error so an anomaly-check outage never takes the export down.
func (c *AnomalyChecker) Check(ctx context.Context) (AnomalyVerdict, error) {
	c.mu.Lock()
	if !c.cachedAt.IsZero() && time.Since(c.cachedAt) < anomalyVerdictTTL {
		v := c.cached
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

	v, err := c.evaluate(ctx)
	if err != nil {
		return AnomalyVerdict{}, err
	}

	c.mu.Lock()
	c.cached = v
	c.cachedAt = time.Now()
	c.mu.Unlock()
	return v, nil
}

// evaluate runs the two pinned time-range sums and folds them into a verdict. Both queries
// ride idx_credit_ledger_granted_at (migration 00018); every other credit_ledger index
// leads with volunteer_id/leaf_id, so a global range sum would seq-scan without it.
func (c *AnomalyChecker) evaluate(ctx context.Context) (AnomalyVerdict, error) {
	// today: the last 24h of grants (rolling window, DB clock).
	var today float64
	if err := c.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(credit_amount), 0)::float8
		FROM credit_ledger
		WHERE granted_at >= now() - interval '24 hours'`,
	).Scan(&today); err != nil {
		return AnomalyVerdict{}, err
	}

	// baseline window: [now()-31d, now()-1d) — disjoint from today's 24h window. One query
	// returns the window sum and the distinct-grant-day count. The sum is divided by a
	// FIXED 30-day denominator (zero-activity days pull the average down deliberately, so a
	// sparse head has a low legitimate rate); the distinct-day count arms the breaker only
	// once the window has grants on >= anomalyMinDistinctDays days.
	var (
		windowSum    float64
		distinctDays int
	)
	if err := c.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(credit_amount), 0)::float8,
		       COUNT(DISTINCT date_trunc('day', granted_at))
		FROM credit_ledger
		WHERE granted_at >= now() - interval '31 days'
		  AND granted_at <  now() - interval '1 day'`,
	).Scan(&windowSum, &distinctDays); err != nil {
		return AnomalyVerdict{}, err
	}

	return anomalyVerdict(today, windowSum, distinctDays, c.factor), nil
}

// anomalyVerdict is the pure emission-anomaly decision, factored out of the DB path so the
// arithmetic (arming, the fixed denominator, the factor edge) is unit-testable without a
// database.
func anomalyVerdict(today, windowSum float64, distinctDays int, factor float64) AnomalyVerdict {
	armed := distinctDays >= anomalyMinDistinctDays
	baseline := windowSum / anomalyBaselineDays
	return AnomalyVerdict{
		Halted:   armed && today > factor*baseline,
		Today:    today,
		Baseline: baseline,
		Armed:    armed,
	}
}
