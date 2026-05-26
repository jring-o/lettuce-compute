//go:build integration

package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/custom"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestF19_ParameterSweepE2E tests parameter sweep pattern end-to-end:
// create leaf → generate → volunteer runs → validate → aggregate.
func TestF19_ParameterSweepE2E(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create user.
	userID := f19CreateUser(t, env, ctx, "f19-ps")

	// Create leaf with PARAMETER_SWEEP.
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name:            "F19 Param Sweep E2E",
			Description:     "Param sweep aggregation test",
			ResearchArea:    []string{"testing"},
			TaskPattern:     leaf.PatternParameterSweep,
			IsOngoing:       false,
			Visibility:      leaf.VisibilityPublic,
			CreatorID:       &userID,
		},
		leaf.ValidationConfig{
			RedundancyFactor:   1,
			AgreementThreshold: 1.0,
			ComparisonMode:     "EXACT",
			MaxRetries:         3,
		},
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	// Update with aggregation config.
	dataCfg := leaf.DataConfig{
		TransferStrategy:   "INLINE",
		AggregationFormat:  "JSON",
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
		SplittingConfig: map[string]interface{}{
			"temperature": []interface{}{float64(100), float64(200), float64(300)},
			"pressure":    []interface{}{float64(1), float64(2)},
		},
	}
	resp := httpReq(t, "PUT", leafURL, leaf.UpdateLeafRequest{DataConfig: &dataCfg})
	requireStatus(t, resp, http.StatusOK, "update data config")
	resp.Body.Close()

	// Generate work units: 3 temperatures × 2 pressures = 6 work units.
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"temperature": []interface{}{float64(100), float64(200), float64(300)},
			"pressure":    []interface{}{float64(1), float64(2)},
		},
	}
	resp = httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate work units")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 6 {
		t.Fatalf("work_units_created = %d, want 6", genResp.WorkUnitsCreated)
	}

	// Register volunteer and process all 6 work units.
	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "F19 PS Volunteer")

	for i := 0; i < 6; i++ {
		wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID,
			PublicKey:   pubKey,
		})
		if err != nil {
			t.Fatalf("request work unit %d: %v", i, err)
		}

		// Compute a result based on the parameters.
		outputData := []byte(fmt.Sprintf(`{"result": %d}`, (i+1)*10))
		hash := sha256.Sum256(outputData)
		checksum := hex.EncodeToString(hash[:])

		_, err = env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuResp.WorkUnitId,
			VolunteerId:          volID,
			PublicKey:            pubKey,
			OutputData:           outputData,
			OutputChecksumSha256: checksum,
			Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 10, CpuSecondsUser: 5, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("submit result %d: %v", i, err)
		}
	}

	// Wait for validation to complete.
	time.Sleep(500 * time.Millisecond)

	// Aggregate.
	resp = httpReq(t, "POST", leafURL+"/aggregate", nil)
	requireStatus(t, resp, http.StatusOK, "aggregate")
	var aggResp struct {
		Data struct {
			Status              string          `json:"status"`
			Format              string          `json:"format"`
			Result              json.RawMessage `json:"result"`
			WorkUnitsAggregated int             `json:"work_units_aggregated"`
			WorkUnitsTotal      int             `json:"work_units_total"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &aggResp)

	if aggResp.Data.WorkUnitsAggregated < 1 {
		t.Errorf("work_units_aggregated = %d, want > 0", aggResp.Data.WorkUnitsAggregated)
	}
	if aggResp.Data.Format != "json" {
		t.Errorf("format = %q, want json", aggResp.Data.Format)
	}

	// Verify the result is a JSON array.
	var rows []json.RawMessage
	if err := json.Unmarshal(aggResp.Data.Result, &rows); err != nil {
		t.Fatalf("result is not a JSON array: %v", err)
	}
	if len(rows) != aggResp.Data.WorkUnitsAggregated {
		t.Errorf("rows = %d, want %d", len(rows), aggResp.Data.WorkUnitsAggregated)
	}

	// GET should return cached result.
	resp = httpReq(t, "GET", leafURL+"/aggregate", nil)
	requireStatus(t, resp, http.StatusOK, "get cached aggregate")
	resp.Body.Close()
}

// TestF19_MapReduceE2E tests map-reduce pattern end-to-end with sum reducer.
func TestF19_MapReduceE2E(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := f19CreateUser(t, env, ctx, "f19-mr")

	// Build 3 JSON records as input data. Each record = 1 chunk.
	inputRecords := `{"chunk":1,"values":[1,2,3,4,5,6,7,8,9,10]}
{"chunk":2,"values":[11,12,13,14,15,16,17,18,19,20]}
{"chunk":3,"values":[21,22,23,24,25,26,27,28,29,30]}
`

	byRecord := "by_record"

	// Create MAP_REDUCE leaf.
	createReq := leaf.CreateLeafRequest{
		Name:            "F19 MapReduce E2E",
		Description:     "Map-reduce aggregation test",
		ResearchArea:    []string{"testing"},
		TaskPattern:     leaf.PatternMapReduce,
		IsOngoing:       false,
		Visibility:      leaf.VisibilityPublic,
		CreatorID:       &userID,
	}
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs", createReq)
	requireStatus(t, resp, http.StatusCreated, "create leaf")
	var proj leaf.Leaf
	decodeJSON(t, resp, &proj)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	resp = httpReq(t, "POST", leafURL+"/configure", nil)
	requireStatus(t, resp, http.StatusOK, "configure")
	resp.Body.Close()

	execCfg := leaf.ExecutionConfig{
		Runtime: "NATIVE", Binaries: map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		MaxMemoryMB: 4096, MaxDiskMB: 10240, MaxCPUSeconds: 3600,
	}
	valCfg := leaf.ValidationConfig{
		RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3,
	}
	ftCfg := leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds: 60, MissedHeartbeatsThreshold: 3, DeadlineMultiplier: 3.0, MaxReassignments: 3,
	}
	dataCfg := leaf.DataConfig{
		TransferStrategy:   "INLINE",
		AggregationFormat:  "JSON",
		SplittingStrategy:  &byRecord,
		SplittingConfig:    map[string]interface{}{"records_per_chunk": float64(1)},
		AggregationConfig:  map[string]any{"reducer_type": "sum", "reducer_field": "result"},
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
	}
	creditCfg := leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0}
	resp = httpReq(t, "PUT", leafURL, leaf.UpdateLeafRequest{
		ExecutionConfig: &execCfg, ValidationConfig: &valCfg,
		FaultToleranceConfig: &ftCfg, DataConfig: &dataCfg, CreditConfig: &creditCfg,
	})
	requireStatus(t, resp, http.StatusOK, "update configs")
	resp.Body.Close()

	resp = httpReq(t, "POST", leafURL+"/activate", nil)
	requireStatus(t, resp, http.StatusOK, "activate")
	resp.Body.Close()

	// Generate work units: 3 records, 1 per chunk = 3 work units.
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"input_data": inputRecords,
		},
	}
	resp = httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 3 {
		t.Fatalf("work_units_created = %d, want 3", genResp.WorkUnitsCreated)
	}

	// Volunteer processes all chunks.
	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "F19 MR Volunteer")

	chunkSums := []int{10, 10, 10} // Each chunk has 10 lines.
	for i := 0; i < 3; i++ {
		wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID, PublicKey: pubKey,
		})
		if err != nil {
			t.Fatalf("request work unit %d: %v", i, err)
		}

		outputData := []byte(fmt.Sprintf(`{"result": %d}`, chunkSums[i]))
		hash := sha256.Sum256(outputData)
		checksum := hex.EncodeToString(hash[:])

		_, err = env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuResp.WorkUnitId, VolunteerId: volID, PublicKey: pubKey,
			OutputData: outputData, OutputChecksumSha256: checksum,
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 5, CpuSecondsUser: 3, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("submit result %d: %v", i, err)
		}
	}

	time.Sleep(500 * time.Millisecond)

	// Aggregate with sum reducer.
	resp = httpReq(t, "POST", leafURL+"/aggregate", nil)
	requireStatus(t, resp, http.StatusOK, "aggregate")
	var aggResp struct {
		Data struct {
			Status              string          `json:"status"`
			Result              json.RawMessage `json:"result"`
			WorkUnitsAggregated int             `json:"work_units_aggregated"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &aggResp)

	if aggResp.Data.WorkUnitsAggregated < 1 {
		t.Errorf("work_units_aggregated = %d, want > 0", aggResp.Data.WorkUnitsAggregated)
	}

	var sumResult map[string]interface{}
	if err := json.Unmarshal(aggResp.Data.Result, &sumResult); err != nil {
		t.Fatalf("unmarshal sum result: %v", err)
	}
	if sumResult["sum"] != float64(30) {
		t.Errorf("sum = %v, want 30", sumResult["sum"])
	}
}

