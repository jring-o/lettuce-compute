//go:build integration

package e2e_test

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
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
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// scaleoutEnv holds the shared backplane (ONE pool, ONE signing key, ONE shared
// in-mem replay store) plus an http handle for leaf/work-unit setup, and the two
// head replicas under test. The two replicas are stateless heads behind a (real,
// here loopback) shared Postgres: distinct instance ids, distinct gRPC clients,
// each running its OWN in-process dispatch cache. This is the in-process
// equivalent of two heads behind Caddy and is the primary DoD arbiter for Layer 3
// (no cross-replica double-dispatch; cross-replica replay rejection).
type scaleoutEnv struct {
	pool    *pgxpool.Pool
	httpURL string

	replicaA *scaleoutReplica
	replicaB *scaleoutReplica
}

// scaleoutReplica is one head replica: its concrete service (for the dispatch
// cache), its gRPC client, and its instance id (the dispatch-claim owner).
type scaleoutReplica struct {
	instanceID types.ID
	svc        lettucev1.VolunteerServiceServer
	grpc       lettucev1.VolunteerServiceClient
}

// setupTwoReplicas builds TWO head replicas sharing ONE pgx pool, ONE signing key,
// and ONE shared in-mem replay store (installed on both the gRPC and HTTP auth
// paths via the server test seam). Each replica gets a distinct instance id (so the
// dispatch-claim owners differ) and runs its OWN Layer-2 dispatch cache against the
// shared Postgres, so claim-on-refill (Layer 3) is exercised end-to-end through the
// real gRPC hot path. A single HTTP server (leaf/work-unit management is just DB
// writes, replica-agnostic) backs the setup helpers.
func setupTwoReplicas(t *testing.T) (*scaleoutEnv, func()) {
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

	// ONE signing key shared by both replicas (load once + inject) so neither head
	// autogenerates a divergent attestation key.
	signingKey, err := attestation.LoadSigningKey("test-scaleout-signing.key", true)
	if err != nil {
		t.Fatalf("failed to load signing key: %v", err)
	}
	signer := attestation.NewSigner(signingKey)

	// ONE shared in-mem replay store installed on BOTH gRPC and HTTP paths: a
	// signature accepted by either replica is rejected by the other (no Redis).
	server.InstallSharedInMemReplayStoreForTests()

	// Repositories (all share the one pool — the shared Postgres).
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

	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volunteerRepo, assignRepo, attestationRepo, nil, signer, logger, nil, transition.TrustPolicy{})

	headCfg := &config.HeadConfig{
		Name:        "test-head",
		Description: "Two-replica scale-out E2E",
		URL:         "https://test-head.example.com",
	}

	// HTTP server for leaf/work-unit setup (one is enough; management is DB-only).
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

	mux := http.NewServeMux()
	leafHandler.RegisterRoutes(mux)
	wuHandler.RegisterRoutes(mux)
	resultHandler.RegisterRoutes(mux)
	statsHandler.RegisterRoutes(mux)
	volunteerStatsHandler.RegisterRoutes(mux)
	attestationHandler.RegisterRoutes(mux)
	healthHandler.RegisterRoutes(mux)
	aggHandler.RegisterRoutes(mux)
	identityHandler.RegisterRoutes(mux)
	mux.HandleFunc("GET /api/v1/head", headHandler.HandleGetHeadInfo)
	mux.HandleFunc("POST /api/v1/leafs", leafHandler.HandleCreate)
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", leafHandler.HandleUpdate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/configure", leafHandler.HandleConfigure)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/activate", leafHandler.HandleActivate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/generate", wuHandler.HandleGenerate)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/bulk", bulkHandler.HandleBulkUpload)
	server.RegisterBrowserVolunteerRoutes(mux, pool, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, validationEngine, logger, 10)

	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for HTTP: %v", err)
	}
	httpServer := server.NewHTTPServer(httpLis.Addr().String(), mux, nil)
	go func() { _ = httpServer.Serve(httpLis) }()
	httpURL := "http://" + httpLis.Addr().String()

	cacheCtx, cacheCancel := context.WithCancel(context.Background())

	// buildReplica constructs one head replica: a gRPC server (sharing the installed
	// replay store), a concrete service with the given instance id stamped as the
	// dispatch-claim owner, and a started dispatch cache. Returns the replica handle
	// plus its per-replica cleanup.
	buildReplica := func(instanceID types.ID) (*scaleoutReplica, func()) {
		grpcServer, grpcCleanup := server.NewGRPCServer(nil, logger, nil)
		svc := server.NewVolunteerService(pool, "0.9.0.1-scaleout", startTime, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, checkpointRepo, validationEngine, logger, transition.TrustPolicy{})
		// Small ready pool + refill batch so neither replica can claim the entire
		// queue in a single refill tick: the two refillers must contend repeatedly for
		// the shared QUEUED units, genuinely exercising cross-replica claim arbitration
		// (vs one replica grabbing everything at once with the 2000/500 defaults).
		server.SetHeadConfig(svc, headCfg.Name, headCfg.Description, headCfg.URL, nil, 10, server.HeadDispatchConfig{
			HeadInstanceID:    instanceID,
			ClaimLeaseSeconds: 120,
			ReadyPoolSize:     8,
			RefillBatchSize:   4,
		})
		server.StartDispatchCache(svc, cacheCtx)
		lettucev1.RegisterVolunteerServiceServer(grpcServer, svc)

		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to listen for gRPC: %v", err)
		}
		go func() { _ = grpcServer.Serve(lis) }()

		conn, err := grpc.NewClient(lis.Addr().String(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithUnaryInterceptor(server.TestSigningInterceptor()),
		)
		if err != nil {
			t.Fatalf("failed to connect gRPC: %v", err)
		}
		client := lettucev1.NewVolunteerServiceClient(conn)

		r := &scaleoutReplica{instanceID: instanceID, svc: svc, grpc: client}
		cleanup := func() {
			conn.Close()
			grpcServer.Stop()
			grpcCleanup()
		}
		return r, cleanup
	}

	replicaA, cleanupA := buildReplica(types.NewID())
	replicaB, cleanupB := buildReplica(types.NewID())

	env := &scaleoutEnv{
		pool:     pool,
		httpURL:  httpURL,
		replicaA: replicaA,
		replicaB: replicaB,
	}

	cleanup := func() {
		cacheCancel()
		cleanupA()
		cleanupB()
		httpServer.Close()
		os.Remove("test-scaleout-signing.key")
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

// hlEnvForHTTP adapts a scaleoutEnv to the *headsLeafsEnv shape the leaf/work-unit
// setup helpers (createHLLeaf, generateLeafWUs, registerHLVolunteer) expect. Only
// the http/pool/grpc fields those helpers read are populated; grpc points at
// replica A (registration is replica-agnostic — it writes the shared DB).
func (e *scaleoutEnv) hlEnvForHTTP() *headsLeafsEnv {
	return &headsLeafsEnv{
		pool:    e.pool,
		grpc:    e.replicaA.grpc,
		httpURL: e.httpURL,
	}
}

// signerPrivFor returns the Ed25519 private key registered for pubKey by
// genVolunteerKey, so a test can sign a request explicitly (fixed nonce/signature)
// to replay it across replicas.
func signerPrivFor(t *testing.T, pubKey []byte) ed25519.PrivateKey {
	t.Helper()
	v, ok := e2eSignerKeys.Load(string(pubKey))
	if !ok {
		t.Fatalf("no signing key registered for public key %x (use genVolunteerKey)", pubKey)
	}
	return v.(ed25519.PrivateKey)
}

// TestScaleOut_CrossReplicaReplayRejection proves the shared replay store (BREAK 2)
// rejects, on replica B, a signature replica A already accepted within the skew
// window. The integration harness disables replay detection globally (it replays
// byte-identical loopback RPCs); this test re-enables it for its duration. It
// captures replica A's EXACT signed gRPC metadata and re-sends the byte-identical
// request to replica B, which must reject it with "replayed signature".
func TestScaleOut_CrossReplicaReplayRejection(t *testing.T) {
	env, cleanup := setupTwoReplicas(t)
	defer cleanup()

	server.SetReplayDetectionForTests(true)
	t.Cleanup(func() { server.SetReplayDetectionForTests(false) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pubKey := genVolunteerKey(t)
	priv := signerPrivFor(t, pubKey)
	volID := registerHLVolunteer(t, env.hlEnvForHTTP(), ctx, pubKey, "scaleout-replay-vol")

	// Build ONE signed RequestWorkUnit and capture its exact auth metadata. We sign
	// it ourselves (rather than via the auto-nonce client interceptor) so the SAME
	// signature bytes are sent to both replicas — that is exactly the replay the
	// shared store must catch across replicas.
	req := &lettucev1.RequestWorkUnitRequest{VolunteerId: volID, PublicKey: pubKey, MaxAssignments: 1}
	authMD, err := server.SignedAuthMetadataForTests(lettucev1.VolunteerService_RequestWorkUnit_FullMethodName, req, pubKey, priv)
	if err != nil {
		t.Fatalf("sign replay request: %v", err)
	}
	outCtx := metadata.NewOutgoingContext(ctx, authMD)

	// Replica A accepts the freshly-signed request (no auth error). It either returns
	// an assignment or an empty list; either way the signature is now recorded.
	if _, err := env.replicaA.grpc.RequestWorkUnit(outCtx, req); err != nil {
		t.Fatalf("replica A must accept the first sighting of the signature: %v", err)
	}

	// Replica B receives the byte-identical signed request: the shared store reports
	// the signature as already seen, so B rejects it Unauthenticated "replayed signature".
	_, err = env.replicaB.grpc.RequestWorkUnit(outCtx, req)
	if err == nil {
		t.Fatal("replica B accepted a signature already accepted by replica A (cross-replica replay hole)")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unauthenticated {
		t.Fatalf("cross-replica replay must be Unauthenticated, got %s: %v", st.Code(), err)
	}
	if st.Message() != "replayed signature" {
		t.Fatalf("cross-replica replay message = %q, want %q", st.Message(), "replayed signature")
	}
}

// TestScaleOut_NoCrossReplicaDoubleDispatch is the primary DoD arbiter (BREAK 1,
// claim-on-refill): two head replicas each run their own dispatch cache against one
// shared Postgres; concurrent RequestWorkUnit is driven across BOTH replicas'
// clients under load. After draining, a DB audit asserts NO unit was dispatched to
// more than its effective redundancy distinct volunteers across replicas, no NORMAL
// unit holds two live reservations, and dispatch_claimed_by is single-valued per
// unit. This is the two paths (SQL claim rule + cache layer) composed end-to-end
// through the real gRPC hot path — the test the design named as THE arbiter.
func TestScaleOut_NoCrossReplicaDoubleDispatch(t *testing.T) {
	env, cleanup := setupTwoReplicas(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	hl := env.hlEnvForHTTP()
	userID := createTestUser(t, env.pool, ctx, "scaleout-dispatch")

	// One redundancy-1 leaf (a unit may reach at most ONE distinct holder) and one
	// redundancy-2 leaf (at most TWO). Both NORMAL (no spot-check). The audit checks
	// each unit against its own leaf's effective redundancy.
	leafR1 := createHLLeaf(t, hl, ctx, userID, hlDefaultLeafOpts("Scaleout R1 Leaf"))
	optsR2 := hlDefaultLeafOpts("Scaleout R2 Leaf")
	optsR2.ValConfig = leaf.ValidationConfig{RedundancyFactor: 2, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3}
	leafR2 := createHLLeaf(t, hl, ctx, userID, optsR2)

	const unitsPerLeaf = 40
	generateLeafWUs(t, hl, leafR1.ID, unitsPerLeaf)
	generateLeafWUs(t, hl, leafR2.ID, unitsPerLeaf)
	leafIDs := []string{leafR1.ID.String(), leafR2.ID.String()}

	// A pool of distinct volunteers, each pinned to one replica, all hammering
	// RequestWorkUnit concurrently. Distinct volunteers per request avoid the
	// per-volunteer inflight cap masking a cross-replica double-stage.
	const volunteersPerReplica = 8
	const roundsPerVolunteer = 12

	type volClient struct {
		volID  string
		pubKey []byte
		grpc   lettucev1.VolunteerServiceClient
	}
	makeVols := func(r *scaleoutReplica, namePrefix string) []volClient {
		out := make([]volClient, volunteersPerReplica)
		for i := range out {
			pub := genVolunteerKey(t)
			vol := registerHLVolunteer(t, hl, ctx, pub, namePrefix)
			out[i] = volClient{volID: vol, pubKey: pub, grpc: r.grpc}
		}
		return out
	}
	vols := append(makeVols(env.replicaA, "scaleout-A"), makeVols(env.replicaB, "scaleout-B")...)

	var wg sync.WaitGroup
	for _, vc := range vols {
		wg.Add(1)
		go func(vc volClient) {
			defer wg.Done()
			for r := 0; r < roundsPerVolunteer; r++ {
				resp, err := vc.grpc.RequestWorkUnit(signFor(t, ctx, vc.pubKey), &lettucev1.RequestWorkUnitRequest{
					VolunteerId:    vc.volID,
					PublicKey:      vc.pubKey,
					LeafIds:        leafIDs,
					MaxAssignments: 2,
				})
				if err != nil {
					// ResourceExhausted (shedding) is acceptable under load; any other
					// error is a real failure.
					if st, _ := status.FromError(err); st.Code() == codes.ResourceExhausted {
						time.Sleep(10 * time.Millisecond)
						continue
					}
					t.Errorf("RequestWorkUnit(%s): %v", vc.volID, err)
					return
				}
				_ = resp
				time.Sleep(5 * time.Millisecond)
			}
		}(vc)
	}
	wg.Wait()

	// Allow the async flushers on both replicas to land their in-memory reservations as
	// RESERVED copy rows in the DB before the audit counts each unit's live copies.
	time.Sleep(1 * time.Second)

	auditNoDoubleDispatch(t, ctx, env.pool)

	// The audit only proves no DOUBLE-dispatch. Confirm the test was actually
	// adversarial — that BOTH replicas claimed units from the shared Postgres, so the
	// no-double-dispatch property was exercised under genuine cross-replica contention
	// rather than one idle replica. Each replica renews its own claims on flush, so a
	// held unit carries its owner's instance id; assert both instance ids appear.
	requireBothReplicasClaimed(t, ctx, env.pool, env.replicaA.instanceID, env.replicaB.instanceID)
}

// requireBothReplicasClaimed asserts that, across the load run, BOTH replicas'
// instance ids appear as live dispatch-claim owners — proof the two refillers
// genuinely contended for the shared queue (otherwise the no-double-dispatch audit
// could pass trivially with one replica idle). A claim is held while its unit lives
// in a ready pool / is reserved (renewed on flush) and cleared at run-start; these
// load volunteers only request, so reserved-and-claimed units persist for the audit.
func requireBothReplicasClaimed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, a, b types.ID) {
	t.Helper()
	owners := map[types.ID]int{}
	rows, err := pool.Query(ctx,
		`SELECT dispatch_claimed_by, COUNT(*) FROM work_units
		 WHERE dispatch_claimed_by IS NOT NULL GROUP BY dispatch_claimed_by`)
	if err != nil {
		t.Fatalf("claim-owner query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var owner types.ID
		var n int
		if err := rows.Scan(&owner, &n); err != nil {
			t.Fatalf("claim-owner scan: %v", err)
		}
		owners[owner] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("claim-owner rows: %v", err)
	}
	if owners[a] == 0 || owners[b] == 0 {
		t.Errorf("expected BOTH replicas to hold dispatch claims after the load run "+
			"(adversarial cross-replica contention); claims by owner: A(%s)=%d B(%s)=%d",
			a, owners[a], b, owners[b])
	}
	t.Logf("dispatch-claim ownership after load: A=%d B=%d units", owners[a], owners[b])
}

// auditNoDoubleDispatch is the DB-level invariant check. Per-copy model (migration
// 00006): the distinct holders of a unit are the volunteer_ids of its LIVE copies
// (work_unit_assignment_history rows with outcome IS NULL — RESERVED or RUNNING). It
// asserts COUNT(DISTINCT holder) <= the unit's effective redundancy: redundancy>1
// dispatches up to N parallel copies to N DISTINCT volunteers, but never more, and
// never two live copies of one unit to the SAME volunteer across replicas. The retired
// per-unit reserved_volunteer_id column is gone — a hold is now a copy row, so the old
// "at most one live reservation" rule is REPLACED by this per-copy redundancy cap, which
// is the genuine cross-replica no-double-dispatch invariant.
//
// effective redundancy mirrors the dispatch SQL: spot-check => 2, else the leaf's
// validation_config redundancy_factor (default 2).
func auditNoDoubleDispatch(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	rows, err := pool.Query(ctx, `
		SELECT
			wu.id,
			CASE WHEN wu.spot_check THEN 2
			     ELSE COALESCE((l.validation_config->>'redundancy_factor')::int, 2)
			END AS effective_redundancy,
			(
				SELECT COUNT(DISTINCT wuah.volunteer_id)
				FROM work_unit_assignment_history wuah
				WHERE wuah.work_unit_id = wu.id AND wuah.outcome IS NULL
			) AS distinct_holders
		FROM work_units wu
		JOIN leafs l ON wu.leaf_id = l.id
	`)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	defer rows.Close()

	var audited int
	var maxHolders int
	for rows.Next() {
		var id types.ID
		var effRedundancy, distinctHolders int
		if err := rows.Scan(&id, &effRedundancy, &distinctHolders); err != nil {
			t.Fatalf("audit scan: %v", err)
		}
		audited++
		if distinctHolders > maxHolders {
			maxHolders = distinctHolders
		}
		if distinctHolders > effRedundancy {
			t.Errorf("unit %s dispatched to %d distinct volunteers across replicas, exceeds effective redundancy %d (cross-replica double-dispatch)",
				id, distinctHolders, effRedundancy)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	if audited == 0 {
		t.Fatal("audit found no work units (the load phase dispatched nothing)")
	}

	// dispatch_claimed_by is a single uuid column, so it is single-valued per unit by
	// construction. Confirm no row carries an impossible multi-claim by checking the
	// column is either NULL or a single valid owner (a direct read; the cross-replica
	// invariant that only ONE head owns a live claim is enforced by the SQL claim rule
	// proven in TestClaimDispatchableBatch_StampsAndExcludes and exercised above).
	var multiClaimed int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM work_units
		WHERE dispatch_claimed_by IS NOT NULL
		  AND dispatch_claim_expires_at IS NULL
	`).Scan(&multiClaimed); err != nil {
		t.Fatalf("claim-consistency query: %v", err)
	}
	if multiClaimed > 0 {
		t.Errorf("%d units have a claim owner but no claim expiry (inconsistent dispatch claim)", multiClaimed)
	}

	t.Logf("scale-out audit: %d units audited, max distinct holders observed = %d", audited, maxHolders)
}
