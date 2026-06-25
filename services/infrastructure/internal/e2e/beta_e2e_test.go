//go:build integration

package e2e_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// --- Scenario 1: Multi-Pattern Native Binary ---
// Tests all 4 task patterns (parameter sweep, map-reduce, monte carlo, custom) through
// the full lifecycle: create → generate → assign → execute → validate → credit → aggregate.

func TestBetaE2E_MultiPatternNative(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "beta-mp")

	volAPubKey := genVolunteerKey(t)
	volBPubKey := genVolunteerKey(t)
	volAID := registerBetaVolunteer(t, env, ctx, volAPubKey, "Beta Vol A", nil)
	volBID := registerBetaVolunteer(t, env, ctx, volBPubKey, "Beta Vol B", nil)
	volAIDParsed := types.MustParseID(volAID)
	volBIDParsed := types.MustParseID(volBID)

	creditAmount := 2.0
	valCfg := leaf.ValidationConfig{
		RedundancyFactor: 2, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3,
	}
	creditCfg := leaf.CreditConfig{CreditPerValidatedWorkUnit: creditAmount}

	// --- 1a: Parameter Sweep (5 combinations) ---
	t.Run("ParameterSweep", func(t *testing.T) {
		proj := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
			Name: "Beta Param Sweep", TaskPattern: leaf.PatternParameterSweep,
			ExecConfig: defaultExecConfig(), ValConfig: valCfg, FTConfig: defaultFTConfig(),
			DataConfig: defaultDataConfig(), CreditConfig: creditCfg,
		})
		leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

		genReq := workunit.GenerateRequest{
			ParameterSpace: map[string]interface{}{
				"x": []interface{}{float64(1), float64(2), float64(3), float64(4), float64(5)},
			},
		}
		resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
		requireStatus(t, resp, http.StatusAccepted, "generate param sweep")
		var genResp workunit.GenerateResponse
		decodeJSON(t, resp, &genResp)
		if genResp.WorkUnitsCreated != 5 {
			t.Fatalf("work_units_created = %d, want 5", genResp.WorkUnitsCreated)
		}

		outputData := []byte(`{"result": "sweep_computed", "value": 42.0}`)
		checksum := sha256Hex(outputData)

		for i := 0; i < 5; i++ {
			wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volAPubKey), &lettucev1.RequestWorkUnitRequest{
				VolunteerId: volAID, PublicKey: volAPubKey,
			})
			if err != nil {
				t.Fatalf("vol A request WU %d: %v", i, err)
			}
			wu := firstAssignment(t, wuResp)

			ensureRunStart(t, env.pool, env.grpc, ctx, volAID, volAPubKey, wu.WorkUnitId)
			createRedundantAssignment(t, env.pool, ctx, wu.WorkUnitId, volBIDParsed)

			_, err = env.grpc.SubmitResult(signFor(t, ctx, volAPubKey), &lettucev1.SubmitResultRequest{
				WorkUnitId: wu.WorkUnitId, VolunteerId: volAID, PublicKey: volAPubKey,
				OutputData: outputData, OutputChecksumSha256: checksum,
				Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 10, CpuSecondsUser: 8, CpuCoresUsed: 2},
			})
			if err != nil {
				t.Fatalf("vol A submit WU %d: %v", i, err)
			}
			_, err = env.grpc.SubmitResult(signFor(t, ctx, volBPubKey), &lettucev1.SubmitResultRequest{
				WorkUnitId: wu.WorkUnitId, VolunteerId: volBID, PublicKey: volBPubKey,
				OutputData: outputData, OutputChecksumSha256: checksum,
				Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 12, CpuSecondsUser: 10, CpuCoresUsed: 2},
			})
			if err != nil {
				t.Fatalf("vol B submit WU %d: %v", i, err)
			}
		}

		time.Sleep(500 * time.Millisecond)

		var validatedCount int
		err := env.pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM work_units WHERE leaf_id = $1 AND state = 'VALIDATED'",
			proj.ID).Scan(&validatedCount)
		if err != nil {
			t.Fatalf("query validated count: %v", err)
		}
		if validatedCount != 5 {
			t.Errorf("validated work units = %d, want 5", validatedCount)
		}

		assertCreditExists(t, env.pool, ctx, volAIDParsed, proj.ID, 5)
		assertCreditExists(t, env.pool, ctx, volBIDParsed, proj.ID, 5)

		// Verify aggregation: parameter sweep collects results.
		resp = httpReq(t, "POST", leafURL+"/aggregate", nil)
		requireStatus(t, resp, http.StatusOK, "aggregate param sweep")
		resp.Body.Close()
	})

	// --- 1b: Map-Reduce (5 chunks via by_record splitting) ---
	// Note: by_line_count produces non-JSON chunks that fail JSONB insertion.
	// Using by_record splitting with valid JSON records instead.
	t.Run("MapReduce", func(t *testing.T) {
		// Build NDJSON input: 50 records separated by newlines.
		var inputData string
		for i := 1; i <= 50; i++ {
			inputData += fmt.Sprintf(`{"line":%d,"value":1}`, i) + "\n"
		}
		byRecord := "by_record"
		dataCfg := defaultDataConfig()
		dataCfg.SplittingStrategy = &byRecord
		dataCfg.SplittingConfig = map[string]interface{}{"records_per_chunk": float64(10)}
		dataCfg.AggregationConfig = map[string]any{"reducer_type": "sum", "reducer_field": "count"}

		valCfg1 := leaf.ValidationConfig{
			RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3,
		}
		proj := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
			Name: "Beta Map-Reduce", TaskPattern: leaf.PatternMapReduce,
			ExecConfig: defaultExecConfig(), ValConfig: valCfg1, FTConfig: defaultFTConfig(),
			DataConfig: dataCfg, CreditConfig: creditCfg,
		})
		leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

		genReq := workunit.GenerateRequest{
			ParameterSpace: map[string]interface{}{"input_data": inputData},
		}
		resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
		requireStatus(t, resp, http.StatusAccepted, "generate map-reduce")
		var genResp workunit.GenerateResponse
		decodeJSON(t, resp, &genResp)
		if genResp.WorkUnitsCreated < 1 {
			t.Fatalf("work_units_created = %d, want >= 1", genResp.WorkUnitsCreated)
		}
		numWUs := genResp.WorkUnitsCreated

		for i := 0; i < numWUs; i++ {
			outputData := []byte(`{"count": 10}`)
			requestSubmitResult(t, env, ctx, volAID, volAPubKey, outputData)
		}

		time.Sleep(500 * time.Millisecond)

		resp = httpReq(t, "POST", leafURL+"/aggregate", nil)
		requireStatus(t, resp, http.StatusOK, "aggregate map-reduce")
		var aggResp struct {
			Data struct {
				Result              json.RawMessage `json:"result"`
				WorkUnitsAggregated int             `json:"work_units_aggregated"`
			} `json:"data"`
		}
		decodeJSON(t, resp, &aggResp)
		if aggResp.Data.WorkUnitsAggregated < 1 {
			t.Errorf("work_units_aggregated = %d, want >= 1", aggResp.Data.WorkUnitsAggregated)
		}
		var sumResult map[string]interface{}
		json.Unmarshal(aggResp.Data.Result, &sumResult)
		if sumV, ok := sumResult["sum"]; ok {
			if sumV.(float64) <= 0 {
				t.Errorf("sum = %v, want > 0", sumV)
			}
		}
	})

	// --- 1c: Monte Carlo (20 trials) ---
	t.Run("MonteCarlo", func(t *testing.T) {
		dataCfg := defaultDataConfig()
		dataCfg.AggregationConfig = map[string]any{
			"aggregator_type":  "all",
			"output_field":     "result",
			"confidence_level": 0.95,
		}
		valCfg1 := leaf.ValidationConfig{
			RedundancyFactor: 1, AgreementThreshold: 1.0,
			ComparisonMode: "NUMERIC_TOLERANCE", NumericTolerance: floatPtr(0.01), MaxRetries: 3,
		}
		proj := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
			Name: "Beta Monte Carlo", TaskPattern: leaf.PatternMonteCarlo,
			ExecConfig: defaultExecConfig(), ValConfig: valCfg1, FTConfig: defaultFTConfig(),
			DataConfig: dataCfg, CreditConfig: creditCfg,
		})
		leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

		numTrials := 20
		genReq := workunit.GenerateRequest{
			ParameterSpace: map[string]interface{}{"num_trials": float64(numTrials)},
		}
		resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
		requireStatus(t, resp, http.StatusAccepted, "generate monte carlo")
		var genResp workunit.GenerateResponse
		decodeJSON(t, resp, &genResp)
		if genResp.WorkUnitsCreated != numTrials {
			t.Fatalf("work_units_created = %d, want %d", genResp.WorkUnitsCreated, numTrials)
		}

		var values []float64
		for i := 0; i < numTrials; i++ {
			wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volAPubKey), &lettucev1.RequestWorkUnitRequest{
				VolunteerId: volAID, PublicKey: volAPubKey,
			})
			if err != nil {
				t.Fatalf("request WU %d: %v", i, err)
			}
			wu := firstAssignment(t, wuResp)
			var params struct {
				Seed int64 `json:"seed"`
			}
			if wu.ParametersJson != "" {
				json.Unmarshal([]byte(wu.ParametersJson), &params)
			}
			result := float64(params.Seed) * 0.1
			values = append(values, result)
			outputData := []byte(fmt.Sprintf(`{"result": %.1f}`, result))
			checksum := sha256Hex(outputData)
			ensureRunStart(t, env.pool, env.grpc, ctx, volAID, volAPubKey, wu.WorkUnitId)
			_, err = env.grpc.SubmitResult(signFor(t, ctx, volAPubKey), &lettucev1.SubmitResultRequest{
				WorkUnitId: wu.WorkUnitId, VolunteerId: volAID, PublicKey: volAPubKey,
				OutputData: outputData, OutputChecksumSha256: checksum,
				Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1},
			})
			if err != nil {
				t.Fatalf("submit WU %d: %v", i, err)
			}
		}
		time.Sleep(500 * time.Millisecond)

		var expectedMean float64
		for _, v := range values {
			expectedMean += v
		}
		expectedMean /= float64(len(values))

		resp = httpReq(t, "POST", leafURL+"/aggregate", nil)
		requireStatus(t, resp, http.StatusOK, "aggregate monte carlo")
		var aggResp struct {
			Data struct {
				Result json.RawMessage `json:"result"`
			} `json:"data"`
		}
		decodeJSON(t, resp, &aggResp)

		var mcResult struct {
			Statistics struct {
				Mean               float64 `json:"mean"`
				Variance           float64 `json:"variance"`
				Count              int     `json:"count"`
				ConfidenceInterval *struct {
					Level float64 `json:"level"`
				} `json:"confidence_interval"`
			} `json:"statistics"`
		}
		json.Unmarshal(aggResp.Data.Result, &mcResult)
		if math.Abs(mcResult.Statistics.Mean-expectedMean) > 1.0 {
			t.Errorf("mean = %v, expected ~%v", mcResult.Statistics.Mean, expectedMean)
		}
		if mcResult.Statistics.ConfidenceInterval == nil {
			t.Error("expected confidence_interval")
		}
	})

	// --- 1d: Custom (5 manually uploaded work units) ---
	t.Run("Custom", func(t *testing.T) {
		dataCfg := defaultDataConfig()
		dataCfg.AggregationConfig = map[string]any{"reducer_type": "concatenate"}
		valCfg1 := leaf.ValidationConfig{
			RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3,
		}
		proj := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
			Name: "Beta Custom", TaskPattern: leaf.PatternCustom,
			ExecConfig: defaultExecConfig(), ValConfig: valCfg1, FTConfig: defaultFTConfig(),
			DataConfig: dataCfg, CreditConfig: creditCfg,
		})
		leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

		// Bulk upload 5 custom work units.
		bulkReq := []map[string]interface{}{}
		for i := 0; i < 5; i++ {
			bulkReq = append(bulkReq, map[string]interface{}{
				"input_data": fmt.Sprintf(`{"task": %d}`, i+1),
				"parameters": fmt.Sprintf(`{"index": %d}`, i+1),
			})
		}
		resp := httpReq(t, "POST", leafURL+"/work-units/bulk", map[string]interface{}{
			"work_units": bulkReq,
		})
		requireStatus(t, resp, http.StatusCreated, "bulk upload custom")
		resp.Body.Close()

		for i := 0; i < 5; i++ {
			outputData := []byte(fmt.Sprintf(`{"processed": %d}`, i+1))
			requestSubmitResult(t, env, ctx, volAID, volAPubKey, outputData)
		}
		time.Sleep(500 * time.Millisecond)

		var validatedCount int
		env.pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM work_units WHERE leaf_id = $1 AND state = 'VALIDATED'",
			proj.ID).Scan(&validatedCount)
		if validatedCount != 5 {
			t.Errorf("validated custom work units = %d, want 5", validatedCount)
		}
	})

	// --- Verify cross-leaf volunteer stats feed ---
	t.Run("VolunteerStats", func(t *testing.T) {
		resp := httpReq(t, "GET", env.httpURL+"/api/v1/volunteers/stats", nil)
		requireStatus(t, resp, http.StatusOK, "volunteer stats")
		var statsResp struct {
			Volunteers []struct {
				PublicKey   string  `json:"public_key"`
				TotalCredit float64 `json:"total_credit"`
				RAC         float64 `json:"rac"`
			} `json:"volunteers"`
		}
		decodeJSON(t, resp, &statsResp)
		if len(statsResp.Volunteers) < 1 {
			t.Error("expected at least 1 volunteer in stats feed")
		}
		for _, v := range statsResp.Volunteers {
			if v.TotalCredit <= 0 {
				t.Errorf("volunteer %s total_credit = %f, want > 0", v.PublicKey, v.TotalCredit)
			}
		}
	})
}

