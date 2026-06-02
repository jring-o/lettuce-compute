//go:build integration

package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/custom"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/mapreduce"
	"github.com/lettuce-compute/infrastructure/internal/montecarlo"
	"github.com/lettuce-compute/infrastructure/internal/paramsweep"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestV08_Scenario1_MapReduceFullLifecycle tests map-reduce end-to-end:
// create leaf → generate from 50-line dataset → volunteer runs → validate → aggregate sum.
func TestV08_Scenario1_MapReduceFullLifecycle(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v08-mr")

	// Build a 50-line dataset. Each line is a JSON record with a value.
	var inputData string
	for i := 1; i <= 50; i++ {
		inputData += fmt.Sprintf(`{"line":%d,"value":%d}`, i, 1) + "\n"
	}

	// by_record (not by_line_count) so each chunk is a JSON array of records that
	// inserts cleanly into JSONB; by_line_count would group raw NDJSON lines into a
	// non-JSON blob and fail the bulk insert (same workaround as the beta suite).
	byRecord := "by_record"
	createReq := leaf.CreateLeafRequest{
		Name:            "V08 Map-Reduce Lifecycle",
		Description:     "Map-reduce with sum reducer on 50-line dataset",
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

	alpineImage := "alpine:latest"
	execCfg := leaf.ExecutionConfig{
		Runtime: "CONTAINER", Image: &alpineImage,
		MaxMemoryMB: 4096, MaxDiskMB: 10240, MaxCPUSeconds: 3600,
	}
	valCfg := leaf.ValidationConfig{
		RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3,
	}
	ftCfg := leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds: 60, MissedHeartbeatsThreshold: 3, DeadlineMultiplier: 3.0, MaxReassignments: 3,
	}
	dataCfg := leaf.DataConfig{
		TransferStrategy:  "INLINE",
		AggregationFormat: "JSON",
		SplittingStrategy: &byRecord,
		SplittingConfig:   map[string]interface{}{"records_per_chunk": float64(10)},
		AggregationConfig: map[string]any{"reducer_type": "sum", "reducer_field": "count"},
		MaxInputSizeBytes: 1048576, MaxOutputSizeBytes: 104857600,
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

	// Generate work units: 50 lines / 10 lines per chunk = 5 work units.
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"input_data": inputData,
		},
	}
	resp = httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 5 {
		t.Fatalf("work_units_created = %d, want 5", genResp.WorkUnitsCreated)
	}

	// Register volunteer.
	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "V08 MR Volunteer")

	// Process all 5 chunks. Each chunk has 10 lines, return count=10.
	for i := 0; i < 5; i++ {
		wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID, PublicKey: pubKey,
		})
		if err != nil {
			t.Fatalf("request work unit %d: %v", i, err)
		}

		// Each chunk returns count of lines processed (10).
		outputData := []byte(`{"count": 10}`)
		hash := sha256.Sum256(outputData)
		checksum := hex.EncodeToString(hash[:])

		ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuResp.Assignments[0].WorkUnitId)
		_, err = env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuResp.Assignments[0].WorkUnitId, VolunteerId: volID, PublicKey: pubKey,
			OutputData: outputData, OutputChecksumSha256: checksum,
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 5, CpuSecondsUser: 3, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("submit result %d: %v", i, err)
		}
	}

	time.Sleep(500 * time.Millisecond)

	// Aggregate → verify sum equals 50 (5 chunks × 10).
	resp = httpReq(t, "POST", leafURL+"/aggregate", nil)
	requireStatus(t, resp, http.StatusOK, "aggregate")
	var aggResp struct {
		Data struct {
			Status              string          `json:"status"`
			Result              json.RawMessage `json:"result"`
			WorkUnitsAggregated int             `json:"work_units_aggregated"`
			WorkUnitsTotal      int             `json:"work_units_total"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &aggResp)

	if aggResp.Data.WorkUnitsAggregated != 5 {
		t.Errorf("work_units_aggregated = %d, want 5", aggResp.Data.WorkUnitsAggregated)
	}

	var sumResult map[string]interface{}
	if err := json.Unmarshal(aggResp.Data.Result, &sumResult); err != nil {
		t.Fatalf("unmarshal sum result: %v", err)
	}
	if sumResult["sum"] != float64(50) {
		t.Errorf("sum = %v, want 50", sumResult["sum"])
	}
}

// TestV08_Scenario2_MonteCarloStatistics tests Monte Carlo with statistical aggregation.
func TestV08_Scenario2_MonteCarloStatistics(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v08-mc")

	createReq := leaf.CreateLeafRequest{
		Name:            "V08 Monte Carlo Statistics",
		Description:     "Monte Carlo with full statistical aggregation",
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
		NumericTolerance: floatPtr(0.01), MaxRetries: 3,
	}
	ftCfg := leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds: 60, MissedHeartbeatsThreshold: 3, DeadlineMultiplier: 3.0, MaxReassignments: 3,
	}
	dataCfg := leaf.DataConfig{
		TransferStrategy:  "INLINE",
		AggregationFormat: "JSON",
		AggregationConfig: map[string]any{
			"aggregator_type":  "all",
			"output_field":     "result",
			"confidence_level": 0.95,
		},
		MaxInputSizeBytes: 1048576, MaxOutputSizeBytes: 104857600,
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

	// Generate 200 trials (kept smaller for CI speed).
	numTrials := 50
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

	// Volunteer processes all trials. Result = seed * 0.1.
	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "V08 MC Volunteer")

	var values []float64
	for i := 0; i < numTrials; i++ {
		wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID, PublicKey: pubKey,
		})
		if err != nil {
			t.Fatalf("request work unit %d: %v", i, err)
		}

		var params struct {
			Seed int64 `json:"seed"`
		}
		if wuResp.Assignments[0].ParametersJson != "" {
			json.Unmarshal([]byte(wuResp.Assignments[0].ParametersJson), &params)
		}

		result := float64(params.Seed) * 0.1
		values = append(values, result)
		outputData := []byte(fmt.Sprintf(`{"result": %.1f}`, result))
		hash := sha256.Sum256(outputData)
		checksum := hex.EncodeToString(hash[:])

		ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuResp.Assignments[0].WorkUnitId)
		_, err = env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuResp.Assignments[0].WorkUnitId, VolunteerId: volID, PublicKey: pubKey,
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

	// Verify mean.
	if math.Abs(mcResult.Statistics.Mean-expectedMean) > 0.5 {
		t.Errorf("mean = %v, expected ~%v", mcResult.Statistics.Mean, expectedMean)
	}

	// Verify variance matches expected (computed from same values).
	if math.Abs(mcResult.Statistics.Variance-expectedVariance) > 0.5 {
		t.Errorf("variance = %v, expected ~%v", mcResult.Statistics.Variance, expectedVariance)
	}

	// Verify CI exists.
	if mcResult.Statistics.ConfidenceInterval == nil {
		t.Error("expected confidence_interval")
	} else if mcResult.Statistics.ConfidenceInterval.Level != 0.95 {
		t.Errorf("CI level = %v, want 0.95", mcResult.Statistics.ConfidenceInterval.Level)
	}
}

// TestV08_Scenario3_CustomBulkUpload tests custom pattern with bulk upload and concatenate reducer.
func TestV08_Scenario3_CustomBulkUpload(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v08-cu")

	createReq := leaf.CreateLeafRequest{
		Name:            "V08 Custom Bulk Upload",
		Description:     "Custom pattern with 10 bulk work units",
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

	// Bulk upload 10 custom work units.
	var wuInputs []custom.WorkUnitInput
	for i := 0; i < 10; i++ {
		wuInputs = append(wuInputs, custom.WorkUnitInput{
			InputData:  json.RawMessage(fmt.Sprintf(`{"task":%d}`, i)),
			Parameters: json.RawMessage(fmt.Sprintf(`{"index":%d}`, i)),
		})
	}
	bulkReq := custom.BulkUploadRequest{WorkUnits: wuInputs}
	resp = httpReq(t, "POST", leafURL+"/work-units/bulk", bulkReq)
	requireStatus(t, resp, http.StatusCreated, "bulk upload")
	var bulkResp custom.BulkUploadResponse
	decodeJSON(t, resp, &bulkResp)
	if bulkResp.WorkUnitsCreated != 10 {
		t.Fatalf("work_units_created = %d, want 10", bulkResp.WorkUnitsCreated)
	}

	// Verify /work-units/generate returns 400 for CUSTOM pattern.
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{"x": float64(1)},
	}
	resp = httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		t.Error("expected /generate to reject CUSTOM pattern, but got success")
	} else {
		resp.Body.Close()
		t.Logf("CUSTOM /generate correctly rejected with status %d", resp.StatusCode)
	}

	// Volunteer processes all 10 work units.
	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "V08 Custom Volunteer")

	for i := 0; i < 10; i++ {
		wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID, PublicKey: pubKey,
		})
		if err != nil {
			t.Fatalf("request work unit %d: %v", i, err)
		}

		outputData := []byte(fmt.Sprintf(`{"output":"result_%d"}`, i))
		hash := sha256.Sum256(outputData)
		checksum := hex.EncodeToString(hash[:])

		ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuResp.Assignments[0].WorkUnitId)
		_, err = env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuResp.Assignments[0].WorkUnitId, VolunteerId: volID, PublicKey: pubKey,
			OutputData: outputData, OutputChecksumSha256: checksum,
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 2, CpuSecondsUser: 1, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("submit result %d: %v", i, err)
		}
	}

	time.Sleep(500 * time.Millisecond)

	// Aggregate → verify concatenated output contains all 10 results.
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

	if aggResp.Data.WorkUnitsAggregated != 10 {
		t.Errorf("work_units_aggregated = %d, want 10", aggResp.Data.WorkUnitsAggregated)
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(aggResp.Data.Result, &arr); err != nil {
		t.Fatalf("result is not a JSON array: %v", err)
	}
	if len(arr) != 10 {
		t.Errorf("concatenated array length = %d, want 10", len(arr))
	}
}

// TestV08_Scenario4_ExternalStorageReference tests external storage reference flow.
func TestV08_Scenario4_ExternalStorageReference(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v08-ext")
	// External storage reference is a map-reduce data concern (input_data_ref is fed
	// to the map-reduce splitter); a PARAMETER_SWEEP leaf would treat input_data_ref
	// as a sweep parameter and reject it. Use MAP_REDUCE, mirroring the F20 suite.
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name:            "V08 External Storage",
			Description:     "External storage reference test",
			ResearchArea:    []string{"testing"},
			TaskPattern:     leaf.PatternMapReduce,
			IsOngoing:       false,
			Visibility:      leaf.VisibilityPublic,
			CreatorID:       &userID,
		},
		leaf.ValidationConfig{
			RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3,
		},
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	// Update to EXTERNAL_REFERENCE. map_reduce requires a splitting_strategy.
	extURL := "https://storage.example.com/v08-datasets"
	byRecord := "by_record"
	dataCfg := leaf.DataConfig{
		TransferStrategy:   "EXTERNAL_REFERENCE",
		ExternalBaseURL:    &extURL,
		AggregationFormat:  "JSON",
		MaxInputSizeBytes:  104857600,
		MaxOutputSizeBytes: 104857600,
		SplittingStrategy:  &byRecord,
		SplittingConfig:    map[string]interface{}{"records_per_chunk": float64(10)},
	}
	resp := httpReq(t, "PUT", leafURL, leaf.UpdateLeafRequest{DataConfig: &dataCfg})
	requireStatus(t, resp, http.StatusOK, "update to EXTERNAL_REFERENCE")
	resp.Body.Close()

	// Generate work units referencing external input data.
	inputRef := "https://storage.example.com/v08-datasets/input.csv"
	genReq := workunit.GenerateRequest{
		InputDataRef: &inputRef,
	}
	resp = httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate with external ref")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	if genResp.WorkUnitsCreated < 1 {
		t.Fatalf("work_units_created = %d, want >= 1", genResp.WorkUnitsCreated)
	}

	// Register volunteer.
	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "V08 External Volunteer")

	// Request work unit and verify input_data_url is populated.
	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volID, PublicKey: pubKey,
	})
	if err != nil {
		t.Fatalf("request work unit: %v", err)
	}
	if wuResp.Assignments[0].InputDataUrl == "" {
		t.Error("expected input_data_url for external reference leaf")
	}

	// Submit result with output_data_url.
	outputData := []byte(`{"result": "external_computed"}`)
	hash := sha256.Sum256(outputData)
	checksum := hex.EncodeToString(hash[:])

	ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuResp.Assignments[0].WorkUnitId)
	submitResp, err := env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuResp.Assignments[0].WorkUnitId,
		VolunteerId:          volID,
		PublicKey:            pubKey,
		OutputDataUrl:        "https://storage.example.com/results/v08-wu-" + wuResp.Assignments[0].WorkUnitId + ".json",
		OutputChecksumSha256: checksum,
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 10, CpuSecondsUser: 5, CpuCoresUsed: 2},
	})
	if err != nil {
		t.Fatalf("submit with output_data_url: %v", err)
	}
	if !submitResp.Accepted {
		t.Errorf("result not accepted: %s", submitResp.Message)
	}

	// Verify output_data_ref stored.
	var outputRef *string
	err = env.pool.QueryRow(ctx,
		"SELECT output_data_ref FROM results WHERE id = $1",
		submitResp.ResultId,
	).Scan(&outputRef)
	if err != nil {
		t.Fatalf("query output_data_ref: %v", err)
	}
	if outputRef == nil || *outputRef == "" {
		t.Error("expected output_data_ref in results table")
	}
}

// TestV08_Scenario5_LazyGeneration tests lazy generation for ongoing Monte Carlo leaf.
func TestV08_Scenario5_LazyGeneration(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v08-lazy")

	createReq := leaf.CreateLeafRequest{
		Name:            "V08 Lazy Generation",
		Description:     "Ongoing leaf with lazy work unit generation",
		ResearchArea:    []string{"testing"},
		TaskPattern:     leaf.PatternMonteCarlo,
		IsOngoing:       true,
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
		NumericTolerance: floatPtr(0.01), MaxRetries: 3,
	}
	ftCfg := leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds: 60, MissedHeartbeatsThreshold: 3, DeadlineMultiplier: 3.0, MaxReassignments: 3,
	}
	dataCfg := leaf.DataConfig{
		TransferStrategy:  "INLINE",
		AggregationFormat: "JSON",
		AggregationConfig: map[string]any{
			"aggregator_type": "all", "output_field": "result", "confidence_level": 0.95,
		},
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
		GenerationMode:     "lazy",
		LazyThreshold:      10,
		LazyBatchSize:      20,
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

	// Generate initial batch of 20 work units.
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"num_trials": float64(20),
		},
	}
	resp = httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate initial batch")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 20 {
		t.Fatalf("initial work_units_created = %d, want 20", genResp.WorkUnitsCreated)
	}

	// Register volunteer and process 15 work units (QUEUED drops to 5, below threshold of 10).
	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "V08 Lazy Volunteer")

	for i := 0; i < 15; i++ {
		wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID, PublicKey: pubKey,
		})
		if err != nil {
			t.Fatalf("request work unit %d: %v", i, err)
		}

		outputData := []byte(fmt.Sprintf(`{"result": %d}`, i+1))
		hash := sha256.Sum256(outputData)
		checksum := hex.EncodeToString(hash[:])

		ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuResp.Assignments[0].WorkUnitId)
		_, err = env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuResp.Assignments[0].WorkUnitId, VolunteerId: volID, PublicKey: pubKey,
			OutputData: outputData, OutputChecksumSha256: checksum,
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("submit result %d: %v", i, err)
		}
	}

	time.Sleep(500 * time.Millisecond)

	// Create lazy manager with real generators and trigger check.
	logger := slog.Default()
	patternRouter := generate.NewRouter(paramsweep.Generate, mapreduce.Generate, montecarlo.Generate, custom.Generate, logger)
	lazyMgr := generate.NewLazyManager(
		patternRouter,
		workunit.NewPgxWorkUnitRepository(env.pool),
		workunit.NewPgxBatchRepository(env.pool),
		leaf.NewPgxRepository(env.pool),
		logger,
	)

	// Trigger lazy check — QUEUED count is 5 (20 - 15 processed), below threshold of 10.
	generated, err := lazyMgr.CheckAndGenerate(ctx, proj.ID)
	if err != nil {
		t.Fatalf("lazy CheckAndGenerate: %v", err)
	}
	if generated == 0 {
		t.Error("expected lazy manager to generate work units (QUEUED below threshold)")
	} else {
		t.Logf("lazy manager generated %d work units", generated)
	}

	// Verify cursor was updated.
	updatedProj, err := leaf.NewPgxRepository(env.pool).GetByID(ctx, proj.ID)
	if err != nil {
		t.Fatalf("get updated leaf: %v", err)
	}
	cursor := updatedProj.DataConfig.SplittingConfig["_cursor"]
	if cursor == nil {
		t.Error("expected cursor in splitting_config after lazy generation")
	} else {
		cursorMap, ok := cursor.(map[string]any)
		if ok {
			seedOffset := cursorMap["last_seed_offset"]
			t.Logf("cursor seed_offset = %v, total_generated = %v", seedOffset, cursorMap["total_generated"])
		}
	}

	// Verify leaf does NOT transition to COMPLETED (is_ongoing=true).
	var leafState string
	err = env.pool.QueryRow(ctx, "SELECT state FROM leafs WHERE id = $1", proj.ID).Scan(&leafState)
	if err != nil {
		t.Fatalf("query leaf state: %v", err)
	}
	if leafState == "COMPLETED" {
		t.Error("ongoing leaf should NOT transition to COMPLETED")
	}
	t.Logf("leaf state = %s (expected ACTIVE for ongoing)", leafState)
}

// TestV08_Scenario6_AllPatternsRegression is a regression test that creates
// one leaf per pattern, generates/uploads work units, validates counts,
// and does a quick aggregation check for each.
func TestV08_Scenario6_AllPatternsRegression(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v08-reg")
	pubKey := []byte(genVolunteerKey(t))

	volID := registerVolunteer(t, env, ctx, pubKey, "V08 Regression Volunteer")

	type patternTest struct {
		name        string
		pattern     leaf.TaskPattern
		setupData   func(t *testing.T, leafURL string)
		expectedWUs int
	}

	tests := []patternTest{
		{
			name:    "ParameterSweep",
			pattern: leaf.PatternParameterSweep,
			setupData: func(t *testing.T, leafURL string) {
				genReq := workunit.GenerateRequest{
					ParameterSpace: map[string]interface{}{
						"x": []interface{}{float64(1), float64(2), float64(3)},
					},
				}
				resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
				requireStatus(t, resp, http.StatusAccepted, "generate param sweep")
				resp.Body.Close()
			},
			expectedWUs: 3,
		},
		{
			name:    "MapReduce",
			pattern: leaf.PatternMapReduce,
			setupData: func(t *testing.T, leafURL string) {
				genReq := workunit.GenerateRequest{
					ParameterSpace: map[string]interface{}{
						"input_data": "{\"a\":1}\n{\"b\":2}\n",
					},
				}
				resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
				requireStatus(t, resp, http.StatusAccepted, "generate map-reduce")
				resp.Body.Close()
			},
			expectedWUs: 2,
		},
		{
			name:    "MonteCarlo",
			pattern: leaf.PatternMonteCarlo,
			setupData: func(t *testing.T, leafURL string) {
				genReq := workunit.GenerateRequest{
					ParameterSpace: map[string]interface{}{
						"num_trials": float64(3),
					},
				}
				resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
				requireStatus(t, resp, http.StatusAccepted, "generate monte carlo")
				resp.Body.Close()
			},
			expectedWUs: 3,
		},
		{
			name:    "Custom",
			pattern: leaf.PatternCustom,
			setupData: func(t *testing.T, leafURL string) {
				bulkReq := custom.BulkUploadRequest{
					WorkUnits: []custom.WorkUnitInput{
						{InputData: json.RawMessage(`{"task":1}`), Parameters: json.RawMessage(`{"idx":0}`)},
						{InputData: json.RawMessage(`{"task":2}`), Parameters: json.RawMessage(`{"idx":1}`)},
					},
				}
				resp := httpReq(t, "POST", leafURL+"/work-units/bulk", bulkReq)
				requireStatus(t, resp, http.StatusCreated, "bulk upload custom")
				resp.Body.Close()
			},
			expectedWUs: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			createReq := leaf.CreateLeafRequest{
				Name:            "V08 Regression " + tc.name,
				Description:     "Regression test for " + tc.name,
				ResearchArea:    []string{"testing"},
				TaskPattern:     tc.pattern,
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

			byRecord := "by_record"
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
			if tc.pattern == leaf.PatternMapReduce {
				dataCfg.SplittingStrategy = &byRecord
				dataCfg.SplittingConfig = map[string]interface{}{"records_per_chunk": float64(1)}
			}
			if tc.pattern == leaf.PatternMonteCarlo {
				dataCfg.AggregationConfig = map[string]any{
					"aggregator_type": "all", "output_field": "result", "confidence_level": 0.95,
				}
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

			// Generate/upload work units.
			tc.setupData(t, leafURL)

			// Verify work unit count.
			var wuCount int
			err := env.pool.QueryRow(ctx,
				"SELECT COUNT(*) FROM work_units WHERE leaf_id = $1",
				proj.ID,
			).Scan(&wuCount)
			if err != nil {
				t.Fatalf("query work unit count: %v", err)
			}
			if wuCount != tc.expectedWUs {
				t.Errorf("work unit count = %d, want %d", wuCount, tc.expectedWUs)
			}

			// Process all work units.
			for i := 0; i < tc.expectedWUs; i++ {
				wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
					VolunteerId: volID, PublicKey: pubKey,
				})
				if err != nil {
					t.Fatalf("request work unit %d: %v", i, err)
				}

				outputData := []byte(fmt.Sprintf(`{"result": %d}`, i+1))
				hash := sha256.Sum256(outputData)
				checksum := hex.EncodeToString(hash[:])

				ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuResp.Assignments[0].WorkUnitId)
				_, err = env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
					WorkUnitId: wuResp.Assignments[0].WorkUnitId, VolunteerId: volID, PublicKey: pubKey,
					OutputData: outputData, OutputChecksumSha256: checksum,
					Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1},
				})
				if err != nil {
					t.Fatalf("submit result %d: %v", i, err)
				}
			}

			time.Sleep(500 * time.Millisecond)

			// Aggregate.
			resp = httpReq(t, "POST", leafURL+"/aggregate", nil)
			requireStatus(t, resp, http.StatusOK, "aggregate")
			var aggResp struct {
				Data struct {
					Status              string `json:"status"`
					WorkUnitsAggregated int    `json:"work_units_aggregated"`
				} `json:"data"`
			}
			decodeJSON(t, resp, &aggResp)

			if aggResp.Data.WorkUnitsAggregated < 1 {
				t.Errorf("work_units_aggregated = %d, want > 0", aggResp.Data.WorkUnitsAggregated)
			}
			t.Logf("%s: aggregated %d/%d work units, status=%s",
				tc.name, aggResp.Data.WorkUnitsAggregated, tc.expectedWUs, aggResp.Data.Status)
		})
	}
}
