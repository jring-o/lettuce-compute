package leaf

import (
	"context"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Repository defines the data-access interface for leafs.
type Repository interface {
	Create(ctx context.Context, p *Leaf) error
	GetByID(ctx context.Context, id types.ID) (*Leaf, error)
	GetBySlug(ctx context.Context, slug string, creatorID *types.ID) (*Leaf, error)
	GetBySlugPublic(ctx context.Context, slug string) (*Leaf, error)
	List(ctx context.Context, filters LeafListFilters, page types.PaginationRequest) ([]*Leaf, types.PaginationResponse, error)
	Update(ctx context.Context, p *Leaf) error
	Delete(ctx context.Context, id types.ID) error
}
