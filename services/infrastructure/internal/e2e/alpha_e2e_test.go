//go:build integration

package e2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/aggregation"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/custom"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/mapreduce"
	"github.com/lettuce-compute/infrastructure/internal/montecarlo"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/health"
	"github.com/lettuce-compute/infrastructure/internal/paramsweep"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
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

// adaptGenerate wraps paramsweep.Generate as a workunit.GenerateFunc (used by existing tests).
var adaptGenerate workunit.GenerateFunc = paramsweep.Generate

// testEnv holds all server handles and repos for the Alpha E2E test.
type testEnv struct {
	pool       *pgxpool.Pool
	grpc       lettucev1.VolunteerServiceClient
	httpURL    string
	signingPub ed25519.PublicKey
}

// setupAlphaServer creates HTTP and gRPC servers wired with all repos including
// health metrics, attestation signing, volunteer stats, and results.
func setupAlphaServer(t *testing.T) (*testEnv, func()) {
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

	// Load or generate signing key.
	signingKey, err := attestation.LoadSigningKey("test-alpha-signing.key", true)
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

	// Validation engine with signer.
	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volunteerRepo, assignRepo, attestationRepo, signer, logger)

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

	// Aggregation engine and handler.
	aggEngine := aggregation.NewEngine(resultRepo, wuRepo, leafRepo, logger)
	aggHandler := aggregation.NewAggregationHandler(aggEngine, logger)

	// Custom pattern bulk upload handler.
	bulkHandler := custom.NewBulkUploadHandler(wuRepo, batchRepo, leafRepo, logger)

	mux := http.NewServeMux()
	leafHandler.RegisterRoutes(mux)
	wuHandler.RegisterRoutes(mux)
	resultHandler.RegisterRoutes(mux)
	statsHandler.RegisterRoutes(mux)
	volunteerStatsHandler.RegisterRoutes(mux)
	attestationHandler.RegisterRoutes(mux)
	healthHandler.RegisterRoutes(mux)
	aggHandler.RegisterRoutes(mux)
	// Mutating leaf/work-unit routes: production registers these behind auth
	// middleware (see server.router), while LeafHandler.RegisterRoutes now only
	// exposes the read-only GET routes. The e2e harness drives them directly
	// (unauthenticated) to exercise the full lifecycle.
	mux.HandleFunc("POST /api/v1/leafs", leafHandler.HandleCreate)
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
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/bulk", bulkHandler.HandleBulkUpload)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/results", resultHandler.HandleListByLeaf)
	// GET aggregate is registered by aggHandler.RegisterRoutes (public); only the
	// mutating POST needs to be added here (production registers it behind auth).
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", aggHandler.HandleAggregate)

	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for HTTP: %v", err)
	}
	httpServer := server.NewHTTPServer(httpLis.Addr().String(), mux, nil)
	go func() { _ = httpServer.Serve(httpLis) }()
	httpURL := "http://" + httpLis.Addr().String()

	// gRPC server.
	grpcServer, grpcCleanup := server.NewGRPCServer(nil, logger)
	defer grpcCleanup()
	volunteerSvc := server.NewVolunteerService(pool, "0.6.0-alpha", startTime, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, nil, validationEngine, logger)
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

	env := &testEnv{
		pool:       pool,
		grpc:       client,
		httpURL:    httpURL,
		signingPub: signer.PublicKey(),
	}

	cleanup := func() {
		conn.Close()
		grpcServer.Stop()
		httpServer.Close()
		os.Remove("test-alpha-signing.key")
		// Clean tables in dependency order.
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

// --- Scenario 1: Full Researcher → Volunteer → Credit Pipeline ---

func TestAlphaE2E_Scenario1_FullPipeline(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// --- Step 1: Create user and leaf ---
	userID := types.NewID()
	_, err := env.pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		userID,
		fmt.Sprintf("alpha-e2e-%s@test.example.com", uuid.New().String()[:8]),
		fmt.Sprintf("alpha-e2e-%s", uuid.New().String()[:8]),
		"Alpha E2E Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("step 1: create user: %v", err)
	}

	creditAmount := 5.0
	proj := createAndActivateProject(t, env, ctx, leaf.CreateLeafRequest{
		Name:         "Alpha E2E Full Pipeline",
		Description:  "Comprehensive alpha end-to-end test",
		ResearchArea: []string{"distributed-computing", "physics"},
		TaskPattern:  leaf.PatternParameterSweep,
		IsOngoing:    false,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}, leaf.ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}, leaf.CreditConfig{
		CreditPerValidatedWorkUnit: creditAmount,
	})

	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	// --- Step 2: Generate 5 work units ---
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"x": []interface{}{float64(1), float64(2), float64(3), float64(4), float64(5)},
		},
	}
	resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate work units")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)
	if genResp.WorkUnitsCreated != 5 {
		t.Fatalf("work_units_created = %d, want 5", genResp.WorkUnitsCreated)
	}

	// --- Step 3: Register volunteers A and B ---
	volAPubKey := genVolunteerKey(t)
	volBPubKey := genVolunteerKey(t)

	volAID := registerVolunteer(t, env, ctx, volAPubKey, "Alpha Vol A")
	volBID := registerVolunteer(t, env, ctx, volBPubKey, "Alpha Vol B")

	volAIDParsed := types.MustParseID(volAID)
	volBIDParsed := types.MustParseID(volBID)

	// --- Step 4: Process all 5 work units with both volunteers ---
	outputData := []byte(`{"result": "alpha_e2e_computed", "value": 42.0}`)
	checksum := sha256Hex(outputData)

	for i := 0; i < 5; i++ {
		// Vol A requests and submits.
		wuResp, reqErr := env.grpc.RequestWorkUnit(signFor(t, ctx, volAPubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volAID,
			PublicKey:   volAPubKey,
		})
		if reqErr != nil {
			t.Fatalf("WU %d: vol A request: %v", i+1, reqErr)
		}

		// Create redundant assignment for vol B.
		createRedundantAssignment(t, env.pool, ctx, wuResp.WorkUnitId, volBIDParsed)

		// Vol A submits.
		_, subErr := env.grpc.SubmitResult(signFor(t, ctx, volAPubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuResp.WorkUnitId,
			VolunteerId:          volAID,
			PublicKey:            volAPubKey,
			OutputData:           outputData,
			OutputChecksumSha256: checksum,
			Metadata: &lettucev1.ExecutionMetadata{
				WallClockSeconds: 120,
				CpuSecondsUser:   100,
				CpuCoresUsed:     4,
				PeakMemoryMb:     2048,
			},
		})
		if subErr != nil {
			t.Fatalf("WU %d: vol A submit: %v", i+1, subErr)
		}

		// Vol B submits matching result → triggers validation.
		_, subErr = env.grpc.SubmitResult(signFor(t, ctx, volBPubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId:           wuResp.WorkUnitId,
			VolunteerId:          volBID,
			PublicKey:            volBPubKey,
			OutputData:           outputData,
			OutputChecksumSha256: checksum,
			Metadata: &lettucev1.ExecutionMetadata{
				WallClockSeconds: 150,
				CpuSecondsUser:   130,
				CpuCoresUsed:     2,
				PeakMemoryMb:     1024,
			},
		})
		if subErr != nil {
			t.Fatalf("WU %d: vol B submit: %v", i+1, subErr)
		}
	}

	// --- Step 5: Verify all work units are VALIDATED ---
	var validatedCount int
	err = env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_units WHERE leaf_id = $1 AND state = 'VALIDATED'",
		proj.ID).Scan(&validatedCount)
	if err != nil {
		t.Fatalf("query validated count: %v", err)
	}
	if validatedCount != 5 {
		t.Errorf("validated work units = %d, want 5", validatedCount)
	}

	// --- Step 6: Verify credit ledger ---
	var creditCount int
	var creditTotal float64
	err = env.pool.QueryRow(ctx,
		"SELECT COUNT(*), COALESCE(SUM(credit_amount), 0) FROM credit_ledger WHERE leaf_id = $1",
		proj.ID).Scan(&creditCount, &creditTotal)
	if err != nil {
		t.Fatalf("query credit: %v", err)
	}
	// 5 WUs * 2 volunteers = 10 credit entries
	if creditCount != 10 {
		t.Errorf("credit entries = %d, want 10", creditCount)
	}
	if creditTotal != 50.0 { // 10 * 5.0
		t.Errorf("total credit = %.1f, want 50.0", creditTotal)
	}

	// --- Step 7: Verify RAC ---
	var racCountA, racCountB float64
	err = env.pool.QueryRow(ctx,
		"SELECT rac FROM volunteer_rac WHERE volunteer_id = $1 AND leaf_id = $2",
		volAIDParsed, proj.ID).Scan(&racCountA)
	if err != nil {
		t.Fatalf("query vol A RAC: %v", err)
	}
	if racCountA <= 0 {
		t.Errorf("vol A RAC = %f, want > 0", racCountA)
	}

	err = env.pool.QueryRow(ctx,
		"SELECT rac FROM volunteer_rac WHERE volunteer_id = $1 AND leaf_id = $2",
		volBIDParsed, proj.ID).Scan(&racCountB)
	if err != nil {
		t.Fatalf("query vol B RAC: %v", err)
	}
	if racCountB <= 0 {
		t.Errorf("vol B RAC = %f, want > 0", racCountB)
	}

	// --- Step 8: Verify attestations ---
	var attCount int
	err = env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM credit_attestations WHERE leaf_id = $1 AND validation_outcome = 'AGREED'",
		proj.ID).Scan(&attCount)
	if err != nil {
		t.Fatalf("query attestations: %v", err)
	}
	if attCount != 10 { // 5 WUs * 2 volunteers
		t.Errorf("AGREED attestations = %d, want 10", attCount)
	}

	// --- Step 13: Verify volunteer stats endpoint ---
	resp = httpReq(t, "GET", env.httpURL+"/api/v1/volunteers/"+volAID+"/stats", nil)
	requireStatus(t, resp, http.StatusOK, "volunteer stats")
	var volStats struct {
		VolunteerID             string `json:"volunteer_id"`
		TotalCredit             float64 `json:"total_credit"`
		TotalWorkUnitsCompleted int    `json:"total_work_units_completed"`
		Leafs                   []struct {
			LeafID   string  `json:"leaf_id"`
			TotalCredit float64 `json:"total_credit"`
			RAC         float64 `json:"rac"`
		} `json:"leafs"`
	}
	decodeJSON(t, resp, &volStats)

	if volStats.TotalCredit != 25.0 {
		t.Errorf("vol A total_credit = %f, want 25.0", volStats.TotalCredit)
	}
	if volStats.TotalWorkUnitsCompleted != 5 {
		t.Errorf("vol A completed = %d, want 5", volStats.TotalWorkUnitsCompleted)
	}
	if len(volStats.Leafs) != 1 {
		t.Errorf("vol A leaf count = %d, want 1", len(volStats.Leafs))
	}

	// --- Step 14: Verify results list endpoint ---
	resp = httpReq(t, "GET", leafURL+"/results", nil)
	requireStatus(t, resp, http.StatusOK, "results list")
	var resultsResp struct {
		Data []struct {
			ID               string `json:"id"`
			WorkUnitID       string `json:"work_unit_id"`
			ValidationStatus string `json:"validation_status"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &resultsResp)

	if len(resultsResp.Data) != 10 { // 5 WUs * 2 results each
		t.Errorf("results count = %d, want 10", len(resultsResp.Data))
	}
	for _, r := range resultsResp.Data {
		if r.ValidationStatus != "AGREED" {
			t.Errorf("result %s status = %s, want AGREED", r.ID, r.ValidationStatus)
		}
	}
}

// --- Scenario 2: Disagreement and Reassignment ---

func TestAlphaE2E_Scenario2_Disagreement(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create user and leaf with redundancy=2.
	userID := createTestUser(t, env.pool, ctx, "alpha-s2")
	creditAmount := 3.0
	proj := createAndActivateProject(t, env, ctx, leaf.CreateLeafRequest{
		Name:         "Alpha E2E Disagreement",
		Description:  "Disagreement and reassignment test",
		ResearchArea: []string{"testing"},
		TaskPattern:  leaf.PatternParameterSweep,
		IsOngoing:    false,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}, leaf.ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}, leaf.CreditConfig{
		CreditPerValidatedWorkUnit: creditAmount,
	})

	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	// Generate 1 work unit.
	genReq := workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{
			"x": []interface{}{float64(99)},
		},
	}
	resp := httpReq(t, "POST", leafURL+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate")
	var genResp workunit.GenerateResponse
	decodeJSON(t, resp, &genResp)

	// Register volunteers A and B.
	volAPubKey := genVolunteerKey(t)
	volBPubKey := genVolunteerKey(t)
	volAID := registerVolunteer(t, env, ctx, volAPubKey, "Disagree Vol A")
	volBID := registerVolunteer(t, env, ctx, volBPubKey, "Disagree Vol B")
	volBIDParsed := types.MustParseID(volBID)

	// Vol A requests work unit.
	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volAPubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volAID,
		PublicKey:   volAPubKey,
	})
	if err != nil {
		t.Fatalf("vol A request: %v", err)
	}

	// Create redundant assignment for vol B.
	createRedundantAssignment(t, env.pool, ctx, wuResp.WorkUnitId, volBIDParsed)

	// Submit DIFFERENT results → disagreement.
	outputA := []byte(`{"answer": "alpha"}`)
	outputB := []byte(`{"answer": "bravo"}`)

	_, err = env.grpc.SubmitResult(signFor(t, ctx, volAPubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuResp.WorkUnitId,
		VolunteerId:          volAID,
		PublicKey:            volAPubKey,
		OutputData:           outputA,
		OutputChecksumSha256: sha256Hex(outputA),
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 60, CpuSecondsUser: 50, CpuCoresUsed: 1},
	})
	if err != nil {
		t.Fatalf("vol A submit: %v", err)
	}

	_, err = env.grpc.SubmitResult(signFor(t, ctx, volBPubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuResp.WorkUnitId,
		VolunteerId:          volBID,
		PublicKey:            volBPubKey,
		OutputData:           outputB,
		OutputChecksumSha256: sha256Hex(outputB),
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 60, CpuSecondsUser: 50, CpuCoresUsed: 1},
	})
	if err != nil {
		t.Fatalf("vol B submit: %v", err)
	}

	// Verify: DISAGREED attestations with credit_amount=0.
	var disagreedAttCount int
	err = env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM credit_attestations WHERE work_unit_id = $1 AND validation_outcome = 'DISAGREED' AND credit_amount = 0",
		types.MustParseID(wuResp.WorkUnitId)).Scan(&disagreedAttCount)
	if err != nil {
		t.Fatalf("query disagreed attestations: %v", err)
	}
	if disagreedAttCount != 2 {
		t.Errorf("DISAGREED attestations = %d, want 2", disagreedAttCount)
	}

	// Verify: work unit re-queued with HIGH priority.
	var wuState, priority string
	err = env.pool.QueryRow(ctx,
		"SELECT state, priority FROM work_units WHERE id = $1",
		types.MustParseID(wuResp.WorkUnitId)).Scan(&wuState, &priority)
	if err != nil {
		t.Fatalf("query WU state: %v", err)
	}
	if wuState != "QUEUED" {
		t.Errorf("WU state = %q, want QUEUED", wuState)
	}
	if priority != "HIGH" {
		t.Errorf("WU priority = %q, want HIGH", priority)
	}

	// Register volunteers C and D and resolve the work unit.
	volCPubKey := genVolunteerKey(t)
	volDPubKey := genVolunteerKey(t)
	volCID := registerVolunteer(t, env, ctx, volCPubKey, "Disagree Vol C")
	volDID := registerVolunteer(t, env, ctx, volDPubKey, "Disagree Vol D")
	volDIDParsed := types.MustParseID(volDID)

	// Vol C requests reassigned work unit.
	wuRespC, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volCPubKey), &lettucev1.RequestWorkUnitRequest{
		VolunteerId: volCID,
		PublicKey:   volCPubKey,
	})
	if err != nil {
		t.Fatalf("vol C request: %v", err)
	}
	if wuRespC.WorkUnitId != wuResp.WorkUnitId {
		t.Errorf("vol C got WU %s, want %s (reassigned)", wuRespC.WorkUnitId, wuResp.WorkUnitId)
	}

	// Create redundant assignment for vol D.
	createRedundantAssignment(t, env.pool, ctx, wuResp.WorkUnitId, volDIDParsed)

	// Both submit matching results.
	outputGood := []byte(`{"answer": "correct"}`)
	checksumGood := sha256Hex(outputGood)

	_, err = env.grpc.SubmitResult(signFor(t, ctx, volCPubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuResp.WorkUnitId,
		VolunteerId:          volCID,
		PublicKey:            volCPubKey,
		OutputData:           outputGood,
		OutputChecksumSha256: checksumGood,
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 60, CpuSecondsUser: 50, CpuCoresUsed: 1},
	})
	if err != nil {
		t.Fatalf("vol C submit: %v", err)
	}

	_, err = env.grpc.SubmitResult(signFor(t, ctx, volDPubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuResp.WorkUnitId,
		VolunteerId:          volDID,
		PublicKey:            volDPubKey,
		OutputData:           outputGood,
		OutputChecksumSha256: checksumGood,
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 60, CpuSecondsUser: 50, CpuCoresUsed: 1},
	})
	if err != nil {
		t.Fatalf("vol D submit: %v", err)
	}

	// Verify: WU now VALIDATED.
	err = env.pool.QueryRow(ctx,
		"SELECT state FROM work_units WHERE id = $1",
		types.MustParseID(wuResp.WorkUnitId)).Scan(&wuState)
	if err != nil {
		t.Fatalf("query final state: %v", err)
	}
	if wuState != "VALIDATED" {
		t.Errorf("WU state = %q, want VALIDATED", wuState)
	}

	// Verify: credit only granted to C and D (not A or B for this work unit).
	var creditForAB int
	err = env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM credit_ledger WHERE work_unit_id = $1 AND volunteer_id IN ($2, $3)",
		types.MustParseID(wuResp.WorkUnitId), types.MustParseID(volAID), types.MustParseID(volBID),
	).Scan(&creditForAB)
	if err != nil {
		t.Fatalf("query A/B credit: %v", err)
	}
	if creditForAB != 0 {
		t.Errorf("credit entries for A/B = %d, want 0", creditForAB)
	}

	var creditForCD int
	err = env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM credit_ledger WHERE work_unit_id = $1 AND volunteer_id IN ($2, $3)",
		types.MustParseID(wuResp.WorkUnitId), types.MustParseID(volCID), types.MustParseID(volDID),
	).Scan(&creditForCD)
	if err != nil {
		t.Fatalf("query C/D credit: %v", err)
	}
	if creditForCD != 2 {
		t.Errorf("credit entries for C/D = %d, want 2", creditForCD)
	}
}

// --- Helpers ---

func createTestUser(t *testing.T, pool *pgxpool.Pool, ctx context.Context, prefix string) types.ID {
	t.Helper()
	userID := types.NewID()
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		userID,
		fmt.Sprintf("%s-%s@test.example.com", prefix, uuid.New().String()[:8]),
		fmt.Sprintf("%s-%s", prefix, uuid.New().String()[:8]),
		prefix+" Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return userID
}

func createAndActivateProject(
	t *testing.T,
	env *testEnv,
	ctx context.Context,
	req leaf.CreateLeafRequest,
	valCfg leaf.ValidationConfig,
	creditCfg leaf.CreditConfig,
) leaf.Leaf {
	t.Helper()

	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs", req)
	requireStatus(t, resp, http.StatusCreated, "create leaf")
	var proj leaf.Leaf
	decodeJSON(t, resp, &proj)

	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()

	// Configure.
	resp = httpReq(t, "POST", leafURL+"/configure", nil)
	requireStatus(t, resp, http.StatusOK, "configure")
	decodeJSON(t, resp, &proj)

	// Update configs.
	execCfg := leaf.ExecutionConfig{
		Runtime:         "NATIVE",
		Binaries:        map[string]string{"linux-amd64": "https://example.com/bin/linux-amd64"},
		BinaryChecksums: map[string]string{"linux-amd64": "0000000000000000000000000000000000000000000000000000000000000000"},
		MaxMemoryMB:     4096,
		MaxDiskMB:       10240,
		MaxCPUSeconds:   3600,
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
		SplittingConfig:    map[string]interface{}{"x": []interface{}{float64(1)}},
	}
	// map_reduce leafs require a splitting_strategy (param-sweep must NOT have one);
	// supply a valid by_record strategy so map_reduce leafs pass activation validation.
	if req.TaskPattern == leaf.PatternMapReduce {
		byRecord := "by_record"
		dataCfg.SplittingStrategy = &byRecord
		dataCfg.SplittingConfig = map[string]interface{}{"records_per_chunk": float64(10)}
	}
	updateReq := leaf.UpdateLeafRequest{
		ExecutionConfig:      &execCfg,
		ValidationConfig:     &valCfg,
		FaultToleranceConfig: &ftCfg,
		DataConfig:           &dataCfg,
		CreditConfig:         &creditCfg,
	}
	resp = httpReq(t, "PUT", leafURL, updateReq)
	requireStatus(t, resp, http.StatusOK, "update configs")
	decodeJSON(t, resp, &proj)

	// Activate.
	resp = httpReq(t, "POST", leafURL+"/activate", nil)
	requireStatus(t, resp, http.StatusOK, "activate")
	decodeJSON(t, resp, &proj)

	if proj.State != leaf.StateActive {
		t.Fatalf("leaf state = %q, want ACTIVE", proj.State)
	}
	return proj
}

func registerVolunteer(t *testing.T, env *testEnv, ctx context.Context, pubKey []byte, name string) string {
	t.Helper()
	// C1: RegisterVolunteer must be signed by the key being registered (authedKey == req.PublicKey).
	regResp, err := env.grpc.RegisterVolunteer(signFor(t, ctx, pubKey), &lettucev1.RegisterVolunteerRequest{
		PublicKey:   pubKey,
		DisplayName: name,
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores:      8,
			CpuModel:      "Test CPU",
			MaxCpuCores:   4,
			MemoryTotalMb: 32768,
			MaxMemoryMb:   16384,
		},
		// Support NATIVE and CONTAINER so this shared helper can serve both native
		// leafs and the v08 CONTAINER (alpine) map-reduce scenario; the scheduler
		// matches a leaf's runtime against this set.
		AvailableRuntimes: []string{"NATIVE", "CONTAINER"},
		SchedulingMode:    "ALWAYS",
	})
	if err != nil {
		t.Fatalf("register volunteer %s: %v", name, err)
	}
	return regResp.VolunteerId
}

func createRedundantAssignment(t *testing.T, pool *pgxpool.Pool, ctx context.Context, wuIDStr string, volID types.ID) {
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

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func httpReq(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func requireStatus(t *testing.T, resp *http.Response, want int, step string) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("[%s] expected %d, got %d: %s", step, want, resp.StatusCode, body)
	}
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func decompressGZ(t *testing.T, data []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	return out
}
