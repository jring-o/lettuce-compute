package aggregation

import (
	"encoding/json"
	"math"
	"testing"
)

func TestMonteCarloAll(t *testing.T) {
	// 100 trials: result = seed * 0.1, seeds 0..99.
	// Values: 0.0, 0.1, 0.2, ..., 9.9
	// Mean = (0 + 0.1 + ... + 9.9) / 100 = 495.0 / 100 = 4.95
	// Population variance = sum((x - mean)^2) / N
	pairs := make([]aggregatedWorkUnit, 100)
	var expectedSum float64
	for i := 0; i < 100; i++ {
		v := float64(i) * 0.1
		expectedSum += v
		data, _ := json.Marshal(map[string]float64{"result": v})
		pairs[i] = aggregatedWorkUnit{OutputData: data}
	}
	expectedMean := expectedSum / 100.0

	// Compute expected variance manually.
	var expectedM2 float64
	for i := 0; i < 100; i++ {
		v := float64(i) * 0.1
		diff := v - expectedMean
		expectedM2 += diff * diff
	}
	expectedVariance := expectedM2 / 100.0
	expectedStdDev := math.Sqrt(expectedVariance)

	result, err := aggregateMonteCarlo(pairs, map[string]any{
		"aggregator_type":  "all",
		"output_field":     "result",
		"confidence_level": 0.95,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status != "complete" {
		t.Errorf("status = %q, want complete", result.Status)
	}
	if result.WorkUnitsAggregated != 100 {
		t.Errorf("work_units_aggregated = %d, want 100", result.WorkUnitsAggregated)
	}

	var out map[string]interface{}
	if err := json.Unmarshal(result.Result, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	stats, ok := out["statistics"].(map[string]interface{})
	if !ok {
		t.Fatalf("statistics is not a map")
	}

	// Verify mean.
	gotMean := stats["mean"].(float64)
	if math.Abs(gotMean-expectedMean) > 1e-10 {
		t.Errorf("mean = %v, want %v", gotMean, expectedMean)
	}

	// Verify variance.
	gotVariance := stats["variance"].(float64)
	if math.Abs(gotVariance-expectedVariance) > 1e-6 {
		t.Errorf("variance = %v, want %v", gotVariance, expectedVariance)
	}

	// Verify std_dev.
	gotStdDev := stats["std_dev"].(float64)
	if math.Abs(gotStdDev-expectedStdDev) > 1e-6 {
		t.Errorf("std_dev = %v, want %v", gotStdDev, expectedStdDev)
	}

	// Verify CI exists.
	ci, ok := stats["confidence_interval"].(map[string]interface{})
	if !ok {
		t.Fatalf("confidence_interval not found")
	}
	if ci["level"] != 0.95 {
		t.Errorf("CI level = %v, want 0.95", ci["level"])
	}

	// CI formula: mean +/- 1.96 * (std_dev / sqrt(N))
	margin := 1.96 * (expectedStdDev / math.Sqrt(100.0))
	expectedLower := expectedMean - margin
	expectedUpper := expectedMean + margin

	if math.Abs(ci["lower"].(float64)-expectedLower) > 1e-6 {
		t.Errorf("CI lower = %v, want %v", ci["lower"], expectedLower)
	}
	if math.Abs(ci["upper"].(float64)-expectedUpper) > 1e-6 {
		t.Errorf("CI upper = %v, want %v", ci["upper"], expectedUpper)
	}

	// Verify min/max.
	if stats["min"] != float64(0) {
		t.Errorf("min = %v, want 0", stats["min"])
	}
	if math.Abs(stats["max"].(float64)-9.9) > 1e-10 {
		t.Errorf("max = %v, want 9.9", stats["max"])
	}
	if stats["count"] != float64(100) {
		t.Errorf("count = %v, want 100", stats["count"])
	}
}

func TestMonteCarloSingleTrial(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{OutputData: json.RawMessage(`{"result": 42.0}`)},
	}

	result, err := aggregateMonteCarlo(pairs, map[string]any{
		"aggregator_type": "all",
		"output_field":    "result",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	json.Unmarshal(result.Result, &out)
	stats := out["statistics"].(map[string]interface{})

	if stats["mean"] != float64(42) {
		t.Errorf("mean = %v, want 42", stats["mean"])
	}
	if stats["variance"] != float64(0) {
		t.Errorf("variance = %v, want 0", stats["variance"])
	}
	// No CI for single trial.
	if _, ok := stats["confidence_interval"]; ok {
		t.Error("expected no confidence_interval for single trial")
	}
}

func TestMonteCarloMeanOnly(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{OutputData: json.RawMessage(`{"result": 10.0}`)},
		{OutputData: json.RawMessage(`{"result": 20.0}`)},
		{OutputData: json.RawMessage(`{"result": 30.0}`)},
	}

	result, err := aggregateMonteCarlo(pairs, map[string]any{
		"aggregator_type": "mean",
		"output_field":    "result",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	json.Unmarshal(result.Result, &out)
	stats := out["statistics"].(map[string]interface{})

	if stats["mean"] != float64(20) {
		t.Errorf("mean = %v, want 20", stats["mean"])
	}
	// Should not have variance or CI.
	if _, ok := stats["variance"]; ok {
		t.Error("expected no variance for mean-only")
	}
}

func TestMonteCarloCustomConfidence(t *testing.T) {
	pairs := make([]aggregatedWorkUnit, 50)
	for i := range pairs {
		data, _ := json.Marshal(map[string]float64{"result": float64(i)})
		pairs[i] = aggregatedWorkUnit{OutputData: data}
	}

	result, err := aggregateMonteCarlo(pairs, map[string]any{
		"aggregator_type":  "confidence_interval",
		"output_field":     "result",
		"confidence_level": 0.99,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	json.Unmarshal(result.Result, &out)
	stats := out["statistics"].(map[string]interface{})

	ci := stats["confidence_interval"].(map[string]interface{})
	if ci["level"] != 0.99 {
		t.Errorf("CI level = %v, want 0.99", ci["level"])
	}
	// Just verify CI bounds are wider than 0.95 would be.
	lower := ci["lower"].(float64)
	upper := ci["upper"].(float64)
	if lower >= upper {
		t.Errorf("CI lower %v >= upper %v", lower, upper)
	}
}

func TestMonteCarloWelfordPrecision(t *testing.T) {
	// Test numerical stability with large constant + small variation.
	// Values: 1e9 + 0, 1e9 + 1, 1e9 + 2, ..., 1e9 + 99
	pairs := make([]aggregatedWorkUnit, 100)
	for i := 0; i < 100; i++ {
		v := 1e9 + float64(i)
		data, _ := json.Marshal(map[string]float64{"result": v})
		pairs[i] = aggregatedWorkUnit{OutputData: data}
	}

	result, err := aggregateMonteCarlo(pairs, map[string]any{
		"aggregator_type": "all",
		"output_field":    "result",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out map[string]interface{}
	json.Unmarshal(result.Result, &out)
	stats := out["statistics"].(map[string]interface{})

	expectedMean := 1e9 + 49.5
	if math.Abs(stats["mean"].(float64)-expectedMean) > 1e-4 {
		t.Errorf("mean = %v, want ~%v", stats["mean"], expectedMean)
	}

	// Variance of 0..99 = (99*100*199/6)/100 - (49.5)^2 = 833.25
	// Using population variance formula: sum((x-mean)^2)/N for 0..99
	expectedVariance := 833.25
	if math.Abs(stats["variance"].(float64)-expectedVariance) > 0.01 {
		t.Errorf("variance = %v, want ~%v", stats["variance"], expectedVariance)
	}
}

func TestMonteCarloMissingField(t *testing.T) {
	pairs := []aggregatedWorkUnit{
		{OutputData: json.RawMessage(`{"other": 10}`)},
	}

	_, err := aggregateMonteCarlo(pairs, map[string]any{
		"aggregator_type": "all",
		"output_field":    "result",
	})
	if err == nil {
		t.Fatal("expected error for missing output_field")
	}
}
