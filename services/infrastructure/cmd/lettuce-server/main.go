package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/atproto"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/bootstrap"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/custom"
	"github.com/lettuce-compute/infrastructure/internal/database"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/health"
	"github.com/lettuce-compute/infrastructure/internal/identity"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/mapreduce"
	"github.com/lettuce-compute/infrastructure/internal/montecarlo"
	"github.com/lettuce-compute/infrastructure/internal/paramsweep"
	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	configPath := flag.String("config", "lettuce.yaml", "path to configuration file")
	flag.Parse()

	fmt.Printf("lettuce-server %s (built %s) %s/%s\n", version, buildTime, runtime.GOOS, runtime.GOARCH)

	// Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	// Validate required admin API key.
	adminAPIKey := os.Getenv("LETTUCE_ADMIN_API_KEY")
	if adminAPIKey == "" {
		fmt.Fprintln(os.Stderr, "LETTUCE_ADMIN_API_KEY is required. Generate one with: openssl rand -base64 32")
		os.Exit(1)
	}

	// Resolve this head replica's stable instance id ONCE (Layer 3): it is the
	// dispatch-claim owner, the leadership log identity, and a log dimension.
	// Generated here from config/env (auto-uuid when unset) so every log line and
	// the dispatch claim agree on the same value for the process lifetime.
	instanceID := cfg.Head.EffectiveInstanceID()

	// Initialize logger. Stamp the instance id on every log line so multi-replica
	// deployments are attributable to a specific head.
	logger := logging.NewLogger(cfg.Log.Level, cfg.Log.Format).With("head_instance_id", instanceID.String())
	logging.SetDefault(logger)

	// Apply the operator-tuned NoDeadline reclaim ceiling so it actually changes
	// the deadline_seconds stamped on NoDeadline work units (eager generation, the
	// lazy generation manager, and custom bulk upload all read this). Done before
	// any generation path is wired so the knob is never a silent no-op.
	generate.SetNoDeadlineCeilingSeconds(cfg.Head.EffectiveNoDeadlineCeilingSeconds())

	// Load TLS config.
	tlsCfg, err := server.LoadTLSConfig(cfg.TLS)
	if err != nil {
		slog.Error("failed to load TLS config", "error", err)
		os.Exit(1)
	}

	if tlsCfg != nil {
		slog.Info("TLS enabled")
	} else {
		slog.Info("TLS disabled (no certificate configured)")
	}

	// Connect to database.
	ctx := context.Background()
	pool, err := database.ConnectWithRetry(ctx, cfg.Database, 5, 1*time.Second)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	slog.Info("database connected")

	// Run migrations.
	if err := database.RunMigrations(cfg.Database.DatabaseURL()); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}

	// Bootstrap admin user and dashboard API key (idempotent).
	if err := bootstrap.AdminUser(ctx, pool, logger); err != nil {
		slog.Error("failed to bootstrap admin user", "error", err)
		os.Exit(1)
	}
	if err := bootstrap.DashboardAPIKey(ctx, pool, logger); err != nil {
		slog.Error("failed to bootstrap dashboard API key", "error", err)
		os.Exit(1)
	}

	// Load attestation signing key. Fails closed if the key file is missing
	// unless dev auto-generation is explicitly opted into (LETTUCE_SIGNING_KEY_AUTOGEN).
	signingKey, err := attestation.LoadSigningKey(cfg.Signing.PrivateKeyPath, cfg.Signing.AutoGenerate)
	if err != nil {
		slog.Error("failed to load signing key", "error", err)
		os.Exit(1)
	}
	attestationSigner := attestation.NewSigner(signingKey)
	slog.Info("attestation signing key loaded", "path", cfg.Signing.PrivateKeyPath)

	startTime := time.Now()

	// Create API key repository.
	apiKeyRepo := apikey.NewPgxRepository(pool)

	// Create identity challenge store.
	challengeStore := identity.NewPgxChallengeStore(pool, logger)

	// Create repositories (shared between HTTP router and gRPC service).
	volunteerRepo := volunteer.NewPgxRepository(pool)
	// The dispatch queries resolve the trusted-corroborator reservation per leaf from the
	// head trust-gate policy (built from the same config sources as the validation-side
	// trustPolicy below). The zero policy (gate off, the default) leaves every dispatch query
	// byte-for-byte as before, so this is safe to wire unconditionally.
	wuRepo := workunit.NewPgxWorkUnitRepository(pool).
		WithTrustDispatch(server.TrustDispatchFromHeadConfig(&cfg.Head))
	leafRepo := leaf.NewPgxRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	resultRepo := result.NewPgxRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)
	creditRepo := credit.NewPgxRepository(pool)
	racRepo := credit.NewPgxRACRepository(pool)
	reliabilityRepo := reliability.NewPgxRepository(pool)
	attestationRepo := attestation.NewPgxRepository(pool)

	// Create checkpoint repository.
	checkpointDir := cfg.Storage.CheckpointDir
	if checkpointDir == "" {
		checkpointDir = "data/checkpoints"
	}
	checkpointRepo := checkpoint.NewPgxRepository(pool, checkpointDir)

	// Account-level trust (see internal/trust): ONE store + ONE resolved head policy threaded
	// into the validation engine (accrual), the transitioner + submit paths (the acceptance
	// gate), and the volunteer service (submit-time score stamping). The gate is off by default;
	// stamping and accrual are always active so trust accumulates before enforcement is enabled.
	trustRepo := trust.NewPgxRepository(pool)
	trustPolicy := server.TrustPolicyFromHeadConfig(&cfg.Head)
	if trustPolicy.GateEnabled {
		slog.Info("volunteer trust gate enabled",
			"min_trusted_corroborators", trustPolicy.DefaultMinCorroborators,
			"trust_floor", trustPolicy.DefaultFloor)
	}

	// Automatic standing backpressure (BG-24b PR-B): when enabled, every adjudicated
	// result folds into the submitting volunteer's decayed rejection-rate signal, which
	// drives AUTO-owned standing transitions with hysteresis (OK -> PROBATION -> BENCHED
	// and back); the standing itself is enforced by dispatch and validation since #88.
	// OFF by default — a nil recorder keeps the validation engine byte-for-byte as
	// before, legacy lifetime-rate WARN included.
	var standingRecorder standing.Recorder
	if cfg.Head.StandingBackpressureEnabled {
		standingRecorder = standing.NewPgxRecorder(pool, standing.BackpressureConfig{
			ProbationRate: cfg.Head.EffectiveStandingProbationRate(),
			OKRate:        cfg.Head.EffectiveStandingOKRate(),
			BenchRate:     cfg.Head.EffectiveStandingBenchRate(),
			MinSample:     float64(cfg.Head.EffectiveStandingMinSample()),
			BenchFor:      time.Duration(cfg.Head.EffectiveStandingBenchMinutes()) * time.Minute,
		})
		slog.Info("standing backpressure enabled",
			"probation_rate", cfg.Head.EffectiveStandingProbationRate(),
			"ok_rate", cfg.Head.EffectiveStandingOKRate(),
			"bench_rate", cfg.Head.EffectiveStandingBenchRate(),
			"min_sample", cfg.Head.EffectiveStandingMinSample(),
			"bench_minutes", cfg.Head.EffectiveStandingBenchMinutes())
	}

	// Create validation engine (shared between HTTP browser handlers and gRPC service).
	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volunteerRepo, assignRepo, attestationRepo, reliabilityRepo, attestationSigner, logger, trustRepo, trustPolicy).
		WithStandingBackpressure(standingRecorder)

	// The single transitioner (TODO #50): the sole decider of work-unit redundancy state.
	// SubmitResult and the fault monitor delegate every "complete / validate / reject / wait /
	// dead-letter" decision to it. The validation engine is its comparator + accept/reject
	// implementation; the per-unit lock is the cross-replica Postgres advisory lock. The head
	// trust gate is overlaid onto each leaf's policy here.
	transitioner := transition.NewTransitioner(
		transition.NewPgxLocker(pool, logger),
		wuRepo, leafRepo, resultRepo, validationEngine, trustPolicy, logger)

	// Parse trusted reverse-proxy networks for trust-aware client-IP extraction.
	// (Config validation already verified these parse; this cannot fail here.)
	trustedProxies, err := cfg.Server.ParsedTrustedProxies()
	if err != nil {
		slog.Error("failed to parse trusted proxies", "error", err)
		os.Exit(1)
	}
	if len(trustedProxies) > 0 {
		slog.Info("trusted proxies configured for client-IP extraction", "count", len(trustedProxies))
	} else {
		slog.Info("no trusted proxies configured; forwarding headers (X-Forwarded-For/X-Real-IP) are not trusted")
	}

	// Layer 3 scale-out: shared replay store + shared rate-limit store. When a
	// Redis URL is configured, a single Redis client backs BOTH the cross-replica
	// anti-replay dedup (key = signature alone, GLOBAL) and the cross-replica
	// rate-limit buckets, so N replicas behind the proxy do not double-accept a
	// replayed signature or grant each client N× its budget. Empty RedisURL keeps
	// the single-replica in-process behavior (in-mem replay cache + token buckets).
	//
	// The replay-store failure policy (fail-open default vs fail-closed) is applied
	// to BOTH the gRPC and HTTP auth paths.
	server.SetReplayFailsOpen(cfg.Head.ReplayFailsOpen())
	if cfg.Head.RedisURL != "" {
		redisClient, rerr := server.NewRedisClient(ctx, cfg.Head.RedisURL)
		if rerr != nil {
			slog.Error("failed to connect to redis for shared replay/rate-limit store", "error", rerr)
			os.Exit(1)
		}
		defer func() { _ = redisClient.Close() }()
		// One Redis client backs the shared replay store (both HTTP and gRPC auth)
		// and the shared rate-limit buckets. Install the shared replay store now;
		// the gRPC auth path receives it via server.SetSharedReplayStore (read by
		// NewGRPCServer below).
		server.SetSharedReplayStore(redisClient)    // gRPC + HTTP auth paths
		server.SetRateLimitRedisClient(redisClient) // shared rate-limit buckets
		slog.Info("shared replay + rate-limit store enabled (multi-replica)",
			"fail_mode", cfg.Head.EffectiveReplayFailMode())
	} else {
		slog.Info("no redis configured; using in-process replay cache + rate-limit buckets (single-replica)")
	}

	// Optional ATProto DID identity binding. The atproto client — and therefore both
	// the bind endpoint and the re-check worker — is constructed ONLY when the operator
	// enables binding; the default (disabled) leaves the whole subsystem inert.
	var atprotoClient *atproto.Client
	if cfg.Head.DIDBindingEnabled {
		atprotoClient = atproto.NewClient(cfg.Head.EffectiveDIDResolverURL(), nil, logger)
		slog.Info("DID identity binding enabled",
			"resolver", cfg.Head.EffectiveDIDResolverURL(),
			"collection", cfg.Head.EffectiveDIDBindingCollection())
	}

	// Create HTTP router and server.
	deps := &server.Dependencies{
		Pool:             pool,
		Logger:           logger,
		Version:          version,
		StartTime:        startTime,
		CORSOrigins:      cfg.Server.CORSOrigins,
		SigningPublicKey: attestationSigner.PublicKey(),
		AdminAPIKey:      adminAPIKey,
		ApiKeyRepo:       apiKeyRepo,
		ChallengeStore:   challengeStore,
		HeadConfig:       &cfg.Head,
		ValidationEngine: validationEngine,
		TrustedProxies:   trustedProxies,
		AtprotoClient:    atprotoClient,
	}
	router, rateLimitCleanup := server.NewRouter(deps)
	defer rateLimitCleanup()
	httpServer := server.NewHTTPServer(cfg.Server.HTTPAddr, router, tlsCfg)

	// Optional operator override for the gRPC rate-limit budgets (requests per
	// minute). Useful when a large fleet legitimately shares one source IP (a
	// single NAT, or a loopback load test). Unset/non-positive leaves defaults.
	if v := os.Getenv("LETTUCE_GRPC_PER_IP_RATE_LIMIT"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			server.SetGRPCRateLimits(n, 0)
			slog.Info("per-IP gRPC rate limit overridden", "per_min", n)
		}
	}
	if v := os.Getenv("LETTUCE_GRPC_PER_PUBKEY_RATE_LIMIT"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			server.SetGRPCRateLimits(0, n)
			slog.Info("per-pubkey gRPC rate limit overridden", "per_min", n)
		}
	}

	// Create gRPC server and register VolunteerService.
	grpcServer, grpcRateLimitCleanup := server.NewGRPCServer(tlsCfg, logger, trustedProxies)
	defer grpcRateLimitCleanup()

	volunteerSvc := server.NewVolunteerService(pool, version, startTime, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, checkpointRepo, validationEngine, logger, trustPolicy)
	weights := make(map[string]int32, len(cfg.Head.DefaultLeafWeights))
	for k, v := range cfg.Head.DefaultLeafWeights {
		weights[k] = int32(v)
	}
	server.SetHeadConfig(volunteerSvc, cfg.Head.Name, cfg.Head.Description, cfg.Head.URL, weights, cfg.Head.EffectiveMaxInflight(),
		server.HeadDispatchConfig{
			MaxBatchPerRequest:      cfg.Head.EffectiveMaxBatch(),
			LeaseSeconds:            cfg.Head.EffectiveLeaseSeconds(),
			MinSendIntervalSeconds:  cfg.Head.EffectiveMinSendIntervalSeconds(),
			MinRetryDelaySeconds:    cfg.Head.EffectiveMinRetryDelaySeconds(),
			MaxRetryDelaySeconds:    cfg.Head.EffectiveMaxRetryDelaySeconds(),
			RetryDelayJitterPct:     cfg.Head.EffectiveRetryDelayJitterPct(),
			TargetRequestRatePerSec: cfg.Head.EffectiveTargetRequestRatePerSec(),
			// Layer 2/3: in-process dispatch cache.
			ReadyPoolSize:           cfg.Head.EffectiveReadyPoolSize(),
			RefillBatchSize:         cfg.Head.EffectiveRefillBatchSize(),
			DispatchAdmissionCap:    cfg.Head.EffectiveDispatchAdmissionCap(),
			MaintenanceAdmissionCap: cfg.Head.EffectiveMaintenanceAdmissionCap(),
			FlushIntervalMs:         cfg.Head.EffectiveFlushIntervalMs(),
			FlushBatchSize:          cfg.Head.EffectiveFlushBatchSize(),
			// Layer 3: claim-on-refill. The instance id (configured or auto-generated)
			// is this replica's dispatch-claim owner; the bulk refill stamps it on each
			// staged unit so no other replica can double-hand it. Always set, so a
			// single-replica deploy is correct too (it simply reclaims its own claims).
			HeadInstanceID:    instanceID,
			ClaimLeaseSeconds: cfg.Head.EffectiveClaimLeaseSeconds(),
			// TODO #54: reliability-weighted adaptive in-flight quota.
			ReliabilityQuotaEnabled: cfg.Head.EffectiveReliabilityQuotaEnabled(),
			ReliabilityQuotaFloor:   cfg.Head.EffectiveReliabilityQuotaFloor(),
		})
	lettucev1.RegisterVolunteerServiceServer(grpcServer, volunteerSvc)

	// Start HTTP server.
	go func() {
		slog.Info("HTTP server starting", "addr", cfg.Server.HTTPAddr)
		var listenErr error
		if tlsCfg != nil {
			listenErr = httpServer.ListenAndServeTLS("", "")
		} else {
			listenErr = httpServer.ListenAndServe()
		}
		if listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			slog.Error("HTTP server error", "error", listenErr)
			os.Exit(1)
		}
	}()

	// Start gRPC server.
	go func() {
		lis, lisErr := net.Listen("tcp", cfg.Server.GRPCAddr)
		if lisErr != nil {
			slog.Error("failed to listen for gRPC", "addr", cfg.Server.GRPCAddr, "error", lisErr)
			os.Exit(1)
		}
		slog.Info("gRPC server starting", "addr", cfg.Server.GRPCAddr)
		if serveErr := grpcServer.Serve(lis); serveErr != nil {
			slog.Error("gRPC server error", "error", serveErr)
			os.Exit(1)
		}
	}()

	// Start background monitors.
	monitorCtx, monitorCancel := context.WithCancel(ctx)
	defer monitorCancel()

	// Layer 3 scale-out: split background work into PER-REPLICA jobs (run on every
	// head) and SINGLETON jobs (run on exactly one head — the advisory-lock leader).
	//
	// PER-REPLICA: the in-process dispatch cache (refiller + async flusher +
	// reconciler). Each replica owns the dispatch claims it stamped at refill
	// (claim-on-refill), so this MUST run everywhere — gating it would starve a
	// non-leader replica of work. After this, RequestWorkUnit serves reservations
	// from the cache with zero DB I/O on the hot path and sheds with
	// ResourceExhausted under overload.
	server.StartDispatchCache(volunteerSvc, monitorCtx)

	// SINGLETON (leader-only): jobs that double-act or pollute derived data if run
	// on every replica. A Postgres advisory lock elects exactly one leader; the
	// leader runs the closure below under a child context that is cancelled the
	// moment leadership is lost, so the jobs stop cleanly and a follower takes over.
	//
	//   - lazyManager: a per-leaf generation cursor read-modify-write with NO lock;
	//     two replicas on the same tick double-generate units and clobber the cursor
	//     (CONFIRMED-HARMFUL — the headline reason this gate exists).
	//   - healthRecorder: a plain INSERT with no upsert; N replicas write duplicate
	//     hourly metric rows that double-count operator dashboards.
	//   - faultMonitor: deadline sweep + reassignment + lapsed-reservation reclaim +
	//     the Layer 3 expired-dispatch-claim hygiene sweep. Its mutations are
	//     single-winner-guarded (safe-but-wasteful), but gating eliminates duplicate
	//     scans/logs AND keeps the hygiene sweep single-acting.
	//   - racUpdater / staleVolunteerMonitor / challengeStore cleanup: idempotent
	//     guarded UPDATE/DELETE sweeps; gated for tidiness now that the wrapper exists.
	faultMonitor := server.NewFaultMonitor(wuRepo, assignRepo, checkpointRepo, leafRepo, reliabilityRepo, transitioner, logger)
	if cfg.Head.StandingBackpressureEnabled {
		// Operator visibility for the backpressure machine: a throttled WARN naming the
		// population it currently holds in PROBATION/BENCHED. Wired only when the machine
		// is on, so the sweep stays zero-cost by default.
		faultMonitor = faultMonitor.WithStandingPopulation(standing.NewPgxRepository(pool))
	}
	staleVolunteerMonitor := server.NewStaleVolunteerMonitor(volunteerRepo, logger)
	racUpdater := credit.NewRACUpdater(racRepo, logger)
	artifactGC := server.NewArtifactVersionGC(leafRepo, cfg.Head.EffectiveArtifactRetentionKeep(), logger)
	patternRouter := generate.NewRouter(paramsweep.Generate, mapreduce.Generate, montecarlo.Generate, custom.Generate, logger)
	lazyManager := generate.NewLazyManager(patternRouter, wuRepo, batchRepo, leafRepo, logger)
	healthRecorder := health.NewRecorder(pool, stats.NewEngine(pool), leafRepo, logger)

	// Optional DID-binding re-check worker (leader-only): re-verifies bindings on a TTL
	// and revokes those whose authorization record is gone or repudiated. Constructed
	// only when DID binding is enabled (same gate as the atproto client above).
	var didRecheckWorker *identity.DIDRecheckWorker
	if atprotoClient != nil {
		didRecheckWorker = identity.NewDIDRecheckWorker(atprotoClient, volunteerRepo, cfg.Head, logger)
	}

	leadershipMgr := server.NewLeadershipManager(pool, logger)
	go leadershipMgr.Run(monitorCtx, instanceID.String(), func(leaderCtx context.Context) {
		// Each Start/Run blocks (ticker loop) and is started with its own goroutine
		// on leaderCtx; all stop cleanly when leadership is lost or the head shuts
		// down (leaderCtx is a child of monitorCtx, cancelled in either case).
		go faultMonitor.Start(leaderCtx)
		go staleVolunteerMonitor.Start(leaderCtx)
		go racUpdater.Start(leaderCtx)
		go challengeStore.StartCleanup(leaderCtx)
		go lazyManager.Run(leaderCtx, 30*time.Second)
		go healthRecorder.Start(leaderCtx)
		go artifactGC.Start(leaderCtx)
		if didRecheckWorker != nil {
			go didRecheckWorker.Start(leaderCtx)
		}
		slog.Info("singleton background jobs started (leader)", "head_instance_id", instanceID.String())
	})

	slog.Info("startup complete",
		"http_addr", cfg.Server.HTTPAddr,
		"grpc_addr", cfg.Server.GRPCAddr,
		"version", version,
		"head_instance_id", instanceID.String(),
	)

	// Wait for graceful shutdown.
	server.GracefulShutdown(ctx, httpServer, grpcServer, pool, 30*time.Second)
	monitorCancel()
}
