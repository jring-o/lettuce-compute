package montecarlo

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

const (
	maxNumTrials           = 10_000_000
	defaultSeedStrategy    = "sequential"
	defaultSeedOffset      = 0
)

// trialParams is the JSON structure stored in each work unit's Parameters field.
type trialParams struct {
	Seed       int64 `json:"seed"`
	TrialIndex int   `json:"trial_index"`
}

// Generate creates Monte Carlo work units, one per trial, each with a unique seed.
// Seeds are generated according to the seed_strategy:
//   - "sequential": seeds are seed_offset, seed_offset+1, ..., seed_offset+N-1
//   - "hash": seeds are SHA-256(leaf_id + trial_index) truncated to int64
func Generate(
	ctx context.Context,
	proj *leaf.Leaf,
	parameterSpace map[string]interface{},
	batchSize int,
	wuRepo workunit.WorkUnitRepository,
	batchRepo workunit.BatchRepository,
) (*workunit.GenerateResult, error) {
	if proj.TaskPattern != leaf.PatternMonteCarlo {
		return nil, apierror.ValidationError(
			fmt.Sprintf("monte carlo generator requires MONTE_CARLO task pattern, got %q", proj.TaskPattern),
			nil,
		)
	}

	batchSize = generate.ClampBatchSize(batchSize)

	// Extract configuration from request parameterSpace, falling back to project DataConfig.SplittingConfig.
	config := mergeConfig(parameterSpace, proj.DataConfig.SplittingConfig)

	numTrials, err := extractNumTrials(config)
	if err != nil {
		return nil, err
	}

	seedStrategy, err := extractSeedStrategy(config)
	if err != nil {
		return nil, err
	}

	seedOffset, err := extractSeedOffset(config)
	if err != nil {
		return nil, err
	}

	sharedConfig, err := extractSharedConfig(config)
	if err != nil {
		return nil, err
	}

	// Resolve work unit field defaults.
	codeArtifactRef := generate.ResolveCodeArtifactRef(proj)
	deadlineSeconds := generate.ResolveDeadlineSeconds(proj)
	maxReassignments := proj.FaultToleranceConfig.MaxReassignments

	// Split into batches.
	numBatches := (numTrials + batchSize - 1) / batchSize

	result := &workunit.GenerateResult{
		BatchIDs: make([]types.ID, 0, numBatches),
		Status:   "complete",
	}

	nextSeqNum, err := generate.ResolveNextSequenceNumber(ctx, proj.ID, batchRepo)
	if err != nil {
		return nil, err
	}

	for batchIdx := 0; batchIdx < numBatches; batchIdx++ {
		start := batchIdx * batchSize
		end := start + batchSize
		if end > numTrials {
			end = numTrials
		}
		batchCount := end - start

		batch := &workunit.Batch{
			LeafID:      proj.ID,
			SequenceNumber: nextSeqNum + batchIdx,
			TotalWorkUnits: batchCount,
		}
		if err := batchRepo.Create(ctx, batch); err != nil {
			return nil, apierror.Internal(fmt.Sprintf("create batch %d", batchIdx), err)
		}

		wus := make([]*workunit.WorkUnit, batchCount)
		for i := 0; i < batchCount; i++ {
			trialIndex := start + i
			seed := generateSeed(seedStrategy, seedOffset, trialIndex, proj.ID)

			params, err := json.Marshal(trialParams{
				Seed:       seed,
				TrialIndex: trialIndex,
			})
			if err != nil {
				return nil, apierror.Internal("failed to marshal trial parameters", err)
			}

			wus[i] = &workunit.WorkUnit{
				LeafID:        proj.ID,
				BatchID:          &batch.ID,
				State:            workunit.WorkUnitStateCreated,
				Priority:         workunit.WorkUnitPriorityNormal,
				CodeArtifactRef:  codeArtifactRef,
				Parameters:       params,
				InputData:        sharedConfig,
				DeadlineSeconds:  deadlineSeconds,
				MaxReassignments: maxReassignments,
			}
		}

		if err := wuRepo.BulkCreate(ctx, wus); err != nil {
			return nil, apierror.Internal(fmt.Sprintf("bulk create work units for batch %d", batchIdx), err)
		}

		if _, err := wuRepo.BulkTransitionByBatch(ctx, batch.ID, workunit.WorkUnitStateCreated, workunit.WorkUnitStateQueued); err != nil {
			return nil, apierror.Internal(fmt.Sprintf("transition batch %d to queued", batchIdx), err)
		}

		result.BatchIDs = append(result.BatchIDs, batch.ID)
		result.WorkUnitsCreated += batchCount
	}

	slog.InfoContext(ctx, "monte carlo work units generated",
		"leaf_id", proj.ID,
		"work_units", result.WorkUnitsCreated,
		"batches", len(result.BatchIDs),
		"seed_strategy", seedStrategy,
	)

	return result, nil
}

// generateSeed produces a seed for the given trial index based on the strategy.
func generateSeed(strategy string, offset, trialIndex int, leafID types.ID) int64 {
	if strategy == "hash" {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", leafID.String(), trialIndex)))
		seed := int64(binary.BigEndian.Uint64(h[:8]) & 0x7FFFFFFFFFFFFFFF)
		return seed
	}
	// sequential
	return int64(offset) + int64(trialIndex)
}

// mergeConfig creates a merged config map, preferring request values over project defaults.
func mergeConfig(request, projectDefault map[string]interface{}) map[string]interface{} {
	merged := make(map[string]interface{})
	for k, v := range projectDefault {
		merged[k] = v
	}
	for k, v := range request {
		merged[k] = v
	}
	return merged
}

func extractNumTrials(config map[string]interface{}) (int, error) {
	v, ok := config["num_trials"]
	if !ok {
		return 0, apierror.ValidationError("num_trials is required for monte carlo generation", nil)
	}
	n, err := toInt(v)
	if err != nil || n < 1 {
		return 0, apierror.ValidationError("num_trials must be an integer >= 1", nil)
	}
	if n > maxNumTrials {
		return 0, apierror.ValidationError(
			fmt.Sprintf("num_trials must be <= %d", maxNumTrials), nil)
	}
	return n, nil
}

func extractSeedStrategy(config map[string]interface{}) (string, error) {
	v, ok := config["seed_strategy"]
	if !ok {
		return defaultSeedStrategy, nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", apierror.ValidationError("seed_strategy must be a string", nil)
	}
	switch s {
	case "sequential", "hash":
		return s, nil
	default:
		return "", apierror.ValidationError(
			fmt.Sprintf("invalid seed_strategy: %q; must be \"sequential\" or \"hash\"", s), nil)
	}
}

func extractSeedOffset(config map[string]interface{}) (int, error) {
	v, ok := config["seed_offset"]
	if !ok {
		return defaultSeedOffset, nil
	}
	n, err := toInt(v)
	if err != nil || n < 0 {
		return 0, apierror.ValidationError("seed_offset must be a non-negative integer", nil)
	}
	return n, nil
}

func extractSharedConfig(config map[string]interface{}) (json.RawMessage, error) {
	v, ok := config["shared_config"]
	if !ok || v == nil {
		return nil, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, apierror.ValidationError("shared_config must be valid JSON", nil)
	}
	return json.RawMessage(data), nil
}

// toInt converts a JSON-decoded number to int.
func toInt(v interface{}) (int, error) {
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int", v)
	}
}
