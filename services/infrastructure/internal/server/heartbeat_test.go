//go:build integration

package server_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// hbKey is a registered test volunteer's Ed25519 keypair used to sign authenticated
// heartbeat-test RPCs (the auth interceptor requires every RPC to be signed).
type hbKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newHBTestKeyPair(t *testing.T) hbKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	return hbKey{pub: pub, priv: priv}
}

// signHB returns a context that signs the outgoing RPC with k.
func signHB(ctx context.Context, k hbKey) context.Context {
	return server.ContextWithTestSigner(ctx, k.pub, k.priv)
}

// setupHeartbeatServer creates a gRPC server with real database repos.
func setupHeartbeatServer(t *testing.T) (
	*pgxpool.Pool,
	lettucev1.VolunteerServiceClient,
	func(),
) {
	t.Helper()

	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	startTime := time.Now()

	grpcServer, grpcCleanup := server.NewGRPCServer(nil, logger)
	defer grpcCleanup()
	volunteerRepo := volunteer.NewPgxRepository(pool)
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	leafRepo := leaf.NewPgxRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	resultRepo := result.NewPgxRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)
	creditRepo := credit.NewPgxRepository(pool)
	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volunteerRepo, assignRepo, nil, nil, logger)
	volunteerSvc := server.NewVolunteerService(pool, "0.3.0-test", startTime, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, nil, validationEngine, logger)
	lettucev1.RegisterVolunteerServiceServer(grpcServer, volunteerSvc)

	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for gRPC: %v", err)
	}
	go func() { _ = grpcServer.Serve(grpcLis) }()

	conn, err := grpc.NewClient(grpcLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(server.TestSigningInterceptor()),
	)
	if err != nil {
		t.Fatalf("failed to connect gRPC: %v", err)
	}
	client := lettucev1.NewVolunteerServiceClient(conn)

	cleanup := func() {
		conn.Close()
		grpcServer.Stop()
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return pool, client, cleanup
}

// createHBTestProject creates an ACTIVE project with specified fault tolerance config.
func createHBTestProject(t *testing.T, pool *pgxpool.Pool, creatorID *types.ID, state string) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	slug := "hb-project-" + uuid.New().String()[:8]
	_, err := pool.Exec(ctx, `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, $4, $5, 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false,"gpu_type":"","max_memory_mb":4096,"max_disk_mb":10240,"max_cpu_seconds":86400,"network_access":false,"min_vram_gb":0}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3,"checkpointing_enabled":false}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false,"min_bandwidth_mbps":0,"min_gpu_vram_mb":0}',
			false, 'PUBLIC', $6
		)`,
		id, "Test Leaf "+slug, slug, "A heartbeat test project", state, creatorID,
	)
	if err != nil {
		t.Fatalf("failed to create test leaf: %v", err)
	}
	return id
}

func createHBTestUser(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	username := "hb-user-" + uuid.New().String()[:8]
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		id, username+"@test.example.com", username, "Test User "+username,
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("failed to create test user: %v", err)
	}
	return id
}

// createAssignedWorkUnit creates a work unit in ASSIGNED state with a volunteer assigned.
func createAssignedWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID, volunteerID types.ID) types.ID {
	t.Helper()
	ctx := context.Background()
	wuID := types.NewID()
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds,
			assigned_volunteer_id, assigned_at, last_heartbeat_at,
			reassignment_count, max_reassignments, flagged_for_review
		) VALUES (
			$1, $2, 'ASSIGNED', 'NORMAL',
			'{"x": 42}', 'ref://test', '{"n": 100}',
			300, 3600,
			$3, $4, $4,
			0, 3, false
		)`,
		wuID, leafID, volunteerID, now,
	)
	if err != nil {
		t.Fatalf("failed to create assigned work unit: %v", err)
	}

	// Create assignment history entry.
	_, err = pool.Exec(ctx, `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, wuID, volunteerID, now)
	if err != nil {
		t.Fatalf("failed to create assignment history: %v", err)
	}

	return wuID
}

