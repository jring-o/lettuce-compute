//go:build integration

package server_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/transition"
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

// cpKey is a registered test volunteer's Ed25519 keypair. The auth interceptor
// requires every RPC to be signed, so tests carry the keypair and wrap each call's
// context with it via signCP.
type cpKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newCPTestKeyPair(t *testing.T) cpKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	return cpKey{pub: pub, priv: priv}
}

// signCP returns a context that signs the outgoing RPC with k (see
// server.TestSigningInterceptor wired into the checkpoint test client).
func signCP(ctx context.Context, k cpKey) context.Context {
	return server.ContextWithTestSigner(ctx, k.pub, k.priv)
}

func setupCheckpointServer(t *testing.T) (
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
	storageDir := t.TempDir()

	grpcServer, grpcCleanup := server.NewGRPCServer(nil, logger, nil)
	defer grpcCleanup()
	volunteerRepo := volunteer.NewPgxRepository(pool)
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	leafRepo := leaf.NewPgxRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	resultRepo := result.NewPgxRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)
	creditRepo := credit.NewPgxRepository(pool)
	checkpointRepo := checkpoint.NewPgxRepository(pool, storageDir)
	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volunteerRepo, assignRepo, nil, nil, nil, logger, nil, transition.TrustPolicy{})
	volunteerSvc := server.NewVolunteerService(pool, "0.9.0-test", startTime, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, checkpointRepo, validationEngine, logger, transition.TrustPolicy{})
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
		_, _ = pool.Exec(ctx, "DELETE FROM file_uploads WHERE file_type = 'CHECKPOINT'")
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

// createCPTestProject creates an ACTIVE project with checkpointing enabled.
func createCPTestProject(t *testing.T, pool *pgxpool.Pool, creatorID *types.ID, checkpointingEnabled bool) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	slug := "cp-project-" + uuid.New().String()[:8]

	ftConfig := `{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3,"checkpointing_enabled":false}`
	if checkpointingEnabled {
		ftConfig = `{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3,"checkpointing_enabled":true,"checkpoint_interval_seconds":60,"max_checkpoint_size_bytes":1048576}`
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, $4, 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false,"gpu_type":"","max_memory_mb":4096,"max_disk_mb":10240,"max_cpu_seconds":86400,"network_access":false,"min_vram_gb":0}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			$5,
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false,"min_bandwidth_mbps":0,"min_gpu_vram_mb":0}',
			false, 'PUBLIC', $6
		)`,
		id, "Test Leaf "+slug, slug, "A checkpoint test project", ftConfig, creatorID,
	)
	if err != nil {
		t.Fatalf("failed to create test leaf: %v", err)
	}
	return id
}

func createCPTestUser(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	username := "cp-user-" + uuid.New().String()[:8]
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

func createCPAssignedWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID, volunteerID types.ID) types.ID {
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

	_, err = pool.Exec(ctx, `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, wuID, volunteerID, now)
	if err != nil {
		t.Fatalf("failed to create assignment history: %v", err)
	}

	return wuID
}

func TestSaveCheckpoint_ValidSave(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPTestProject(t, pool, &userID, true)

	key := newCPTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signCP(ctx, key), &lettucev1.RegisterVolunteerRequest{
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

	wuID := createCPAssignedWorkUnit(t, pool, leafID, volunteerID)

	data := []byte("checkpoint-data-v1")
	resp, err := client.SaveCheckpoint(signCP(ctx, key), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        volunteerID.String(),
		PublicKey:          "", // Not validated in current impl
		CheckpointData:     data,
		CheckpointSequence: 1,
	})
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if !resp.Accepted {
		t.Error("expected accepted = true")
	}

	// Verify work unit has checkpoint metadata.
	var lastCPSeq int
	var lastCPAt *time.Time
	err = pool.QueryRow(ctx,
		"SELECT last_checkpoint_sequence, last_checkpoint_at FROM work_units WHERE id = $1", wuID,
	).Scan(&lastCPSeq, &lastCPAt)
	if err != nil {
		t.Fatalf("query checkpoint metadata: %v", err)
	}
	if lastCPSeq != 1 {
		t.Errorf("last_checkpoint_sequence = %d, want 1", lastCPSeq)
	}
	if lastCPAt == nil {
		t.Error("last_checkpoint_at should be set")
	}

	// Verify file_uploads record.
	var fileType string
	var checkpointSeq int
	err = pool.QueryRow(ctx,
		"SELECT file_type, checkpoint_sequence FROM file_uploads WHERE work_unit_id = $1 AND file_type = 'CHECKPOINT'", wuID,
	).Scan(&fileType, &checkpointSeq)
	if err != nil {
		t.Fatalf("query file_uploads: %v", err)
	}
	if checkpointSeq != 1 {
		t.Errorf("checkpoint_sequence = %d, want 1", checkpointSeq)
	}
}

