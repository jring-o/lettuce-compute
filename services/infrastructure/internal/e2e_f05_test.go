//go:build integration

package internal_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
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

// init relaxes the server's anti-replay and per-IP gRPC rate limiting for this
// integration test binary only. These production abuse-prevention mechanisms
// conflict with the e2e harness driving many byte-identical RPCs from loopback;
// the seam (server.SetGRPCSecurityForIntegrationTests) is integration-build-only
// and leaves production behavior unchanged.
func init() {
	server.SetGRPCSecurityForIntegrationTests()
}

// setupF05Server creates both HTTP and gRPC servers wired with real database repos.
func setupF05Server(t *testing.T) (
	*pgxpool.Pool,
	lettucev1.VolunteerServiceClient,
	string, // HTTP test server URL
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

	// HTTP server for REST endpoints (project, work unit generation).
	leafRepo := leaf.NewPgxRepository(pool)
	leafHandler := leaf.NewLeafHandler(leafRepo, pool, logger)

	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)
	wuHandler := workunit.NewWorkUnitHandler(wuRepo, batchRepo, leafRepo, adaptGenerate, workunit.NewRepoBatchSink(wuRepo, batchRepo), logger)

	mux := http.NewServeMux()
	leafHandler.RegisterRoutes(mux)
	wuHandler.RegisterRoutes(mux)
	// Mutating routes: production registers these behind auth middleware; the e2e
	// harness drives them directly (unauthenticated) to exercise the full lifecycle.
	// Create binds creator_id to the caller (★BG-11d-write); inject an admin viewer
	// so this harness can still set creator_id via the request body.
	mux.HandleFunc("POST /api/v1/leafs", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(leaf.WithViewer(r.Context(), leaf.Viewer{IsAdmin: true, Authed: true}))
		leafHandler.HandleCreate(w, r)
	})
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", leafHandler.HandleUpdate)
	mux.HandleFunc("DELETE /api/v1/leafs/{leaf_id}", leafHandler.HandleDelete)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/configure", leafHandler.HandleConfigure)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/activate", leafHandler.HandleActivate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/pause", leafHandler.HandlePause)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/resume", leafHandler.HandleResume)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/archive", leafHandler.HandleArchive)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/generate", wuHandler.HandleGenerate)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/work-units", wuHandler.HandleList)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/work-units/{work_unit_id}", wuHandler.HandleGet)

	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for HTTP: %v", err)
	}
	httpServer := server.NewHTTPServer(httpLis.Addr().String(), mux, nil)
	go func() { _ = httpServer.Serve(httpLis) }()
	httpURL := "http://" + httpLis.Addr().String()

	// gRPC server for volunteer service.
	grpcServer, grpcCleanup := server.NewGRPCServer(nil, logger, nil)
	defer grpcCleanup()
	volunteerRepo := volunteer.NewPgxRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	resultRepo := result.NewPgxRepository(pool)
	creditRepo := credit.NewPgxRepository(pool)
	racRepo := credit.NewPgxRACRepository(pool)
	attestationRepo := attestation.NewPgxRepository(pool)
	_, signKey, _ := ed25519.GenerateKey(rand.Reader)
	signer := attestation.NewSigner(signKey)
	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volunteerRepo, assignRepo, attestationRepo, nil, signer, logger, nil, transition.TrustPolicy{})
	volunteerSvc := server.NewVolunteerService(pool, "0.3.0-test", startTime, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, nil, validationEngine, logger, transition.TrustPolicy{})
	lettucev1.RegisterVolunteerServiceServer(grpcServer, volunteerSvc)

	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for gRPC: %v", err)
	}
	go func() { _ = grpcServer.Serve(grpcLis) }()

	conn, err := grpc.NewClient(grpcLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(server.TestSigningInterceptor()),
	)
	if err != nil {
		t.Fatalf("failed to connect gRPC: %v", err)
	}
	client := lettucev1.NewVolunteerServiceClient(conn)

	cleanup := func() {
		conn.Close()
		grpcServer.Stop()
		httpServer.Close()
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM audit_repairs")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_attestations")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteer_rac")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
		_, _ = pool.Exec(ctx, "DELETE FROM result_audits")
		_, _ = pool.Exec(ctx, "DELETE FROM trusted_runners")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return pool, client, httpURL, cleanup
}

// e2eVolunteerKey is a real Ed25519 keypair for an integration-test volunteer.
// C1 requires every non-public gRPC call to carry a valid signature, so tests
// must keep the private half (the old fixed make([]byte,32) fakes had none).
type e2eVolunteerKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

