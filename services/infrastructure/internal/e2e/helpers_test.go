//go:build integration

package e2e_test

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/aggregation"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/database"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/custom"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/health"
	"github.com/lettuce-compute/infrastructure/internal/identity"
	"github.com/lettuce-compute/infrastructure/internal/mapreduce"
	"github.com/lettuce-compute/infrastructure/internal/montecarlo"
	"github.com/lettuce-compute/infrastructure/internal/paramsweep"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// betaEnv holds all server handles for Beta E2E tests, extending testEnv with checkpoint support.
type betaEnv struct {
	pool       *pgxpool.Pool
	grpc       lettucev1.VolunteerServiceClient
	httpURL    string
	signingPub ed25519.PublicKey
	storageDir string
}

// setupBetaServer creates HTTP and gRPC servers wired with all repos including
// checkpoint support (which setupAlphaServer lacks).
func setupBetaServer(t *testing.T) (*betaEnv, func()) {
	t.Helper()

	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}

	// Apply all pending migrations.
	if err := database.RunMigrations(dbURL); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	startTime := time.Now()
	storageDir := t.TempDir()

	signingKey, err := attestation.LoadSigningKey("test-beta-signing.key", true)
	if err != nil {
		t.Fatalf("failed to load signing key: %v", err)
	}
	signer := attestation.NewSigner(signingKey)

	// Repositories.
	leafRepo := leaf.NewPgxRepository(pool)
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)
	volunteerRepo := volunteer.NewPgxRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	resultRepo := result.NewPgxRepository(pool)
	creditRepo := credit.NewPgxRepository(pool)
	racRepo := credit.NewPgxRACRepository(pool)
	attestationRepo := attestation.NewPgxRepository(pool)
	checkpointRepo := checkpoint.NewPgxRepository(pool, storageDir)

	// Validation engine with signer.
	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volunteerRepo, assignRepo, attestationRepo, nil, signer, logger, nil, transition.TrustPolicy{})

	// HTTP server with all endpoints.
	leafHandler := leaf.NewLeafHandler(leafRepo, pool, logger)
	patternRouter := generate.NewRouter(paramsweep.Generate, mapreduce.Generate, montecarlo.Generate, custom.Generate, logger)
	wuHandler := workunit.NewWorkUnitHandler(wuRepo, batchRepo, leafRepo, patternRouter.Generate, logger)
	resultHandler := result.NewResultHandler(resultRepo, leafRepo, logger)
	statsEngine := stats.NewEngine(pool)
	statsHandler := stats.NewStatsHandler(statsEngine, leafRepo, logger)
	volunteerStatsHandler := credit.NewVolunteerStatsHandler(pool, volunteerRepo, racRepo, creditRepo, leafRepo, logger)
	attestationHandler := attestation.NewHandler(attestationRepo, signer.PublicKey(), logger)
	healthHandler := health.NewHandler(pool, statsEngine, leafRepo, logger, "test-head")

	aggEngine := aggregation.NewEngine(resultRepo, wuRepo, leafRepo, logger)
	aggHandler := aggregation.NewAggregationHandler(aggEngine, logger)
	bulkHandler := custom.NewBulkUploadHandler(wuRepo, batchRepo, leafRepo, logger)

	// S72: Identity verification and credit analysis.
	challengeStore := identity.NewPgxChallengeStore(pool, logger)
	identityHandler := identity.NewHandler(challengeStore, volunteerRepo, creditRepo, pool, logger)
	analysisHandler := credit.NewAnalysisHandler(pool, leafRepo, logger)

	mux := http.NewServeMux()

	// Public routes (via RegisterRoutes).
	leafHandler.RegisterRoutes(mux)
	wuHandler.RegisterRoutes(mux)
	resultHandler.RegisterRoutes(mux)
	statsHandler.RegisterRoutes(mux)
	volunteerStatsHandler.RegisterRoutes(mux)
	attestationHandler.RegisterRoutes(mux)
	healthHandler.RegisterRoutes(mux)
	aggHandler.RegisterRoutes(mux)
	identityHandler.RegisterRoutes(mux)

	// Protected routes registered without auth middleware for test convenience.
	mux.HandleFunc("POST /api/v1/leafs", leafHandler.HandleCreate)
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", leafHandler.HandleUpdate)
	mux.HandleFunc("DELETE /api/v1/leafs/{leaf_id}", leafHandler.HandleDelete)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/activate", leafHandler.HandleActivate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/pause", leafHandler.HandlePause)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/resume", leafHandler.HandleResume)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/archive", leafHandler.HandleArchive)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/configure", leafHandler.HandleConfigure)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/work-units", wuHandler.HandleList)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/work-units/{work_unit_id}", wuHandler.HandleGet)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/generate", wuHandler.HandleGenerate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/bulk", bulkHandler.HandleBulkUpload)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/results", resultHandler.HandleListByLeaf)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", aggHandler.HandleAggregate)
	mux.HandleFunc("GET /api/v1/credit/analysis/cross-leaf", analysisHandler.HandleCrossLeaf)
	mux.HandleFunc("GET /api/v1/credit/analysis/{leaf_id}", analysisHandler.HandleLeafAnalysis)
	mux.HandleFunc("GET /api/v1/volunteers/{id}/credit/breakdown", analysisHandler.HandleVolunteerBreakdown)

	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for HTTP: %v", err)
	}
	httpServer := server.NewHTTPServer(httpLis.Addr().String(), mux, nil)
	go func() { _ = httpServer.Serve(httpLis) }()
	httpURL := "http://" + httpLis.Addr().String()

	// gRPC server with checkpoint support.
	grpcServer, grpcCleanup := server.NewGRPCServer(nil, logger, nil)
	defer grpcCleanup()
	volunteerSvc := server.NewVolunteerService(pool, "0.9.0-beta", startTime, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, checkpointRepo, validationEngine, logger, transition.TrustPolicy{})
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

	env := &betaEnv{
		pool:       pool,
		grpc:       client,
		httpURL:    httpURL,
		signingPub: signer.PublicKey(),
		storageDir: storageDir,
	}

	cleanup := func() {
		conn.Close()
		grpcServer.Stop()
		httpServer.Close()
		os.Remove("test-beta-signing.key")
		_, _ = pool.Exec(ctx, "DELETE FROM identity_challenges")
		_, _ = pool.Exec(ctx, "DELETE FROM file_uploads")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_attestations")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteer_rac")
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM leaf_stats_snapshots")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return env, cleanup
}

