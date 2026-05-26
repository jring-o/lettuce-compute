//go:build integration

package internal_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestE2EF07VolunteerProtocol tests the full volunteer protocol flow from the
// server's perspective: register → request → heartbeat → submit → validate.
// It simulates a volunteer by making direct gRPC calls (same pattern as F05).
func TestE2EF07VolunteerProtocol(t *testing.T) {
	pool, grpcClient, httpURL, cleanup := setupF05Server(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// --- Step 1: Create test user and leaf ---
	userID := types.NewID()
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		userID,
		fmt.Sprintf("e2e-f07-%s@test.example.com", uuid.New().String()[:8]),
		fmt.Sprintf("e2e-f07-%s", uuid.New().String()[:8]),
		"E2E F07 Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("step 1: create test user: %v", err)
	}

	createReq := leaf.CreateLeafRequest{
		Name:         "E2E F07 Volunteer Protocol",
		Description:  "End-to-end test for volunteer protocol flow",
		ResearchArea: []string{"distributed-computing"},
		TaskPattern:  leaf.PatternParameterSweep,
		IsOngoing:    false,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}
	resp := e2eRequest(t, "POST", httpURL+"/api/v1/leafs", createReq)
	e2eRequireStatus(t, resp, http.StatusCreated, "1a: create leaf")
	var proj leaf.Leaf
	e2eDecode(t, resp, &proj)

	leafURL := httpURL + "/api/v1/leafs/" + proj.ID.String()

	// --- Step 2: Configure and activate leaf ---
	resp = e2eRequest(t, "POST", leafURL+"/configure", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "2a: configure")
	e2eDecode(t, resp, &proj)

	execCfg := leaf.ExecutionConfig{
		Runtime:         "NATIVE",
		Binaries:        map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		GPUType:         "ANY",
		MaxMemoryMB:     4096,
		MaxDiskMB:       10240,
		MaxCPUSeconds:   3600,
	}
	valCfg := leaf.ValidationConfig{
		RedundancyFactor:   1, // Single result is enough
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	ftCfg := leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds:  60,
		MissedHeartbeatsThreshold: 3,
		DeadlineMultiplier:        3.0,
		MaxReassignments:          3,
	}
	dataCfg := leaf.DataConfig{
		TransferStrategy:   "INLINE",
		AggregationFormat:  "JSON",
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
		SplittingConfig: map[string]interface{}{
			"x": []interface{}{float64(42)},
		},
	}
	updateReq := leaf.UpdateLeafRequest{
		ExecutionConfig:      &execCfg,
		ValidationConfig:     &valCfg,
		FaultToleranceConfig: &ftCfg,
		DataConfig:           &dataCfg,
	}
	resp = e2eRequest(t, "PUT", leafURL, updateReq)
	e2eRequireStatus(t, resp, http.StatusOK, "2b: update configs")
	e2eDecode(t, resp, &proj)

	resp = e2eRequest(t, "POST", leafURL+"/activate", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "2c: activate")
	e2eDecode(t, resp, &proj)
	if proj.State != leaf.StateActive {
		t.Fatalf("step 2: state = %q, want ACTIVE", proj.State)
	}

	// --- Step 3: Generate 1 work unit ---
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"x": []interface{}{float64(42)},
		},
	}
	resp = e2eRequest(t, "POST", leafURL+"/work-units/generate", genReq)
	e2eRequireStatus(t, resp, http.StatusAccepted, "3: generate work units")
	var genResp workunit.GenerateResponse
	e2eDecode(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 1 {
		t.Fatalf("step 3: work_units_created = %d, want 1", genResp.WorkUnitsCreated)
	}

	// --- Step 4: Register volunteer ---
	volKey := newE2EVolunteerKey(t)
	pubKey := []byte(volKey.pub)
	regResp, err := grpcClient.RegisterVolunteer(volKey.sign(ctx), &lettucev1.RegisterVolunteerRequest{
		PublicKey:   pubKey,
		DisplayName: "E2E F07 Volunteer",
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores:      8,
			CpuModel:      "Test CPU",
			MaxCpuCores:   4,
			MemoryTotalMb: 16384,
			MaxMemoryMb:   8192,
		},
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	})
	if err != nil {
		t.Fatalf("step 4: RegisterVolunteer: %v", err)
	}
	if !regResp.Registered {
		t.Fatal("step 4: expected registered = true")
	}
	volID := regResp.VolunteerId
	if volID == "" {
		t.Fatal("step 4: expected non-empty volunteer_id")
	}

	// --- Step 5: Request work unit ---
	wuResp, err := grpcClient.RequestWorkUnit(volKey.sign(ctx), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volID,
		PublicKey:   pubKey,
	})
	if err != nil {
		t.Fatalf("step 5: RequestWorkUnit: %v", err)
	}
	if wuResp.WorkUnitId == "" {
		t.Fatal("step 5: expected non-empty work_unit_id")
	}
	if wuResp.ProjectId != proj.ID.String() {
		t.Errorf("step 5: leaf_id = %q, want %q", wuResp.ProjectId, proj.ID.String())
	}
	if wuResp.Runtime != "NATIVE" {
		t.Errorf("step 5: runtime = %q, want NATIVE", wuResp.Runtime)
	}
	if wuResp.HeartbeatIntervalSeconds <= 0 {
		t.Errorf("step 5: heartbeat_interval_seconds = %d, want > 0", wuResp.HeartbeatIntervalSeconds)
	}
	if len(wuResp.InputData) == 0 && wuResp.InputDataUrl == "" && wuResp.ParametersJson == "" {
		t.Error("step 5: expected input_data, input_data_url, or parameters_json")
	}

	// --- Step 6: Send heartbeat ---
	hbResp, err := grpcClient.Heartbeat(volKey.sign(ctx), &lettucev1.HeartbeatRequest{
		WorkUnitId:  wuResp.WorkUnitId,
		VolunteerId: volID,
		Status:      "RUNNING",
		ProgressPct: 0.0,
	})
	if err != nil {
		t.Fatalf("step 6: Heartbeat: %v", err)
	}
	if !hbResp.ContinueExecution {
		t.Errorf("step 6: continue_execution = false, want true")
	}

	// --- Step 7: Submit result ---
	outputData := []byte(`{"result": "f07_test_complete", "value": 42.0}`)
	hash := sha256.Sum256(outputData)
	checksum := hex.EncodeToString(hash[:])

	submitResp, err := grpcClient.SubmitResult(volKey.sign(ctx), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuResp.WorkUnitId,
		VolunteerId:          volID,
		PublicKey:            pubKey,
		OutputData:           outputData,
		OutputChecksumSha256: checksum,
		Metadata: &lettucev1.ExecutionMetadata{
			WallClockSeconds: 120,
			CpuSecondsUser:   100.5,
			CpuSecondsSystem: 5.2,
			CpuCoresUsed:     4,
			PeakMemoryMb:     2048,
			DiskReadMb:       50,
			DiskWriteMb:      10,
		},
	})
	if err != nil {
		t.Fatalf("step 7: SubmitResult: %v", err)
	}
	if !submitResp.Accepted {
		t.Fatalf("step 7: expected accepted = true, message = %q", submitResp.Message)
	}
	if submitResp.ResultId == "" {
		t.Fatal("step 7: expected non-empty result_id")
	}

	// --- Step 8: Verify work unit state ---
	wuID, _ := types.ParseID(wuResp.WorkUnitId)
	var wuState string
	err = pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", wuID).Scan(&wuState)
	if err != nil {
		t.Fatalf("step 8: query work unit: %v", err)
	}
	if wuState != "COMPLETED" && wuState != "VALIDATED" {
		t.Errorf("step 8: work unit state = %q, want COMPLETED or VALIDATED", wuState)
	}

	// --- Step 9: Verify no more work available ---
	_, err = grpcClient.RequestWorkUnit(volKey.sign(ctx), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volID,
		PublicKey:   pubKey,
	})
	if err == nil {
		t.Fatal("step 9: expected error for no work available")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("step 9: expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("step 9: expected NotFound, got %s: %s", st.Code(), st.Message())
	}

	// --- Step 10: Verify volunteer ID stability ---
	// Re-register with the same public key — should return same volunteer ID.
	regResp2, err := grpcClient.RegisterVolunteer(volKey.sign(ctx), &lettucev1.RegisterVolunteerRequest{
		PublicKey:   pubKey,
		DisplayName: "E2E F07 Volunteer Updated",
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores:      16,
			CpuModel:      "Upgraded CPU",
			MaxCpuCores:   8,
			MemoryTotalMb: 32768,
			MaxMemoryMb:   16384,
		},
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	})
	if err != nil {
		t.Fatalf("step 10: re-register: %v", err)
	}
	if regResp2.VolunteerId != volID {
		t.Errorf("step 10: volunteer ID changed on re-register: %q != %q", regResp2.VolunteerId, volID)
	}
	if regResp2.Registered {
		t.Error("step 10: expected registered = false on re-register")
	}

	// --- Step 11: Verify assignment history ---
	var assignOutcome string
	err = pool.QueryRow(ctx,
		"SELECT outcome FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2",
		wuID, types.MustParseID(volID),
	).Scan(&assignOutcome)
	if err != nil {
		t.Fatalf("step 11: query assignment history: %v", err)
	}
	if assignOutcome != "COMPLETED" {
		t.Errorf("step 11: assignment outcome = %q, want COMPLETED", assignOutcome)
	}

	// --- Step 12: Verify result checksum matches ---
	resultID, _ := types.ParseID(submitResp.ResultId)
	var storedChecksum string
	err = pool.QueryRow(ctx,
		"SELECT output_checksum FROM results WHERE id = $1",
		resultID,
	).Scan(&storedChecksum)
	if err != nil {
		t.Fatalf("step 12: query result checksum: %v", err)
	}
	if storedChecksum != checksum {
		t.Errorf("step 12: stored checksum = %q, want %q", storedChecksum, checksum)
	}
}
