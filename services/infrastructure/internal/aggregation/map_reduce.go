package aggregation

import (
	"encoding/json"
	"fmt"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
)

const treeReductionThreshold = 1000
const treeReductionGroupSize = 100

// aggregateMapReduce applies a built-in reducer to mapped outputs.
func aggregateMapReduce(pairs []aggregatedWorkUnit, config map[string]any) (*AggregateResult, error) {
	reducerType, _ := config["reducer_type"].(string)
	reducerField, _ := config["reducer_field"].(string)
	mergeStrategy, _ := config["merge_strategy"].(string)

	if reducerType == "" {
		return nil, apierror.ValidationError("map-reduce aggregation_config missing reducer_type", nil)
	}
	if reducerField == "" && reducerType != "concatenate" && reducerType != "merge" {
		return nil, apierror.ValidationError("map-reduce aggregation_config missing reducer_field", nil)
	}

	var resultData json.RawMessage
	var err error

	switch reducerType {
	case "sum":
		resultData, err = reducerSum(pairs, reducerField)
	case "average":
		resultData, err = reducerAverage(pairs, reducerField)
	case "concatenate":
		resultData, err = reducerConcatenate(pairs)
	case "merge":
		resultData, err = reducerMerge(pairs, mergeStrategy)
	default:
		return nil, apierror.ValidationError(fmt.Sprintf("unknown reducer_type: %q", reducerType), nil)
	}

	if err != nil {
		return nil, err
	}

	return &AggregateResult{
		Status:              "complete",
		Format:              "json",
		Result:              resultData,
		WorkUnitsAggregated: len(pairs),
	}, nil
}

// extractNumericField extracts a numeric value from output_data[field].
func extractNumericField(outputData json.RawMessage, field string) (float64, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(outputData, &m); err != nil {
		return 0, fmt.Errorf("unmarshal output_data: %w", err)
	}
	v, ok := m[field]
	if !ok {
		return 0, fmt.Errorf("field %q not found in output_data", field)
	}
	num, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("field %q is not numeric", field)
	}
	return num, nil
}

// sumValues sums a float64 slice, using tree reduction for large datasets.
func sumValues(values []float64) float64 {
	if len(values) > treeReductionThreshold {
		return treeReduceSum(values)
	}
	var total float64
	for _, v := range values {
		total += v
	}
	return total
}

// reducerSum computes the numeric sum of output_data[reducer_field].
// Uses tree reduction for large datasets (> 1000 results).
func reducerSum(pairs []aggregatedWorkUnit, field string) (json.RawMessage, error) {
	values, err := extractAllNumeric(pairs, field)
	if err != nil {
		return nil, err
	}

	total := sumValues(values)
	out := map[string]interface{}{
		"sum":   total,
		"count": len(values),
	}
	return json.Marshal(out)
}

// reducerAverage computes the mean of output_data[reducer_field].
func reducerAverage(pairs []aggregatedWorkUnit, field string) (json.RawMessage, error) {
	values, err := extractAllNumeric(pairs, field)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("no numeric values found for field %q", field)
	}

	total := sumValues(values)
	mean := total / float64(len(values))
	out := map[string]interface{}{
		"average": mean,
		"count":   len(values),
		"sum":     total,
	}
	return json.Marshal(out)
}

// reducerConcatenate collects all output_data into a JSON array.
func reducerConcatenate(pairs []aggregatedWorkUnit) (json.RawMessage, error) {
	results := make([]json.RawMessage, 0, len(pairs))
	for _, p := range pairs {
		results = append(results, p.OutputData)
	}
	return json.Marshal(results)
}

// reducerMerge merges all output_data objects into one.
func reducerMerge(pairs []aggregatedWorkUnit, strategy string) (json.RawMessage, error) {
	if len(pairs) == 0 {
		return json.Marshal(map[string]interface{}{})
	}

	merged := make(map[string]interface{})
	for _, p := range pairs {
		var obj map[string]interface{}
		if err := json.Unmarshal(p.OutputData, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal output_data for merge: %w", err)
		}
		if strategy == "deep" {
			deepMerge(merged, obj)
		} else {
			shallowMerge(merged, obj)
		}
	}
	return json.Marshal(merged)
}

// extractAllNumeric extracts the numeric field from every pair.
func extractAllNumeric(pairs []aggregatedWorkUnit, field string) ([]float64, error) {
	values := make([]float64, 0, len(pairs))
	for i, p := range pairs {
		v, err := extractNumericField(p.OutputData, field)
		if err != nil {
			return nil, apierror.ValidationError(
				fmt.Sprintf("result %d: %v", i, err), nil,
			)
		}
		values = append(values, v)
	}
	return values, nil
}

// treeReduceSum performs tree reduction for summing large slices.
func treeReduceSum(values []float64) float64 {
	if len(values) <= treeReductionGroupSize {
		var s float64
		for _, v := range values {
			s += v
		}
		return s
	}

	var groupSums []float64
	for i := 0; i < len(values); i += treeReductionGroupSize {
		end := i + treeReductionGroupSize
		if end > len(values) {
			end = len(values)
		}
		var s float64
		for _, v := range values[i:end] {
			s += v
		}
		groupSums = append(groupSums, s)
	}
	return treeReduceSum(groupSums)
}

// shallowMerge copies src keys into dst (overwriting).
func shallowMerge(dst, src map[string]interface{}) {
	for k, v := range src {
		dst[k] = v
	}
}

// deepMerge recursively merges src into dst.
// - Maps: merged recursively.
// - Slices: concatenated.
// - Other: src overwrites.
func deepMerge(dst, src map[string]interface{}) {
	for k, sv := range src {
		dv, exists := dst[k]
		if !exists {
			dst[k] = sv
			continue
		}

		srcMap, srcIsMap := sv.(map[string]interface{})
		dstMap, dstIsMap := dv.(map[string]interface{})
		if srcIsMap && dstIsMap {
			deepMerge(dstMap, srcMap)
			continue
		}

		srcSlice, srcIsSlice := sv.([]interface{})
		dstSlice, dstIsSlice := dv.([]interface{})
		if srcIsSlice && dstIsSlice {
			dst[k] = append(dstSlice, srcSlice...)
			continue
		}

		dst[k] = sv
	}
}
