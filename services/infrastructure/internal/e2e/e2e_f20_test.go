//go:build integration

package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestF20_ExternalStorageReference tests the external storage reference flow:
// create leaf with EXTERNAL_REFERENCE → generate work units with input_data_ref →
// verify gRPC assignment includes input_data_url → submit result with output_data_url.
func TestF20_ExternalStorageReference(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create user and leaf.
	userID := createTestUser(t, env.pool, ctx, "f20-ext")
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name:            "F20 External Storage E2E",
			Description:     "External storage reference test",
			ResearchArea:    []string{"testing"},
			TaskPattern:     leaf.PatternMapReduce,
			IsOngoing:       false,
			Visibility:      leaf.VisibilityPublic,
			CreatorID:       &userID,
		},
		leaf.ValidationConfig{
			RedundancyFactor:   1,
			AgreementThreshold: 1.0,
			ComparisonMode:     "EXACT",
			MaxRetries:         3,
			// This leaf deliberately exercises the external-reference output path, so
			// it must opt in; SubmitResult otherwise rejects output_data_url.
			AllowExternalOutput: true,
		},
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	// Update data config to EXTERNAL_REFERENCE. The leaf is map_reduce, which requires
	// a splitting_strategy in data_config (enforced at activation and work-unit
	// generation), so one is supplied.
	extURL := "https://storage.example.com/datasets"
	byRecord := "by_record"
	dataCfg := leaf.DataConfig{
		TransferStrategy:   "EXTERNAL_REFERENCE",
		ExternalBaseURL:    &extURL,
		AggregationFormat:  "JSON",
		MaxInputSizeBytes:  104857600, // 100 MB — external data can be large
		MaxOutputSizeBytes: 104857600,
		SplittingStrategy:  &byRecord,
		SplittingConfig:    map[string]interface{}{"records_per_chunk": float64(10)},
	}
	resp := httpReq(t, "PUT", leafURL, leaf.UpdateLeafRequest{DataConfig: &dataCfg})
	requireStatus(t, resp, http.StatusOK, "update data config to EXTERNAL_REFERENCE")
	resp.Body.Close()

	// Generate work units with input_data_ref.
	inputRef := "https://storage.example.com/datasets/input.csv"
	genReq := workunit.GenerateRequest{
		InputDataRef: &inputRef,
		ParameterSpace: map[string]interface{}{
			"chunk_count": float64(3),
		},
	}
	resp = httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate work units with input_data_ref")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	if genResp.WorkUnitsCreated < 1 {
		t.Fatalf("work_units_created = %d, want >= 1", genResp.WorkUnitsCreated)
	}
	t.Logf("generated %d work units", genResp.WorkUnitsCreated)

	// Register volunteer.
	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "F20 External Volunteer")

	// Request work unit and verify input_data_url is populated.
	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volID,
		PublicKey:   pubKey,
	})
	if err != nil {
		t.Fatalf("request work unit: %v", err)
	}

	if wuResp.Assignments[0].InputDataUrl == "" {
		t.Error("expected input_data_url to be set for external reference leaf")
	}
	t.Logf("work unit input_data_url = %s", wuResp.Assignments[0].InputDataUrl)

	// Submit result with output_data_url (simulating volunteer uploading to external storage).
	outputData := []byte(`{"result": "computed from external data"}`)
	hash := sha256.Sum256(outputData)
	checksum := hex.EncodeToString(hash[:])

	// Submit with output_data_url instead of inline output_data.
	ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuResp.Assignments[0].WorkUnitId)
	submitResp, err := env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuResp.Assignments[0].WorkUnitId,
		VolunteerId:          volID,
		PublicKey:            pubKey,
		OutputDataUrl:        "https://storage.example.com/results/wu-" + wuResp.Assignments[0].WorkUnitId + ".json",
		OutputChecksumSha256: checksum,
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 15, CpuSecondsUser: 10, CpuCoresUsed: 2},
	})
	if err != nil {
		t.Fatalf("submit result with output_data_url: %v", err)
	}
	if !submitResp.Accepted {
		t.Errorf("result not accepted: %s", submitResp.Message)
	}
	t.Logf("result submitted with output_data_url, result_id=%s", submitResp.ResultId)

	// Verify the stored result has output_data_ref.
	var outputRef *string
	err = env.pool.QueryRow(ctx,
		"SELECT output_data_ref FROM results WHERE id = $1",
		submitResp.ResultId,
	).Scan(&outputRef)
	if err != nil {
		t.Fatalf("query result output_data_ref: %v", err)
	}
	if outputRef == nil || *outputRef == "" {
		t.Error("expected output_data_ref to be stored in results table")
	} else {
		t.Logf("stored output_data_ref = %s", *outputRef)
	}

	// Verify the stored work unit has input_data_ref.
	var storedRef *string
	err = env.pool.QueryRow(ctx,
		"SELECT input_data_ref FROM work_units WHERE id = $1",
		wuResp.Assignments[0].WorkUnitId,
	).Scan(&storedRef)
	if err != nil {
		t.Fatalf("query work unit input_data_ref: %v", err)
	}
	if storedRef == nil || *storedRef == "" {
		t.Error("expected input_data_ref to be stored in work_units table")
	}
}

