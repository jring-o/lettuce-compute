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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// graceStartedUnit drives a fresh redundancy-1 leaf to the point where one
// volunteer holds an OPEN, run-started copy of a single work unit. It returns the
// volunteer key/id, public key, and the work unit id so a test can simulate the
// copy's deadline lapsing and then submit late.
func graceStartedUnit(t *testing.T, pool *pgxpool.Pool, grpcClient lettucev1.VolunteerServiceClient, httpURL string) (e2eVolunteerKey, string, []byte, types.ID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	userID := types.NewID()
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		userID,
		fmt.Sprintf("e2e-grace-%s@test.example.com", uuid.New().String()[:8]),
		fmt.Sprintf("e2e-grace-%s", uuid.New().String()[:8]),
		"E2E Grace Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}

	createReq := leaf.CreateLeafRequest{
		Name:         "E2E Late Result Grace",
		Description:  "End-to-end test for late-result grace acceptance",
		ResearchArea: []string{"distributed-computing"},
		TaskPattern:  leaf.PatternParameterSweep,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}
	resp := e2eRequest(t, "POST", httpURL+"/api/v1/leafs", createReq)
	e2eRequireStatus(t, resp, http.StatusCreated, "create leaf")
	var proj leaf.Leaf
	e2eDecode(t, resp, &proj)
	leafURL := httpURL + "/api/v1/leafs/" + proj.ID.String()

	resp = e2eRequest(t, "POST", leafURL+"/configure", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "configure")
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
		RedundancyFactor:   1,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	ftCfg := leaf.FaultToleranceConfig{
		DeadlineMultiplier: 3.0,
		MaxReassignments:   3,
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
	resp = e2eRequest(t, "PUT", leafURL, leaf.UpdateLeafRequest{
		ExecutionConfig:      &execCfg,
		ValidationConfig:     &valCfg,
		FaultToleranceConfig: &ftCfg,
		DataConfig:           &dataCfg,
	})
	e2eRequireStatus(t, resp, http.StatusOK, "update configs")
	e2eDecode(t, resp, &proj)

	resp = e2eRequest(t, "POST", leafURL+"/activate", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "activate")

	resp = e2eRequest(t, "POST", leafURL+"/work-units/generate", workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{"x": []interface{}{float64(42)}},
	})
	e2eRequireStatus(t, resp, http.StatusAccepted, "generate work units")
	var genResp workunit.GenerateResponse
	e2eDecode(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 1 {
		t.Fatalf("work_units_created = %d, want 1", genResp.WorkUnitsCreated)
	}

	volKey := newE2EVolunteerKey(t)
	pubKey := []byte(volKey.pub)
	regResp, err := grpcClient.RegisterVolunteer(volKey.sign(ctx), &lettucev1.RegisterVolunteerRequest{
		PublicKey:   pubKey,
		DisplayName: "E2E Grace Volunteer",
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 8, CpuModel: "Test CPU", MaxCpuCores: 4, MemoryTotalMb: 16384, MaxMemoryMb: 8192,
		},
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	volID := regResp.VolunteerId

	wuResp, err := grpcClient.RequestWorkUnit(volKey.sign(ctx), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volID, PublicKey: pubKey,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit: %v", err)
	}
	if len(wuResp.Assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(wuResp.Assignments))
	}
	wuIDStr := wuResp.Assignments[0].WorkUnitId

	swResp, err := grpcClient.StartWork(volKey.sign(ctx), &lettucev1.StartWorkRequest{
		WorkUnitId: wuIDStr, VolunteerId: volID,
	})
	if err != nil {
		t.Fatalf("StartWork: %v", err)
	}
	if !swResp.Ok {
		t.Fatalf("StartWork ok=false: %s", swResp.Message)
	}

	return volKey, volID, pubKey, types.MustParseID(wuIDStr)
}

// graceSubmit submits a result for wuID as volunteer volID.
func graceSubmit(t *testing.T, grpcClient lettucev1.VolunteerServiceClient, volKey e2eVolunteerKey, volID string, pubKey []byte, wuID types.ID) (*lettucev1.SubmitResultResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	outputData := []byte(`{"result": "grace_test", "value": 42.0}`)
	hash := sha256.Sum256(outputData)
	return grpcClient.SubmitResult(volKey.sign(ctx), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuID.String(),
		VolunteerId:          volID,
		PublicKey:            pubKey,
		OutputData:           outputData,
		OutputChecksumSha256: hex.EncodeToString(hash[:]),
		Metadata: &lettucev1.ExecutionMetadata{
			WallClockSeconds: 120, CpuSecondsUser: 100, CpuCoresUsed: 4, PeakMemoryMb: 2048,
		},
	})
}