// newE2EVolunteerKey generates a fresh signing keypair for a test volunteer.
func newE2EVolunteerKey(t *testing.T) e2eVolunteerKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate volunteer key: %v", err)
	}
	return e2eVolunteerKey{pub: pub, priv: priv}
}

// sign wraps ctx so the client signing interceptor signs the outgoing RPC with
// this volunteer's key (see server.TestSigningInterceptor / ContextWithTestSigner).
func (k e2eVolunteerKey) sign(ctx context.Context) context.Context {
	return server.ContextWithTestSigner(ctx, k.pub, k.priv)
}

// TestE2EF05Lifecycle covers the full F05 flow:
// register volunteer → request work unit → submit result → verify state transitions.
// Also tests redundant computation with two volunteers.
func TestE2EF05Lifecycle(t *testing.T) {
	pool, grpcClient, httpURL, cleanup := setupF05Server(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// --- Step 1: Create a test user and leaf via REST ---
	userID := types.NewID()
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		userID,
		fmt.Sprintf("e2e-f05-%s@test.example.com", uuid.New().String()[:8]),
		fmt.Sprintf("e2e-f05-%s", uuid.New().String()[:8]),
		"E2E F05 Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}

	createReq := leaf.CreateLeafRequest{
		Name:         "E2E F05 Lifecycle Project",
		Description:  "End-to-end test for register → request → submit flow",
		ResearchArea: []string{"physics"},
		TaskPattern:  leaf.PatternParameterSweep,
		IsOngoing:    false,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}
	resp := e2eRequest(t, "POST", httpURL+"/api/v1/leafs", createReq)
	e2eRequireStatus(t, resp, http.StatusCreated, "1: create leaf")
	var proj leaf.Leaf
	e2eDecode(t, resp, &proj)

	leafURL := httpURL + "/api/v1/leafs/" + proj.ID.String()

	// --- Step 2: Configure leaf ---
	resp = e2eRequest(t, "POST", leafURL+"/configure", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "2: configure")
	e2eDecode(t, resp, &proj)

	// --- Step 3: Update with execution + validation configs ---
	execCfg := leaf.ExecutionConfig{
		Runtime:         "NATIVE",
		Binaries:        map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		GPUType:         "ANY",
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
		HeartbeatIntervalSeconds:  300,
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
			"x": []interface{}{float64(1), float64(2)},
		},
	}
	updateReq := leaf.UpdateLeafRequest{
		ExecutionConfig:      &execCfg,
		ValidationConfig:     &valCfg,
		FaultToleranceConfig: &ftCfg,
		DataConfig:           &dataCfg,
	}
	resp = e2eRequest(t, "PUT", leafURL, updateReq)
	e2eRequireStatus(t, resp, http.StatusOK, "3: update configs")
	e2eDecode(t, resp, &proj)

	// --- Step 4: Activate leaf ---
	resp = e2eRequest(t, "POST", leafURL+"/activate", nil)
	e2eRequireStatus(t, resp, http.StatusOK, "4: activate")
	e2eDecode(t, resp, &proj)
	if proj.State != leaf.StateActive {
		t.Fatalf("step 4: state = %q, want ACTIVE", proj.State)
	}

	// --- Step 5: Generate work units via parameter sweep ---
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"x": []interface{}{float64(1), float64(2)},
		},
	}
	resp = e2eRequest(t, "POST", leafURL+"/work-units/generate", genReq)
	e2eRequireStatus(t, resp, http.StatusAccepted, "5: generate work units")
	var genResp workunit.GenerateResponse
	e2eDecode(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 2 {
		t.Fatalf("step 5: work_units_created = %d, want 2", genResp.WorkUnitsCreated)
	}

	// --- Step 6: Register volunteer 1 via gRPC ---
	key1 := newE2EVolunteerKey(t)
	pubKey1 := []byte(key1.pub)
	regResp1, err := grpcClient.RegisterVolunteer(key1.sign(ctx), &lettucev1.RegisterVolunteerRequest{
		PublicKey:   pubKey1,
		DisplayName: "E2E Volunteer 1",
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
		t.Fatalf("step 6: RegisterVolunteer 1: %v", err)
	}
	if !regResp1.Registered {
		t.Fatal("step 6: expected registered = true")
	}
	vol1ID := regResp1.VolunteerId

	// --- Step 7: Request work unit for volunteer 1 ---
	wuResp, err := grpcClient.RequestWorkUnit(key1.sign(ctx), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: vol1ID,
		PublicKey:   pubKey1,
	})
	if err != nil {
		t.Fatalf("step 7: RequestWorkUnit: %v", err)
	}
	if len(wuResp.Assignments) != 1 {
		t.Fatalf("step 7: expected 1 assignment, got %d", len(wuResp.Assignments))
	}
	wu := wuResp.Assignments[0]
	if wu.WorkUnitId == "" {
		t.Fatal("step 7: expected non-empty work_unit_id")
	}
	if wu.LeafId != proj.ID.String() {
		t.Errorf("step 7: leaf_id = %q, want %q", wu.LeafId, proj.ID.String())
	}
	if wu.Runtime != "NATIVE" {
		t.Errorf("step 7: runtime = %q, want NATIVE", wu.Runtime)
	}

	// --- Step 8: Submit result for volunteer 1 ---
	// Run-start: StartWork flips the reserved (QUEUED) unit to ASSIGNED and creates
	// the active assignment_history row SubmitResult requires (buffered units are
	// leased via reservation columns, no history row, until run-start).
	if _, swErr := grpcClient.StartWork(key1.sign(ctx), &lettucev1.StartWorkRequest{
		WorkUnitId:  wu.WorkUnitId,
		VolunteerId: vol1ID,
	}); swErr != nil {
		t.Fatalf("step 8: StartWork run-start: %v", swErr)
	}

	outputData := []byte(`{"result": "computation_complete", "value": 3.14159}`)
	hash := sha256.Sum256(outputData)
	checksum := hex.EncodeToString(hash[:])

	submitResp, err := grpcClient.SubmitResult(key1.sign(ctx), &lettucev1.SubmitResultRequest{
		WorkUnitId:          wu.WorkUnitId,
		VolunteerId:         vol1ID,
		PublicKey:           pubKey1,
		OutputData:          outputData,
		OutputChecksumSha256: checksum,
		Metadata: &lettucev1.ExecutionMetadata{
			WallClockSeconds: 3600,
			CpuSecondsUser:   3200,
			CpuSecondsSystem: 50,
			CpuCoresUsed:     4,
			PeakMemoryMb:     2048,
			DiskReadMb:       500,
			DiskWriteMb:      100,
			NetworkRxMb:      10,
			NetworkTxMb:      5,
		},
	})
	if err != nil {
		t.Fatalf("step 8: SubmitResult: %v", err)
	}
	if !submitResp.Accepted {
		t.Fatalf("step 8: expected accepted = true, message = %q", submitResp.Message)
	}
	if submitResp.ResultId == "" {
		t.Fatal("step 8: expected non-empty result_id")
	}

	// --- Step 9: Work unit is NOT yet complete (redundancy_factor=2 needs two results) ---
	wuID, _ := types.ParseID(wu.WorkUnitId)
	var wuState string
	if err := pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", wuID).Scan(&wuState); err != nil {
		t.Fatalf("step 9: query work unit: %v", err)
	}
	if wuState == "COMPLETED" || wuState == "VALIDATED" {
		t.Errorf("step 9: work unit reached %q after a single result; redundancy_factor=2 requires two", wuState)
	}

	// --- Step 10: First result is stored PENDING, awaiting a corroborating result ---
	resultID, _ := types.ParseID(submitResp.ResultId)
	var valStatus string
	if err := pool.QueryRow(ctx, "SELECT validation_status FROM results WHERE id = $1", resultID).Scan(&valStatus); err != nil {
		t.Fatalf("step 10: query result: %v", err)
	}
	if valStatus != "PENDING" {
		t.Errorf("step 10: validation_status = %q, want PENDING", valStatus)
	}

	// --- Step 11: Register volunteer 2 ---
	key2 := newE2EVolunteerKey(t)
	pubKey2 := []byte(key2.pub)
	regResp2, err := grpcClient.RegisterVolunteer(key2.sign(ctx), &lettucev1.RegisterVolunteerRequest{
		PublicKey:   pubKey2,
		DisplayName: "E2E Volunteer 2",
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
		t.Fatalf("step 11: RegisterVolunteer 2: %v", err)
	}
	vol2ID := regResp2.VolunteerId

	// --- Step 12: Assign the SAME work unit to volunteer 2 (redundancy_factor=2) ---
	// The scheduler does not hand the same unit to a second volunteer via RequestWorkUnit,
	// so the redundant assignment is created directly (mirrors the maintained e2e suite).
	createRedundantAssignment(t, pool, ctx, wu.WorkUnitId, types.MustParseID(vol2ID))

	// --- Step 13: Volunteer 2 submits a CORROBORATING (identical) result ---
	submitResp2, err := grpcClient.SubmitResult(key2.sign(ctx), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wu.WorkUnitId,
		VolunteerId:          vol2ID,
		PublicKey:            pubKey2,
		OutputData:           outputData,
		OutputChecksumSha256: checksum,
		Metadata: &lettucev1.ExecutionMetadata{
			WallClockSeconds: 1800,
			CpuSecondsUser:   1600,
			CpuSecondsSystem: 25,
			CpuCoresUsed:     2,
			PeakMemoryMb:     1024,
		},
	})
	if err != nil {
		t.Fatalf("step 13: SubmitResult vol2: %v", err)
	}
	if !submitResp2.Accepted {
		t.Fatalf("step 13: expected accepted = true, message = %q", submitResp2.Message)
	}

	// --- Step 14: With two agreeing results, the work unit reaches a terminal validated state ---
	if err := pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", wuID).Scan(&wuState); err != nil {
		t.Fatalf("step 14: query work unit: %v", err)
	}
	if wuState != "COMPLETED" && wuState != "VALIDATED" {
		t.Errorf("step 14: work unit state = %q, want COMPLETED/VALIDATED after two agreeing results", wuState)
	}

	// --- Step 15: Two results stored for the work unit ---
	var resultCount int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM results WHERE work_unit_id = $1", wuID).Scan(&resultCount); err != nil {
		t.Fatalf("step 15: count results: %v", err)
	}
	if resultCount != 2 {
		t.Errorf("step 15: results for work unit = %d, want 2", resultCount)
	}

	// --- Step 16: Agreeing results produce AGREED attestations (one per volunteer) ---
	var agreedAttestations int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM credit_attestations WHERE work_unit_id = $1 AND validation_outcome = 'AGREED'",
		wuID).Scan(&agreedAttestations); err != nil {
		t.Fatalf("step 16: query attestations: %v", err)
	}
	if agreedAttestations != 2 {
		t.Errorf("step 16: AGREED attestations = %d, want 2", agreedAttestations)
	}

	// --- Step 17: A volunteer with no active assignment cannot submit ---
	key3 := newE2EVolunteerKey(t)
	pubKey3 := []byte(key3.pub)
	regResp3, err := grpcClient.RegisterVolunteer(key3.sign(ctx), &lettucev1.RegisterVolunteerRequest{
		PublicKey:   pubKey3,
		DisplayName: "E2E Volunteer 3",
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores:      2,
			CpuModel:      "Test CPU",
			MaxCpuCores:   1,
			MemoryTotalMb: 8192,
			MaxMemoryMb:   4096,
		},
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	})
	if err != nil {
		t.Fatalf("step 17: RegisterVolunteer 3: %v", err)
	}
	_, err = grpcClient.SubmitResult(key3.sign(ctx), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wu.WorkUnitId,
		VolunteerId:          regResp3.VolunteerId,
		PublicKey:            pubKey3,
		OutputData:           outputData,
		OutputChecksumSha256: checksum,
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 100, CpuSecondsUser: 80, CpuCoresUsed: 1},
	})
	if err == nil {
		t.Fatal("step 17: expected error for submission with no active assignment")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("step 17: expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("step 17: expected FailedPrecondition, got %s", st.Code())
	}

	// --- Step 18: Checksum mismatch is rejected ---
	_, err = grpcClient.SubmitResult(key1.sign(ctx), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wu.WorkUnitId,
		VolunteerId:          vol1ID,
		PublicKey:            pubKey1,
		OutputData:           outputData,
		OutputChecksumSha256: "0000000000000000000000000000000000000000000000000000000000000000",
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 100, CpuSecondsUser: 80, CpuCoresUsed: 1},
	})
	if err == nil {
		t.Fatal("step 18: expected error for checksum mismatch")
	}
	st, _ = status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("step 18: expected InvalidArgument, got %s", st.Code())
	}
}

// createRedundantAssignment directly inserts a second assignment so two volunteers
// can corroborate the same work unit under redundancy_factor=2 (the scheduler does
// not hand the same unit to a second volunteer via RequestWorkUnit).
func createRedundantAssignment(t *testing.T, pool *pgxpool.Pool, ctx context.Context, wuIDStr string, volID types.ID) {
	t.Helper()
	wuID := types.MustParseID(wuIDStr)
	_, err := pool.Exec(ctx,
		"INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at) VALUES ($1, $2, $3)",
		wuID, volID, time.Now().UTC())
	if err != nil {
		t.Fatalf("create redundant assignment: %v", err)
	}
}
