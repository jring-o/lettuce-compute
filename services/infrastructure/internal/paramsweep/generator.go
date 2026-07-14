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

// sortedKeys returns the parameter keys in the deterministic (alphabetical) order the
// odometer decode uses — the same order CartesianProduct enumerates, so window(i..j) under
// the decoder equals full-product[i..j].
func sortedKeys(params map[string][]interface{}) []string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// totalCombinations computes the size of the full Cartesian product from the per-key value
// counts alone (∏ len(values_k)) — no materialization. The product is overflow-checked
// (hardening note (e)): ~64 two-value parameters overflow int64, and an overflowed total of
// 0/negative would flow into the `offset >= total` exhaustion early-return — a lazy tick would
// then emit zero units and markExhausted would stamp the leaf complete-with-zero-units,
// SILENTLY (the pre-windowing code at least panicked loudly in make()). Any space whose true
// size exceeds int64 is incompletable regardless, so refusing it loses no real coverage.
func totalCombinations(keys []string, params map[string][]interface{}) (int, error) {
	total := 1
	for _, k := range keys {
		n := len(params[k])
		if n == 0 {
			return 0, nil
		}
		if total > math.MaxInt/n {
			return 0, apierror.ValidationError(
				"parameter space too large: the combination count overflows a 64-bit integer; remove parameters or values",
				map[string]string{"reason": "combination_count_overflow"})
		}
		total *= n
	}
	return total, nil
}

// decodeCombination recovers the combination at global index `index` in the full product by
// positional odometer decode: rightmost key varies fastest, matching CartesianProduct's forward
// odometer (which increments the last key first). This is the O(1)-memory replacement for
// materializing the whole product and slicing it — the science depends on this yielding exactly
// full-product[index].
func decodeCombination(keys []string, params map[string][]interface{}, index int) map[string]interface{} {
	combo := make(map[string]interface{}, len(keys))
	idx := index
	for j := len(keys) - 1; j >= 0; j-- {
		k := keys[j]
		radix := len(params[k])
		combo[k] = params[k][idx%radix]
		idx /= radix
	}
	return combo
}

// Generate orchestrates parameter sweep work unit generation: validate batch_size → parse
// parameter space → compute the total product size from lengths → generate combinations by
// odometer decode (never materializing the product) → persist each batch atomically through the
// sink.
//
// Pacing (design §4.7): an optional "_offset" key marks the LAZY path. When present, exactly one
// window of min(batchSize, total-offset) combinations is generated in ONE batch (true top-up
// pacing — the pre-fix code flooded the entire remaining space in a single tick). When absent
// (eager), the full remaining space is generated batch-by-batch. Memory is O(batch) either way.
func Generate(
	ctx context.Context,
	proj *leaf.Leaf,
	parameterSpace map[string]interface{},
	batchSize int,
	sink workunit.BatchSink,
) (*workunit.GenerateResult, error) {
	batchSize = generate.ClampBatchSize(batchSize)

	// Extract _offset if present (marks the lazy path).
	offset := 0
	lazy := false
	cleanParams := make(map[string]interface{}, len(parameterSpace))
	for k, v := range parameterSpace {
		if k == "_offset" {
			lazy = true
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

	// Deterministic key order + total size, computed from lengths alone (no materialization).
	keys := sortedKeys(expanded)
	total, err := totalCombinations(keys, expanded)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, apierror.ValidationError("parameter space produced no combinations", nil)
	}

	if total > largeSpaceThreshold {
		slog.WarnContext(ctx, "large parameter space",
			"leaf_id", proj.ID,
			"combinations", total,
		)
	}

	// Exhaustion: offset at or past the end generates nothing.
	if offset >= total {
		return &workunit.GenerateResult{
			Status: "complete",
		}, nil
	}

	// Window to generate this call: one batch-sized window on the lazy path, the whole remainder
	// on the eager path.
	remaining := total - offset
	need := remaining
	if lazy {
		need = min(batchSize, remaining)
	}

	// Resolve work unit field defaults from project.
	codeArtifactRef := generate.ResolveCodeArtifactRef(proj)
	deadlineSeconds := generate.ResolveDeadlineSeconds(proj)
	maxReassignments := proj.FaultToleranceConfig.MaxReassignments

	numBatches := (need + batchSize - 1) / batchSize

	result := &workunit.GenerateResult{
		BatchIDs: make([]types.ID, 0, numBatches),
		Status:   "complete",
	}

	nextSeqNum, err := sink.NextSequenceNumber(ctx, proj.ID)
	if err != nil {
		return nil, err
	}

	for batchIdx := 0; batchIdx < numBatches; batchIdx++ {
		batchStart := batchIdx * batchSize // relative to the `need` window
		batchEnd := batchStart + batchSize
		if batchEnd > need {
			batchEnd = need
		}
		batchCount := batchEnd - batchStart

		batch := &workunit.Batch{
			LeafID:         proj.ID,
			SequenceNumber: nextSeqNum + batchIdx,
			TotalWorkUnits: batchCount,
		}

		wus := make([]*workunit.WorkUnit, batchCount)
		for i := 0; i < batchCount; i++ {
			// Global combination index into the full product (window base = offset).
			combo := decodeCombination(keys, expanded, offset+batchStart+i)
			params, err := json.Marshal(combo)
			if err != nil {
				return nil, apierror.Internal("failed to marshal parameters", err)
			}
			wus[i] = &workunit.WorkUnit{
				LeafID:           proj.ID,
				State:            workunit.WorkUnitStateCreated,
				Priority:         workunit.WorkUnitPriorityNormal,
				CodeArtifactRef:  codeArtifactRef,
				Parameters:       params,
				DeadlineSeconds:  deadlineSeconds,
				MaxReassignments: maxReassignments,
			}
		}

		// One atomic write: batch row + units + CREATED->QUEUED transition (design §4.8).
		if err := sink.PersistBatch(ctx, batch, wus, nil); err != nil {
			return nil, err
		}

		result.BatchIDs = append(result.BatchIDs, batch.ID)
		result.WorkUnitsCreated += batchCount
	}

	slog.InfoContext(ctx, "parameter sweep generated",
		"leaf_id", proj.ID,
		"work_units", result.WorkUnitsCreated,
		"batches", len(result.BatchIDs),
	)

	return result, nil
}
