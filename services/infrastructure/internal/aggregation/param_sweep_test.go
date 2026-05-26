package aggregation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAggregateParamSweep_JSONOutput(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{Parameters: json.RawMessage(`{"temperature":100,"pressure":1.0}`), OutputData: json.RawMessage(`{"result":42.5}`)},
		{Parameters: json.RawMessage(`{"temperature":200,"pressure":1.0}`), OutputData: json.RawMessage(`{"result":85.0}`)},
		{Parameters: json.RawMessage(`{"temperature":100,"pressure":2.0}`), OutputData: json.RawMessage(`{"result":45.0}`)},
		{Parameters: json.RawMessage(`{"temperature":200,"pressure":2.0}`), OutputData: json.RawMessage(`{"result":90.0}`)},
		{Parameters: json.RawMessage(`{"temperature":300,"pressure":1.0}`), OutputData: json.RawMessage(`{"result":127.5}`)},
	}

	result, err := aggregateParamSweep(pairs, "json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status != "complete" {
		t.Errorf("status = %q, want complete", result.Status)
	}
	if result.WorkUnitsAggregated != 5 {
		t.Errorf("work_units_aggregated = %d, want 5", result.WorkUnitsAggregated)
	}
	if result.Format != "json" {
		t.Errorf("format = %q, want json", result.Format)
	}

	var rows []paramSweepRow
	if err := json.Unmarshal(result.Result, &rows); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("rows = %d, want 5", len(rows))
	}

	// Verify first row has parameters and result.
	var params map[string]interface{}
	if err := json.Unmarshal(rows[0].Parameters, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params["temperature"] != float64(100) {
		t.Errorf("temperature = %v, want 100", params["temperature"])
	}
}

func TestAggregateParamSweep_CSVOutput(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{Parameters: json.RawMessage(`{"pressure":1.0,"temperature":100}`), OutputData: json.RawMessage(`{"result":42.5}`)},
		{Parameters: json.RawMessage(`{"pressure":2.0,"temperature":200}`), OutputData: json.RawMessage(`{"result":85.0}`)},
	}

	result, err := aggregateParamSweep(pairs, "csv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Format != "csv" {
		t.Errorf("format = %q, want csv", result.Format)
	}

	lines := strings.Split(strings.TrimSpace(result.ResultCSV), "\n")
	if len(lines) != 3 { // header + 2 data rows
		t.Fatalf("lines = %d, want 3: %q", len(lines), result.ResultCSV)
	}

	// Header should be sorted alphabetically.
	if lines[0] != "pressure,temperature,result" {
		t.Errorf("header = %q, want pressure,temperature,result", lines[0])
	}

	// First data row.
	if lines[1] != "1,100,42.5" {
		t.Errorf("row 1 = %q, want 1,100,42.5", lines[1])
	}
}

func TestAggregateParamSweep_EmptyPairs(t *testing.T) {
	result, err := aggregateParamSweep([]aggregatedWorkUnit{}, "json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkUnitsAggregated != 0 {
		t.Errorf("work_units_aggregated = %d, want 0", result.WorkUnitsAggregated)
	}
}