// betaLeafOpts configures a leaf for Beta E2E tests.
type betaLeafOpts struct {
	Name         string
	TaskPattern  leaf.TaskPattern
	ExecConfig   leaf.ExecutionConfig
	ValConfig    leaf.ValidationConfig
	FTConfig     leaf.FaultToleranceConfig
	DataConfig   leaf.DataConfig
	CreditConfig leaf.CreditConfig
	ResourceReqs *leaf.ResourceRequirements
}

// createBetaLeaf creates, configures, and activates a leaf with the given options.
func createBetaLeaf(t *testing.T, env *betaEnv, ctx context.Context, userID types.ID, opts betaLeafOpts) leaf.Leaf {
	t.Helper()

	createReq := leaf.CreateLeafRequest{
		Name:         opts.Name,
		Description:  "Beta E2E test leaf",
		ResearchArea: []string{"testing"},
		TaskPattern:  opts.TaskPattern,
		IsOngoing:    false,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs", createReq)
	requireStatus(t, resp, http.StatusCreated, "create leaf")
	var lf leaf.Leaf
	decodeJSON(t, resp, &lf)

	leafURL := env.httpURL + "/api/v1/leafs/" + lf.ID.String()

	resp = httpReq(t, "POST", leafURL+"/configure", nil)
	requireStatus(t, resp, http.StatusOK, "configure")
	decodeJSON(t, resp, &lf)

	updateReq := leaf.UpdateLeafRequest{
		ExecutionConfig:      &opts.ExecConfig,
		ValidationConfig:     &opts.ValConfig,
		FaultToleranceConfig: &opts.FTConfig,
		DataConfig:           &opts.DataConfig,
		CreditConfig:         &opts.CreditConfig,
	}
	if opts.ResourceReqs != nil {
		updateReq.ResourceRequirements = opts.ResourceReqs
	}
	resp = httpReq(t, "PUT", leafURL, updateReq)
	requireStatus(t, resp, http.StatusOK, "update configs")
	decodeJSON(t, resp, &lf)

	resp = httpReq(t, "POST", leafURL+"/activate", nil)
	requireStatus(t, resp, http.StatusOK, "activate")
	decodeJSON(t, resp, &lf)

	if lf.State != leaf.StateActive {
		t.Fatalf("leaf %s state = %q, want ACTIVE", opts.Name, lf.State)
	}
	return lf
}

// registerBetaVolunteer registers a volunteer with optional GPU capabilities.
func registerBetaVolunteer(t *testing.T, env *betaEnv, ctx context.Context, pubKey []byte, name string, gpus []*lettucev1.GpuInfo) string {
	t.Helper()
	hw := &lettucev1.HardwareCapabilities{
		CpuCores:        8,
		CpuModel:        "Test CPU",
		MaxCpuCores:     4,
		MemoryTotalMb:   32768,
		MaxMemoryMb:     16384,
		DiskAvailableMb: 102400,
		MaxDiskMb:       51200,
		Gpus:            gpus,
	}
	runtimes := []string{"NATIVE"}
	if gpus != nil {
		runtimes = append(runtimes, "CONTAINER")
	}
	regResp, err := env.grpc.RegisterVolunteer(signFor(t, ctx, pubKey), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         pubKey,
		DisplayName:       name,
		Hardware:          hw,
		AvailableRuntimes: runtimes,
		SchedulingMode:    "ALWAYS",
	})
	if err != nil {
		t.Fatalf("register volunteer %s: %v", name, err)
	}
	return regResp.VolunteerId
}

