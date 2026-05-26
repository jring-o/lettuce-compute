package volunteer

import (
	"context"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// VolunteerListFilters controls filtering for List queries.
type VolunteerListFilters struct {
	IsActive       *bool           `json:"is_active,omitempty"`
	SchedulingMode *SchedulingMode `json:"scheduling_mode,omitempty"`
}

// Repository defines the data-access interface for volunteers.
type Repository interface {
	Create(ctx context.Context, v *Volunteer) error
	GetByID(ctx context.Context, id types.ID) (*Volunteer, error)
	GetByPublicKey(ctx context.Context, publicKey []byte) (*Volunteer, error)
	GetByUserID(ctx context.Context, userID types.ID) (*Volunteer, error)
	Update(ctx context.Context, v *Volunteer) error
	UpdateLastSeen(ctx context.Context, id types.ID) error
	SetActive(ctx context.Context, id types.ID, active bool) error
	IncrementWorkUnitsCompleted(ctx context.Context, id types.ID) error
	IncrementWorkUnitsRejected(ctx context.Context, id types.ID) error
	List(ctx context.Context, filters VolunteerListFilters, page types.PaginationRequest) ([]*Volunteer, types.PaginationResponse, error)

	// MarkInactiveOlderThan sets is_active = false for all volunteers
	// whose last_seen_at < NOW() - threshold. Returns count of updated rows.
	MarkInactiveOlderThan(ctx context.Context, threshold time.Duration) (int, error)
}
