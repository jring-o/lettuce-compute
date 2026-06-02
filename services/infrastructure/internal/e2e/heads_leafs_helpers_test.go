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
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/custom"
	"github.com/lettuce-compute/infrastructure/internal/database"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/health"
	"github.com/lettuce-compute/infrastructure/internal/identity"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/mapreduce"
	"github.com/lettuce-compute/infrastructure/internal/montecarlo"
	"github.com/lettuce-compute/infrastructure/internal/paramsweep"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// headsLeafsEnv holds all server handles for Heads & Leafs E2E tests.
type headsLeafsEnv struct {
	pool       *pgxpool.Pool
	grpc       lettucev1.VolunteerServiceClient
	httpURL    string
	signingPub ed25519.PublicKey
	storageDir string
}

// setupHeadsLeafsServer creates HTTP and gRPC servers with head config for E2E tests.
func setupHeadsLeafsServer(t *testing.T) (*headsLeafsEnv, func()) {
	t.Helper()

	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}

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

	signingKey, err := attestation.LoadSigningKey("test-hl-signing.key", true)
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

	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volunteerRepo, assignRepo, attestationRepo, signer, logger)

	// Head configuration.
	headCfg := &config.HeadConfig{
		Name:        "test-head",
		Description: "Test head for E2E",
		URL:         "https://test-head.example.com",
		DefaultLeafWeights: map[string]int{
			"leaf-a": 50,
			"leaf-b": 30,
			"leaf-c": 20,
		},
	}
	// HTTP handlers.
	leafHandler := leaf.NewLeafHandler(leafRepo, pool, logger)
	headHandler := leaf.NewHeadHandler(headCfg, pool, logger)
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
	challengeStore := identity.NewPgxChallengeStore(pool, logger)
	identityHandler := identity.NewHandler(challengeStore, volunteerRepo, creditRepo, pool, logger)
	analysisHandler := credit.NewAnalysisHandler(pool, leafRepo, logger)

	mux := http.NewServeMux()

	// Public routes.
	leafHandler.RegisterRoutes(mux)
	wuHandler.RegisterRoutes(mux)
	resultHandler.RegisterRoutes(mux)
	statsHandler.RegisterRoutes(mux)
	volunteerStatsHandler.RegisterRoutes(mux)
	attestationHandler.RegisterRoutes(mux)
	healthHandler.RegisterRoutes(mux)
	aggHandler.RegisterRoutes(mux)
	identityHandler.RegisterRoutes(mux)

	// Head info endpoint.
	mux.HandleFunc("GET /api/v1/head", headHandler.HandleGetHeadInfo)

	// Protected routes (no auth middleware for test convenience).
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

	// Deprecated project aliases.
	mux.HandleFunc("POST /api/v1/projects", leafHandler.HandleCreate)
	mux.HandleFunc("GET /api/v1/projects", leafHandler.HandleListDeprecated)
	mux.HandleFunc("GET /api/v1/projects/{leaf_id}", leafHandler.HandleGetDeprecated)

	// Browser volunteer REST endpoints (Ed25519 auth).
	server.RegisterBrowserVolunteerRoutes(mux, pool, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, validationEngine, logger, 10)

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
	volunteerSvc := server.NewVolunteerService(pool, "0.9.0.1-heads-leafs", startTime, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, checkpointRepo, validationEngine, logger)
	server.SetHeadConfig(volunteerSvc, headCfg.Name, headCfg.Description, headCfg.URL, map[string]int32{"leaf-a": 50, "leaf-b": 30, "leaf-c": 20}, 10, server.HeadDispatchConfig{})
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

	env := &headsLeafsEnv{
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
		os.Remove("test-hl-signing.key")
		_, _ = pool.Exec(ctx, "DELETE FROM identity_challenges")
		_, _ = pool.Exec(ctx, "DELETE FROM file_uploads")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_attestations")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteer_rac")
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
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

// hlLeafOpts configures a leaf for Heads & Leafs E2E tests.
type hlLeafOpts struct {
	Name         string
	TaskPattern  leaf.TaskPattern
	ExecConfig   leaf.ExecutionConfig
	ValConfig    leaf.ValidationConfig
	FTConfig     leaf.FaultToleranceConfig
	DataConfig   leaf.DataConfig
	CreditConfig leaf.CreditConfig
	ResourceReqs *leaf.ResourceRequirements
}

// createHLLeaf creates, configures, and activates a leaf.
func createHLLeaf(t *testing.T, env *headsLeafsEnv, ctx context.Context, userID types.ID, opts hlLeafOpts) leaf.Leaf {
	t.Helper()

	createReq := leaf.CreateLeafRequest{
		Name:         opts.Name,
		Description:  "Heads & Leafs E2E test leaf: " + opts.Name,
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

// generateLeafWUs generates work units for a leaf via the REST API.
func generateLeafWUs(t *testing.T, env *headsLeafsEnv, leafID types.ID, count int) {
	t.Helper()
	params := make([]interface{}, count)
	for i := 0; i < count; i++ {
		params[i] = float64(i + 1)
	}
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"x": params,
		},
	}
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs/"+leafID.String()+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate WUs")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	if genResp.WorkUnitsCreated != count {
		t.Fatalf("work_units_created = %d, want %d", genResp.WorkUnitsCreated, count)
	}
}

// getHeadInfo calls GET /api/v1/head and returns the response.
func getHeadInfo(t *testing.T, env *headsLeafsEnv) leaf.HeadInfoResponse {
	t.Helper()
	resp := httpReq(t, "GET", env.httpURL+"/api/v1/head", nil)
	requireStatus(t, resp, http.StatusOK, "get head info")
	var headInfo leaf.HeadInfoResponse
	decodeJSON(t, resp, &headInfo)
	return headInfo
}