// firstAssignment returns the single work unit assignment from a
// RequestWorkUnitResponse, failing the test if the batch is not exactly one
// unit. Most e2e flows request work one unit at a time.
func firstAssignment(t *testing.T, resp *lettucev1.RequestWorkUnitResponse) *lettucev1.WorkUnitAssignment {
	t.Helper()
	if len(resp.Assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(resp.Assignments))
	}
	return resp.Assignments[0]
}

// requestSubmitResult requests a work unit and submits a result. Returns the work unit ID.
func requestSubmitResult(t *testing.T, env *betaEnv, ctx context.Context, volID string, pubKey, outputData []byte) string {
	t.Helper()
	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volID,
		PublicKey:   pubKey,
	})
	if err != nil {
		t.Fatalf("request work unit: %v", err)
	}
	if len(wuResp.Assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(wuResp.Assignments))
	}
	wuID := wuResp.Assignments[0].WorkUnitId

	// Run-start: a RUNNING heartbeat flips the reserved (QUEUED) unit to
	// ASSIGNED/RUNNING and creates the active assignment_history row SubmitResult
	// requires. Buffered units are leased via reservation columns (no history row)
	// until run-start, so the realistic flow heartbeats RUNNING before submitting.
	ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuID)

	checksum := sha256Hex(outputData)
	_, err = env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuID,
		VolunteerId:          volID,
		PublicKey:            pubKey,
		OutputData:           outputData,
		OutputChecksumSha256: checksum,
		Metadata: &lettucev1.ExecutionMetadata{
			WallClockSeconds: 10,
			CpuSecondsUser:   8,
			CpuCoresUsed:     2,
			PeakMemoryMb:     512,
		},
	})
	if err != nil {
		t.Fatalf("submit result for WU %s: %v", wuID, err)
	}
	return wuID
}

// assertCreditExists verifies that credit ledger entries exist for a volunteer on a leaf.
func assertCreditExists(t *testing.T, pool *pgxpool.Pool, ctx context.Context, volunteerID, leafID types.ID, minEntries int) float64 {
	t.Helper()
	var count int
	var total float64
	err := pool.QueryRow(ctx,
		"SELECT COUNT(*), COALESCE(SUM(credit_amount), 0) FROM credit_ledger WHERE volunteer_id = $1 AND leaf_id = $2",
		volunteerID, leafID).Scan(&count, &total)
	if err != nil {
		t.Fatalf("query credit ledger: %v", err)
	}
	if count < minEntries {
		t.Errorf("credit entries for volunteer %s on leaf %s = %d, want >= %d", volunteerID, leafID, count, minEntries)
	}
	return total
}

// defaultExecConfig returns a standard execution config for testing.
// Native leafs require each binary to be an https URL and to carry a SHA-256
// checksum (C2 binary-integrity hardening), so both are supplied.
func defaultExecConfig() leaf.ExecutionConfig {
	return leaf.ExecutionConfig{
		Runtime:         "NATIVE",
		Binaries:        map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		MaxMemoryMB:     4096,
		MaxDiskMB:       10240,
		MaxCPUSeconds:   3600,
	}
}

// defaultFTConfig returns a standard fault tolerance config.
func defaultFTConfig() leaf.FaultToleranceConfig {
	return leaf.FaultToleranceConfig{
		HeartbeatIntervalSeconds:  60,
		MissedHeartbeatsThreshold: 3,
		DeadlineMultiplier:        3.0,
		MaxReassignments:          3,
	}
}

// defaultDataConfig returns a standard data config.
func defaultDataConfig() leaf.DataConfig {
	return leaf.DataConfig{
		TransferStrategy:   "INLINE",
		AggregationFormat:  "JSON",
		MaxInputSizeBytes:  1048576,
		MaxOutputSizeBytes: 104857600,
	}
}
