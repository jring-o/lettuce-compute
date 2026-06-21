//go:build integration

package internal_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestE2EV03Lifecycle covers the full v0.3 distribution engine:
// distribution → execution → validation → fault tolerance → credit.
// Five scenarios: happy path, disagreement+reassignment, heartbeat timeout,
// max reassignments → FAILED, and assignment history audit.
//
// Note: The current assignment model transitions QUEUED→ASSIGNED on first assignment,
// so redundant assignments for the same work unit require direct SQL setup.
// This is the expected Alpha behavior — true multi-assignment will be part of Beta.
func TestE2EV03Lifecycle(t *testing.T) {
	pool, grpcClient, httpURL, cleanup := setupF05Server(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- Setup: Create user and leaf ---
	userID := types.NewID()
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		userID,
		fmt.Sprintf("e2e-v03-%s@test.example.com", uuid.New().String()[:8]),
		fmt.Sprintf("e2e-v03-%s", uuid.New().String()[:8]),
		"E2E V03 Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}

	createReq := leaf.CreateLeafRequest{
		Name:         "E2E V03 Lifecycle Project",
		Description:  "End-to-end test for v0.3 distribution engine",
		ResearchArea: []string{"physics"},
		TaskPattern:  leaf.PatternParameterSweep,
		IsOngoing:    false,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}
	resp := e2eRequest(t, "POST", httpURL+"/api/v1/leafs", createReq)
	e2eRequireStatus(t, resp, http.StatusCreated, "setup: create leaf")
	var proj leaf.Leaf
	e2eDecode(t, resp, &proj)

	leafURL := httpURL + "/api/v1/leafs/" + proj.ID.String()

	// Configure leaf.
	resp = e2eRequest(t, "POST", leafURL+"/configure", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "setup: configure")
	e2eDecode(t, resp, &proj)

	// Update with validation config: redundancy_factor=2, exact comparison.
	execCfg := leaf.ExecutionConfig{
		Runtime:         "NATIVE",
		Binaries:        map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		MaxMemoryMB:     4096,
		MaxDiskMB:       10240,
		MaxCPUSeconds:   86400,
	}
	valCfg := leaf.ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	ftCfg := leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds:  60,
		MissedHeartbeatsThreshold: 2,
		DeadlineMultiplier:        3.0,
		MaxReassignments:          3,
	}
	dataCfg := leaf.DataConfig{
		TransferStrategy:   "INLINE",
		AggregationFormat:  "JSON",
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
		SplittingConfig: map[string]interface{}{
			"x": []interface{}{float64(1), float64(2), float64(3)},
		},
	}
	creditCfg := leaf.CreditConfig{
		CreditPerValidatedWorkUnit: 1.0,
	}
	updateReq := leaf.UpdateLeafRequest{
		ExecutionConfig:      &execCfg,
		ValidationConfig:     &valCfg,
		FaultToleranceConfig: &ftCfg,
		DataConfig:           &dataCfg,
		CreditConfig:         &creditCfg,
	}
	resp = e2eRequest(t, "PUT", leafURL, updateReq)
	e2eRequireStatus(t, resp, http.StatusOK, "setup: update configs")
	e2eDecode(t, resp, &proj)

	// Activate leaf.
	resp = e2eRequest(t, "POST", leafURL+"/activate", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "setup: activate")
	e2eDecode(t, resp, &proj)
	if proj.State != leaf.StateActive {
		t.Fatalf("setup: state = %q, want ACTIVE", proj.State)
	}

	// Generate 3 work units via parameter sweep.
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"x": []interface{}{float64(1), float64(2), float64(3)},
		},
	}
	resp = e2eRequest(t, "POST", leafURL+"/work-units/generate", genReq)
	e2eRequireStatus(t, resp, http.StatusAccepted, "setup: generate work units")
	var genResp workunit.GenerateResponse
	e2eDecode(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 3 {
		t.Fatalf("setup: work_units_created = %d, want 3", genResp.WorkUnitsCreated)
	}

	// Register Volunteer A.
	keyA := newE2EVolunteerKey(t)
	pubKeyA := []byte(keyA.pub)
	regRespA, err := grpcClient.RegisterVolunteer(keyA.sign(ctx), &lettucev1.RegisterVolunteerRequest{
		PublicKey:   pubKeyA,
		DisplayName: "E2E V03 Volunteer A",
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores:      8,
			CpuModel:      "AMD Ryzen 7",
			MaxCpuCores:   4,
			MemoryTotalMb: 32768,
			MaxMemoryMb:   16384,
		},
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	})
	if err != nil {
		t.Fatalf("register volunteer A: %v", err)
	}
	volAID := regRespA.VolunteerId
	volAIDParsed := types.MustParseID(volAID)

	// Register Volunteer B.
	keyB := newE2EVolunteerKey(t)
	pubKeyB := []byte(keyB.pub)
	regRespB, err := grpcClient.RegisterVolunteer(keyB.sign(ctx), &lettucev1.RegisterVolunteerRequest{
		PublicKey:   pubKeyB,
		DisplayName: "E2E V03 Volunteer B",
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores:      4,
			CpuModel:      "Intel i5",
			MaxCpuCores:   2,
			MemoryTotalMb: 16384,
			MaxMemoryMb:   8192,
		},
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	})
	if err != nil {
		t.Fatalf("register volunteer B: %v", err)
	}
	volBID := regRespB.VolunteerId
	volBIDParsed := types.MustParseID(volBID)

	// Helper: create a redundant assignment for vol B on a work unit that vol A already has.
	// This simulates the redundancy_factor=2 model where both volunteers are assigned.
	createRedundantAssignment := func(t *testing.T, wuIDStr string, volID types.ID) {
		t.Helper()
		wuID := types.MustParseID(wuIDStr)
		now := time.Now().UTC()
		_, err := pool.Exec(ctx,
			"INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at) VALUES ($1, $2, $3)",
			wuID, volID, now)
		if err != nil {
			t.Fatalf("create redundant assignment: %v", err)
		}
	}

	// =====================================================
	// Scenario 1: Happy path — validation succeeds
	// =====================================================
	t.Run("Scenario1_HappyPath", func(t *testing.T) {
		// Volunteer A requests work → work unit #1.
		wuRespA, err := grpcClient.RequestWorkUnit(keyA.sign(ctx), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volAID,
			PublicKey:   pubKeyA,
		})
		if err != nil {
			t.Fatalf("vol A request work: %v", err)
		}
		if len(wuRespA.Assignments) != 1 {
			t.Fatalf("vol A expected 1 assignment, got %d", len(wuRespA.Assignments))
		}
		wuID1 := wuRespA.Assignments[0].WorkUnitId

		// Volunteer A run-starts its reserved copy (StartWork: RESERVED -> RUNNING). Per-copy
		// model: the WORK UNIT stays QUEUED so its other redundancy copies keep dispatching.
		swResp, err := grpcClient.StartWork(keyA.sign(ctx), &lettucev1.StartWorkRequest{
			WorkUnitId:  wuID1,
			VolunteerId: volAID,
		})
		if err != nil {
			t.Fatalf("vol A StartWork: %v", err)
		}
		if !swResp.Ok {
			t.Errorf("StartWork should return ok = true (%s)", swResp.Message)
		}

		// Verify the unit stays QUEUED and vol A's copy is now RUNNING (started_at set).
		var wuState string
		err = pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1",
			types.MustParseID(wuID1)).Scan(&wuState)
		if err != nil {
			t.Fatalf("query state: %v", err)
		}
		if wuState != "QUEUED" {
			t.Errorf("work unit state = %q, want QUEUED (per-copy: only the copy runs)", wuState)
		}
		var runningCopies int
		err = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL AND started_at IS NOT NULL",
			types.MustParseID(wuID1), volAIDParsed).Scan(&runningCopies)
		if err != nil {
			t.Fatalf("count running copies: %v", err)
		}
		if runningCopies != 1 {
			t.Errorf("running copies for vol A = %d, want 1", runningCopies)
		}

		// Create redundant assignment for vol B (redundancy_factor=2).
		createRedundantAssignment(t, wuID1, volBIDParsed)

		// Vol A submits result → work unit COMPLETED.
		outputData := []byte(`{"result": "computation_complete", "value": 3.14159}`)
		checksum := sha256Hex(outputData)

		submitA, err := grpcClient.SubmitResult(keyA.sign(ctx), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuID1,
			VolunteerId:          volAID,
			PublicKey:            pubKeyA,
			OutputData:           outputData,
			OutputChecksumSha256: checksum,
			Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 3600, CpuSecondsUser: 3200, CpuCoresUsed: 4},
		})
		if err != nil {
			t.Fatalf("vol A submit: %v", err)
		}
		if !submitA.Accepted {
			t.Fatalf("vol A submit not accepted: %s", submitA.Message)
		}

		// Vol B submits identical result → triggers validation.
		submitB, err := grpcClient.SubmitResult(keyB.sign(ctx), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuID1,
			VolunteerId:          volBID,
			PublicKey:            pubKeyB,
			OutputData:           outputData,
			OutputChecksumSha256: checksum,
			Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 1800, CpuSecondsUser: 1600, CpuCoresUsed: 2},
		})
		if err != nil {
			t.Fatalf("vol B submit: %v", err)
		}
		if !submitB.Accepted {
			t.Fatalf("vol B submit not accepted: %s", submitB.Message)
		}

		// Verify: work unit VALIDATED.
		err = pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1",
			types.MustParseID(wuID1)).Scan(&wuState)
		if err != nil {
			t.Fatalf("query validated state: %v", err)
		}
		if wuState != "VALIDATED" {
			t.Errorf("work unit state = %q, want VALIDATED", wuState)
		}

		// Verify: 2 AGREED results.
		var agreedCount int
		err = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM results WHERE work_unit_id = $1 AND validation_status = 'AGREED'",
			types.MustParseID(wuID1)).Scan(&agreedCount)
		if err != nil {
			t.Fatalf("count agreed: %v", err)
		}
		if agreedCount != 2 {
			t.Errorf("agreed results = %d, want 2", agreedCount)
		}

		// Verify: 2 credit_ledger entries with amount 1.0 each.
		var creditCount int
		var creditSum float64
		err = pool.QueryRow(ctx,
			"SELECT COUNT(*), COALESCE(SUM(credit_amount), 0) FROM credit_ledger WHERE work_unit_id = $1",
			types.MustParseID(wuID1)).Scan(&creditCount, &creditSum)
		if err != nil {
			t.Fatalf("query credits: %v", err)
		}
		if creditCount != 2 {
			t.Errorf("credit entries = %d, want 2", creditCount)
		}
		if creditSum != 2.0 {
			t.Errorf("total credit = %.1f, want 2.0", creditSum)
		}

		// Verify volunteer counters.
		var completedA int
		err = pool.QueryRow(ctx,
			"SELECT total_work_units_completed FROM volunteers WHERE id = $1",
			volAIDParsed).Scan(&completedA)
		if err != nil {
			t.Fatalf("query vol A completed: %v", err)
		}
		if completedA != 1 {
			t.Errorf("vol A completed = %d, want 1", completedA)
		}
	})

	// =====================================================
	// Scenario 2: Validation failure — results disagree → reassign
	// =====================================================
	t.Run("Scenario2_DisagreementReassignment", func(t *testing.T) {
		// Vol A requests work → work unit #2.
		wuRespA, err := grpcClient.RequestWorkUnit(keyA.sign(ctx), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volAID,
			PublicKey:   pubKeyA,
		})
		if err != nil {
			t.Fatalf("vol A request work: %v", err)
		}
		if len(wuRespA.Assignments) != 1 {
			t.Fatalf("vol A expected 1 assignment, got %d", len(wuRespA.Assignments))
		}
		wuID2 := wuRespA.Assignments[0].WorkUnitId

		// Vol A run-starts (StartWork) so its reserved unit flips to ASSIGNED and
		// gets the active history row SubmitResult needs.
		if _, swErr := grpcClient.StartWork(keyA.sign(ctx), &lettucev1.StartWorkRequest{
			WorkUnitId: wuID2, VolunteerId: volAID,
		}); swErr != nil {
			t.Fatalf("vol A StartWork: %v", swErr)
		}

		// Create redundant assignment for vol B.
		createRedundantAssignment(t, wuID2, volBIDParsed)

		// Submit DIFFERENT results → disagreement.
		outputA := []byte(`{"result": "aaa"}`)
		outputB := []byte(`{"result": "bbb"}`)

		_, err = grpcClient.SubmitResult(keyA.sign(ctx), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuID2,
			VolunteerId:          volAID,
			PublicKey:            pubKeyA,
			OutputData:           outputA,
			OutputChecksumSha256: sha256Hex(outputA),
			Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 100, CpuSecondsUser: 80, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("vol A submit: %v", err)
		}

		_, err = grpcClient.SubmitResult(keyB.sign(ctx), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuID2,
			VolunteerId:          volBID,
			PublicKey:            pubKeyB,
			OutputData:           outputB,
			OutputChecksumSha256: sha256Hex(outputB),
			Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 100, CpuSecondsUser: 80, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("vol B submit: %v", err)
		}

		// Verify: work unit was REJECTED then re-queued (QUEUED with HIGH priority).
		var wuState, priority string
		var reassignCount int
		err = pool.QueryRow(ctx,
			"SELECT state, priority, reassignment_count FROM work_units WHERE id = $1",
			types.MustParseID(wuID2)).Scan(&wuState, &priority, &reassignCount)
		if err != nil {
			t.Fatalf("query reassigned state: %v", err)
		}
		if wuState != "QUEUED" {
			t.Errorf("state = %q, want QUEUED (reassigned)", wuState)
		}
		if priority != "HIGH" {
			t.Errorf("priority = %q, want HIGH", priority)
		}
		if reassignCount != 1 {
			t.Errorf("reassignment_count = %d, want 1", reassignCount)
		}

		// Both results should be DISAGREED.
		var disagreedCount int
		err = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM results WHERE work_unit_id = $1 AND validation_status = 'DISAGREED'",
			types.MustParseID(wuID2)).Scan(&disagreedCount)
		if err != nil {
			t.Fatalf("count disagreed: %v", err)
		}
		if disagreedCount != 2 {
			t.Errorf("disagreed results = %d, want 2", disagreedCount)
		}

		// Re-queued work unit needs DIFFERENT volunteers for round 2 because the
		// uq_results_work_unit_volunteer constraint prevents the same volunteer from
		// submitting multiple results for the same work unit. Register volunteers C and D.
		keyC := newE2EVolunteerKey(t)
		pubKeyC := []byte(keyC.pub)
		regRespC, err := grpcClient.RegisterVolunteer(keyC.sign(ctx), &lettucev1.RegisterVolunteerRequest{
			PublicKey:   pubKeyC,
			DisplayName: "E2E V03 Volunteer C",
			Hardware: &lettucev1.HardwareCapabilities{
				CpuCores: 8, CpuModel: "AMD Ryzen 9", MaxCpuCores: 4,
				MemoryTotalMb: 32768, MaxMemoryMb: 16384,
			},
			AvailableRuntimes: []string{"NATIVE"},
			SchedulingMode:    "ALWAYS",
		})
		if err != nil {
			t.Fatalf("register volunteer C: %v", err)
		}
		volCID := regRespC.VolunteerId

		keyD := newE2EVolunteerKey(t)
		pubKeyD := []byte(keyD.pub)
		regRespD, err := grpcClient.RegisterVolunteer(keyD.sign(ctx), &lettucev1.RegisterVolunteerRequest{
			PublicKey:   pubKeyD,
			DisplayName: "E2E V03 Volunteer D",
			Hardware: &lettucev1.HardwareCapabilities{
				CpuCores: 4, CpuModel: "Intel i7", MaxCpuCores: 2,
				MemoryTotalMb: 16384, MaxMemoryMb: 8192,
			},
			AvailableRuntimes: []string{"NATIVE"},
			SchedulingMode:    "ALWAYS",
		})
		if err != nil {
			t.Fatalf("register volunteer D: %v", err)
		}
		volDID := regRespD.VolunteerId
		volDIDParsed := types.MustParseID(volDID)

		// Vol C requests → should get reassigned work unit (HIGH priority).
		wuRespC, err := grpcClient.RequestWorkUnit(keyC.sign(ctx), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volCID,
			PublicKey:   pubKeyC,
		})
		if err != nil {
			t.Fatalf("vol C request reassigned work: %v", err)
		}
		if len(wuRespC.Assignments) != 1 {
			t.Fatalf("vol C expected 1 assignment, got %d", len(wuRespC.Assignments))
		}
		if wuRespC.Assignments[0].WorkUnitId != wuID2 {
			t.Errorf("vol C got %s, want %s (reassigned)", wuRespC.Assignments[0].WorkUnitId, wuID2)
		}

		// Vol C run-starts the reassigned unit so it gets its active history row.
		if _, swErr := grpcClient.StartWork(keyC.sign(ctx), &lettucev1.StartWorkRequest{
			WorkUnitId: wuID2, VolunteerId: volCID,
		}); swErr != nil {
			t.Fatalf("vol C StartWork: %v", swErr)
		}

		// Create redundant assignment for vol D.
		createRedundantAssignment(t, wuID2, volDIDParsed)

		// Both submit matching results this time.
		outputGood := []byte(`{"result": "correct_answer"}`)
		checksumGood := sha256Hex(outputGood)

		_, err = grpcClient.SubmitResult(keyC.sign(ctx), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuID2,
			VolunteerId:          volCID,
			PublicKey:            pubKeyC,
			OutputData:           outputGood,
			OutputChecksumSha256: checksumGood,
			Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 100, CpuSecondsUser: 80, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("vol C submit good: %v", err)
		}

		_, err = grpcClient.SubmitResult(keyD.sign(ctx), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuID2,
			VolunteerId:          volDID,
			PublicKey:            pubKeyD,
			OutputData:           outputGood,
			OutputChecksumSha256: checksumGood,
			Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 100, CpuSecondsUser: 80, CpuCoresUsed: 1},
		})
		if err != nil {
			t.Fatalf("vol D submit good: %v", err)
		}

		// Verify: work unit #2 VALIDATED on second attempt.
		err = pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1",
			types.MustParseID(wuID2)).Scan(&wuState)
		if err != nil {
			t.Fatalf("query final state: %v", err)
		}
		if wuState != "VALIDATED" {
			t.Errorf("state = %q, want VALIDATED", wuState)
		}
	})

	// =====================================================
	// Scenario 3: Heartbeat timeout → reassignment
	// =====================================================
	t.Run("Scenario3_DeadlineTimeout", func(t *testing.T) {
		// Vol A requests work → work unit #3.
		wuRespA, err := grpcClient.RequestWorkUnit(keyA.sign(ctx), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volAID,
			PublicKey:   pubKeyA,
		})
		if err != nil {
			t.Fatalf("vol A request work: %v", err)
		}
		if len(wuRespA.Assignments) != 1 {
			t.Fatalf("vol A expected 1 assignment, got %d", len(wuRespA.Assignments))
		}
		wuID3 := wuRespA.Assignments[0].WorkUnitId

		// Vol A run-starts: its RESERVED copy flips to RUNNING (started_at set). Per-copy
		// model: the WORK UNIT stays QUEUED; only the copy runs.
		_, err = grpcClient.StartWork(keyA.sign(ctx), &lettucev1.StartWorkRequest{
			WorkUnitId:  wuID3,
			VolunteerId: volAID,
		})
		if err != nil {
			t.Fatalf("vol A StartWork: %v", err)
		}

		// Simulate a vanished volunteer past its deadline: backdate vol A's RUNNING COPY's
		// started_at and force a short per-copy deadline so the deadline-based copy sweep
		// (FindExpiredCopies) reclaims it. (Per-task heartbeat timeouts are gone; liveness
		// is the per-copy deadline — property 5.)
		_, err = pool.Exec(ctx,
			`UPDATE work_unit_assignment_history
			 SET started_at = NOW() - INTERVAL '1 hour', deadline_seconds = 1
			 WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL`,
			types.MustParseID(wuID3), volAIDParsed)
		if err != nil {
			t.Fatalf("backdate running copy: %v", err)
		}

		// Run fault monitor ScanOnce.
		wuRepo := workunit.NewPgxWorkUnitRepository(pool)
		assignRepo := assignment.NewPgxRepository(pool)
		monitor := server.NewFaultMonitor(wuRepo, assignRepo, nil, nil, testLogger())
		if err := monitor.ScanOnce(ctx); err != nil {
			t.Fatalf("ScanOnce: %v", err)
		}

		// Verify: the timed-out copy is closed and the unit stays QUEUED with no live copy
		// left — it immediately redispatches a fresh copy (uncapped, property 6). Per-copy
		// model: the deadline sweep does NOT bump reassignment_count or escalate priority;
		// it closes the copy and leaves the unit dispatchable.
		var wuState string
		err = pool.QueryRow(ctx,
			"SELECT state FROM work_units WHERE id = $1",
			types.MustParseID(wuID3)).Scan(&wuState)
		if err != nil {
			t.Fatalf("query state: %v", err)
		}
		if wuState != "QUEUED" {
			t.Errorf("state = %q, want QUEUED", wuState)
		}
		var liveCopies int
		err = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NULL",
			types.MustParseID(wuID3)).Scan(&liveCopies)
		if err != nil {
			t.Fatalf("count live copies: %v", err)
		}
		if liveCopies != 0 {
			t.Errorf("live copies = %d, want 0 (the timed-out copy was closed)", liveCopies)
		}

		// Verify the copy outcome = EXPIRED: a run-started copy that missed its deadline.
		// (The old heartbeat-abandonment ABANDONED path for a RUNNING copy no longer exists;
		// ABANDONED is now reserved for a buffered copy whose holder vanished pre-start.)
		var outcome string
		err = pool.QueryRow(ctx,
			"SELECT outcome FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NOT NULL ORDER BY outcome_at DESC LIMIT 1",
			types.MustParseID(wuID3), volAIDParsed).Scan(&outcome)
		if err != nil {
			t.Fatalf("query assignment outcome: %v", err)
		}
		if outcome != "EXPIRED" {
			t.Errorf("assignment outcome = %q, want EXPIRED (deadline-based reclaim)", outcome)
		}

		// Vol B picks up work unit #3 (HIGH priority, re-queued).
		wuRespB, err := grpcClient.RequestWorkUnit(keyB.sign(ctx), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volBID,
			PublicKey:   pubKeyB,
		})
		if err != nil {
			t.Fatalf("vol B request after timeout: %v", err)
		}
		if len(wuRespB.Assignments) != 1 {
			t.Fatalf("vol B expected 1 assignment, got %d", len(wuRespB.Assignments))
		}
		if wuRespB.Assignments[0].WorkUnitId != wuID3 {
			t.Errorf("vol B got %s, want %s", wuRespB.Assignments[0].WorkUnitId, wuID3)
		}

		// Vol B run-starts (StartWork) so its reserved unit flips to ASSIGNED and
		// gets the active history row SubmitResult needs.
		if _, swErr := grpcClient.StartWork(keyB.sign(ctx), &lettucev1.StartWorkRequest{
			WorkUnitId: wuID3, VolunteerId: volBID,
		}); swErr != nil {
			t.Fatalf("vol B StartWork: %v", swErr)
		}

		// Create redundant assignment for vol A.
		createRedundantAssignment(t, wuID3, volAIDParsed)

		// Vol B submits result.
		output := []byte(`{"result": "wu3_result"}`)
		_, err = grpcClient.SubmitResult(keyB.sign(ctx), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuID3,
			VolunteerId:          volBID,
			PublicKey:            pubKeyB,
			OutputData:           output,
			OutputChecksumSha256: sha256Hex(output),
			Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 200, CpuSecondsUser: 180, CpuCoresUsed: 2},
		})
		if err != nil {
			t.Fatalf("vol B submit: %v", err)
		}

		// Vol A submits matching result → validation.
		_, err = grpcClient.SubmitResult(keyA.sign(ctx), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuID3,
			VolunteerId:          volAID,
			PublicKey:            pubKeyA,
			OutputData:           output,
			OutputChecksumSha256: sha256Hex(output),
			Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 200, CpuSecondsUser: 180, CpuCoresUsed: 4},
		})
		if err != nil {
			t.Fatalf("vol A submit: %v", err)
		}

		// Verify: work unit #3 VALIDATED.
		err = pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1",
			types.MustParseID(wuID3)).Scan(&wuState)
		if err != nil {
			t.Fatalf("query final state: %v", err)
		}
		if wuState != "VALIDATED" {
			t.Errorf("state = %q, want VALIDATED", wuState)
		}
	})

	// =====================================================
	// Scenario 4: Dead-letter at max_total_copies (requeue is UNCAPPED)
	// =====================================================
	// Per-copy model (migration 00006): there is NO per-reassignment cap. A timed-out
	// copy is closed EXPIRED and the unit redispatches freely while it stays QUEUED;
	// the unit only dead-letters (FAILED + flagged) once the TOTAL copies ever created
	// reaches max_total_copies with redundancy still unmet and no live copy. Probe that
	// ceiling with max_total_copies=2 (was: assert FAILED after max_reassignments).
	t.Run("Scenario4_DeadLetterAtMaxTotalCopies", func(t *testing.T) {
		wuID4 := types.NewID()
		_, err := pool.Exec(ctx, `
			INSERT INTO work_units (
				id, leaf_id, state, priority, input_data, code_artifact_ref,
				parameters, deadline_seconds, max_total_copies, reassignment_count
			) VALUES ($1, $2, 'QUEUED', 'NORMAL', '{"x": 99}', 'ref://test',
				'{"iter": 1}', 3600, 2, 0)`,
			wuID4, proj.ID)
		if err != nil {
			t.Fatalf("create wu4: %v", err)
		}

		wuRepo := workunit.NewPgxWorkUnitRepository(pool)
		reservedUntil := time.Now().UTC().Add(time.Hour)

		// Copy 1: reserve a copy for vol A, then time it out (close EXPIRED). The unit
		// stays QUEUED — there is no per-unit ASSIGNED/EXPIRED in the per-copy model.
		cp1, err := wuRepo.ReserveCopy(ctx, wuID4, volAIDParsed, nil, reservedUntil, 3600)
		if err != nil {
			t.Fatalf("reserve copy 1: %v", err)
		}
		if err := wuRepo.CloseCopy(ctx, cp1.ID, "EXPIRED"); err != nil {
			t.Fatalf("close copy 1: %v", err)
		}

		// One timed-out copy must NOT dead-letter — requeue is uncapped (total=1 < 2).
		failed, err := wuRepo.DeadLetterIfExhausted(ctx, wuID4)
		if err != nil {
			t.Fatalf("dead-letter probe after copy 1: %v", err)
		}
		if failed {
			t.Error("should NOT dead-letter after a single timed-out copy (requeue is uncapped)")
		}

		// Copy 2: reserve + time out again. Total copies now reaches max_total_copies=2.
		cp2, err := wuRepo.ReserveCopy(ctx, wuID4, volAIDParsed, nil, reservedUntil, 3600)
		if err != nil {
			t.Fatalf("reserve copy 2: %v", err)
		}
		if err := wuRepo.CloseCopy(ctx, cp2.ID, "EXPIRED"); err != nil {
			t.Fatalf("close copy 2: %v", err)
		}

		// Total copies (2) >= max_total_copies (2), redundancy unmet, no live copy →
		// the unit dead-letters FAILED + flagged_for_review.
		failed, err = wuRepo.DeadLetterIfExhausted(ctx, wuID4)
		if err != nil {
			t.Fatalf("dead-letter probe after copy 2: %v", err)
		}
		if !failed {
			t.Fatal("should dead-letter once total copies reaches max_total_copies")
		}

		// Verify it is FAILED + flagged in DB.
		var finalState string
		var flagged bool
		err = pool.QueryRow(ctx,
			"SELECT state, flagged_for_review FROM work_units WHERE id = $1", wuID4).
			Scan(&finalState, &flagged)
		if err != nil {
			t.Fatalf("query final state: %v", err)
		}
		if finalState != "FAILED" {
			t.Errorf("state = %q, want FAILED", finalState)
		}
		if !flagged {
			t.Error("flagged_for_review should be true")
		}
	})

	// =====================================================
	// Scenario 5: Assignment history audit trail
	// =====================================================
	t.Run("Scenario5_AssignmentHistory", func(t *testing.T) {
		// Query assignment history for a validated work unit (from Scenario 1).
		var wuID1 types.ID
		err := pool.QueryRow(ctx,
			"SELECT id FROM work_units WHERE leaf_id = $1 AND state = 'VALIDATED' ORDER BY created_at ASC LIMIT 1",
			proj.ID).Scan(&wuID1)
		if err != nil {
			t.Fatalf("find validated work unit: %v", err)
		}

		rows, err := pool.Query(ctx,
			"SELECT outcome FROM work_unit_assignment_history WHERE work_unit_id = $1 AND outcome IS NOT NULL ORDER BY assigned_at ASC",
			wuID1)
		if err != nil {
			t.Fatalf("query history: %v", err)
		}
		defer rows.Close()

		var outcomes []string
		for rows.Next() {
			var o string
			if err := rows.Scan(&o); err != nil {
				t.Fatalf("scan outcome: %v", err)
			}
			outcomes = append(outcomes, o)
		}

		// Scenario 1's work unit should have at least 1 COMPLETED outcome.
		if len(outcomes) < 1 {
			t.Fatalf("expected at least 1 assignment history entry with outcome, got %d", len(outcomes))
		}

		hasCompleted := false
		for _, o := range outcomes {
			if o == "COMPLETED" {
				hasCompleted = true
			}
		}
		if !hasCompleted {
			t.Errorf("expected at least one COMPLETED outcome, got %v", outcomes)
		}
	})
}

// sha256Hex computes the SHA-256 hex digest of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// testLogger returns a slog.Logger for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}
