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

	// NoDeadlineCeilingSeconds is the synthetic deadline stamped on a NoDeadline
	// leaf's work units so the head always has a reclaim bound for a unit on a
	// vanished volunteer. With per-task heartbeats removed, liveness is purely
	// deadline-based; a literal 0 deadline_seconds would make FindExpiredWorkUnits
	// skip the unit (its deadline_seconds > 0 guard), permanently stranding a unit
	// whose volunteer died. Stamping a large ceiling instead keeps NoDeadline's
	// run-forever semantics for the runtime (6h is effectively no timeout for
	// execution) while guaranteeing the head reclaims it at the ceiling.
	//
	// This default mirrors config.defaultNoDeadlineCeilingSeconds (6h). It is the
	// fallback used when an operator has not set head.no_deadline_ceiling_seconds;
	// the configured value is applied via SetNoDeadlineCeilingSeconds at startup
	// (see cmd/lettuce-server/main.go) so the operator knob actually changes the
	// stamped deadline rather than being a silent no-op.
	NoDeadlineCeilingSeconds = 21600
)

// noDeadlineCeilingSeconds is the effective synthetic ceiling stamped on
// NoDeadline work units. It defaults to the package constant and is overridden
// once at startup from head.no_deadline_ceiling_seconds via
// SetNoDeadlineCeilingSeconds. It is set before any generation runs (eager HTTP
// generation, the lazy generation manager, and custom bulk upload all run after
// startup wiring), so no synchronization is required.
var noDeadlineCeilingSeconds = NoDeadlineCeilingSeconds

// SetNoDeadlineCeilingSeconds overrides the synthetic reclaim ceiling stamped on
// NoDeadline work units' deadline_seconds. main.go calls it once at startup with
// HeadConfig.EffectiveNoDeadlineCeilingSeconds() so the operator knob is live for
// every generation path. A non-positive value leaves the package default in
// place. Call this before serving any generation request.
func SetNoDeadlineCeilingSeconds(seconds int) {
	if seconds > 0 {
		noDeadlineCeilingSeconds = seconds
	}
}

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

// ResolveDeadlineSeconds computes the work-unit deadline from the leaf's deadline
// multiplier. For a NoDeadline leaf it returns the effective synthetic ceiling
// (head.no_deadline_ceiling_seconds, default NoDeadlineCeilingSeconds) rather
// than 0: with per-task heartbeats removed, liveness is purely deadline-based,
// and a literal 0 would make FindExpiredWorkUnits skip the unit (its
// deadline_seconds > 0 guard), permanently stranding a unit whose volunteer died.
// The ceiling keeps NoDeadline's run-forever semantics for the runtime
// (effectively no execution timeout) while guaranteeing the head always reclaims
// the unit by the ceiling. Lowering the operator knob lowers this stamped value
// for newly generated units, tightening reclaim.
func ResolveDeadlineSeconds(proj *leaf.Leaf) int {
	if proj.FaultToleranceConfig.NoDeadline {
		return noDeadlineCeilingSeconds
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
