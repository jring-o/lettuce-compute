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
	"strings"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/atproto"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/audit"
	"github.com/lettuce-compute/infrastructure/internal/bootstrap"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/contentverify"
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
	"github.com/lettuce-compute/infrastructure/internal/safego"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
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

	// SSRF escape-hatch guard (BG-14): LETTUCE_BINARY_URL_ALLOW_INSECURE disables the
	// https-required / no-internal-IP screen on leaf binary/module/viz/input URLs. It
	// exists only for local dev and integration tests; a production head must never
	// set it. Warn loudly at boot when it is active so it can never be on unnoticed.
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("LETTUCE_BINARY_URL_ALLOW_INSECURE"))); v == "1" || v == "true" || v == "yes" {
		logger.Warn("SECURITY: LETTUCE_BINARY_URL_ALLOW_INSECURE is set — leaf URL SSRF screening (https-required, no internal IPs) is DISABLED. This must NEVER be set in production; unset it unless this is a local dev/test head.")
	}

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

	// Result audits (design §7, observe-only phase): the trusted-runner registry and the
	// audit job store. Constructed unconditionally — the admin registry surface and the
	// AuditService must work before the sampling knob is ever flipped (an empty registry
	// fails every claim closed), and the registry also gates the trust-accrual witness
	// upgrade below.
	auditRunnersRepo := audit.NewPgxRunnersRepository(pool)
	auditsRepo := audit.NewPgxAuditsRepository(pool)

	// Create validation engine (shared between HTTP browser handlers and gRPC service).
	// WithEmissionCap wires the per-account rolling-24h credit cap (0 = unlimited, the
	// default): an over-cap grant is suppressed at the choke point — never a validation
	// failure — bounding the daily burst any single account can mint.
	// WithTrustedRunners gates the D9 accrual-witness upgrade on REGISTRY STATE (empty
	// registry = today's rule exactly), so wiring it unconditionally is the design: the
	// operator registering a runner IS the opt-in. WithResultAudits arms the post-hoc
	// sampling hook only when LETTUCE_HEAD_RESULT_AUDIT_ENABLED is set; leafRepo doubles
	// as the artifact-version resolver so a sampled audit pins the exec config the
	// winner actually ran.
	// WithTxRunner is the finalization ATOMICITY wiring (E1 §4.1, BG-21/★BG-21e): without it
	// the engine runs the non-transactional passthrough and the marks, the VALIDATED flip, and
	// the credit rows commit as separate autocommits — a crash between them loses credit or
	// strands the unit unrepairably. NewVolunteerService refuses to build a pool-backed service
	// over an engine without a runner, so removing this line fails the head at boot (and the
	// wiring meta-test in main_test.go fails in CI).
	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volunteerRepo, assignRepo, attestationRepo, reliabilityRepo, attestationSigner, logger, trustRepo, trustPolicy).
		WithTxRunner(validation.NewPgxFinalizationTxRunner(pool)).
		WithStandingBackpressure(standingRecorder).
		WithEmissionCap(cfg.Head.MaxDailyCreditPerAccount).
		WithTrustedRunners(auditRunnersRepo).
		WithResultAudits(auditsRepo, cfg.Head.ResultAuditEnabled, cfg.Head.EffectiveResultAuditRate(), leafRepo).
		WithRepairSupport(auditsRepo)
	if cfg.Head.ResultAuditEnabled {
		slog.Info("result audits enabled",
			"head_rate", cfg.Head.EffectiveResultAuditRate())
	}

	// The single transitioner (TODO #50): the sole decider of work-unit redundancy state.
	// SubmitResult and the fault monitor delegate every "complete / validate / reject / wait /
	// dead-letter" decision to it. The validation engine is its comparator + accept/reject
	// implementation; the per-unit lock is the cross-replica Postgres advisory lock. The head
	// trust gate is overlaid onto each leaf's policy here. The lock instance is shared with
	// the enforcement worker below (same key space — an enforcement pass serializes against
	// in-flight decisions on the same unit).
	unitLocker := transition.NewPgxLocker(pool, logger)
	transitioner := transition.NewTransitioner(
		unitLocker,
		wuRepo, leafRepo, resultRepo, validationEngine, trustPolicy, logger)

	// Finalization recovery sweeper (E1 §4.2): a leader-gated, unconditional reconciler that
	// re-drives finalization-stalled work units through the idempotent transitioner — the
	// standing re-scan half of finalization liveness. It reads the two strand-shape candidate
	// queries off wuRepo and calls transitioner.Evaluate per unit. Started in the leader block
	// below, beside the revocation reconciler.
	recoverySweeper := transition.NewRecoverySweeper(
		wuRepo, transitioner,
		time.Duration(cfg.Head.EffectiveFinalizationSweepIntervalSeconds())*time.Second,
		time.Duration(cfg.Head.EffectiveFinalizationSweepGraceSeconds())*time.Second,
		cfg.Head.EffectiveFinalizationSweepBatch(),
		logger)

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

	// Emission-anomaly circuit breaker: constructed only when the operator armed the
	// halt. The SAME instance backs the export's 503 gate (via router deps) and the
	// fault monitor's WARN sweep below, so both consult one cached verdict.
	var anomalyChecker *credit.AnomalyChecker
	if cfg.Head.EmissionAnomalyHaltEnabled {
		anomalyChecker = credit.NewAnomalyChecker(pool, cfg.Head.EffectiveEmissionAnomalyFactor())
		slog.Info("emission anomaly halt armed",
			"factor", cfg.Head.EffectiveEmissionAnomalyFactor())
	}

	// Create HTTP router and server.
	// Revocation emitter (attestation v2, design §8.4): signs the clawback record the
	// credit admin handler emits and the leader-gated reconciler recovers. Shares the
	// head signer; its repository view is a plain pgx wrapper over the shared pool.
	revocationEmitter := attestation.NewRevocationEmitter(
		pool, attestation.NewPgxRepository(pool), attestationSigner, logger)

	deps := &server.Dependencies{
		Pool:              pool,
		Logger:            logger,
		Version:           version,
		StartTime:         startTime,
		CORSOrigins:       cfg.Server.CORSOrigins,
		SigningPublicKey:  attestationSigner.PublicKey(),
		AdminAPIKey:       adminAPIKey,
		ApiKeyRepo:        apiKeyRepo,
		ChallengeStore:    challengeStore,
		HeadConfig:        &cfg.Head,
		ValidationEngine:  validationEngine,
		TrustedProxies:    trustedProxies,
		AtprotoClient:     atprotoClient,
		AnomalyChecker:    anomalyChecker,
		RevocationEmitter: revocationEmitter,
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
	// Pre-decode per-IP stream budget (BG-18) — the tap-level flood backstop.
	// Same NAT'ed-fleet rationale as the request budgets above.
	if v := os.Getenv("LETTUCE_GRPC_PER_IP_STREAM_LIMIT"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			server.SetGRPCStreamRateLimit(n)
			slog.Info("per-IP gRPC stream budget overridden", "per_min", n)
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
	// Registration admission cap (design §4.1) — the same resolved policy the router
	// hands the browser register path, so both create surfaces enforce one number.
	// Zero value (knob off, the default) leaves gRPC registration unchanged.
	server.SetAdmissionPolicy(volunteerSvc, server.RegistrationCapFromHeadConfig(&cfg.Head))
	// Registration proof-of-work (design §4.1) — enforcement defaults OFF; the
	// effective difficulty/TTL are threaded regardless so challenge issuance
	// (GetRegistrationChallenge / the REST register-challenge endpoint) works
	// probe-free before any enforcement flip.
	server.SetRegistrationPowPolicy(volunteerSvc, server.RegistrationPowFromHeadConfig(&cfg.Head))
	// Per-account host cap (BG-25) — ON by default (10, 30-day activity window):
	// bounds how many server-issued per-machine host ids one account may hold; a
	// machine past the cap works in the shared per-account bucket.
	server.SetHostCapPolicy(volunteerSvc, server.HostCapFromHeadConfig(&cfg.Head))
	// External-output content verification (design §10, BG-02b) — OFF by default:
	// with the knob off SubmitResult refuses every output_data_url at the front
	// door, so no volunteer-claimed checksum can enter the held-verification
	// pipeline, let alone validation.
	server.SetContentFetchPolicy(volunteerSvc, cfg.Head.ContentFetchEnabled)
	lettucev1.RegisterVolunteerServiceServer(grpcServer, volunteerSvc)

	// AuditService (design §7.3): the trusted-runner claim/submit surface. Registered
	// unconditionally — the interceptor authenticates it by default (its methods are
	// deliberately NOT in grpcPublicMethods) and an empty registry fails every claim
	// closed with PermissionDenied, so a head with audits off exposes nothing. The
	// adjudicator is the head-side verdict function; a runner only ever returns bytes.
	auditSvc := server.NewAuditService(auditRunnersRepo, auditsRepo, wuRepo, leafRepo, volunteerRepo, validation.AdjudicateAudit, resultRepo, cfg.Head.AuditEnforcementEnabled, logger)
	lettucev1.RegisterAuditServiceServer(grpcServer, auditSvc)

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
	if anomalyChecker != nil {
		// Operator visibility for the emission circuit breaker: a throttled WARN when
		// the export has self-frozen. Wired only when the halt is armed, sharing the
		// export gate's checker (one cached verdict for both).
		faultMonitor = faultMonitor.WithEmissionAnomalyCheck(func(ctx context.Context) (bool, float64, float64, error) {
			v, err := anomalyChecker.Check(ctx)
			return v.Halted, v.Today, v.Baseline, err
		})
	}
	if cfg.Head.StandingBackpressureEnabled {
		// Operator visibility for the backpressure machine: a throttled WARN naming the
		// population it currently holds in PROBATION/BENCHED. Wired only when the machine
		// is on, so the sweep stays zero-cost by default.
		faultMonitor = faultMonitor.WithStandingPopulation(standing.NewPgxRepository(pool))
	}
	if cfg.Head.ResultAuditEnabled || cfg.Head.AuditEnforcementEnabled {
		// Operator visibility for the audit net (design §7.4): throttled WARNs on new
		// MISMATCH verdicts, on queue decay (EXPIRED growth / claim starvation), and on
		// the owner-selectable ineligible lanes. Wired when audits are on — and ALSO
		// when only enforcement is on (design §9.9: an operator may disable sampling
		// while the enforcement sweep drains the recorded backlog; its lanes must not
		// go dark). The closure composes the DB stats with the engine's in-memory
		// ineligible counter.
		faultMonitor = faultMonitor.WithResultAuditStats(func(ctx context.Context) (audit.Stats, error) {
			s, err := auditsRepo.Stats(ctx)
			if err != nil {
				return audit.Stats{}, err
			}
			s.IneligibleByLeaf = validationEngine.AuditIneligibleCounts()
			return s, nil
		})
	}
	if cfg.Head.AuditEnforcementEnabled {
		// Slice-3 enforcement lanes (design §9.8): ENFORCED/CONTRADICTED deltas, STALLED
		// count, and the enforcement-horizon aging guard against the maturation window.
		faultMonitor = faultMonitor.WithEnforcementWatch(cfg.Head.CreditMaturationDays)
	}
	// Content-verification health probe (design §10.10) — wired UNCONDITIONALLY, unlike
	// the audit probe: held ref rows can exist while the content-fetch knob is OFF
	// (stragglers from an enabled era draining on the 24h holding-expiry lane), and that
	// is exactly the state the probe's knob-off lane must page about. One aggregate
	// query per scan; both filters are positive-form (§10.0 item 4).
	faultMonitor = faultMonitor.WithContentVerificationStats(func(ctx context.Context) (server.ContentVerificationStats, error) {
		var s server.ContentVerificationStats
		var oldestSeconds float64
		err := pool.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE validation_status = 'AWAITING_CONTENT_VERIFICATION')::int,
				COALESCE(EXTRACT(EPOCH FROM (now() - MIN(created_at)
					FILTER (WHERE validation_status = 'AWAITING_CONTENT_VERIFICATION'))), 0),
				COUNT(*) FILTER (WHERE validation_status = 'CONTENT_VERIFICATION_FAILED')::int
			FROM results
			WHERE validation_status IN ('AWAITING_CONTENT_VERIFICATION', 'CONTENT_VERIFICATION_FAILED')`,
		).Scan(&s.Held, &oldestSeconds, &s.FailedTotal)
		if err != nil {
			return s, err
		}
		s.OldestHeldAge = time.Duration(oldestSeconds * float64(time.Second))
		s.FetchEnabled = cfg.Head.ContentFetchEnabled
		return s, nil
	})
	staleVolunteerMonitor := server.NewStaleVolunteerMonitor(volunteerRepo, logger)
	racUpdater := credit.NewRACUpdater(racRepo, logger)
	artifactGC := server.NewArtifactVersionGC(leafRepo, cfg.Head.EffectiveArtifactRetentionKeep(), logger)
	patternRouter := generate.NewRouter(paramsweep.Generate, mapreduce.Generate, montecarlo.Generate, custom.Generate, logger)
	// The lazy generation store persists each batch AND its cursor advance in one transaction
	// (design §4.8), so a crash or leadership-failover overlap cannot duplicate or lose ordinals.
	genSink := generate.NewPgxBatchSink(pool, logger)
	lazyManager := generate.NewLazyManager(patternRouter, wuRepo, genSink, leafRepo, logger)
	healthRecorder := health.NewRecorder(pool, stats.NewEngine(pool), leafRepo, logger)

	// Optional DID-binding re-check worker (leader-only): re-verifies bindings on a TTL
	// and revokes those whose authorization record is gone or repudiated. Constructed
	// only when DID binding is enabled (same gate as the atproto client above).
	var didRecheckWorker *identity.DIDRecheckWorker
	if atprotoClient != nil {
		didRecheckWorker = identity.NewDIDRecheckWorker(atprotoClient, volunteerRepo, cfg.Head, logger)
	}

	// Audit-enforcement worker (design §9): constructed ONLY when the knob is on —
	// the default (off) leaves verdicts observe-only exactly as slice 2 shipped them.
	// Every seam is a narrow adapter over the real repositories (enforcement_wiring.go);
	// the repair path and the mutual ground-truth check live in the validation engine
	// (the adjudicator-closure precedent).
	var enforcementWorker *audit.EnforcementWorker
	if cfg.Head.AuditEnforcementEnabled {
		enforcementWorker = audit.NewEnforcementWorker(audit.EnforcementDeps{
			Audits:  auditsRepo,
			Slasher: trustRepo,
			Credit: &creditEnforcer{
				ledger: creditRepo,
				adj:    credit.NewPgxAdjustmentsRepository(pool),
				rac:    racRepo,
				logger: logger,
			},
			Revocations: revocationEmitter,
			Results:     &fraudSetLoader{results: resultRepo},
			Repairer:    validationEngine,
			Disposer: workunit.NewEnforcementDemoter(
				wuRepo, newEnforcementBudgetResolver(leafRepo, trustPolicy), logger),
			Locker:         &enforcementUnitLocker{locker: unitLocker},
			Agreement:      validation.AdjudicateGroundTruthAgreement,
			MaturationDays: cfg.Head.CreditMaturationDays,
			Logger:         logger,
		})
		slog.Info("audit enforcement armed",
			"maturation_days", cfg.Head.CreditMaturationDays)
	}

	// Content-verification worker (design §10.6): fetches and hashes external output
	// references so a ref result can only ever vote on a HEAD-computed checksum
	// (BG-02b). Constructed unconditionally — see the start-site comment below; the
	// KNOB gates fetching inside the worker, not the worker itself. The evaluate seam
	// is a closure over the single transitioner (the browser-submit precedent), so
	// contentverify never imports the transition machinery.
	contentVerifyWorker := contentverify.NewWorker(
		pool,
		contentverify.NewHTTPClient(),
		cfg.Head.ContentFetchEnabled,
		cfg.Head.EffectiveContentFetchMaxBytes(),
		func(ctx context.Context, workUnitID types.ID) error {
			_, err := transitioner.Evaluate(ctx, workUnitID)
			return err
		},
		logger,
	)
	if cfg.Head.ContentFetchEnabled {
		slog.Info("external-output content verification armed",
			"max_fetch_bytes", cfg.Head.EffectiveContentFetchMaxBytes())
	}

	// Every background job below launches through safego.Go (BG-19): a panic in a
	// ticker loop is recovered and the loop restarted with backoff, instead of one
	// poison row crash-looping the whole head. The gRPC/HTTP servers keep their own
	// per-request recovery; their two fail-fast serve goroutines above are the only
	// intentional bare launches left in this file.
	leadershipMgr := server.NewLeadershipManager(pool, logger)
	safego.Go(monitorCtx, logger, "leadership-manager", func(ctx context.Context) {
		leadershipMgr.Run(ctx, instanceID.String(), func(leaderCtx context.Context) {
			// Each Start/Run blocks (ticker loop) and is started with its own goroutine
			// on leaderCtx; all stop cleanly when leadership is lost or the head shuts
			// down (leaderCtx is a child of monitorCtx, cancelled in either case).
			safego.Go(leaderCtx, logger, "fault-monitor", faultMonitor.Start)
			safego.Go(leaderCtx, logger, "stale-volunteer-monitor", staleVolunteerMonitor.Start)
			safego.Go(leaderCtx, logger, "rac-updater", racUpdater.Start)
			safego.Go(leaderCtx, logger, "challenge-store-cleanup", challengeStore.StartCleanup)
			safego.Go(leaderCtx, logger, "lazy-generation-manager", func(ctx context.Context) {
				lazyManager.Run(ctx, 30*time.Second)
			})
			safego.Go(leaderCtx, logger, "leaf-health-recorder", healthRecorder.Start)
			safego.Go(leaderCtx, logger, "artifact-gc", artifactGC.Start)
			if didRecheckWorker != nil {
				safego.Go(leaderCtx, logger, "did-recheck-worker", didRecheckWorker.Start)
			}
			// Registration-admission counter retention sweep (design §4.1) — started only
			// when the creation cap is on (the machine-enabled wiring idiom): with the knob
			// off nothing writes the table, so there is nothing to sweep.
			if cfg.Head.RegistrationCapEnabled {
				safego.Go(leaderCtx, logger, "registration-counter-sweeper",
					admission.NewCounterSweeper(pool, logger).Start)
			}
			// Registration proof-of-work challenge sweep — UNCONDITIONAL, unlike the
			// counter sweep: challenge ISSUANCE works even while enforcement is off
			// (probe-free clients), so expired rows can accumulate regardless of the knob.
			safego.Go(leaderCtx, logger, "registration-challenge-sweeper",
				admission.NewChallengeSweeper(pool, logger).Start)
			// Audit reclaim sweep (design §7.5) — UNCONDITIONAL like the challenge sweep,
			// deliberately NOT gated on the audit knob: open audit rows can outlive an
			// enabled period (operator samples, then flips the knob off), and while OPEN
			// they pin their artifact versions against the GC prune. The sweep expires
			// them within the queue lifetime regardless, releasing the pins; on a head
			// that never enabled audits it is two no-op UPDATEs a minute on the leader.
			safego.Go(leaderCtx, logger, "audit-reclaim-worker",
				audit.NewReclaimWorker(auditsRepo, logger).Start)
			// Revocation reconciliation sweep (design §8.4) — UNCONDITIONAL like the reclaim
			// sweep: a clawback whose best-effort in-handler emission failed must still get
			// its signed revocation attestation, and re-POSTing the clawback endpoint can
			// never re-reach emission (the adjustment already exists). On a head with no
			// missing revocations it is one indexed no-row query per sweep on the leader.
			safego.Go(leaderCtx, logger, "revocation-reconciler",
				attestation.NewRevocationReconciler(revocationEmitter, 10*time.Minute, logger).Run)
			// Finalization recovery sweep (E1 §4.2) — UNCONDITIONAL like the revocation
			// reconciler: it is a correctness reconciler for finalization liveness (a
			// crashed/lost post-commit Evaluate must still re-drive), and on a healthy head it
			// is two indexed near-empty queries per interval on the leader.
			safego.Go(leaderCtx, logger, "finalization-recovery-sweeper", recoverySweeper.Run)
			// Audit-enforcement sweep (design §9.2-§9.3) — gated on the knob, UNLIKE the
			// reclaim sweep: with enforcement off, verdicts must stay observe-only
			// byte-identically, and this worker is the only actor that executes
			// consequences. Verdicts recorded while the knob was off are stamped
			// ineligible and stay unactionable even after a later restart with it on.
			if enforcementWorker != nil {
				safego.Go(leaderCtx, logger, "audit-enforcement-worker", enforcementWorker.Start)
			}
			// Content-verification sweep (design §10.6) — UNCONDITIONAL like the reclaim
			// sweep (S4): the worker is also the janitor for held ref rows stranded by a
			// content-fetch knob flip on→off, which must drain via the 24h holding-expiry
			// lane without a config change. The KNOB gates fetching per row inside the
			// worker (off = no network I/O ever); on a head that never enabled fetching it
			// is one indexed no-row query per tick on the leader.
			safego.Go(leaderCtx, logger, "content-verification-worker", contentVerifyWorker.Start)
			slog.Info("singleton background jobs started (leader)", "head_instance_id", instanceID.String())
		})
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
