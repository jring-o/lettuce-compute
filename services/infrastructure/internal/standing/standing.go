// Package standing exposes the account-standing surface (BG-24b): the
// operator admin API over volunteers.standing and the narrow reads the
// enforcement layers consume. The standing VALUES and the effective-standing
// resolution rule live in internal/volunteer (volunteer.Standing*,
// volunteer.EffectiveStanding); this package deliberately holds no second copy
// of that rule. The automatic rejection-rate backpressure machine that drives
// AUTO rows ships separately; until then every standing change is an operator
// action.
package standing

import (
	"context"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Entry is one volunteer's standing row as consumers read it. Standing and
// Source are the RAW stored values (never empty; DB defaults 'OK'/'AUTO') —
// enforcement resolves them through volunteer.EffectiveStanding rather than
// reading Standing directly.
type Entry struct {
	VolunteerID  types.ID   `json:"volunteer_id"`
	Standing     string     `json:"standing"`
	BenchedUntil *time.Time `json:"benched_until,omitempty"`
	Source       string     `json:"standing_source"`
	Reason       *string    `json:"standing_reason,omitempty"`
	ChangedAt    *time.Time `json:"standing_changed_at,omitempty"`
}

// Repository is the data-access surface for account standing. Implementations
// write ONLY the standing columns of the volunteers table (the volunteer
// repository's Create/Update never touch them).
type Repository interface {
	// SetOperator sets a volunteer's standing as an OPERATOR action (source
	// OPERATOR, reason recorded, standing_changed_at = now()). benchedUntil is
	// required semantics only for BENCHED (NULL = indefinite); it is cleared for
	// other standings.
	SetOperator(ctx context.Context, volunteerID types.ID, standingValue string, benchedUntil *time.Time, reason string) (*Entry, error)
	// Clear returns a volunteer to OK/AUTO with reason and benched_until cleared
	// (the operator release path).
	Clear(ctx context.Context, volunteerID types.ID) (*Entry, error)
	// Get returns the standing entry for one volunteer (nil, nil when the
	// volunteer does not exist).
	Get(ctx context.Context, volunteerID types.ID) (*Entry, error)
	// ListNonOK pages the rows whose STORED standing is not OK (the partial-index
	// scan backing the admin list).
	ListNonOK(ctx context.Context, limit, offset int) ([]*Entry, error)
	// AllNonOK returns every row whose stored standing is not OK, keyed by
	// volunteer id — the dispatch cache's TTL-snapshot read (small by
	// construction: the whole population is OK unless the operator or the
	// backpressure machine has acted).
	AllNonOK(ctx context.Context) (map[types.ID]Entry, error)
}
