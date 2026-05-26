package paramsweep

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

const (
	largeSpaceThreshold = 100000
	floatEpsilon        = 1e-9
)

// ParseParameterSpace parses a raw parameter space definition into expanded
// parameter lists. Each parameter value must be either an explicit list (JSON
// array) or a range object with min, max, step fields.
func ParseParameterSpace(raw map[string]interface{}) (map[string][]interface{}, error) {
	if len(raw) == 0 {
		return nil, apierror.ValidationError("parameter space is empty", map[string]string{
			"reason": "at least one parameter is required",
		})
	}

	result := make(map[string][]interface{}, len(raw))

	for name, value := range raw {
		switch v := value.(type) {
		case []interface{}:
			if len(v) == 0 {
				return nil, apierror.ValidationError(
					fmt.Sprintf("parameter %q has empty list", name),
					map[string]string{"field": name, "reason": "empty_list"},
				)
			}
			result[name] = v

		case map[string]interface{}:
			expanded, err := expandRange(name, v)
			if err != nil {
				return nil, err
			}
			result[name] = expanded

		default:
			return nil, apierror.ValidationError(
				fmt.Sprintf("parameter %q has unrecognized format: expected list or range object", name),
				map[string]string{"field": name, "reason": "invalid_format"},
			)
		}
	}

	return result, nil
}

// expandRange expands a range object {"min": float64, "max": float64, "step": float64}
// into an explicit list of values.
func expandRange(name string, r map[string]interface{}) ([]interface{}, error) {
	minVal, ok := toFloat64(r["min"])
	if !ok {
		return nil, apierror.ValidationError(
			fmt.Sprintf("parameter %q range missing or invalid 'min' field", name),
			map[string]string{"field": name, "reason": "missing_min"},
		)
	}
	maxVal, ok := toFloat64(r["max"])
	if !ok {
		return nil, apierror.ValidationError(
			fmt.Sprintf("parameter %q range missing or invalid 'max' field", name),
			map[string]string{"field": name, "reason": "missing_max"},
		)
	}
	step, ok := toFloat64(r["step"])
	if !ok {
		return nil, apierror.ValidationError(
			fmt.Sprintf("parameter %q range missing or invalid 'step' field", name),
			map[string]string{"field": name, "reason": "missing_step"},
		)
	}

	if minVal > maxVal+floatEpsilon {
		return nil, apierror.ValidationError(
			fmt.Sprintf("parameter %q range has min (%v) > max (%v)", name, minVal, maxVal),
			map[string]string{"field": name, "reason": "min_greater_than_max"},
		)
	}
	if step <= 0 {
		return nil, apierror.ValidationError(
			fmt.Sprintf("parameter %q range has step <= 0", name),
			map[string]string{"field": name, "reason": "invalid_step"},
		)
	}

	var values []interface{}
	for v := minVal; v <= maxVal+floatEpsilon; v += step {
		// Round to step precision to avoid floating point drift
		values = append(values, roundToStep(v, minVal, step))
	}

	return values, nil
}

// roundToStep rounds a value to the nearest step increment from the origin
// to counteract floating point accumulation error.
func roundToStep(v, origin, step float64) float64 {
	steps := math.Round((v - origin) / step)
	return origin + steps*step
}

// toFloat64 converts a JSON number (float64 or integer types) to float64.
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// CartesianProduct computes all combinations across all parameter dimensions.
// Parameter keys are sorted alphabetically for deterministic ordering.
func CartesianProduct(params map[string][]interface{}) []map[string]interface{} {
	if len(params) == 0 {
		return nil
	}

	// Sort keys for deterministic ordering.
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Compute total combinations.
	total := 1
	for _, k := range keys {
		total *= len(params[k])
	}

	combinations := make([]map[string]interface{}, 0, total)

	// Iterative Cartesian product: track indices into each parameter list.
	indices := make([]int, len(keys))
	for i := 0; i < total; i++ {
		combo := make(map[string]interface{}, len(keys))
		for j, k := range keys {
			combo[k] = params[k][indices[j]]
		}
		combinations = append(combinations, combo)

		// Increment indices (odometer-style, rightmost increments first).
		for j := len(keys) - 1; j >= 0; j-- {
			indices[j]++
			if indices[j] < len(params[keys[j]]) {
				break
			}
			indices[j] = 0
		}
	}

	return combinations
}

