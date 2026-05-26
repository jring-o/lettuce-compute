package result

import (
	"context"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// ResultFilters holds optional filter parameters for listing results.
type ResultFilters struct {
	ValidationStatus *ValidationStatus
	WorkUnitID       *types.ID
	VolunteerID      *types.ID
}

// Repository defines the data-access interface for results.
type Repository interface {
	Create(ctx context.Context, r *Result) error
	GetByID(ctx context.Context, id types.ID) (*Result, error)
	ListByWorkUnit(ctx context.Context, workUnitID types.ID) ([]*Result, error)
	ListByVolunteer(ctx context.Context, volunteerID types.ID, page types.PaginationRequest) ([]*Result, types.PaginationResponse, error)
	ListByLeaf(ctx context.Context, projectID types.ID, filters ResultFilters, page types.PaginationRequest) ([]*Result, types.PaginationResponse, error)
	CountByWorkUnit(ctx context.Context, workUnitID types.ID) (int, error)
	CountPendingByWorkUnit(ctx context.Context, workUnitID types.ID) (int, error)
	UpdateValidationStatus(ctx context.Context, id types.ID, status ValidationStatus) error
	BatchUpdateValidationStatus(ctx context.Context, ids []types.ID, status ValidationStatus) error
}