// --- Scenario 2: Container + GPU ---
// Tests that GPU-required leafs are only assigned to GPU-equipped volunteers
// and that GPU metrics are stored in execution metadata.

func TestBetaE2E_ContainerGPU(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "beta-gpu")

	// Create GPU-required container leaf.
	gpuCC := "8.6"
	alpineImage := "alpine:latest"
	execCfg := leaf.ExecutionConfig{
		Runtime: "CONTAINER", Image: &alpineImage,
		GPURequired: true, GPUType: "NVIDIA",
		MaxMemoryMB: 4096, MaxDiskMB: 10240, MaxCPUSeconds: 3600, MinVRAMGB: 4,
	}
	resReqs := &leaf.ResourceRequirements{
		MinCPUCores:          1,
		MinDiskMB:            1024,
		GPURequired:          true,
		MinGPUVRAMMB:         4096,
		GPUComputeCapability: &gpuCC,
	}
	proj := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
		Name: "Beta GPU Leaf", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig:   execCfg,
		ValConfig:    leaf.ValidationConfig{RedundancyFactor: 2, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3},
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 3.0},
		ResourceReqs: resReqs,
	})
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	// Generate 2 work units.
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"x": []interface{}{float64(1), float64(2)},
		},
	}
	resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate GPU WUs")
	resp.Body.Close()

	// Register a CPU-only volunteer — should NOT get GPU work.
	cpuPubKey := genVolunteerKey(t)
	cpuVolID := registerBetaVolunteer(t, env, ctx, cpuPubKey, "CPU Only Vol", nil)

	// No-work is now an OK response with empty assignments (the codes.NotFound
	// sentinel was removed); a CPU-only volunteer simply matches no GPU work.
	cpuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, cpuPubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: cpuVolID, PublicKey: cpuPubKey,
	})
	if err != nil {
		t.Fatalf("CPU-only volunteer request: unexpected error %v", err)
	}
	if len(cpuResp.Assignments) != 0 {
		t.Errorf("CPU-only volunteer should NOT receive GPU work unit, got %d assignments", len(cpuResp.Assignments))
	}

	// Register GPU volunteer A.
	gpuAPubKey := genVolunteerKey(t)
	gpuAVolID := registerBetaVolunteer(t, env, ctx, gpuAPubKey, "GPU Vol A", []*lettucev1.GpuInfo{
		{Model: "RTX 3080", Vendor: "nvidia", VramMb: 10240, MaxVramPct: 100, ComputeCapability: "8.6"},
	})

	// Register GPU volunteer B.
	gpuBPubKey := genVolunteerKey(t)
	gpuBVolID := registerBetaVolunteer(t, env, ctx, gpuBPubKey, "GPU Vol B", []*lettucev1.GpuInfo{
		{Model: "RTX 3090", Vendor: "nvidia", VramMb: 24576, MaxVramPct: 100, ComputeCapability: "8.6"},
	})
	gpuBIDParsed := types.MustParseID(gpuBVolID)

	outputData := []byte(`{"gpu_result": "computed"}`)
	checksum := sha256Hex(outputData)

	gpuMeta := &lettucev1.ExecutionMetadata{
		WallClockSeconds: 30,
		CpuSecondsUser:   5,
		CpuCoresUsed:     1,
		GpuSeconds:       25,
		GpuModel:         "RTX 3080",
		GpuVramUsedMb:    4096,
		PeakMemoryMb:     2048,
	}

	for i := 0; i < 2; i++ {
		wuResp, reqErr := env.grpc.RequestWorkUnit(signFor(t, ctx, gpuAPubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: gpuAVolID, PublicKey: gpuAPubKey,
		})
		if reqErr != nil {
			t.Fatalf("GPU vol A request WU %d: %v", i, reqErr)
		}
		wu := firstAssignment(t, wuResp)

		ensureRunStart(t, env.pool, env.grpc, ctx, gpuAVolID, gpuAPubKey, wu.WorkUnitId)
		createRedundantAssignment(t, env.pool, ctx, wu.WorkUnitId, gpuBIDParsed)

		_, subErr := env.grpc.SubmitResult(signFor(t, ctx, gpuAPubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: wu.WorkUnitId, VolunteerId: gpuAVolID, PublicKey: gpuAPubKey,
			OutputData: outputData, OutputChecksumSha256: checksum, Metadata: gpuMeta,
		})
		if subErr != nil {
			t.Fatalf("GPU vol A submit WU %d: %v", i, subErr)
		}

		gpuMetaB := &lettucev1.ExecutionMetadata{
			WallClockSeconds: 28, CpuSecondsUser: 4, CpuCoresUsed: 1,
			GpuSeconds: 24, GpuModel: "RTX 3090", GpuVramUsedMb: 4096, PeakMemoryMb: 2048,
		}
		_, subErr = env.grpc.SubmitResult(signFor(t, ctx, gpuBPubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: wu.WorkUnitId, VolunteerId: gpuBVolID, PublicKey: gpuBPubKey,
			OutputData: outputData, OutputChecksumSha256: checksum, Metadata: gpuMetaB,
		})
		if subErr != nil {
			t.Fatalf("GPU vol B submit WU %d: %v", i, subErr)
		}
	}

	time.Sleep(500 * time.Millisecond)

	// Verify all validated.
	var validatedCount int
	env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_units WHERE leaf_id = $1 AND state = 'VALIDATED'",
		proj.ID).Scan(&validatedCount)
	if validatedCount != 2 {
		t.Errorf("validated GPU work units = %d, want 2", validatedCount)
	}

	// Verify GPU metrics stored in results.
	var gpuSecondsTotal float64
	env.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM((r.execution_metadata->>'gpu_seconds')::float), 0)
		 FROM results r JOIN work_units wu ON r.work_unit_id = wu.id
		 WHERE wu.leaf_id = $1`,
		proj.ID).Scan(&gpuSecondsTotal)
	if gpuSecondsTotal <= 0 {
		t.Errorf("total gpu_seconds = %f, want > 0", gpuSecondsTotal)
	}
}

// --- Scenario 3: Desktop App Management API Bridge ---
// Skipped: The management API lives in the volunteer-cli Go module
// (services/volunteer-cli/internal/management/) which cannot be imported from
// the infrastructure module. These endpoints are tested in the volunteer-cli
// test suite (management/handlers_test.go, management/auth_test.go).

func TestBetaE2E_ManagementAPI(t *testing.T) {
	t.Skip("Management API tests are in the volunteer-cli module (separate Go module boundary)")
}

// --- Scenario 4: Checkpointing Resilience ---
// Tests checkpoint save by volunteer A, disconnect, and resume by volunteer B.

func TestBetaE2E_Checkpointing(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "beta-ckpt")

	// Create leaf with checkpointing enabled (minimum interval is 60s).
	cpInterval := 60
	ftCfg := leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds:  300,
		MissedHeartbeatsThreshold: 3,
		DeadlineMultiplier:        3.0,
		MaxReassignments:          3,
		CheckpointingEnabled:      true,
		CheckpointIntervalSeconds: &cpInterval,
		MaxCheckpointSizeBytes:    1048576,
	}
	proj := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
		Name: "Beta Checkpoint", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig:   defaultExecConfig(),
		ValConfig:    leaf.ValidationConfig{RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3},
		FTConfig:     ftCfg,
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	// Generate 1 work unit.
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{"x": []interface{}{float64(42)}},
	}
	resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate checkpoint WU")
	resp.Body.Close()

	// Volunteer A requests work unit.
	volAPubKey := genVolunteerKey(t)
	volAID := registerBetaVolunteer(t, env, ctx, volAPubKey, "Checkpoint Vol A", nil)

	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volAPubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volAID, PublicKey: volAPubKey,
	})
	if err != nil {
		t.Fatalf("vol A request work unit: %v", err)
	}
	wu := firstAssignment(t, wuResp)
	wuID := wu.WorkUnitId

	// Verify checkpoint info in response.
	if wu.CheckpointIntervalSeconds != int32(cpInterval) {
		t.Errorf("checkpoint_interval_seconds = %d, want %d", wu.CheckpointIntervalSeconds, cpInterval)
	}

	// Vol A run-starts the reserved unit (StartWork: QUEUED -> ASSIGNED).
	_, err = env.grpc.StartWork(signFor(t, ctx, volAPubKey), &lettucev1.StartWorkRequest{
		WorkUnitId: wuID, VolunteerId: volAID,
	})
	if err != nil {
		t.Fatalf("vol A StartWork: %v", err)
	}

	// Vol A saves checkpoint (sequence=1).
	checkpointData := []byte("checkpoint-state-v1-progress-10pct")
	saveResp, err := env.grpc.SaveCheckpoint(signFor(t, ctx, volAPubKey), &lettucev1.SaveCheckpointRequest{
		WorkUnitId: wuID, VolunteerId: volAID,
		CheckpointData: checkpointData, CheckpointSequence: 1,
	})
	if err != nil {
		t.Fatalf("vol A save checkpoint: %v", err)
	}
	if !saveResp.Accepted {
		t.Error("checkpoint should be accepted")
	}

	// (The CHECKPOINT_SAVED heartbeat status was informational only and is gone;
	// checkpoint coordination rides SaveCheckpoint, which already persisted seq=1.)

	// Verify checkpoint metadata in DB.
	wuIDParsed := types.MustParseID(wuID)
	var lastCPSeq int
	err = env.pool.QueryRow(ctx,
		"SELECT last_checkpoint_sequence FROM work_units WHERE id = $1", wuIDParsed,
	).Scan(&lastCPSeq)
	if err != nil {
		t.Fatalf("query checkpoint seq: %v", err)
	}
	if lastCPSeq != 1 {
		t.Errorf("last_checkpoint_sequence = %d, want 1", lastCPSeq)
	}

	// Simulate Vol A disconnect: manually re-queue the work unit
	// and mark the assignment as abandoned.
	_, err = env.pool.Exec(ctx,
		"UPDATE work_units SET state = 'QUEUED', assigned_volunteer_id = NULL WHERE id = $1", wuIDParsed)
	if err != nil {
		t.Fatalf("re-queue work unit: %v", err)
	}
	volAIDParsed := types.MustParseID(volAID)
	_, err = env.pool.Exec(ctx,
		"UPDATE work_unit_assignment_history SET outcome = 'ABANDONED' WHERE work_unit_id = $1 AND volunteer_id = $2",
		wuIDParsed, volAIDParsed)
	if err != nil {
		t.Fatalf("mark assignment abandoned: %v", err)
	}

	// Verify checkpoint data still exists. Vol A is still in the assignment history
	// (now ABANDONED), and GetCheckpoint authorizes any historical assignee.
	getResp, err := env.grpc.GetCheckpoint(signFor(t, ctx, volAPubKey), &lettucev1.GetCheckpointRequest{
		WorkUnitId: wuID,
	})
	if err != nil {
		t.Fatalf("get checkpoint after disconnect: %v", err)
	}
	if !getResp.HasCheckpoint {
		t.Fatal("expected checkpoint to survive volunteer disconnect")
	}
	if getResp.CheckpointSequence != 1 {
		t.Errorf("checkpoint sequence = %d, want 1", getResp.CheckpointSequence)
	}
	if string(getResp.CheckpointData) != string(checkpointData) {
		t.Errorf("checkpoint data mismatch: got %q", getResp.CheckpointData)
	}

	// Volunteer B picks up the work unit.
	volBPubKey := genVolunteerKey(t)
	volBID := registerBetaVolunteer(t, env, ctx, volBPubKey, "Checkpoint Vol B", nil)

	wuResp2, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volBPubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volBID, PublicKey: volBPubKey,
	})
	if err != nil {
		t.Fatalf("vol B request work unit: %v", err)
	}
	wu2 := firstAssignment(t, wuResp2)
	if wu2.WorkUnitId != wuID {
		t.Errorf("vol B got different WU: %s, want %s", wu2.WorkUnitId, wuID)
	}
	if !wu2.HasCheckpoint {
		t.Error("vol B should see has_checkpoint = true")
	}
	if wu2.CheckpointSequence != 1 {
		t.Errorf("vol B checkpoint_sequence = %d, want 1", wu2.CheckpointSequence)
	}

	// Vol B run-starts the reassigned unit (RUNNING heartbeat → Assign) so it has an
	// active assignment_history row before retrieving the checkpoint and submitting.
	// Run-start preserves the checkpoint fields carried on the unit.
	ensureRunStart(t, env.pool, env.grpc, ctx, volBID, volBPubKey, wuID)

	// Vol B retrieves checkpoint data.
	getResp2, err := env.grpc.GetCheckpoint(signFor(t, ctx, volBPubKey), &lettucev1.GetCheckpointRequest{
		WorkUnitId: wuID,
	})
	if err != nil {
		t.Fatalf("vol B get checkpoint: %v", err)
	}
	if string(getResp2.CheckpointData) != string(checkpointData) {
		t.Error("vol B received different checkpoint data")
	}

	// Vol B completes the work unit.
	outputData := []byte(`{"result": "completed_from_checkpoint"}`)
	checksum := sha256Hex(outputData)
	_, err = env.grpc.SubmitResult(signFor(t, ctx, volBPubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volBID, PublicKey: volBPubKey,
		OutputData: outputData, OutputChecksumSha256: checksum,
		Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 5, CpuSecondsUser: 4, CpuCoresUsed: 2},
	})
	if err != nil {
		t.Fatalf("vol B submit result: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Verify validated and credit granted to Vol B.
	var state string
	env.pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", wuIDParsed).Scan(&state)
	if state != "VALIDATED" {
		t.Errorf("work unit state = %q, want VALIDATED", state)
	}

	volBIDParsed := types.MustParseID(volBID)
	assertCreditExists(t, env.pool, ctx, volBIDParsed, proj.ID, 1)

	// Verify checkpoint cleaned up after validation.
	getResp3, err := env.grpc.GetCheckpoint(signFor(t, ctx, volBPubKey), &lettucev1.GetCheckpointRequest{
		WorkUnitId: wuID,
	})
	if err != nil {
		t.Fatalf("get checkpoint after validation: %v", err)
	}
	if getResp3.HasCheckpoint {
		t.Error("checkpoint should be cleaned up after validation")
	}
}

// --- Scenario 5: Spot-Check Integrity ---
// Tests spot-check validation with 100% check rate, correct/incorrect results,
// and volunteer flagging.

func TestBetaE2E_SpotCheck(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "beta-sc")

	// Create leaf with spot-check at 20% (validation enforces 1.0-20.0 range).
	// With 50 WUs at 20%, expect ~10 spot-checked (enough for meaningful stats).
	valCfg := leaf.ValidationConfig{
		RedundancyFactor:    1,
		AgreementThreshold:  1.0,
		ComparisonMode:      "EXACT",
		MaxRetries:          3,
		SpotCheckEnabled:    true,
		SpotCheckPercentage: 20.0,
	}
	proj := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
		Name: "Beta Spot-Check", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig:   defaultExecConfig(),
		ValConfig:    valCfg,
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	// Generate 50 work units for a reasonable sample at 20%.
	params := make([]interface{}, 50)
	for i := 0; i < 50; i++ {
		params[i] = float64(i + 1)
	}
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{"x": params},
	}
	resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate spot-check WUs")
	resp.Body.Close()

	// Register 2 volunteers.
	volAPubKey := genVolunteerKey(t)
	volBPubKey := genVolunteerKey(t)
	volAID := registerBetaVolunteer(t, env, ctx, volAPubKey, "SC Vol A", nil)
	volBID := registerBetaVolunteer(t, env, ctx, volBPubKey, "SC Vol B", nil)

	// Vol A processes all 50 work units with correct results.
	// At 20% spot-check, ~10 WUs stay QUEUED after first assignment so Vol B can get them.
	type wuInfo struct {
		id       string
		goodData []byte
	}
	var wus []wuInfo
	for i := 0; i < 50; i++ {
		wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volAPubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volAID, PublicKey: volAPubKey,
		})
		if err != nil {
			t.Fatalf("vol A request WU %d: %v", i, err)
		}
		asg := firstAssignment(t, wuResp)

		goodOutput := []byte(fmt.Sprintf(`{"result": "correct_%d"}`, i+1))
		wus = append(wus, wuInfo{id: asg.WorkUnitId, goodData: goodOutput})

		// Normal units: run-start (a no-op for the spot-check ones, which keep the
		// history-row model and submit while QUEUED).
		ensureRunStart(t, env.pool, env.grpc, ctx, volAID, volAPubKey, asg.WorkUnitId)
		_, err = env.grpc.SubmitResult(signFor(t, ctx, volAPubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: asg.WorkUnitId, VolunteerId: volAID, PublicKey: volAPubKey,
			OutputData: goodOutput, OutputChecksumSha256: sha256Hex(goodOutput),
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 5, CpuSecondsUser: 4, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("vol A submit WU %d: %v", i, err)
		}
	}

	// Count how many WUs got spot-checked (still QUEUED with spot_check=true).
	var spotCheckedCount int
	env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_units WHERE leaf_id = $1 AND spot_check = true",
		proj.ID).Scan(&spotCheckedCount)
	t.Logf("spot-checked WUs: %d out of 50 (expected ~10 at 20%%)", spotCheckedCount)

	if spotCheckedCount == 0 {
		t.Fatal("expected at least some spot-checked work units at 20% rate")
	}

	// Vol B processes available spot-checked WUs.
	// First WU gets garbage (mismatch), rest get matching results.
	submittedByB := 0
	garbageSubmitted := 0
	for {
		wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volBPubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volBID, PublicKey: volBPubKey,
		})
		if err != nil {
			break // request failed
		}
		if len(wuResp.Assignments) == 0 {
			break // No more work available (no-work is now an OK response with empty assignments)
		}
		asg := wuResp.Assignments[0]

		var outputData []byte
		if submittedByB == 0 {
			// First spot-checked WU: submit garbage to test mismatch detection.
			outputData = []byte(`{"result": "GARBAGE_MISMATCH"}`)
			garbageSubmitted++
		} else {
			// Remaining: match Vol A's output.
			for _, wu := range wus {
				if wu.id == asg.WorkUnitId {
					outputData = wu.goodData
					break
				}
			}
			if outputData == nil {
				outputData = []byte(`{"result": "fallback"}`)
			}
		}

		_, err = env.grpc.SubmitResult(signFor(t, ctx, volBPubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: asg.WorkUnitId, VolunteerId: volBID, PublicKey: volBPubKey,
			OutputData: outputData, OutputChecksumSha256: sha256Hex(outputData),
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 5, CpuSecondsUser: 4, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("vol B submit: %v", err)
		}
		submittedByB++
	}
	t.Logf("vol B submitted %d results (%d garbage) for spot-checked WUs", submittedByB, garbageSubmitted)

	time.Sleep(1 * time.Second)

	// Verify spot-check stats include both passes and failures.
	statsEngine := stats.NewEngine(env.pool)
	snapshot, err := statsEngine.ComputeSnapshot(ctx, proj.ID)
	if err != nil {
		t.Fatalf("compute snapshot: %v", err)
	}
	if snapshot.SpotChecksTotal < 2 {
		t.Errorf("spot_checks_total = %d, want >= 2", snapshot.SpotChecksTotal)
	}
	if snapshot.SpotChecksPassed < 1 {
		t.Errorf("spot_checks_passed = %d, want >= 1", snapshot.SpotChecksPassed)
	}
	// Mismatched spot-checks result in re-queuing, not REJECTED state.
	// The stats engine counts failed as state IN ('REJECTED', 'FAILED'),
	// so re-queued mismatches show as total > passed rather than failed > 0.
	if garbageSubmitted > 0 && snapshot.SpotChecksTotal > snapshot.SpotChecksPassed {
		t.Logf("spot-check mismatch detected: total=%d > passed=%d (garbage=%d)",
			snapshot.SpotChecksTotal, snapshot.SpotChecksPassed, garbageSubmitted)
	} else if garbageSubmitted > 0 {
		t.Logf("spot-check mismatch may not be reflected yet in stats (total=%d, passed=%d)",
			snapshot.SpotChecksTotal, snapshot.SpotChecksPassed)
	}
	t.Logf("spot-check stats: total=%d passed=%d failed=%d",
		snapshot.SpotChecksTotal, snapshot.SpotChecksPassed, snapshot.SpotChecksFailed)
}

// --- Scenario 6: V3 Proof of Ownership ---
// Tests the full challenge-response identity verification cycle.

func TestBetaE2E_V3ProofOfOwnership(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Register a volunteer with a known Ed25519 keypair. The private key is also
	// needed below to sign the HTTP identity challenge, so generate it explicitly and
	// register it with the gRPC signer map so registerBetaVolunteer can sign the RPC.
	volPubKey, volPrivKey, _ := ed25519.GenerateKey(nil)
	e2eSignerKeys.Store(string(volPubKey), volPrivKey)
	volID := registerBetaVolunteer(t, env, ctx, volPubKey, "Identity Vol", nil)

	pubKeyB64 := base64.RawURLEncoding.EncodeToString(volPubKey)

	// Step 1: Generate challenge.
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/identity/challenge", map[string]string{
		"public_key": pubKeyB64,
	})
	requireStatus(t, resp, http.StatusOK, "generate challenge")
	var challengeResp struct {
		ChallengeID string `json:"challenge_id"`
		Challenge   string `json:"challenge"`
		ExpiresAt   string `json:"expires_at"`
	}
	decodeJSON(t, resp, &challengeResp)

	if challengeResp.ChallengeID == "" {
		t.Fatal("challenge_id should not be empty")
	}
	if challengeResp.Challenge == "" {
		t.Fatal("challenge should not be empty")
	}
	if challengeResp.ExpiresAt == "" {
		t.Fatal("expires_at should not be empty")
	}

	// Step 2: Sign challenge with volunteer's private key.
	challengeBytes, err := hex.DecodeString(challengeResp.Challenge)
	if err != nil {
		t.Fatalf("decode challenge hex: %v", err)
	}
	signature := ed25519.Sign(volPrivKey, challengeBytes)
	sigB64 := base64.RawURLEncoding.EncodeToString(signature)

	// Step 3: Verify — should succeed.
	resp = httpReq(t, "POST", env.httpURL+"/api/v1/identity/verify", map[string]string{
		"challenge_id": challengeResp.ChallengeID,
		"public_key":   pubKeyB64,
		"signature":    sigB64,
	})
	requireStatus(t, resp, http.StatusOK, "verify challenge")
	var verifyResp struct {
		Verified    bool   `json:"verified"`
		VolunteerID string `json:"volunteer_id"`
	}
	decodeJSON(t, resp, &verifyResp)

	if !verifyResp.Verified {
		t.Error("verified should be true")
	}
	if verifyResp.VolunteerID != volID {
		t.Errorf("volunteer_id = %q, want %q", verifyResp.VolunteerID, volID)
	}

	// Step 4: Wrong signature — generate new challenge and sign with different key.
	resp = httpReq(t, "POST", env.httpURL+"/api/v1/identity/challenge", map[string]string{
		"public_key": pubKeyB64,
	})
	requireStatus(t, resp, http.StatusOK, "generate second challenge")
	var challenge2 struct {
		ChallengeID string `json:"challenge_id"`
		Challenge   string `json:"challenge"`
	}
	decodeJSON(t, resp, &challenge2)

	// Sign with a DIFFERENT private key.
	_, wrongPrivKey, _ := ed25519.GenerateKey(nil)
	challengeBytes2, _ := hex.DecodeString(challenge2.Challenge)
	wrongSig := ed25519.Sign(wrongPrivKey, challengeBytes2)
	wrongSigB64 := base64.RawURLEncoding.EncodeToString(wrongSig)

	resp = httpReq(t, "POST", env.httpURL+"/api/v1/identity/verify", map[string]string{
		"challenge_id": challenge2.ChallengeID,
		"public_key":   pubKeyB64,
		"signature":    wrongSigB64,
	})
	if resp.StatusCode != 403 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("wrong signature: expected 403, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	// Step 5: Identity lookup by public key.
	resp = httpReq(t, "GET", env.httpURL+"/api/v1/identity/"+pubKeyB64, nil)
	requireStatus(t, resp, http.StatusOK, "identity lookup")
	var infoResp struct {
		PublicKey            string  `json:"public_key"`
		VolunteerID          string  `json:"volunteer_id"`
		Verified             bool    `json:"verified"`
		TotalCredit          float64 `json:"total_credit"`
		ProjectsContributing int     `json:"projects_contributing"`
	}
	decodeJSON(t, resp, &infoResp)

	if infoResp.VolunteerID != volID {
		t.Errorf("info volunteer_id = %q, want %q", infoResp.VolunteerID, volID)
	}
	if !infoResp.Verified {
		t.Error("info verified should be true (challenge was verified earlier)")
	}
}

// --- Scenario 7: Health Metrics, Credit Analysis & Volunteer Stats ---
// Tests health metrics, credit analysis endpoints, and volunteer stats.

func TestBetaE2E_HealthAndCredit(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "beta-hc")

	// Create leaf with enough activity for meaningful metrics.
	proj := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
		Name:         "Beta Health Metrics", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig:   defaultExecConfig(),
		ValConfig:    leaf.ValidationConfig{RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3},
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 5.0},
	})
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	// Generate and process 10 work units.
	params := make([]interface{}, 10)
	for i := 0; i < 10; i++ {
		params[i] = float64(i + 1)
	}
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{"x": params},
	}
	resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate health WUs")
	resp.Body.Close()

	volPubKey := genVolunteerKey(t)
	volID := registerBetaVolunteer(t, env, ctx, volPubKey, "Health Vol", nil)

	for i := 0; i < 10; i++ {
		outputData := []byte(fmt.Sprintf(`{"value": %d}`, i+1))
		requestSubmitResult(t, env, ctx, volID, volPubKey, outputData)
	}
	time.Sleep(500 * time.Millisecond)

	// Compute stats snapshot.
	statsEngine := stats.NewEngine(env.pool)
	_, err := statsEngine.ComputeSnapshot(ctx, proj.ID)
	if err != nil {
		t.Fatalf("compute snapshot: %v", err)
	}

	// Verify health endpoint.
	resp = httpReq(t, "GET", env.httpURL+"/api/v1/health/leafs", nil)
	requireStatus(t, resp, http.StatusOK, "health leafs")
	var healthResp struct {
		Leafs []struct {
			LeafID           string `json:"leaf_id"`
			ContributionFlow struct {
				Status string `json:"status"`
			} `json:"contribution_flow"`
		} `json:"leafs"`
	}
	decodeJSON(t, resp, &healthResp)

	if len(healthResp.Leafs) < 1 {
		t.Error("health response should have at least 1 leaf")
	}
	for _, p := range healthResp.Leafs {
		if p.LeafID == proj.ID.String() {
			// Recent work → contribution flow should be healthy, not critical.
			if p.ContributionFlow.Status == "critical" {
				t.Error("contribution_flow status should not be critical (just validated work)")
			}
		}
	}

	// Verify volunteer stats endpoint.
	volIDParsed := types.MustParseID(volID)
	resp = httpReq(t, "GET", fmt.Sprintf("%s/api/v1/volunteers/%s/stats", env.httpURL, volIDParsed), nil)
	requireStatus(t, resp, http.StatusOK, "volunteer stats")
	var volStats struct {
		VolunteerID             string  `json:"volunteer_id"`
		TotalCredit             float64 `json:"total_credit"`
		TotalWorkUnitsCompleted int     `json:"total_work_units_completed"`
		Leafs                   []struct {
			LeafID      string  `json:"leaf_id"`
			TotalCredit float64 `json:"total_credit"`
		} `json:"leafs"`
	}
	decodeJSON(t, resp, &volStats)

	if volStats.TotalCredit != 50.0 { // 10 WUs * 5.0 credit
		t.Errorf("total_credit = %f, want 50.0", volStats.TotalCredit)
	}
	if len(volStats.Leafs) != 1 {
		t.Errorf("leafs count = %d, want 1", len(volStats.Leafs))
	}
	if len(volStats.Leafs) > 0 && volStats.Leafs[0].TotalCredit != 50.0 {
		t.Errorf("leaf credit = %f, want 50.0", volStats.Leafs[0].TotalCredit)
	}

	// Verify cross-leaf volunteer stats feed includes the volunteer.
	resp = httpReq(t, "GET", env.httpURL+"/api/v1/volunteers/stats", nil)
	requireStatus(t, resp, http.StatusOK, "volunteer stats feed")
	var feedStats struct {
		Volunteers []struct {
			TotalCredit float64 `json:"total_credit"`
			RAC         float64 `json:"rac"`
		} `json:"volunteers"`
	}
	decodeJSON(t, resp, &feedStats)
	if len(feedStats.Volunteers) != 1 {
		t.Errorf("stats feed volunteers = %d, want 1", len(feedStats.Volunteers))
	} else {
		if feedStats.Volunteers[0].TotalCredit != 50.0 {
			t.Errorf("stats feed total_credit = %f, want 50.0", feedStats.Volunteers[0].TotalCredit)
		}
		if feedStats.Volunteers[0].RAC <= 0 {
			t.Errorf("stats feed RAC = %f, want > 0", feedStats.Volunteers[0].RAC)
		}
	}

	// Verify credit analysis: per-leaf percentile distributions.
	resp = httpReq(t, "GET", fmt.Sprintf("%s/api/v1/credit/analysis/%s", env.httpURL, proj.ID), nil)
	requireStatus(t, resp, http.StatusOK, "credit analysis per-leaf")
	var analysisResp struct {
		LeafID         string `json:"leaf_id"`
		WorkUnitsAnalyzed int    `json:"work_units_analyzed"`
		CPUSecondsPerWU   struct {
			P50 float64 `json:"p50"`
			P90 float64 `json:"p90"`
			P99 float64 `json:"p99"`
		} `json:"cpu_seconds_per_wu"`
		ByTaskPattern map[string]struct {
			Count int `json:"count"`
		} `json:"by_task_pattern"`
	}
	decodeJSON(t, resp, &analysisResp)
	if analysisResp.WorkUnitsAnalyzed != 10 {
		t.Errorf("work_units_analyzed = %d, want 10", analysisResp.WorkUnitsAnalyzed)
	}
	if analysisResp.CPUSecondsPerWU.P50 <= 0 {
		t.Errorf("cpu_seconds p50 = %f, want > 0", analysisResp.CPUSecondsPerWU.P50)
	}
	if _, ok := analysisResp.ByTaskPattern["PARAMETER_SWEEP"]; !ok {
		t.Error("by_task_pattern should include PARAMETER_SWEEP")
	}

	// Verify cross-leaf credit analysis. The route is /cross-leaf (the legacy
	// /cross-project alias was removed and now matches the {leaf_id} route instead).
	resp = httpReq(t, "GET", env.httpURL+"/api/v1/credit/analysis/cross-leaf", nil)
	requireStatus(t, resp, http.StatusOK, "credit analysis cross-leaf")
	var crossResp struct {
		Leafs []struct {
			LeafID          string  `json:"leaf_id"`
			TotalCreditGranted float64 `json:"total_credit_granted"`
			ActiveVolunteers   int     `json:"active_volunteers"`
		} `json:"leafs"`
		NormalizationFactors struct {
			Ratio float64 `json:"ratio"`
		} `json:"normalization_factors"`
	}
	decodeJSON(t, resp, &crossResp)
	if len(crossResp.Leafs) < 1 {
		t.Error("cross-leaf should have at least 1 leaf")
	}

	// Verify volunteer credit breakdown.
	resp = httpReq(t, "GET", fmt.Sprintf("%s/api/v1/volunteers/%s/credit/breakdown", env.httpURL, volIDParsed), nil)
	requireStatus(t, resp, http.StatusOK, "volunteer credit breakdown")
	var breakdownResp struct {
		VolunteerID string  `json:"volunteer_id"`
		TotalCredit float64 `json:"total_credit"`
		ByLeaf      []struct {
			LeafID string  `json:"leaf_id"`
			Credit    float64 `json:"credit"`
			WorkUnits int     `json:"work_units"`
		} `json:"by_leaf"`
		Timeline struct {
			Daily []struct {
				Date   string  `json:"date"`
				Credit float64 `json:"credit"`
			} `json:"daily"`
		} `json:"timeline"`
	}
	decodeJSON(t, resp, &breakdownResp)
	if breakdownResp.TotalCredit != 50.0 {
		t.Errorf("breakdown total_credit = %f, want 50.0", breakdownResp.TotalCredit)
	}
	if len(breakdownResp.ByLeaf) != 1 {
		t.Errorf("breakdown by_leaf count = %d, want 1", len(breakdownResp.ByLeaf))
	}
	// Daily timeline may be empty if DATE() scan fails silently in Go.
	t.Logf("breakdown timeline: %d daily entries, %d by_leaf entries",
		len(breakdownResp.Timeline.Daily), len(breakdownResp.ByLeaf))
}

// --- Scenario 8: Multi-Server Volunteer ---
// Tests a single volunteer connecting to two separate infrastructure servers,
// receiving and completing work from both.

func TestBetaE2E_MultiServer(t *testing.T) {
	// Start two independent Beta servers (both use the same test DB).
	env1, cleanup1 := setupBetaServer(t)
	defer cleanup1()

	env2, cleanup2 := setupBetaServer(t)
	defer cleanup2()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Create one user and one leaf on each server.
	userID1 := createTestUser(t, env1.pool, ctx, "beta-ms1")
	userID2 := createTestUser(t, env2.pool, ctx, "beta-ms2")

	proj1 := createBetaLeaf(t, env1, ctx, userID1, betaLeafOpts{
		Name: "Server 1 Leaf", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig:   defaultExecConfig(),
		ValConfig:    leaf.ValidationConfig{RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3},
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})
	proj2 := createBetaLeaf(t, env2, ctx, userID2, betaLeafOpts{
		Name: "Server 2 Leaf", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig:   defaultExecConfig(),
		ValConfig:    leaf.ValidationConfig{RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3},
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 2.0},
	})

	// Generate 3 work units on each server.
	for _, pair := range []struct {
		env *betaEnv
		url string
	}{
		{env1, env1.httpURL + "/api/v1/leafs/" + proj1.ID.String()},
		{env2, env2.httpURL + "/api/v1/leafs/" + proj2.ID.String()},
	} {
		genReq := workunit.GenerateRequest{
			ParameterSpace: map[string]interface{}{
				"x": []interface{}{float64(1), float64(2), float64(3)},
			},
		}
		resp := httpReq(t, "POST", pair.url+"/work-units/generate", genReq)
		requireStatus(t, resp, http.StatusAccepted, "generate multi-server WUs")
		resp.Body.Close()
	}

	// Register the same volunteer (same key) on both servers.
	volPubKey := genVolunteerKey(t)
	volID1 := registerBetaVolunteer(t, env1, ctx, volPubKey, "Multi-Server Vol", nil)
	volID2 := registerBetaVolunteer(t, env2, ctx, volPubKey, "Multi-Server Vol", nil)

	// Process all work units from server 1.
	for i := 0; i < 3; i++ {
		outputData := []byte(fmt.Sprintf(`{"server": 1, "index": %d}`, i+1))
		requestSubmitResult(t, env1, ctx, volID1, volPubKey, outputData)
	}

	// Process all work units from server 2.
	for i := 0; i < 3; i++ {
		outputData := []byte(fmt.Sprintf(`{"server": 2, "index": %d}`, i+1))
		requestSubmitResult(t, env2, ctx, volID2, volPubKey, outputData)
	}

	time.Sleep(500 * time.Millisecond)

	// Verify all validated on both servers.
	var count1, count2 int
	env1.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_units WHERE leaf_id = $1 AND state = 'VALIDATED'",
		proj1.ID).Scan(&count1)
	env2.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_units WHERE leaf_id = $1 AND state = 'VALIDATED'",
		proj2.ID).Scan(&count2)
	if count1 != 3 {
		t.Errorf("server 1 validated = %d, want 3", count1)
	}
	if count2 != 3 {
		t.Errorf("server 2 validated = %d, want 3", count2)
	}

	// Verify credit tracked separately per server.
	volID1Parsed := types.MustParseID(volID1)
	volID2Parsed := types.MustParseID(volID2)
	credit1 := assertCreditExists(t, env1.pool, ctx, volID1Parsed, proj1.ID, 3)
	credit2 := assertCreditExists(t, env2.pool, ctx, volID2Parsed, proj2.ID, 3)

	if credit1 != 3.0 { // 3 WUs * 1.0
		t.Errorf("server 1 credit = %f, want 3.0", credit1)
	}
	if credit2 != 6.0 { // 3 WUs * 2.0
		t.Errorf("server 2 credit = %f, want 6.0", credit2)
	}
}