// TestF19_MonteCarloE2E tests Monte Carlo pattern end-to-end with statistical aggregation.
func TestF19_MonteCarloE2E(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := f19CreateUser(t, env, ctx, "f19-mc")

	createReq := leaf.CreateLeafRequest{
		Name:            "F19 Monte Carlo E2E",
		Description:     "Monte Carlo aggregation test",
		ResearchArea:    []string{"testing"},
		TaskPattern:     leaf.PatternMonteCarlo,
		IsOngoing:       false,
		Visibility:      leaf.VisibilityPublic,
		CreatorID:       &userID,
	}
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs", createReq)
	requireStatus(t, resp, http.StatusCreated, "create leaf")
	var proj leaf.Leaf
	decodeJSON(t, resp, &proj)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	resp = httpReq(t, "POST", leafURL+"/configure", nil)
	requireStatus(t, resp, http.StatusOK, "configure")
	resp.Body.Close()

	execCfg := leaf.ExecutionConfig{
		Runtime: "NATIVE", Binaries: map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		MaxMemoryMB: 4096, MaxDiskMB: 10240, MaxCPUSeconds: 3600,
	}
	valCfg := leaf.ValidationConfig{
		RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "NUMERIC_TOLERANCE",
		NumericTolerance: floatPtr(0.001), MaxRetries: 3,
	}
	ftCfg := leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds: 60, MissedHeartbeatsThreshold: 3, DeadlineMultiplier: 3.0, MaxReassignments: 3,
	}
	dataCfg := leaf.DataConfig{
		TransferStrategy:   "INLINE",
		AggregationFormat:  "JSON",
		AggregationConfig: map[string]any{
			"aggregator_type":  "all",
			"output_field":     "result",
			"confidence_level": 0.95,
		},
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
	}
	creditCfg := leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0}
	resp = httpReq(t, "PUT", leafURL, leaf.UpdateLeafRequest{
		ExecutionConfig: &execCfg, ValidationConfig: &valCfg,
		FaultToleranceConfig: &ftCfg, DataConfig: &dataCfg, CreditConfig: &creditCfg,
	})
	requireStatus(t, resp, http.StatusOK, "update configs")
	resp.Body.Close()

	resp = httpReq(t, "POST", leafURL+"/activate", nil)
	requireStatus(t, resp, http.StatusOK, "activate")
	resp.Body.Close()

	// Generate 20 trials (keeping small for test speed).
	numTrials := 20
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"num_trials": float64(numTrials),
		},
	}
	resp = httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	if genResp.WorkUnitsCreated != numTrials {
		t.Fatalf("work_units_created = %d, want %d", genResp.WorkUnitsCreated, numTrials)
	}

	// Volunteer processes all trials. Each returns result = seed * 0.1.
	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "F19 MC Volunteer")

	// Compute expected results: values will be determined by whatever seed the WU has.
	var values []float64
	for i := 0; i < numTrials; i++ {
		wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID, PublicKey: pubKey,
		})
		if err != nil {
			t.Fatalf("request work unit %d: %v", i, err)
		}

		// Extract seed from parameters.
		var params struct {
			Seed int64 `json:"seed"`
		}
		if wuResp.ParametersJson != "" {
			json.Unmarshal([]byte(wuResp.ParametersJson), &params)
		}

		result := float64(params.Seed) * 0.1
		values = append(values, result)
		outputData := []byte(fmt.Sprintf(`{"result": %.1f}`, result))
		hash := sha256.Sum256(outputData)
		checksum := hex.EncodeToString(hash[:])

		_, err = env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuResp.WorkUnitId, VolunteerId: volID, PublicKey: pubKey,
			OutputData: outputData, OutputChecksumSha256: checksum,
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("submit result %d: %v", i, err)
		}
	}

	time.Sleep(500 * time.Millisecond)

	// Compute expected stats.
	var expectedMean float64
	for _, v := range values {
		expectedMean += v
	}
	expectedMean /= float64(len(values))

	var expectedM2 float64
	for _, v := range values {
		d := v - expectedMean
		expectedM2 += d * d
	}
	expectedVariance := expectedM2 / float64(len(values))

	// Aggregate.
	resp = httpReq(t, "POST", leafURL+"/aggregate", nil)
	requireStatus(t, resp, http.StatusOK, "aggregate")
	var aggResp struct {
		Data struct {
			Status              string          `json:"status"`
			Result              json.RawMessage `json:"result"`
			WorkUnitsAggregated int             `json:"work_units_aggregated"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &aggResp)

	if aggResp.Data.WorkUnitsAggregated < 1 {
		t.Errorf("work_units_aggregated = %d, want > 0", aggResp.Data.WorkUnitsAggregated)
	}

	var mcResult struct {
		Statistics struct {
			Mean               float64 `json:"mean"`
			Variance           float64 `json:"variance"`
			StdDev             float64 `json:"std_dev"`
			Count              int     `json:"count"`
			ConfidenceInterval *struct {
				Level float64 `json:"level"`
				Lower float64 `json:"lower"`
				Upper float64 `json:"upper"`
			} `json:"confidence_interval"`
		} `json:"statistics"`
	}
	if err := json.Unmarshal(aggResp.Data.Result, &mcResult); err != nil {
		t.Fatalf("unmarshal MC result: %v", err)
	}

	// Verify mean (with tolerance for float formatting).
	if math.Abs(mcResult.Statistics.Mean-expectedMean) > 0.5 {
		t.Errorf("mean = %v, expected ~%v", mcResult.Statistics.Mean, expectedMean)
	}

	// Verify variance exists.
	if mcResult.Statistics.Variance < 0 {
		t.Errorf("variance = %v, expected >= 0", mcResult.Statistics.Variance)
	}

	// Verify CI exists for > 1 trial.
	if mcResult.Statistics.ConfidenceInterval == nil {
		t.Error("expected confidence_interval for multi-trial aggregation")
	} else if mcResult.Statistics.ConfidenceInterval.Level != 0.95 {
		t.Errorf("CI level = %v, want 0.95", mcResult.Statistics.ConfidenceInterval.Level)
	}

	_ = expectedVariance
}

// TestF19_CustomE2E tests custom pattern end-to-end with concatenate reducer.
func TestF19_CustomE2E(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := f19CreateUser(t, env, ctx, "f19-cu")

	createReq := leaf.CreateLeafRequest{
		Name:            "F19 Custom E2E",
		Description:     "Custom aggregation test",
		ResearchArea:    []string{"testing"},
		TaskPattern:     leaf.PatternCustom,
		IsOngoing:       false,
		Visibility:      leaf.VisibilityPublic,
		CreatorID:       &userID,
	}
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs", createReq)
	requireStatus(t, resp, http.StatusCreated, "create leaf")
	var proj leaf.Leaf
	decodeJSON(t, resp, &proj)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	resp = httpReq(t, "POST", leafURL+"/configure", nil)
	requireStatus(t, resp, http.StatusOK, "configure")
	resp.Body.Close()

	execCfg := leaf.ExecutionConfig{
		Runtime: "NATIVE", Binaries: map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		MaxMemoryMB: 4096, MaxDiskMB: 10240, MaxCPUSeconds: 3600,
	}
	valCfg := leaf.ValidationConfig{
		RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3,
	}
	ftCfg := leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds: 60, MissedHeartbeatsThreshold: 3, DeadlineMultiplier: 3.0, MaxReassignments: 3,
	}
	dataCfg := leaf.DataConfig{
		TransferStrategy:   "INLINE",
		AggregationFormat:  "JSON",
		AggregationConfig:  map[string]any{"reducer_type": "concatenate"},
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
	}
	creditCfg := leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0}
	resp = httpReq(t, "PUT", leafURL, leaf.UpdateLeafRequest{
		ExecutionConfig: &execCfg, ValidationConfig: &valCfg,
		FaultToleranceConfig: &ftCfg, DataConfig: &dataCfg, CreditConfig: &creditCfg,
	})
	requireStatus(t, resp, http.StatusOK, "update configs")
	resp.Body.Close()

	resp = httpReq(t, "POST", leafURL+"/activate", nil)
	requireStatus(t, resp, http.StatusOK, "activate")
	resp.Body.Close()

	// Bulk upload 5 custom work units.
	bulkReq := custom.BulkUploadRequest{
		WorkUnits: []custom.WorkUnitInput{
			{InputData: json.RawMessage(`{"task":"A"}`), Parameters: json.RawMessage(`{"index":0}`)},
			{InputData: json.RawMessage(`{"task":"B"}`), Parameters: json.RawMessage(`{"index":1}`)},
			{InputData: json.RawMessage(`{"task":"C"}`), Parameters: json.RawMessage(`{"index":2}`)},
			{InputData: json.RawMessage(`{"task":"D"}`), Parameters: json.RawMessage(`{"index":3}`)},
			{InputData: json.RawMessage(`{"task":"E"}`), Parameters: json.RawMessage(`{"index":4}`)},
		},
	}
	resp = httpReq(t, "POST", leafURL+"/work-units/bulk", bulkReq)
	requireStatus(t, resp, http.StatusCreated, "bulk upload")
	var bulkResp custom.BulkUploadResponse
	decodeJSON(t, resp, &bulkResp)
	if bulkResp.WorkUnitsCreated != 5 {
		t.Fatalf("work_units_created = %d, want 5", bulkResp.WorkUnitsCreated)
	}

	// Volunteer processes all 5 work units.
	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "F19 Custom Volunteer")

	for i := 0; i < 5; i++ {
		wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID, PublicKey: pubKey,
		})
		if err != nil {
			t.Fatalf("request work unit %d: %v", i, err)
		}

		outputData := []byte(fmt.Sprintf(`{"output":"result_%d"}`, i))
		hash := sha256.Sum256(outputData)
		checksum := hex.EncodeToString(hash[:])

		_, err = env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuResp.WorkUnitId, VolunteerId: volID, PublicKey: pubKey,
			OutputData: outputData, OutputChecksumSha256: checksum,
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 2, CpuSecondsUser: 1, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("submit result %d: %v", i, err)
		}
	}

	time.Sleep(500 * time.Millisecond)

	// Aggregate with concatenate reducer.
	resp = httpReq(t, "POST", leafURL+"/aggregate", nil)
	requireStatus(t, resp, http.StatusOK, "aggregate")
	var aggResp struct {
		Data struct {
			Status              string          `json:"status"`
			Result              json.RawMessage `json:"result"`
			WorkUnitsAggregated int             `json:"work_units_aggregated"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &aggResp)

	if aggResp.Data.WorkUnitsAggregated < 1 {
		t.Errorf("work_units_aggregated = %d, want > 0", aggResp.Data.WorkUnitsAggregated)
	}

	// Verify concatenated result is a JSON array.
	var arr []json.RawMessage
	if err := json.Unmarshal(aggResp.Data.Result, &arr); err != nil {
		t.Fatalf("result is not a JSON array: %v", err)
	}
	if len(arr) != aggResp.Data.WorkUnitsAggregated {
		t.Errorf("concatenated array length = %d, want %d", len(arr), aggResp.Data.WorkUnitsAggregated)
	}
}

// --- helpers ---

func f19CreateUser(t *testing.T, env *testEnv, ctx context.Context, prefix string) types.ID {
	t.Helper()
	return createTestUser(t, env.pool, ctx, prefix)
}

func floatPtr(f float64) *float64 { return &f }