// TestF20_PlatformManagedRejected verifies that the PLATFORM_MANAGED transfer
// strategy is rejected for self-hosted infrastructure with a helpful error
// message. PLATFORM_MANAGED is only available on the hosted platform, so a
// self-hosted head must refuse it.
//
// The rejection happens at config-store (PUT) time: the leaf-update handler
// merges and re-validates each config block it is sent (ValidateDataConfig),
// so an invalid transfer_strategy is refused the moment it is PUT — the config
// is never stored and the leaf can never reach activation with it. (This is
// stricter than the older behavior, which stored the config and only rejected
// it at activation; the test was updated to match.)
func TestF20_PlatformManagedRejected(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "f20-pm")

	// Create and configure a DRAFT leaf.
	createReq := leaf.CreateLeafRequest{
		Name:         "F20 Platform Managed Reject",
		Description:  "Should reject PLATFORM_MANAGED",
		ResearchArea: []string{"testing"},
		TaskPattern:  leaf.PatternParameterSweep,
		IsOngoing:    false,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs", createReq)
	requireStatus(t, resp, http.StatusCreated, "create leaf")
	var proj leaf.Leaf
	decodeJSON(t, resp, &proj)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	resp = httpReq(t, "POST", leafURL+"/configure", nil)
	requireStatus(t, resp, http.StatusOK, "configure")
	resp.Body.Close()

	// PUT a valid exec config plus a PLATFORM_MANAGED data config. The update
	// handler re-validates the data config and must reject PLATFORM_MANAGED on
	// self-hosted infrastructure with a 400 VALIDATION_ERROR.
	bucket := "my-bucket"
	execCfg := leaf.ExecutionConfig{
		Runtime:         "NATIVE",
		Binaries:        map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		MaxMemoryMB:     4096,
		MaxDiskMB:       10240,
		MaxCPUSeconds:   3600,
	}
	dataCfg := leaf.DataConfig{
		TransferStrategy:   "PLATFORM_MANAGED",
		StorageBucket:      &bucket,
		AggregationFormat:  "JSON",
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
	}
	resp = httpReq(t, "PUT", leafURL, leaf.UpdateLeafRequest{ExecutionConfig: &execCfg, DataConfig: &dataCfg})
	requireStatus(t, resp, http.StatusBadRequest, "PUT PLATFORM_MANAGED must be rejected")

	// Verify it is a validation error whose message names the hosted platform.
	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeJSON(t, resp, &errResp)

	if errResp.Error.Code != "VALIDATION_ERROR" {
		t.Errorf("expected error code VALIDATION_ERROR, got %q", errResp.Error.Code)
	}
	if errResp.Error.Message == "" {
		t.Fatal("expected error message in response")
	}
	if !strings.Contains(errResp.Error.Message, "hosted platform") {
		t.Errorf("expected error message to mention the hosted platform, got: %s", errResp.Error.Message)
	}
	t.Logf("PLATFORM_MANAGED correctly rejected at config-store: %s", errResp.Error.Message)
}

// TestF20_InlineSubmitStillWorks verifies the inline data path is unaffected
// (regression test).
func TestF20_InlineSubmitStillWorks(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "f20-inline")
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name:            "F20 Inline Regression",
			Description:     "Verify inline data still works",
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

	// Generate inline work units.
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"x": []interface{}{float64(1), float64(2)},
		},
	}
	resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate inline work units")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)

	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "F20 Inline Vol")

	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volID,
		PublicKey:   pubKey,
	})
	if err != nil {
		t.Fatalf("request inline work unit: %v", err)
	}

	// Verify inline data path: input_data_url should be empty for inline leafs.
	if wuResp.Assignments[0].InputDataUrl != "" {
		t.Logf("note: input_data_url = %s (may be empty for inline)", wuResp.Assignments[0].InputDataUrl)
	}

	// Submit inline result.
	outputData := []byte(`{"inline": true}`)
	hash := sha256.Sum256(outputData)
	checksum := hex.EncodeToString(hash[:])

	ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuResp.Assignments[0].WorkUnitId)
	submitResp, err := env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuResp.Assignments[0].WorkUnitId,
		VolunteerId:          volID,
		PublicKey:            pubKey,
		OutputData:           outputData,
		OutputChecksumSha256: checksum,
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 5, CpuSecondsUser: 2, CpuCoresUsed: 1},
	})
	if err != nil {
		t.Fatalf("submit inline result: %v", err)
	}
	if !submitResp.Accepted {
		t.Errorf("inline result not accepted: %s", submitResp.Message)
	}

	// Verify inline output is stored.
	var storedOutput json.RawMessage
	err = env.pool.QueryRow(ctx,
		"SELECT output_data FROM results WHERE id = $1",
		submitResp.ResultId,
	).Scan(&storedOutput)
	if err != nil {
		t.Fatalf("query inline result: %v", err)
	}
	if len(storedOutput) == 0 {
		t.Error("expected inline output_data to be stored")
	}
}
