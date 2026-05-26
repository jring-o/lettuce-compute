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
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/bootstrap"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/custom"
	"github.com/lettuce-compute/infrastructure/internal/database"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/health"
	"github.com/lettuce-compute/infrastructure/internal/identity"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/mapreduce"
	"github.com/lettuce-compute/infrastructure/internal/montecarlo"
	"github.com/lettuce-compute/infrastructure/internal/paramsweep"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/stats"
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

	// Initialize logger.
	logger := logging.NewLogger(cfg.Log.Level, cfg.Log.Format)
	logging.SetDefault(logger)

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
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	leafRepo := leaf.NewPgxRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	resultRepo := result.NewPgxRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)
	creditRepo := credit.NewPgxRepository(pool)
	racRepo := credit.NewPgxRACRepository(pool)
	attestationRepo := attestation.NewPgxRepository(pool)

	// Create checkpoint repository.
	checkpointDir := cfg.Storage.CheckpointDir
	if checkpointDir == "" {
		checkpointDir = "data/checkpoints"
	}
	checkpointRepo := checkpoint.NewPgxRepository(pool, checkpointDir)

	// Create validation engine (shared between HTTP browser handlers and gRPC service).
	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, racRepo, volunteerRepo, assignRepo, attestationRepo, attestationSigner, logger)

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

	// Create HTTP router and server.
	deps := &server.Dependencies{
		Pool:             pool,
		Logger:           logger,
		Version:          version,
		StartTime:        startTime,
		CORSOrigins:      cfg.Server.CORSOrigins,
		SigningPublicKey:  attestationSigner.PublicKey(),
		AdminAPIKey:      adminAPIKey,
		ApiKeyRepo:       apiKeyRepo,
		ChallengeStore:   challengeStore,
		HeadConfig:       &cfg.Head,
		ValidationEngine: validationEngine,
		TrustedProxies:   trustedProxies,
	}
	router, rateLimitCleanup := server.NewRouter(deps)
	defer rateLimitCleanup()
	httpServer := server.NewHTTPServer(cfg.Server.HTTPAddr, router, tlsCfg)

	// Create gRPC server and register VolunteerService.
	grpcServer, grpcRateLimitCleanup := server.NewGRPCServer(tlsCfg, logger)
	defer grpcRateLimitCleanup()

	volunteerSvc := server.NewVolunteerService(pool, version, startTime, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, checkpointRepo, validationEngine, logger)
	weights := make(map[string]int32, len(cfg.Head.DefaultLeafWeights))
	for k, v := range cfg.Head.DefaultLeafWeights {
		weights[k] = int32(v)
	}
	server.SetHeadConfig(volunteerSvc, cfg.Head.Name, cfg.Head.Description, cfg.Head.URL, weights, cfg.Head.EffectiveMaxInflight())
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

	faultMonitor := server.NewFaultMonitor(wuRepo, assignRepo, checkpointRepo, leafRepo, logger)
	go faultMonitor.Start(monitorCtx)

	staleVolunteerMonitor := server.NewStaleVolunteerMonitor(volunteerRepo, logger)
	go staleVolunteerMonitor.Start(monitorCtx)

	racUpdater := credit.NewRACUpdater(racRepo, logger)
	go racUpdater.Start(monitorCtx)

	go challengeStore.StartCleanup(monitorCtx)

	// Start lazy generation manager.
	patternRouter := generate.NewRouter(paramsweep.Generate, mapreduce.Generate, montecarlo.Generate, custom.Generate, logger)
	lazyManager := generate.NewLazyManager(patternRouter, wuRepo, batchRepo, leafRepo, logger)
	go lazyManager.Run(monitorCtx, 30*time.Second)

	// Start operator health metrics recorder (always-on).
	healthRecorder := health.NewRecorder(pool, stats.NewEngine(pool), leafRepo, logger)
	go healthRecorder.Start(monitorCtx)
	slog.Info("health metrics recorder started")

	slog.Info("startup complete",
		"http_addr", cfg.Server.HTTPAddr,
		"grpc_addr", cfg.Server.GRPCAddr,
		"version", version,
	)

	// Wait for graceful shutdown.
	server.GracefulShutdown(ctx, httpServer, grpcServer, pool, 30*time.Second)
	monitorCancel()
}
