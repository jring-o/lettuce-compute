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
	if ref := ResolveCodeArtifactRefFromConfig(&proj.ExecutionConfig); ref != "" {
		return ref
	}
	return PlaceholderArtifactRef
}

// ResolveCodeArtifactRefFromConfig is ResolveCodeArtifactRef over a bare execution
// config: the first binary by sorted key, else the image, else "". Dispatch paths
// use it to derive the artifact URL from the EFFECTIVE (pinned or current) config
// (PB-17) with the exact rule generation stamps, so the served code_artifact_url
// always agrees with the served execution_spec.binaries.
func ResolveCodeArtifactRefFromConfig(ec *leaf.ExecutionConfig) string {
	if len(ec.Binaries) > 0 {
		keys := make([]string, 0, len(ec.Binaries))
		for k := range ec.Binaries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return ec.Binaries[keys[0]]
	}
	if ec.Image != nil {
		return *ec.Image
	}
	return ""
}

// ResolveDeadlineSeconds returns the deadline stamped on a new work unit of the
// leaf: the leaf's deadline_seconds when set, else the head's default deadline
// (head.default_deadline_seconds). It delegates to the leaf config so generation
// and the activation-time adequacy check resolve the same number in one place.
//
// The result is always positive. The volunteer stops a running unit at its
// deadline and the head expires a copy not returned by then; FindExpiredWorkUnits
// skips a unit whose deadline_seconds is not positive, so a zero here would strand
// a unit whose volunteer vanished.
func ResolveDeadlineSeconds(proj *leaf.Leaf) int {
	return proj.FaultToleranceConfig.ResolveDeadlineSeconds()
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
