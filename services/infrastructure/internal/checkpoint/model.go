package checkpoint

import (
	"context"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Checkpoint represents a saved checkpoint for a running work unit.
type Checkpoint struct {
	ID                 types.ID  `json:"id"`
	LeafID          types.ID  `json:"leaf_id"`
	WorkUnitID         types.ID  `json:"work_unit_id"`
	VolunteerID        types.ID  `json:"volunteer_id"`
	CheckpointSequence int       `json:"checkpoint_sequence"`
	StorageKey         string    `json:"storage_key"`
	SizeBytes          int64     `json:"size_bytes"`
	ChecksumSHA256     string    `json:"checksum_sha256"`
	CreatedAt          time.Time `json:"created_at"`
}

// Repository defines the data-access interface for checkpoints.
type Repository interface {
	// Save stores a checkpoint, replacing any existing checkpoint for the work unit
	// with a lower sequence number. Updates work_units.last_checkpoint_at and
	// last_checkpoint_sequence.
	Save(ctx context.Context, cp *Checkpoint, data []byte) error

	// GetLatest returns the latest checkpoint and its data for a work unit.
	// Returns nil, nil, nil if no checkpoint exists.
	GetLatest(ctx context.Context, workUnitID types.ID) (*Checkpoint, []byte, error)

	// Delete removes all checkpoints for a work unit (called on completion/failure).
	Delete(ctx context.Context, workUnitID types.ID) error
}
