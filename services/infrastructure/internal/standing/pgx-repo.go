package standing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// DBTX is the minimal pgx surface the repository needs (satisfied by *pgxpool.Pool and a
// pgx.Tx), mirroring trust.DBTX so a standing change can ride the same transaction that
// records the validation outcome that motivated it, rather than only running on the pool.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PgxRepository implements Repository over the standing columns of the volunteers table.
// It writes ONLY those columns; the volunteer repository's Create/Update never touch them.
type PgxRepository struct {
	db DBTX
}

// NewPgxRepository creates a new PgxRepository.
func NewPgxRepository(db DBTX) *PgxRepository {
	return &PgxRepository{db: db}
}

// entryColumns is the projection every read and RETURNING clause shares, in scanEntry order.
const entryColumns = `id, standing, benched_until, standing_source, standing_reason, standing_changed_at`

// scanEntry scans one Entry from a pgx.Row (a QueryRow result or a Rows cursor position).
func scanEntry(row pgx.Row) (*Entry, error) {
	var e Entry
	if err := row.Scan(&e.VolunteerID, &e.Standing, &e.BenchedUntil, &e.Source, &e.Reason, &e.ChangedAt); err != nil {
		return nil, err
	}
	return &e, nil
}

// SetOperator sets a volunteer's standing as an OPERATOR action: source OPERATOR, reason
// recorded, standing_changed_at = now(). benched_until is meaningful only for BENCHED
// (NULL there means indefinite) and is cleared for OK/PROBATION so a stale deadline can
// never resurface. An empty reason persists as NULL. An unknown volunteer id updates no
// row and returns (nil, nil), so the handler can 404.
func (r *PgxRepository) SetOperator(ctx context.Context, volunteerID types.ID, standingValue string, benchedUntil *time.Time, reason string) (*Entry, error) {
	switch standingValue {
	case volunteer.StandingOK, volunteer.StandingProbation, volunteer.StandingBenched:
	default:
		return nil, apierror.ValidationError("invalid standing", nil)
	}
	if standingValue != volunteer.StandingBenched {
		benchedUntil = nil
	}
	var reasonArg *string
	if reason != "" {
		reasonArg = &reason
	}

	row := r.db.QueryRow(ctx, `
		UPDATE volunteers SET
			standing = $2,
			benched_until = $3,
			standing_source = 'OPERATOR',
			standing_reason = $4,
			standing_changed_at = NOW()
		WHERE id = $1
		RETURNING `+entryColumns,
		volunteerID, standingValue, benchedUntil, reasonArg,
	)
	e, err := scanEntry(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, apierror.Internal("failed to set standing", err)
	}
	return e, nil
}

// Clear returns a volunteer to OK/AUTO with benched_until and standing_reason cleared — the
// operator release path (and the AUTO baseline). An unknown volunteer id returns (nil, nil).
func (r *PgxRepository) Clear(ctx context.Context, volunteerID types.ID) (*Entry, error) {
	row := r.db.QueryRow(ctx, `
		UPDATE volunteers SET
			standing = 'OK',
			standing_source = 'AUTO',
			benched_until = NULL,
			standing_reason = NULL,
			standing_changed_at = NOW()
		WHERE id = $1
		RETURNING `+entryColumns,
		volunteerID,
	)
	e, err := scanEntry(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, apierror.Internal("failed to clear standing", err)
	}
	return e, nil
}

// Get returns one volunteer's standing entry, or (nil, nil) when the volunteer does not
// exist.
func (r *PgxRepository) Get(ctx context.Context, volunteerID types.ID) (*Entry, error) {
	row := r.db.QueryRow(ctx,
		`SELECT `+entryColumns+` FROM volunteers WHERE id = $1`, volunteerID)
	e, err := scanEntry(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, apierror.Internal("failed to get standing", err)
	}
	return e, nil
}

// ListNonOK pages the rows whose STORED standing is not OK, newest change first. The
// partial index idx_volunteers_standing (WHERE standing <> 'OK') backs the scan, so it
// stays cheap against the overwhelmingly-OK population.
func (r *PgxRepository) ListNonOK(ctx context.Context, limit, offset int) ([]*Entry, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+entryColumns+`
		FROM volunteers
		WHERE standing <> 'OK'
		ORDER BY standing_changed_at DESC NULLS LAST
		LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list standing entries", err)
	}
	defer rows.Close()

	var out []*Entry
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan standing entry", scanErr)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate standing entries", err)
	}
	return out, nil
}

// AllNonOK returns every row whose stored standing is not OK, keyed by volunteer id — the
// dispatch cache's TTL-snapshot read. The same partial index keeps it cheap: the whole
// population is OK unless the operator (or, later, the backpressure machine) has acted, so
// the result set is small by construction.
func (r *PgxRepository) AllNonOK(ctx context.Context) (map[types.ID]Entry, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+entryColumns+`
		FROM volunteers
		WHERE standing <> 'OK'`,
	)
	if err != nil {
		return nil, apierror.Internal("failed to snapshot standing", err)
	}
	defer rows.Close()

	out := make(map[types.ID]Entry)
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, apierror.Internal("failed to scan standing entry", scanErr)
		}
		out[e.VolunteerID] = *e
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate standing snapshot", err)
	}
	return out, nil
}
