package generate

import (
	"context"
	"sort"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

const (
	DefaultBatchSize       = 10000
	MinBatchSize           = 1
	MaxBatchSize           = 100000
	DefaultDurationSeconds = 3600
	PlaceholderArtifactRef = "ref://placeholder"
)

// ClampBatchSize clamps the batch size to [MinBatchSize, MaxBatchSize],
// defaulting to DefaultBatchSize if unset.
func ClampBatchSize(batchSize int) int {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	return max(MinBatchSize, min(batchSize, MaxBatchSize))
}

// ResolveCodeArtifactRef picks the first available binary from the project's
// execution config (sorted for determinism), falls back to Image, then placeholder.
func ResolveCodeArtifactRef(proj *leaf.Leaf) string {
	if len(proj.ExecutionConfig.Binaries) > 0 {
		keys := make([]string, 0, len(proj.ExecutionConfig.Binaries))
		for k := range proj.ExecutionConfig.Binaries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return proj.ExecutionConfig.Binaries[keys[0]]
	}
	if proj.ExecutionConfig.Image != nil {
		return *proj.ExecutionConfig.Image
	}
	return PlaceholderArtifactRef
}

// ResolveDeadlineSeconds computes deadline from the project's deadline multiplier.
// Returns 0 ("no deadline") when the leaf opts out via NoDeadline; the volunteer
// runtime only enforces a timeout when deadline_seconds > 0, and
// FindExpiredWorkUnits skips zero-deadline units, so liveness falls back to
// heartbeats alone (the unit may run indefinitely while it keeps heartbeating).
func ResolveDeadlineSeconds(proj *leaf.Leaf) int {
	if proj.FaultToleranceConfig.NoDeadline {
		return 0
	}
	multiplier := proj.FaultToleranceConfig.DeadlineMultiplier
	if multiplier <= 0 {
		multiplier = 1.0
	}
	return int(float64(DefaultDurationSeconds) * multiplier)
}

// ResolveNextSequenceNumber queries existing batches and returns the next
// available sequence number.
func ResolveNextSequenceNumber(ctx context.Context, projectID types.ID, batchRepo workunit.BatchRepository) (int, error) {
	batches, _, err := batchRepo.ListByLeaf(ctx, projectID, types.PaginationRequest{PageSize: 200})
	if err != nil {
		return 0, apierror.Internal("query existing batches", err)
	}

	maxSeq := 0
	for _, b := range batches {
		if b.SequenceNumber > maxSeq {
			maxSeq = b.SequenceNumber
		}
	}
	return maxSeq + 1, nil
}
