package assignment

import (
	"context"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Repository defines the data-access interface for work unit assignment history.
type Repository interface {
	Create(ctx context.Context, entry *AssignmentHistoryEntry) error
	GetByID(ctx context.Context, id types.ID) (*AssignmentHistoryEntry, error)
	ListByWorkUnit(ctx context.Context, workUnitID types.ID) ([]*AssignmentHistoryEntry, error)
	ListByVolunteer(ctx context.Context, volunteerID types.ID, page types.PaginationRequest) ([]*AssignmentHistoryEntry, types.PaginationResponse, error)
	CountActiveByWorkUnit(ctx context.Context, workUnitID types.ID) (int, error)
	UpdateOutcome(ctx context.Context, id types.ID, outcome AssignmentOutcome, resultID *types.ID) error
	FindActiveByWorkUnitAndVolunteer(ctx context.Context, workUnitID, volunteerID types.ID) (*AssignmentHistoryEntry, error)
}
