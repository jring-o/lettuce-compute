package trust

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
)

// DBTX is the minimal pgx surface the repository needs (satisfied by *pgxpool.Pool and a
// pgx.Tx), mirroring reliability.DBTX so the trust store can run on the pool or in a tx —
// accrual/slash want to ride the same transaction that records the validation outcome.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PgxRepository implements Repository using pgx.
type PgxRepository struct {
	db DBTX
}

// NewPgxRepository creates a new PgxRepository.
func NewPgxRepository(db DBTX) *PgxRepository {
	return &PgxRepository{db: db}
}

// GetScore returns the subject's current score, or 0 when the subject has no row.
func (r *PgxRepository) GetScore(ctx context.Context, subject string) (int, error) {
	var score int
	err := r.db.QueryRow(ctx,
		`SELECT score FROM volunteer_trust WHERE subject = $1`, subject,
	).Scan(&score)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, apierror.Internal("failed to get trust score", err)
	}
	return score, nil
}

// Get returns the full entry, or nil when the subject is absent.
func (r *PgxRepository) Get(ctx context.Context, subject string) (*Entry, error) {
	var e Entry
	err := r.db.QueryRow(ctx,
		`SELECT subject, score, clean_units, slashed_at, created_at, updated_at
		 FROM volunteer_trust WHERE subject = $1`, subject,
	).Scan(&e.Subject, &e.Score, &e.CleanUnits, &e.SlashedAt, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, apierror.Internal("failed to get trust entry", err)
	}
	return &e, nil
}

// SetScore upserts the subject's score without touching clean_units (operator seeding).
func (r *PgxRepository) SetScore(ctx context.Context, subject string, score int) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO volunteer_trust (subject, score)
		VALUES ($1, $2)
		ON CONFLICT (subject) DO UPDATE SET
			score = $2,
			updated_at = now()`,
		subject, score,
	)
	if err != nil {
		return apierror.Internal("failed to set trust score", err)
	}
	return nil
}

// AccrueCleanUnit increments both clean_units and score by 1 (a new subject starts 1/1).
func (r *PgxRepository) AccrueCleanUnit(ctx context.Context, subject string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO volunteer_trust (subject, score, clean_units)
		VALUES ($1, 1, 1)
		ON CONFLICT (subject) DO UPDATE SET
			score = volunteer_trust.score + 1,
			clean_units = volunteer_trust.clean_units + 1,
			updated_at = now()`,
		subject,
	)
	if err != nil {
		return apierror.Internal("failed to accrue clean unit", err)
	}
	return nil
}

// Slash zeroes the subject's score and stamps slashed_at, retaining clean_units for the
// audit trail. Slashing an absent subject creates a zeroed, slashed row so the event is
// recorded even for a subject that had never accrued.
func (r *PgxRepository) Slash(ctx context.Context, subject string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO volunteer_trust (subject, score, slashed_at)
		VALUES ($1, 0, now())
		ON CONFLICT (subject) DO UPDATE SET
			score = 0,
			slashed_at = now(),
			updated_at = now()`,
		subject,
	)
	if err != nil {
		return apierror.Internal("failed to slash trust subject", err)
	}
	return nil
}

// List returns entries ordered by score DESC, subject ASC.
func (r *PgxRepository) List(ctx context.Context, limit, offset int) ([]*Entry, error) {
	rows, err := r.db.Query(ctx, `
		SELECT subject, score, clean_units, slashed_at, created_at, updated_at
		FROM volunteer_trust
		ORDER BY score DESC, subject ASC
		LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, apierror.Internal("failed to list trust entries", err)
	}
	defer rows.Close()

	var out []*Entry
	for rows.Next() {
		var e Entry
		if scanErr := rows.Scan(&e.Subject, &e.Score, &e.CleanUnits, &e.SlashedAt, &e.CreatedAt, &e.UpdatedAt); scanErr != nil {
			return nil, apierror.Internal("failed to scan trust entry", scanErr)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate trust entries", err)
	}
	return out, nil
}

// AllScores returns a subject -> score map of every positively-scored subject. The
// WHERE score > 0 filter bounds the result to the trusted-subject population (seeded or
// accrued, not slashed to zero), which is what the dispatch cache snapshots for the
// trusted-corroborator reservation. A slashed or never-seeded subject is simply absent
// (a map miss reads as 0 — untrusted — for the caller, the correct default).
func (r *PgxRepository) AllScores(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.Query(ctx, `SELECT subject, score FROM volunteer_trust WHERE score > 0`)
	if err != nil {
		return nil, apierror.Internal("failed to list trust scores", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var subject string
		var score int
		if scanErr := rows.Scan(&subject, &score); scanErr != nil {
			return nil, apierror.Internal("failed to scan trust score", scanErr)
		}
		out[subject] = score
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.Internal("failed to iterate trust scores", err)
	}
	return out, nil
}
