package aggregation

import (
	"encoding/json"
	"math"
	"testing"
)

func TestReducerSum(t *testing.T) {
	pairs := makePairs([]float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100})

	result, err := aggregateMapReduce(pairs, map[string]any{
		"reducer_type":  "sum",
		"reducer_field": "result",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	json.Unmarshal(result.Result, &out)

	if out["sum"] != float64(550) {
		t.Errorf("sum = %v, want 550", out["sum"])
	}
	if out["count"] != float64(10) {
		t.Errorf("count = %v, want 10", out["count"])
	}
}

func TestReducerAverage(t *testing.T) {
	pairs := makePairs([]float64{10, 20, 30, 40, 50})

	result, err := aggregateMapReduce(pairs, map[string]any{
		"reducer_type":  "average",
		"reducer_field": "result",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	json.Unmarshal(result.Result, &out)

	if out["average"] != float64(30) {
		t.Errorf("average = %v, want 30", out["average"])
	}
	if out["sum"] != float64(150) {
		t.Errorf("sum = %v, want 150", out["sum"])
	}
}

func TestReducerConcatenate(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{OutputData: json.RawMessage(`{"a":1}`)},
		{OutputData: json.RawMessage(`{"b":2}`)},
		{OutputData: json.RawMessage(`{"c":3}`)},
	}

	result, err := aggregateMapReduce(pairs, map[string]any{
		"reducer_type": "concatenate",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var arr []map[string]interface{}
	json.Unmarshal(result.Result, &arr)

	if len(arr) != 3 {
		t.Errorf("concatenated length = %d, want 3", len(arr))
	}
	if arr[0]["a"] != float64(1) {
		t.Errorf("first element a = %v, want 1", arr[0]["a"])
	}
}

func TestReducerMergeShallow(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{OutputData: json.RawMessage(`{"a":1,"b":2}`)},
		{OutputData: json.RawMessage(`{"b":3,"c":4}`)},
	}

	result, err := aggregateMapReduce(pairs, map[string]any{
		"reducer_type":   "merge",
		"merge_strategy": "shallow",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	json.Unmarshal(result.Result, &out)

	if out["a"] != float64(1) {
		t.Errorf("a = %v, want 1", out["a"])
	}
	if out["b"] != float64(3) { // shallow: later overwrites
		t.Errorf("b = %v, want 3 (shallow overwrite)", out["b"])
	}
	if out["c"] != float64(4) {
		t.Errorf("c = %v, want 4", out["c"])
	}
}

func TestReducerMergeDeep(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{OutputData: json.RawMessage(`{"nested":{"x":1},"items":[1,2]}`)},
		{OutputData: json.RawMessage(`{"nested":{"y":2},"items":[3,4]}`)},
	}

	result, err := aggregateMapReduce(pairs, map[string]any{
		"reducer_type":   "merge",
		"merge_strategy": "deep",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	json.Unmarshal(result.Result, &out)

	nested, ok := out["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("nested is not a map: %T", out["nested"])
	}
	if nested["x"] != float64(1) {
		t.Errorf("nested.x = %v, want 1", nested["x"])
	}
	if nested["y"] != float64(2) {
		t.Errorf("nested.y = %v, want 2", nested["y"])
	}

	items, ok := out["items"].([]interface{})
	if !ok {
		t.Fatalf("items is not a slice: %T", out["items"])
	}
	if len(items) != 4 {
		t.Errorf("items length = %d, want 4", len(items))
	}
}

func TestReducerSum_TreeReduction(t *testing.T) {
	// Create 1500 results to trigger tree reduction.
	values := make([]float64, 1500)
	var expected float64
	for i := range values {
		values[i] = float64(i + 1)
		expected += float64(i + 1)
	}
	pairs := makePairs(values)

	result, err := aggregateMapReduce(pairs, map[string]any{
		"reducer_type":  "sum",
		"reducer_field": "result",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	json.Unmarshal(result.Result, &out)

	if math.Abs(out["sum"].(float64)-expected) > 0.01 {
		t.Errorf("sum = %v, want %v", out["sum"], expected)
	}
	if out["count"] != float64(1500) {
		t.Errorf("count = %v, want 1500", out["count"])
	}
}

func TestReducerMissingField(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{OutputData: json.RawMessage(`{"value":10}`)},
	}

	_, err := aggregateMapReduce(pairs, map[string]any{
		"reducer_type":  "sum",
		"reducer_field": "missing_field",
	})
	if err == nil {
		t.Fatal("expected error for missing field")
	}
}

func TestReducerMissingType(t *testing.T) {
	_, err := aggregateMapReduce(nil, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing reducer_type")
	}
}

// makePairs creates pairs with output_data containing {"result": value}.
func makePairs(values []float64) []aggregatedWorkUnit {
	pairs := make([]aggregatedWorkUnit, len(values))
	for i, v := range values {
		data, _ := json.Marshal(map[string]float64{"result": v})
		pairs[i] = aggregatedWorkUnit{
			Parameters: json.RawMessage(`{}`),
			OutputData: data,
		}
	}
	return pairs
}