// Generate orchestrates parameter sweep work unit generation:
// validate batch_size → parse parameter space → compute Cartesian product →
// split into batches → create Batch + BulkCreate work units → bulk transition
// CREATED → QUEUED.
//
// Supports an optional "_offset" key in parameterSpace for lazy generation.
// When set, the Cartesian product is computed starting at that index, generating
// up to batchSize work units from the offset position.
func Generate(
	ctx context.Context,
	proj *leaf.Leaf,
	parameterSpace map[string]interface{},
	batchSize int,
	wuRepo workunit.WorkUnitRepository,
	batchRepo workunit.BatchRepository,
) (*workunit.GenerateResult, error) {
	batchSize = generate.ClampBatchSize(batchSize)

	// Extract _offset if present (used for lazy generation).
	offset := 0
	cleanParams := make(map[string]interface{}, len(parameterSpace))
	for k, v := range parameterSpace {
		if k == "_offset" {
			if n, ok := toFloat64(v); ok {
				offset = int(n)
			}
			continue
		}
		cleanParams[k] = v
	}

	// Parse and expand parameter space.
	expanded, err := ParseParameterSpace(cleanParams)
	if err != nil {
		return nil, err
	}

	// Compute all combinations.
	combinations := CartesianProduct(expanded)
	totalCombinations := len(combinations)
	if totalCombinations == 0 {
		return nil, apierror.ValidationError("parameter space produced no combinations", nil)
	}

	// Apply offset: skip to the starting position.
	if offset >= totalCombinations {
		return &workunit.GenerateResult{
			Status: "complete",
		}, nil
	}
	combinations = combinations[offset:]
	remaining := len(combinations)

	// Resolve work unit field defaults from project.
	codeArtifactRef := generate.ResolveCodeArtifactRef(proj)
	deadlineSeconds := generate.ResolveDeadlineSeconds(proj)
	maxReassignments := proj.FaultToleranceConfig.MaxReassignments

	// Split into batches.
	numBatches := (remaining + batchSize - 1) / batchSize

	result := &workunit.GenerateResult{
		BatchIDs: make([]types.ID, 0, numBatches),
		Status:   "complete",
	}

	if totalCombinations > largeSpaceThreshold {
		slog.WarnContext(ctx, "large parameter space",
			"leaf_id", proj.ID,
			"combinations", totalCombinations,
		)
	}

	// Determine starting sequence number by querying existing batches.
	nextSeqNum, err := generate.ResolveNextSequenceNumber(ctx, proj.ID, batchRepo)
	if err != nil {
		return nil, err
	}

	for batchIdx := 0; batchIdx < numBatches; batchIdx++ {
		start := batchIdx * batchSize
		end := start + batchSize
		if end > remaining {
			end = remaining
		}
		batchCombinations := combinations[start:end]

		// Create batch record.
		batch := &workunit.Batch{
			LeafID:      proj.ID,
			SequenceNumber: nextSeqNum + batchIdx,
			TotalWorkUnits: len(batchCombinations),
		}
		if err := batchRepo.Create(ctx, batch); err != nil {
			return nil, apierror.Internal(fmt.Sprintf("create batch %d", batchIdx), err)
		}

		// Build work units for this batch.
		wus := make([]*workunit.WorkUnit, len(batchCombinations))
		for i, combo := range batchCombinations {
			params, err := json.Marshal(combo)
			if err != nil {
				return nil, apierror.Internal("failed to marshal parameters", err)
			}
			wus[i] = &workunit.WorkUnit{
				LeafID:        proj.ID,
				BatchID:          &batch.ID,
				State:            workunit.WorkUnitStateCreated,
				Priority:         workunit.WorkUnitPriorityNormal,
				CodeArtifactRef:  codeArtifactRef,
				Parameters:       params,
				DeadlineSeconds:  deadlineSeconds,
				MaxReassignments: maxReassignments,
			}
		}

		// Bulk insert work units.
		if err := wuRepo.BulkCreate(ctx, wus); err != nil {
			return nil, apierror.Internal(fmt.Sprintf("bulk create work units for batch %d", batchIdx), err)
		}

		// Transition all CREATED work units in this batch to QUEUED.
		if err := bulkTransitionToQueued(ctx, proj.ID, batch.ID, wuRepo); err != nil {
			return nil, apierror.Internal(fmt.Sprintf("transition batch %d to queued", batchIdx), err)
		}

		result.BatchIDs = append(result.BatchIDs, batch.ID)
		result.WorkUnitsCreated += len(batchCombinations)
	}

	slog.InfoContext(ctx, "parameter sweep generated",
		"leaf_id", proj.ID,
		"work_units", result.WorkUnitsCreated,
		"batches", len(result.BatchIDs),
	)

	return result, nil
}


// bulkTransitionToQueued transitions all CREATED work units in a batch to QUEUED
// in a single bulk UPDATE.
func bulkTransitionToQueued(ctx context.Context, leafID, batchID types.ID, wuRepo workunit.WorkUnitRepository) error {
	_, err := wuRepo.BulkTransitionByBatch(ctx, batchID, workunit.WorkUnitStateCreated, workunit.WorkUnitStateQueued)
	return err
}