// effectiveLeafID returns the assignment's LeafId.
func effectiveLeafID(a *lettucev1.WorkUnitAssignment) string {
	return a.LeafId
}

// requestWUFromLeafs requests a work unit with leaf_ids filter via gRPC and
// returns the single assignment the head dispatched.
func requestWUFromLeafs(t *testing.T, env *headsLeafsEnv, ctx context.Context, volID string, pubKey []byte, leafIDs []string) *lettucev1.WorkUnitAssignment {
	t.Helper()
	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volID,
		PublicKey:   pubKey,
		LeafIds:     leafIDs,
	})
	if err != nil {
		t.Fatalf("request work unit: %v", err)
	}
	if len(wuResp.Assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(wuResp.Assignments))
	}
	return wuResp.Assignments[0]
}

// requestWUExpectNone requests work and asserts the server reports no matching
// work. No-work is now an OK response carrying an empty assignments list (the
// codes.NotFound sentinel was removed); used by exclusion tests where a
// volunteer is not eligible.
func requestWUExpectNone(t *testing.T, env *headsLeafsEnv, ctx context.Context, volID string, pubKey []byte, leafIDs []string) {
	t.Helper()
	resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volID,
		PublicKey:   pubKey,
		LeafIds:     leafIDs,
	})
	if err != nil {
		t.Fatalf("request work unit: %v", err)
	}
	if len(resp.Assignments) != 0 {
		t.Fatalf("expected no work available (empty assignments), but got %d", len(resp.Assignments))
	}
}

// submitWUResult submits a result for a work unit via gRPC.
func submitWUResult(t *testing.T, env *headsLeafsEnv, ctx context.Context, volID string, pubKey []byte, wuID string, outputData []byte) {
	t.Helper()
	// Ensure the submitting volunteer has an active assignment_history row. A
	// buffered unit is leased via reservation columns (no history row) until
	// run-start, so the reserving volunteer must run-start (RUNNING heartbeat →
	// Assign) before submitting. A redundant volunteer that was placed on the unit
	// via a direct history-row insert already has its row and never reserved it, so
	// this is a no-op for that volunteer (the run-start guard only fires for a
	// QUEUED unit reserved to this caller).
	ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuID)
	checksum := sha256Hex(outputData)
	_, err := env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
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
}

// ensureRunStart sends a RUNNING heartbeat (run-start) for wuID ONLY when the unit
// is still QUEUED and reserved to this volunteer — i.e. the volunteer that actually
// reserved the unit via RequestWorkUnit. This flips it to ASSIGNED/RUNNING and
// creates the active assignment_history row SubmitResult needs. It is a deliberate
// no-op for a redundant volunteer (placed on the unit via a direct history-row
// insert, never reserved) and for an already-running unit, so it can be called
// generically from the submit helpers without breaking redundant-volunteer flows.
func ensureRunStart(t *testing.T, pool *pgxpool.Pool, grpc lettucev1.VolunteerServiceClient, ctx context.Context, volID string, pubKey []byte, wuID string) {
	t.Helper()
	var state string
	var reservedVol *string
	if err := pool.QueryRow(ctx,
		"SELECT state, reserved_volunteer_id::text FROM work_units WHERE id = $1", wuID).
		Scan(&state, &reservedVol); err != nil {
		t.Fatalf("ensureRunStart: query work unit %s: %v", wuID, err)
	}
	if state != "QUEUED" || reservedVol == nil || *reservedVol != volID {
		return
	}
	if _, err := grpc.Heartbeat(signFor(t, ctx, pubKey), &lettucev1.HeartbeatRequest{
		WorkUnitId:  wuID,
		VolunteerId: volID,
		Status:      "RUNNING",
	}); err != nil {
		t.Fatalf("ensureRunStart: run-start heartbeat for WU %s: %v", wuID, err)
	}
}

// chiSquared computes the chi-squared statistic for observed vs expected frequencies.
func chiSquared(observed, expected []float64) float64 {
	var chi2 float64
	for i := range observed {
		if expected[i] > 0 {
			diff := observed[i] - expected[i]
			chi2 += (diff * diff) / expected[i]
		}
	}
	return chi2
}

// defaultHLValConfig returns a validation config with redundancy=1 (no comparison needed).
func defaultHLValConfig() leaf.ValidationConfig {
	return leaf.ValidationConfig{
		RedundancyFactor:   1,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
}

// registerHLVolunteer registers a volunteer for Heads & Leafs E2E tests.
func registerHLVolunteer(t *testing.T, env *headsLeafsEnv, ctx context.Context, pubKey []byte, name string) string {
	t.Helper()
	hw := &lettucev1.HardwareCapabilities{
		CpuCores:        8,
		CpuModel:        "Test CPU",
		MaxCpuCores:     4,
		MemoryTotalMb:   32768,
		MaxMemoryMb:     16384,
		DiskAvailableMb: 102400,
		MaxDiskMb:       51200,
	}
	regResp, err := env.grpc.RegisterVolunteer(signFor(t, ctx, pubKey), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         pubKey,
		DisplayName:       name,
		Hardware:          hw,
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	})
	if err != nil {
		t.Fatalf("register volunteer %s: %v", name, err)
	}
	return regResp.VolunteerId
}

// hlDefaultLeafOpts returns default leaf options with the given name.
func hlDefaultLeafOpts(name string) hlLeafOpts {
	return hlLeafOpts{
		Name:         name,
		TaskPattern:  leaf.PatternParameterSweep,
		ExecConfig:   defaultExecConfig(),
		ValConfig:    defaultHLValConfig(),
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	}
}