// TestE2ELateResultGrace_Accepted: a volunteer whose copy deadline lapsed (the
// fault monitor closed it EXPIRED) submits its finished result while the unit is
// still un-finalized. The head must accept it under grace rather than discard the
// work, complete/validate the unit, mark the closed copy COMPLETED, and credit it.
func TestE2ELateResultGrace_Accepted(t *testing.T) {
	pool, grpcClient, httpURL, cleanup := setupF05Server(t)
	defer cleanup()
	ctx := context.Background()

	volKey, volID, pubKey, wuID := graceStartedUnit(t, pool, grpcClient, httpURL)
	volUUID := types.MustParseID(volID)

	// Simulate the copy's deadline lapsing: the fault monitor closes the open copy
	// as EXPIRED. The unit stays non-terminal (it would be re-queued for a corroborator).
	if _, err := pool.Exec(ctx,
		"UPDATE work_unit_assignment_history SET outcome='EXPIRED', outcome_at=NOW() WHERE work_unit_id=$1 AND volunteer_id=$2 AND outcome IS NULL",
		wuID, volUUID); err != nil {
		t.Fatalf("expire copy: %v", err)
	}

	var openCount int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id=$1 AND volunteer_id=$2 AND outcome IS NULL",
		wuID, volUUID).Scan(&openCount); err != nil {
		t.Fatalf("count open copies: %v", err)
	}
	if openCount != 0 {
		t.Fatalf("expected 0 open copies after expiry, got %d", openCount)
	}

	// Late submit must be ACCEPTED under grace.
	resp, err := graceSubmit(t, grpcClient, volKey, volID, pubKey, wuID)
	if err != nil {
		t.Fatalf("late SubmitResult returned error: %v", err)
	}
	if !resp.Accepted {
		t.Fatalf("late result not accepted under grace: %q", resp.Message)
	}

	// The unit must progress (redundancy 1 auto-validates a single result).
	var wuState string
	if err := pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id=$1", wuID).Scan(&wuState); err != nil {
		t.Fatalf("query work unit state: %v", err)
	}
	if wuState != "COMPLETED" && wuState != "VALIDATED" {
		t.Errorf("work unit state = %q, want COMPLETED or VALIDATED", wuState)
	}

	// The previously-EXPIRED copy must now be COMPLETED (it carried the result).
	var outcome string
	if err := pool.QueryRow(ctx,
		"SELECT outcome FROM work_unit_assignment_history WHERE work_unit_id=$1 AND volunteer_id=$2",
		wuID, volUUID).Scan(&outcome); err != nil {
		t.Fatalf("query copy outcome: %v", err)
	}
	if outcome != "COMPLETED" {
		t.Errorf("copy outcome = %q, want COMPLETED (grace completion)", outcome)
	}

	// Credit must have been granted for the rescued work.
	var creditRows int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM credit_ledger WHERE work_unit_id=$1 AND volunteer_id=$2",
		wuID, volUUID).Scan(&creditRows); err != nil {
		t.Fatalf("query credit ledger: %v", err)
	}
	if creditRows != 1 {
		t.Errorf("credit_ledger rows = %d, want 1 (late result credited)", creditRows)
	}
}

// TestE2ELateResultGrace_RejectedWhenFinalized: once the unit is finalized
// (VALIDATED/FAILED) its result was already assimilated, so a late submit is
// genuinely useless and must be rejected rather than rescued.
func TestE2ELateResultGrace_RejectedWhenFinalized(t *testing.T) {
	pool, grpcClient, httpURL, cleanup := setupF05Server(t)
	defer cleanup()
	ctx := context.Background()

	volKey, volID, pubKey, wuID := graceStartedUnit(t, pool, grpcClient, httpURL)
	volUUID := types.MustParseID(volID)

	if _, err := pool.Exec(ctx,
		"UPDATE work_unit_assignment_history SET outcome='EXPIRED', outcome_at=NOW() WHERE work_unit_id=$1 AND volunteer_id=$2 AND outcome IS NULL",
		wuID, volUUID); err != nil {
		t.Fatalf("expire copy: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE work_units SET state='VALIDATED', validated_at=NOW() WHERE id=$1", wuID); err != nil {
		t.Fatalf("finalize unit: %v", err)
	}

	resp, err := graceSubmit(t, grpcClient, volKey, volID, pubKey, wuID)
	if err == nil {
		t.Fatalf("expected a FailedPrecondition error for a finalized unit, got accepted=%v msg=%q",
			resp.GetAccepted(), resp.GetMessage())
	}
}