func TestSaveCheckpoint_CheckpointingDisabled(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPTestProject(t, pool, &userID, false) // checkpointing disabled

	key := newCPTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signCP(ctx, key), &lettucev1.RegisterVolunteerRequest{
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

	wuID := createCPAssignedWorkUnit(t, pool, leafID, volunteerID)

	_, err = client.SaveCheckpoint(signCP(ctx, key), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        volunteerID.String(),
		CheckpointData:     []byte("data"),
		CheckpointSequence: 1,
	})
	if err == nil {
		t.Fatal("expected error for disabled checkpointing")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		t.Errorf("expected FAILED_PRECONDITION, got %v", err)
	}
}

func TestSaveCheckpoint_StaleSequence(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPTestProject(t, pool, &userID, true)

	key := newCPTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signCP(ctx, key), &lettucev1.RegisterVolunteerRequest{
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

	wuID := createCPAssignedWorkUnit(t, pool, leafID, volunteerID)

	// Save sequence 2 first.
	_, err = client.SaveCheckpoint(signCP(ctx, key), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        volunteerID.String(),
		CheckpointData:     []byte("data-seq2"),
		CheckpointSequence: 2,
	})
	if err != nil {
		t.Fatalf("SaveCheckpoint seq 2: %v", err)
	}

	// Try to save sequence 1 (lower).
	_, err = client.SaveCheckpoint(signCP(ctx, key), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        volunteerID.String(),
		CheckpointData:     []byte("data-seq1"),
		CheckpointSequence: 1,
	})
	if err == nil {
		t.Fatal("expected error for stale sequence")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.AlreadyExists {
		t.Errorf("expected ALREADY_EXISTS, got %v", err)
	}
}

func TestSaveCheckpoint_OversizedData(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	// Project has max_checkpoint_size_bytes = 1048576 (1 MB)
	leafID := createCPTestProject(t, pool, &userID, true)

	key := newCPTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signCP(ctx, key), &lettucev1.RegisterVolunteerRequest{
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

	wuID := createCPAssignedWorkUnit(t, pool, leafID, volunteerID)

	// Create data larger than 1 MB.
	bigData := make([]byte, 1048577)
	_, err = client.SaveCheckpoint(signCP(ctx, key), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        volunteerID.String(),
		CheckpointData:     bigData,
		CheckpointSequence: 1,
	})
	if err == nil {
		t.Fatal("expected error for oversized data")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Errorf("expected RESOURCE_EXHAUSTED, got %v", err)
	}
}