func TestHeartbeat_UpdatesTimestampAndContinues(t *testing.T) {
	pool, client, cleanup := setupHeartbeatServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createHBTestUser(t, pool)
	leafID := createHBTestProject(t, pool, &userID, "ACTIVE")

	// Register volunteer.
	key := newHBTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signHB(ctx, key), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         key.pub,
		AvailableRuntimes: []string{"NATIVE"},
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 4, MaxCpuCores: 4,
			MemoryTotalMb: 8192, MaxMemoryMb: 8192,
		},
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	volunteerID, _ := types.ParseID(regResp.VolunteerId)

	wuID := createAssignedWorkUnit(t, pool, leafID, volunteerID)

	// Send heartbeat.
	resp, err := client.Heartbeat(signHB(ctx, key), &lettucev1.HeartbeatRequest{
		WorkUnitId:  wuID.String(),
		VolunteerId: volunteerID.String(),
		Status:      "RUNNING",
		ProgressPct: 0.5,
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !resp.ContinueExecution {
		t.Error("expected continue_execution = true")
	}

	// Verify last_heartbeat_at was updated.
	var lastHB time.Time
	err = pool.QueryRow(ctx, "SELECT last_heartbeat_at FROM work_units WHERE id = $1", wuID).Scan(&lastHB)
	if err != nil {
		t.Fatalf("query last_heartbeat_at: %v", err)
	}
	if time.Since(lastHB) > 5*time.Second {
		t.Errorf("last_heartbeat_at too old: %v", lastHB)
	}
}

func TestHeartbeat_FirstHeartbeatTransitionsAssignedToRunning(t *testing.T) {
	pool, client, cleanup := setupHeartbeatServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createHBTestUser(t, pool)
	leafID := createHBTestProject(t, pool, &userID, "ACTIVE")

	key := newHBTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signHB(ctx, key), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         key.pub,
		AvailableRuntimes: []string{"NATIVE"},
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 4, MaxCpuCores: 4,
			MemoryTotalMb: 8192, MaxMemoryMb: 8192,
		},
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	volunteerID, _ := types.ParseID(regResp.VolunteerId)

	wuID := createAssignedWorkUnit(t, pool, leafID, volunteerID)

	// Verify state is ASSIGNED before heartbeat.
	var stateBefore string
	err = pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", wuID).Scan(&stateBefore)
	if err != nil {
		t.Fatalf("query state: %v", err)
	}
	if stateBefore != "ASSIGNED" {
		t.Fatalf("expected ASSIGNED, got %s", stateBefore)
	}

	// Send first heartbeat — should transition to RUNNING.
	resp, err := client.Heartbeat(signHB(ctx, key), &lettucev1.HeartbeatRequest{
		WorkUnitId:  wuID.String(),
		VolunteerId: volunteerID.String(),
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !resp.ContinueExecution {
		t.Error("expected continue_execution = true")
	}

	// Verify state is RUNNING and started_at is set.
	var stateAfter string
	var startedAt *time.Time
	err = pool.QueryRow(ctx, "SELECT state, started_at FROM work_units WHERE id = $1", wuID).Scan(&stateAfter, &startedAt)
	if err != nil {
		t.Fatalf("query state after: %v", err)
	}
	if stateAfter != "RUNNING" {
		t.Errorf("expected RUNNING, got %s", stateAfter)
	}
	if startedAt == nil {
		t.Error("started_at should be set after first heartbeat")
	}
}

// TestHeartbeat_PreparingKeepsAssigned verifies a PREPARING heartbeat (sent while
// a volunteer pulls the image or waits in its prefetch queue) refreshes
// last_heartbeat_at but does NOT transition the unit ASSIGNED -> RUNNING. See item 2.
func TestHeartbeat_PreparingKeepsAssigned(t *testing.T) {
	pool, client, cleanup := setupHeartbeatServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createHBTestUser(t, pool)
	leafID := createHBTestProject(t, pool, &userID, "ACTIVE")

	key := newHBTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signHB(ctx, key), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         key.pub,
		AvailableRuntimes: []string{"NATIVE"},
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 4, MaxCpuCores: 4,
			MemoryTotalMb: 8192, MaxMemoryMb: 8192,
		},
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	volunteerID, _ := types.ParseID(regResp.VolunteerId)

	wuID := createAssignedWorkUnit(t, pool, leafID, volunteerID)

	// Backdate last_heartbeat_at so we can prove the PREPARING heartbeat refreshes it.
	_, err = pool.Exec(ctx, "UPDATE work_units SET last_heartbeat_at = NOW() - INTERVAL '1 hour' WHERE id = $1", wuID)
	if err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}

	resp, err := client.Heartbeat(signHB(ctx, key), &lettucev1.HeartbeatRequest{
		WorkUnitId:  wuID.String(),
		VolunteerId: volunteerID.String(),
		Status:      "PREPARING",
	})
	if err != nil {
		t.Fatalf("PREPARING Heartbeat: %v", err)
	}
	if !resp.ContinueExecution {
		t.Error("expected continue_execution = true")
	}

	var state string
	var startedAt *time.Time
	var lastHB time.Time
	err = pool.QueryRow(ctx,
		"SELECT state, started_at, last_heartbeat_at FROM work_units WHERE id = $1", wuID,
	).Scan(&state, &startedAt, &lastHB)
	if err != nil {
		t.Fatalf("query work unit: %v", err)
	}
	if state != "ASSIGNED" {
		t.Errorf("state = %s, want ASSIGNED (PREPARING must not start the unit)", state)
	}
	if startedAt != nil {
		t.Errorf("started_at = %v, want nil (unit not started yet)", startedAt)
	}
	if time.Since(lastHB) > 5*time.Second {
		t.Errorf("last_heartbeat_at not refreshed by PREPARING: %v", lastHB)
	}
}

