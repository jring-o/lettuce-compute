package credit

import (
	"context"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Repository defines the data-access interface for the credit ledger.
type Repository interface {
	Create(ctx context.Context, entry *LedgerEntry) error
	GetByResultID(ctx context.Context, resultID types.ID) (*LedgerEntry, error)
	SumByVolunteerProject(ctx context.Context, volunteerID, leafID types.ID) (float64, error)
	CountByVolunteerPerProject(ctx context.Context, volunteerID types.ID) (map[types.ID]int, error)
	ListByVolunteer(ctx context.Context, volunteerID types.ID, page types.PaginationRequest) ([]*LedgerEntry, types.PaginationResponse, error)
	ListByLeaf(ctx context.Context, leafID types.ID, page types.PaginationRequest) ([]*LedgerEntry, types.PaginationResponse, error)
}