func TestSaveCheckpoint_WrongVolunteer(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPTestProject(t, pool, &userID, true)

	key1 := newCPTestKeyPair(t)
	regResp1, err := client.RegisterVolunteer(signCP(ctx, key1), &lettucev1.RegisterVolunteerRequest{
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

	key2 := newCPTestKeyPair(t)
	regResp2, err := client.RegisterVolunteer(signCP(ctx, key2), &lettucev1.RegisterVolunteerRequest{
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

	wuID := createCPAssignedWorkUnit(t, pool, leafID, volunteerID1)

	// Try to save checkpoint from wrong volunteer (authenticated as volunteer 2).
	_, err = client.SaveCheckpoint(signCP(ctx, key2), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        regResp2.VolunteerId,
		CheckpointData:     []byte("data"),
		CheckpointSequence: 1,
	})
	if err == nil {
		t.Fatal("expected error for wrong volunteer")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		t.Errorf("expected PERMISSION_DENIED, got %v", err)
	}
}

func TestGetCheckpoint_Exists(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPTestProject(t, pool, &userID, true)

	key := newCPTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signCP(ctx, key), &lettucev1.RegisterVolunteerRequest{
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

	wuID := createCPAssignedWorkUnit(t, pool, leafID, volunteerID)

	data := []byte("get-checkpoint-data")
	hash := sha256.Sum256(data)
	_ = hex.EncodeToString(hash[:])

	_, err = client.SaveCheckpoint(signCP(ctx, key), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        volunteerID.String(),
		CheckpointData:     data,
		CheckpointSequence: 5,
	})
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	resp, err := client.GetCheckpoint(signCP(ctx, key), &lettucev1.GetCheckpointRequest{
		WorkUnitId: wuID.String(),
	})
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if !resp.HasCheckpoint {
		t.Error("expected has_checkpoint = true")
	}
	if string(resp.CheckpointData) != "get-checkpoint-data" {
		t.Errorf("data = %q, want %q", resp.CheckpointData, "get-checkpoint-data")
	}
	if resp.CheckpointSequence != 5 {
		t.Errorf("sequence = %d, want 5", resp.CheckpointSequence)
	}
	if resp.CreatedByVolunteerId != volunteerID.String() {
		t.Errorf("created_by = %q, want %q", resp.CreatedByVolunteerId, volunteerID.String())
	}
	if resp.CreatedAt == "" {
		t.Error("created_at should be set")
	}
}

func TestGetCheckpoint_NoCheckpoint(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPTestProject(t, pool, &userID, true)

	// Register a volunteer and assign it to a work unit so the auth + assignment
	// check passes, then verify GetCheckpoint reports no checkpoint (not an error).
	key := newCPTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signCP(ctx, key), &lettucev1.RegisterVolunteerRequest{
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
	wuID := createCPAssignedWorkUnit(t, pool, leafID, volunteerID)

	resp, err := client.GetCheckpoint(signCP(ctx, key), &lettucev1.GetCheckpointRequest{
		WorkUnitId: wuID.String(),
	})
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if resp.HasCheckpoint {
		t.Error("expected has_checkpoint = false for work unit without a checkpoint")
	}
}

// TestGetCheckpoint_UnassignedDenied verifies an authenticated volunteer that is not
// assigned to the work unit cannot read its checkpoint (information disclosure fix).
func TestGetCheckpoint_UnassignedDenied(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := newCPTestKeyPair(t)
	_, err := client.RegisterVolunteer(signCP(ctx, key), &lettucev1.RegisterVolunteerRequest{
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
	_ = pool

	_, err = client.GetCheckpoint(signCP(ctx, key), &lettucev1.GetCheckpointRequest{
		WorkUnitId: types.NewID().String(),
	})
	if err == nil {
		t.Fatal("expected error for unassigned volunteer")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		t.Errorf("expected PERMISSION_DENIED, got %v", err)
	}
}

func TestRequestWorkUnit_IncludesCheckpointInfo(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPTestProject(t, pool, &userID, true)

	key := newCPTestKeyPair(t)
	regResp, err := client.RegisterVolunteer(signCP(ctx, key), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         key.pub,
		AvailableRuntimes: []string{"NATIVE"},
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 4, MaxCpuCores: 4,
			MemoryTotalMb: 8192, MaxMemoryMb: 8192,
			DiskAvailableMb: 10240, MaxDiskMb: 10240,
		},
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	volunteerID, _ := types.ParseID(regResp.VolunteerId)

	// Create a QUEUED work unit with an existing checkpoint.
	wuID := types.NewID()
	now := time.Now().UTC()
	_, err = pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds,
			reassignment_count, max_reassignments, flagged_for_review,
			last_checkpoint_at, last_checkpoint_sequence
		) VALUES (
			$1, $2, 'QUEUED', 'NORMAL',
			'{"x": 42}', 'ref://test', '{"n": 100}',
			300, 3600,
			1, 3, false,
			$3, 7
		)`,
		wuID, leafID, now,
	)
	if err != nil {
		t.Fatalf("create work unit with checkpoint: %v", err)
	}

	// Request the work unit.
	wuResp, err := client.RequestWorkUnit(signCP(ctx, key), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volunteerID.String(),
		PublicKey:   key.pub,
	})
	if err != nil {
		t.Fatalf("RequestWorkUnit: %v", err)
	}

	if len(wuResp.Assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(wuResp.Assignments))
	}
	wu := wuResp.Assignments[0]
	if !wu.HasCheckpoint {
		t.Error("expected has_checkpoint = true")
	}
	if wu.CheckpointSequence != 7 {
		t.Errorf("checkpoint_sequence = %d, want 7", wu.CheckpointSequence)
	}
	if wu.CheckpointIntervalSeconds != 60 {
		t.Errorf("checkpoint_interval_seconds = %d, want 60", wu.CheckpointIntervalSeconds)
	}
}
