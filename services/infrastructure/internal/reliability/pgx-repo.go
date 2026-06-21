package reliability

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// DBTX is the minimal pgx surface the repository needs (satisfied by *pgxpool.Pool and a
// pgx.Tx), mirroring credit.DBTX so the reliability store can run on the pool or in a tx.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repository is the data-access surface for the per-host reliability signal (TODO #54). It
// is intentionally tiny: the validator + fault monitor record outcomes off the hot path,
// and the dispatch-cache budget refresher reads the decayed scores off the hot path.
type Repository interface {
	// RecordOutcome folds one work-unit outcome into a host's decaying reliability score:
	// the stored score is decayed to NOW(), then a good step is ADDED (good=true) or a bad
	// step is SUBTRACTED (good=false, floored at 0). Keyed on the effective host id
	// (COALESCE(host_id, volunteer_id)). Best-effort for the caller — it is correctness-
	// neutral (pure dispatch shaping), so callers log and continue on error.
	RecordOutcome(ctx context.Context, hostID types.ID, good bool) error
	// ListBudgetInputs returns each recently-active host's CURRENT (read-time-decayed)
	// score, for the off-hot-path budget refresher. Hosts untouched for long enough have
	// decayed back toward the floor and are excluded (they get the cold-start floor on
	// their next request, which is the same result), keeping the scan tight.
	ListBudgetInputs(ctx context.Context) ([]BudgetInput, error)
}

// PgxRepository implements Repository using pgx.
type PgxRepository struct {
	db DBTX
}

// NewPgxRepository creates a new PgxRepository.
func NewPgxRepository(db DBTX) *PgxRepository {
	return &PgxRepository{db: db}
}

// activeWindowDays bounds which hosts the budget refresher reads: a host untouched for
// this long has decayed to a negligible score (several half-lives), so it is treated as
// cold-start (the floor) on its next request without scanning it here.
const activeWindowDays = 14

// RecordOutcome upserts a host's reliability score, applying the RAC-style decay to the
// stored value before adding the good step or subtracting the bad step. The score is
// floored at 0 (a long string of failures parks a host at the floor budget, never
// negative). Mirrors credit.PgxRACRepository.Upsert's decay expression.
func (r *PgxRepository) RecordOutcome(ctx context.Context, hostID types.ID, good bool) error {
	delta := DefaultGoodStep
	goodInc, badInc := 1.0, 0.0
	if !good {
		delta = -DefaultBadStep
		goodInc, badInc = 0.0, 1.0
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO host_reliability (host_id, score, good_total, bad_total, last_updated)
		VALUES ($1, GREATEST(0, $2), $3, $4, NOW())
		ON CONFLICT (host_id) DO UPDATE SET
			score = GREATEST(0, host_reliability.score
				* exp(-EXTRACT(EPOCH FROM (NOW() - host_reliability.last_updated)) * ln(2) / $5)
				+ $2),
			good_total = host_reliability.good_total + $3,
			bad_total = host_reliability.bad_total + $4,
			last_updated = NOW()`,
		hostID, delta, goodInc, badInc, float64(HalfLifeSeconds),
	)
	if err != nil {
		return apierror.Internal("failed to record host reliability", err)
	}
	return nil
}

// ListBudgetInputs returns the current decayed score for every host updated within the
// active window. Decay is applied in SQL (read-time) so the refresher never needs a
// separate decay sweep.
func (r *PgxRepository) ListBudgetInputs(ctx context.Context) ([]BudgetInput, error) {
	rows, err := r.db.Query(ctx, `
		SELECT host_id,
		       GREATEST(0, score * exp(-EXTRACT(EPOCH FROM (NOW() - last_updated)) * ln(2) / $1)) AS cur_score
		FROM host_reliability
		WHERE last_updated > NOW() - make_interval(days => $2)`,
		float64(HalfLifeSeconds), activeWindowDays,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list host reliability", err)
	}
	defer rows.Close()

	var out []BudgetInput
	for rows.Next() {
		var bi BudgetInput
		if scanErr := rows.Scan(&bi.HostID, &bi.Score); scanErr != nil {
			return nil, apierror.Internal("failed to scan host reliability", scanErr)
		}
		out = append(out, bi)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate host reliability", err)
	}
	return out, nil
}
