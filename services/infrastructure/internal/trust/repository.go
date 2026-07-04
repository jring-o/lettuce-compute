package trust

import "context"

// Repository is the data-access surface for the account-level trust signal. Reads are on
// validation's acceptance path (does an agreeing group contain enough trusted subjects);
// writes are the accrual/slash outcomes of validation and the operator's seeding API.
type Repository interface {
	// GetScore returns the subject's current score, 0 when the subject has no row. 0 is
	// the correct default: an unseen subject has earned no trust.
	GetScore(ctx context.Context, subject string) (int, error)
	// Get returns the full entry, nil when the subject is absent.
	Get(ctx context.Context, subject string) (*Entry, error)
	// SetScore upserts the subject's score (operator seeding / correction). It does not
	// touch clean_units — seeding grants quorum power without fabricating an earned-work
	// history, so an auditor can still tell seeded trust from accrued trust.
	SetScore(ctx context.Context, subject string, score int) error
	// AccrueCleanUnit records one corroborated-clean unit: increments BOTH clean_units
	// and score by 1 (upsert; a new subject starts at 1/1).
	AccrueCleanUnit(ctx context.Context, subject string) error
	// Slash zeroes the subject's score and stamps slashed_at (clean_units retained for
	// audit). Upsert: slashing an absent subject creates a zeroed, slashed row.
	Slash(ctx context.Context, subject string) error
	// List returns entries ordered by score DESC, subject ASC.
	List(ctx context.Context, limit, offset int) ([]*Entry, error)
}
