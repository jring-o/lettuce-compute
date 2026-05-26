package aggregation

import (
	"encoding/json"
	"testing"
)

func TestAggregateCustom_NoAggregation(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{OutputData: json.RawMessage(`{"a":1}`)},
		{OutputData: json.RawMessage(`{"b":2}`)},
	}

	result, err := aggregateCustom(pairs, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status != "no_aggregation" {
		t.Errorf("status = %q, want no_aggregation", result.Status)
	}
	if result.Message == "" {
		t.Error("expected non-empty message for no_aggregation")
	}
	if result.WorkUnitsValidated != 2 {
		t.Errorf("work_units_validated = %d, want 2", result.WorkUnitsValidated)
	}
}

func TestAggregateCustom_NoAggregation_NilConfig(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{OutputData: json.RawMessage(`{"a":1}`)},
	}

	result, err := aggregateCustom(pairs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status != "no_aggregation" {
		t.Errorf("status = %q, want no_aggregation", result.Status)
	}
}

func TestAggregateCustom_WithConcatenateReducer(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{OutputData: json.RawMessage(`{"x":1}`)},
		{OutputData: json.RawMessage(`{"x":2}`)},
		{OutputData: json.RawMessage(`{"x":3}`)},
	}

	result, err := aggregateCustom(pairs, map[string]any{
		"reducer_type": "concatenate",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status != "complete" {
		t.Errorf("status = %q, want complete", result.Status)
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(result.Result, &arr); err != nil {
		t.Fatalf("result is not a JSON array: %v", err)
	}
	if len(arr) != 3 {
		t.Errorf("concatenated length = %d, want 3", len(arr))
	}
}

func TestAggregateCustom_WithSumReducer(t *testing.T) {
	pairs := makePairs([]float64{5, 10, 15})

	result, err := aggregateCustom(pairs, map[string]any{
		"reducer_type":  "sum",
		"reducer_field": "result",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	json.Unmarshal(result.Result, &out)
	if out["sum"] != float64(30) {
		t.Errorf("sum = %v, want 30", out["sum"])
	}
}