func TestHeartbeat_PausedProjectReturnsFalse(t *testing.T) {
	pool, client, cleanup := setupHeartbeatServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createHBTestUser(t, pool)
	leafID := createHBTestProject(t, pool, &userID, "PAUSED")

	key := newHBTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signHB(ctx, key), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         key.pub,
		AvailableRuntimes: []string{"NATIVE"},
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 4, MaxCpuCores: 4,
			MemoryTotalMb: 8192, MaxMemoryMb: 8192,
		},
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	volunteerID, _ := types.ParseID(regResp.VolunteerId)

	wuID := createAssignedWorkUnit(t, pool, leafID, volunteerID)

	resp, err := client.Heartbeat(signHB(ctx, key), &lettucev1.HeartbeatRequest{
		WorkUnitId:  wuID.String(),
		VolunteerId: volunteerID.String(),
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if resp.ContinueExecution {
		t.Error("expected continue_execution = false for paused project")
	}
	if resp.Message == "" {
		t.Error("expected non-empty message for paused project")
	}
}

func TestHeartbeat_WrongVolunteerReturnsPermissionDenied(t *testing.T) {
	pool, client, cleanup := setupHeartbeatServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createHBTestUser(t, pool)
	leafID := createHBTestProject(t, pool, &userID, "ACTIVE")

	// Register the assigned volunteer.
	key1 := newHBTestKeyPair(t)
	regResp1, err := client.RegisterVolunteer(signHB(ctx, key1), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         key1.pub,
		AvailableRuntimes: []string{"NATIVE"},
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 4, MaxCpuCores: 4,
			MemoryTotalMb: 8192, MaxMemoryMb: 8192,
		},
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer 1: %v", err)
	}
	volunteerID1, _ := types.ParseID(regResp1.VolunteerId)

	// Register a different volunteer.
	key2 := newHBTestKeyPair(t)
	regResp2, err := client.RegisterVolunteer(signHB(ctx, key2), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         key2.pub,
		AvailableRuntimes: []string{"NATIVE"},
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 4, MaxCpuCores: 4,
			MemoryTotalMb: 8192, MaxMemoryMb: 8192,
		},
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer 2: %v", err)
	}

	wuID := createAssignedWorkUnit(t, pool, leafID, volunteerID1)

	// Send heartbeat from the wrong volunteer (authenticated as volunteer 2).
	_, err = client.Heartbeat(signHB(ctx, key2), &lettucev1.HeartbeatRequest{
		WorkUnitId:  wuID.String(),
		VolunteerId: regResp2.VolunteerId,
	})
	if err == nil {
		t.Fatal("expected error for wrong volunteer")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		t.Errorf("expected PERMISSION_DENIED, got %v", err)
	}
}

func TestHeartbeat_NonActiveWorkUnitReturnsFalse(t *testing.T) {
	pool, client, cleanup := setupHeartbeatServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createHBTestUser(t, pool)
	leafID := createHBTestProject(t, pool, &userID, "ACTIVE")

	key := newHBTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signHB(ctx, key), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         key.pub,
		AvailableRuntimes: []string{"NATIVE"},
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 4, MaxCpuCores: 4,
			MemoryTotalMb: 8192, MaxMemoryMb: 8192,
		},
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	volunteerID, _ := types.ParseID(regResp.VolunteerId)

	// Create a work unit in EXPIRED state.
	wuID := types.NewID()
	now := time.Now().UTC()
	_, err = pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds,
			assigned_volunteer_id, assigned_at, last_heartbeat_at,
			reassignment_count, max_reassignments, flagged_for_review
		) VALUES (
			$1, $2, 'EXPIRED', 'NORMAL',
			'{"x": 42}', 'ref://test', '{"n": 100}',
			300, 3600,
			$3, $4, $4,
			0, 3, false
		)`,
		wuID, leafID, volunteerID, now,
	)
	if err != nil {
		t.Fatalf("create expired work unit: %v", err)
	}

	resp, err := client.Heartbeat(signHB(ctx, key), &lettucev1.HeartbeatRequest{
		WorkUnitId:  wuID.String(),
		VolunteerId: volunteerID.String(),
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if resp.ContinueExecution {
		t.Error("expected continue_execution = false for EXPIRED work unit")
	}
}

func TestHeartbeat_InvalidUUID(t *testing.T) {
	_, client, cleanup := setupHeartbeatServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Sign with an arbitrary key — the handler rejects the malformed UUID before
	// any volunteer lookup, so the key need not correspond to a registered volunteer.
	key := newHBTestKeyPair(t)
	_, err := client.Heartbeat(signHB(ctx, key), &lettucev1.HeartbeatRequest{
		WorkUnitId:  "not-a-uuid",
		VolunteerId: types.NewID().String(),
	})
	if err == nil {
		t.Fatal("expected error for invalid UUID")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected INVALID_ARGUMENT, got %v", err)
	}
}

// TestFaultMonitorScanOnce tests the fault monitor's scanOnce detecting
// expired and abandoned work units.
func TestFaultMonitorScanOnce(t *testing.T) {
	pool, _, cleanup := setupHeartbeatServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createHBTestUser(t, pool)
	leafID := createHBTestProject(t, pool, &userID, "ACTIVE")

	// Create a volunteer. This test only exercises the fault monitor (no gRPC RPC),
	// so the key is inserted directly and never used for signing.
	volunteerID := types.NewID()
	now := time.Now().UTC()
	pubKey := []byte(newHBTestKeyPair(t).pub)
	_, err := pool.Exec(ctx, `
		INSERT INTO volunteers (id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at)
		VALUES ($1, $2, '{"cpu_cores":4,"cpu_model":"test","max_cpu_cores":4,"memory_total_mb":8192,"max_memory_mb":8192,"disk_available_mb":10240,"max_disk_mb":10240}',
			'{NATIVE}', 'ALWAYS', true, $3)`,
		volunteerID, pubKey, now)
	if err != nil {
		t.Fatalf("create volunteer: %v", err)
	}

	// Create an expired work unit: assigned 2 hours ago with 1 second deadline.
	expiredWUID := types.NewID()
	pastTime := now.Add(-2 * time.Hour)
	_, err = pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds,
			assigned_volunteer_id, assigned_at, last_heartbeat_at,
			reassignment_count, max_reassignments, flagged_for_review
		) VALUES (
			$1, $2, 'ASSIGNED', 'NORMAL',
			'{"x": 1}', 'ref://test', '{"n": 1}',
			1, 1,
			$3, $4, $4,
			0, 3, false
		)`, expiredWUID, leafID, volunteerID, pastTime)
	if err != nil {
		t.Fatalf("create expired work unit: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, expiredWUID, volunteerID, pastTime)
	if err != nil {
		t.Fatalf("create expired assignment history: %v", err)
	}

	// Create an abandoned work unit: RUNNING with heartbeat 2 hours ago.
	// Project has heartbeat_interval=300, missed_threshold=3 → abandon after 900s.
	abandonedWUID := types.NewID()
	_, err = pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds,
			assigned_volunteer_id, assigned_at, started_at, last_heartbeat_at,
			reassignment_count, max_reassignments, flagged_for_review
		) VALUES (
			$1, $2, 'RUNNING', 'NORMAL',
			'{"x": 2}', 'ref://test', '{"n": 2}',
			86400, 259200,
			$3, $4, $4, $4,
			0, 3, false
		)`, abandonedWUID, leafID, volunteerID, pastTime)
	if err != nil {
		t.Fatalf("create abandoned work unit: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, abandonedWUID, volunteerID, pastTime)
	if err != nil {
		t.Fatalf("create abandoned assignment history: %v", err)
	}

	// Create the no_deadline orphan (item 3): ASSIGNED but never RUNNING, with a
	// stale heartbeat and deadline_seconds=0. FindExpiredWorkUnits skips it
	// (no deadline); only the broadened abandonment check can reclaim it.
	orphanWUID := types.NewID()
	_, err = pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds,
			assigned_volunteer_id, assigned_at, last_heartbeat_at,
			reassignment_count, max_reassignments, flagged_for_review
		) VALUES (
			$1, $2, 'ASSIGNED', 'NORMAL',
			'{"x": 3}', 'ref://test', '{"n": 3}',
			86400, 0,
			$3, $4, $4,
			0, 3, false
		)`, orphanWUID, leafID, volunteerID, pastTime)
	if err != nil {
		t.Fatalf("create orphan work unit: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, orphanWUID, volunteerID, pastTime)
	if err != nil {
		t.Fatalf("create orphan assignment history: %v", err)
	}

	// Run fault monitor scan.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	monitor := server.NewFaultMonitor(wuRepo, assignRepo, nil, nil, logger)

	if err := monitor.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	// Verify expired work unit transitioned to EXPIRED then reassigned to QUEUED.
	var expiredState string
	err = pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", expiredWUID).Scan(&expiredState)
	if err != nil {
		t.Fatalf("query expired state: %v", err)
	}
	if expiredState != "QUEUED" {
		t.Errorf("expired work unit state = %s, want QUEUED (reassigned)", expiredState)
	}

	// Verify expired assignment has outcome.
	var expiredOutcome *string
	err = pool.QueryRow(ctx, "SELECT outcome FROM work_unit_assignment_history WHERE work_unit_id = $1", expiredWUID).Scan(&expiredOutcome)
	if err != nil {
		t.Fatalf("query expired outcome: %v", err)
	}
	if expiredOutcome == nil || *expiredOutcome != "EXPIRED" {
		t.Errorf("expired assignment outcome = %v, want EXPIRED", expiredOutcome)
	}

	// Verify abandoned work unit transitioned to EXPIRED then reassigned to QUEUED.
	var abandonedState string
	err = pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", abandonedWUID).Scan(&abandonedState)
	if err != nil {
		t.Fatalf("query abandoned state: %v", err)
	}
	if abandonedState != "QUEUED" {
		t.Errorf("abandoned work unit state = %s, want QUEUED (reassigned)", abandonedState)
	}

	// Verify abandoned assignment has ABANDONED outcome.
	var abandonedOutcome *string
	err = pool.QueryRow(ctx, "SELECT outcome FROM work_unit_assignment_history WHERE work_unit_id = $1", abandonedWUID).Scan(&abandonedOutcome)
	if err != nil {
		t.Fatalf("query abandoned outcome: %v", err)
	}
	if abandonedOutcome == nil || *abandonedOutcome != "ABANDONED" {
		t.Errorf("abandoned assignment outcome = %v, want ABANDONED", abandonedOutcome)
	}

	// Verify the no_deadline ASSIGNED orphan was reclaimed (item 3).
	var orphanState string
	err = pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", orphanWUID).Scan(&orphanState)
	if err != nil {
		t.Fatalf("query orphan state: %v", err)
	}
	if orphanState != "QUEUED" {
		t.Errorf("orphan work unit state = %s, want QUEUED (reclaimed from stale ASSIGNED)", orphanState)
	}
	var orphanOutcome *string
	err = pool.QueryRow(ctx, "SELECT outcome FROM work_unit_assignment_history WHERE work_unit_id = $1", orphanWUID).Scan(&orphanOutcome)
	if err != nil {
		t.Fatalf("query orphan outcome: %v", err)
	}
	if orphanOutcome == nil || *orphanOutcome != "ABANDONED" {
		t.Errorf("orphan assignment outcome = %v, want ABANDONED", orphanOutcome)
	}
}

