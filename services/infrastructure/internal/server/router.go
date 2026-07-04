package server

import (
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lettuce-compute/infrastructure/internal/aggregation"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/atproto"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/custom"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/health"
	"github.com/lettuce-compute/infrastructure/internal/identity"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/mapreduce"
	"github.com/lettuce-compute/infrastructure/internal/montecarlo"
	"github.com/lettuce-compute/infrastructure/internal/paramsweep"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Dependencies holds shared dependencies that handlers need.
type Dependencies struct {
	Pool             *pgxpool.Pool
	Logger           *slog.Logger
	Version          string
	StartTime        time.Time
	CORSOrigins      string
	SigningPublicKey ed25519.PublicKey
	AdminAPIKey      string
	ApiKeyRepo       apikey.Repository
	ChallengeStore   *identity.PgxChallengeStore
	HeadConfig       *config.HeadConfig
	ValidationEngine *validation.Engine
	// AtprotoClient backs the optional DID identity-binding endpoint. It is nil
	// unless the operator enabled DID binding (HeadConfig.DIDBindingEnabled), which
	// is the only condition under which the bind-DID route is registered.
	AtprotoClient *atproto.Client
	// TrustedProxies is the set of reverse-proxy networks whose X-Forwarded-For /
	// X-Real-IP headers may be trusted for client-IP extraction (rate limiting and
	// audit logging). EMPTY (nil) by default: forwarding headers are not trusted and
	// the direct peer IP is always used. Populated from config.Server.TrustedProxies.
	TrustedProxies []*net.IPNet
}

// NewRouter creates the HTTP router with all routes and middleware.
// Returns the handler and a cleanup function for the rate limiter.
func NewRouter(deps *Dependencies) (http.Handler, func()) {
	mux := http.NewServeMux()

	// --- Public routes (no auth required) ---

	mux.Handle("GET /api/v1/health", HealthHandler(deps.Pool))

	// Leaf read routes (list, get). These are anonymous-friendly but enforce
	// per-leaf visibility: they are wrapped with leafViewer so the handler can
	// identify the caller (if any) and hide PRIVATE leafs from unauthorized
	// callers. The deprecated /api/v1/projects aliases below are wrapped the
	// same way.
	leafRepo := leaf.NewPgxRepository(deps.Pool)
	leafHandler := leaf.NewLeafHandler(leafRepo, deps.Pool, deps.Logger)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}", leafViewer(leafHandler.HandleGet))
	mux.HandleFunc("GET /api/v1/leafs", leafViewer(leafHandler.HandleList))

	// Head info endpoint (public, no auth).
	headHandler := leaf.NewHeadHandler(deps.HeadConfig, deps.Pool, deps.Logger)
	mux.HandleFunc("GET /api/v1/head", headHandler.HandleGetHeadInfo)

	// Work unit handler (RegisterRoutes is no-op; all routes are protected). The repo carries
	// the head trust-gate dispatch policy so the browser/WASM request-work path resolves the
	// trusted-corroborator reservation identically to the gRPC path; the zero policy (gate
	// off, the default) leaves dispatch unchanged.
	wuRepo := workunit.NewPgxWorkUnitRepository(deps.Pool).
		WithTrustDispatch(TrustDispatchFromHeadConfig(deps.HeadConfig))
	batchRepo := workunit.NewPgxBatchRepository(deps.Pool)
	assignRepo := assignment.NewPgxRepository(deps.Pool)
	patternRouter := generate.NewRouter(adaptParamSweep, adaptMapReduce, adaptMonteCarlo, custom.Generate, deps.Logger)
	wuHandler := workunit.NewWorkUnitHandler(wuRepo, batchRepo, leafRepo, patternRouter.Generate, deps.Logger)
	// Enables operator requeue to close the prior volunteer's assignment outcome.
	wuHandler.SetAssignmentRepo(assignRepo)

	// Result handler (RegisterRoutes is no-op; all routes are protected).
	resultRepo := result.NewPgxRepository(deps.Pool)
	resultHandler := result.NewResultHandler(resultRepo, leafRepo, deps.Logger)

	// Aggregation routes (GET is public, POST is protected).
	aggEngine := aggregation.NewEngine(resultRepo, wuRepo, leafRepo, deps.Logger)
	aggHandler := aggregation.NewAggregationHandler(aggEngine, deps.Logger)
	aggHandler.RegisterRoutes(mux)

	// Stats routes (all public).
	statsEngine := stats.NewEngine(deps.Pool)
	mux.Handle("GET /api/v1/leafs/stats/batch", handleBatchStats(statsEngine))
	statsHandler := stats.NewStatsHandler(statsEngine, leafRepo, deps.Logger)
	statsHandler.RegisterRoutes(mux)

	// Volunteer stats routes (all public).
	volunteerRepo := volunteer.NewPgxRepository(deps.Pool)
	racRepo := credit.NewPgxRACRepository(deps.Pool)
	creditRepo := credit.NewPgxRepository(deps.Pool)
	volunteerStatsHandler := credit.NewVolunteerStatsHandler(deps.Pool, volunteerRepo, racRepo, creditRepo, leafRepo, deps.Logger)
	volunteerStatsHandler.RegisterRoutes(mux)

	// Attestation routes (public — signed credit records).
	if deps.SigningPublicKey != nil {
		attestationRepo := attestation.NewPgxRepository(deps.Pool)
		attestationHandler := attestation.NewHandler(attestationRepo, deps.SigningPublicKey, deps.Logger)
		attestationHandler.RegisterRoutes(mux)
	}

	// Identity verification routes (public — Ed25519 challenge/response).
	identityHandler := identity.NewHandler(deps.ChallengeStore, volunteerRepo, creditRepo, deps.Pool, deps.Logger)
	identityHandler.RegisterRoutes(mux)

	// Optional DID identity-binding endpoint (Ed25519-authenticated). Registered ONLY
	// when the operator enabled DID binding and the atproto client was constructed;
	// when disabled the route is absent and the mux returns its default 404. The
	// endpoint authenticates the volunteer with the same Ed25519 scheme as the browser
	// volunteer routes and hands the handler the authenticated public key explicitly
	// (the identity package cannot read the server package's auth context key).
	if deps.HeadConfig != nil && deps.HeadConfig.DIDBindingEnabled && deps.AtprotoClient != nil {
		bindHandler := identity.NewBindHandler(deps.AtprotoClient, volunteerRepo, *deps.HeadConfig, deps.Logger)
		mux.HandleFunc("POST /api/v1/identity/bind-did", ed25519AuthRequired(func(w http.ResponseWriter, r *http.Request) {
			pubKey, ok := PublicKeyFromContext(r.Context())
			if !ok {
				apierror.WriteError(w, apierror.Unauthorized("missing authenticated key"))
				return
			}
			bindHandler.Handle(w, r, pubKey)
		}))
	}

	// Credit analysis routes (protected — researcher/admin).
	analysisHandler := credit.NewAnalysisHandler(deps.Pool, leafRepo, deps.Logger)

	// Operator health metrics (public, always-on).
	healthLeafName := ""
	if deps.HeadConfig != nil {
		healthLeafName = deps.HeadConfig.Name
	}
	health.NewHandler(deps.Pool, statsEngine, leafRepo, deps.Logger, healthLeafName).RegisterRoutes(mux)

	// --- Protected routes (auth required) ---

	// Helper: requireAuth only.
	authOnly := func(h http.HandlerFunc) http.HandlerFunc {
		return requireAuth(h)
	}

	// Helper: requireAuth + requireLeafOwnership.
	authOwner := func(h http.HandlerFunc) http.HandlerFunc {
		return requireAuth(requireLeafOwnership(h, leafRepo))
	}

	// Helper: requireAuth, then inject the authenticated caller's admin status for the
	// operator-only admin handlers to enforce. Neither the trust nor the standing package
	// can import the server package (import cycle) to read UserFromContext, so — mirroring
	// how leafViewer injects leaf.Viewer — the router passes the admin fact in via
	// {trust,standing}.WithCaller and each handler's requireAdmin does the operator-only
	// 403. One wrapper injects BOTH caller contexts so the trust and standing admin routes
	// share it. requireAuth still runs first so an anonymous request is 401 before any
	// admin logic.
	authAdmin := func(h http.HandlerFunc) http.HandlerFunc {
		return requireAuth(func(w http.ResponseWriter, r *http.Request) {
			u := UserFromContext(r.Context())
			isAdmin := u != nil && u.Role == "ADMIN"
			ctx := trust.WithCaller(r.Context(), trust.Caller{IsAdmin: isAdmin})
			ctx = standing.WithCaller(ctx, standing.Caller{IsAdmin: isAdmin})
			h(w, r.WithContext(ctx))
		})
	}

	// Detailed health (auth required — exposes uptime + DB status).
	mux.HandleFunc("GET /api/v1/health/detailed", authOnly(HealthDetailedHandler(deps.Pool, deps.StartTime)))

	// Leaf mutations.
	mux.HandleFunc("POST /api/v1/leafs", authOnly(leafHandler.HandleCreate))
	mux.HandleFunc("PUT /api/v1/leafs/{leaf_id}", authOwner(leafHandler.HandleUpdate))
	mux.HandleFunc("DELETE /api/v1/leafs/{leaf_id}", authOwner(leafHandler.HandleDelete))

	// Leaf state transitions.
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/activate", authOwner(leafHandler.HandleActivate))
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/pause", authOwner(leafHandler.HandlePause))
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/resume", authOwner(leafHandler.HandleResume))
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/archive", authOwner(leafHandler.HandleArchive))
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/configure", authOwner(leafHandler.HandleConfigure))

	// Artifact version registry (TODO #38): publish an immutable version (snapshots the
	// leaf's current execution_config), list history, activate/roll back, purge.
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/versions", authOwner(leafHandler.HandlePublishVersion))
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/versions", authOwner(leafHandler.HandleListVersions))
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/versions/{version_id}/activate", authOwner(leafHandler.HandleActivateVersion))
	mux.HandleFunc("DELETE /api/v1/leafs/{leaf_id}/versions/{version_id}", authOwner(leafHandler.HandleDeleteVersion))

	// Work unit routes (sensitive reads + mutations).
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/work-units", authOwner(wuHandler.HandleList))
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/work-units/{work_unit_id}", authOwner(wuHandler.HandleGet))
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/generate", authOwner(wuHandler.HandleGenerate))
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/{work_unit_id}/requeue", authOwner(wuHandler.HandleRequeue))

	// Custom pattern bulk upload.
	bulkHandler := custom.NewBulkUploadHandler(wuRepo, batchRepo, leafRepo, deps.Logger)
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/work-units/bulk", authOwner(bulkHandler.HandleBulkUpload))

	// Result routes (sensitive reads).
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/results", authOwner(resultHandler.HandleListByLeaf))

	// Aggregation mutation.
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", authOwner(aggHandler.HandleAggregate))

	// Credit analysis routes (require auth — researcher/admin).
	mux.HandleFunc("GET /api/v1/credit/analysis/cross-leaf", authOnly(analysisHandler.HandleCrossLeaf))
	mux.HandleFunc("GET /api/v1/credit/analysis/{leaf_id}", authOnly(analysisHandler.HandleLeafAnalysis))

	// Trust administration (operator-only — admin API key). Seed, slash, read, and list
	// the account-level trust scores that gate quorum power in validation (see
	// internal/trust). SetScore is the operator's trust bootstrap: accrual only credits a
	// subject corroborated by an already-trusted subject, which is circular until the
	// operator seeds the first trusted subjects here.
	trustRepo := trust.NewPgxRepository(deps.Pool)
	trustHandler := trust.NewHandler(trustRepo, deps.Logger)
	mux.HandleFunc("POST /api/v1/admin/trust", authAdmin(trustHandler.HandleSet))
	mux.HandleFunc("POST /api/v1/admin/trust/slash", authAdmin(trustHandler.HandleSlash))
	mux.HandleFunc("GET /api/v1/admin/trust/{subject}", authAdmin(trustHandler.HandleGet))
	mux.HandleFunc("GET /api/v1/admin/trust", authAdmin(trustHandler.HandleList))

	// Account-standing administration (operator-only — admin API key). Set, clear, read,
	// and list the per-account standing that gates dispatch and quorum countability (see
	// internal/standing). HandleSet is the operator's direct lever: a manually identified
	// attacker must be stoppable regardless of the automatic backpressure machine, so the
	// row it writes is OPERATOR-owned and never auto-changed. Shares the authAdmin wrapper,
	// which injects standing.Caller alongside trust.Caller.
	standingRepo := standing.NewPgxRepository(deps.Pool)
	standingHandler := standing.NewHandler(standingRepo, deps.Logger)
	mux.HandleFunc("POST /api/v1/admin/standing", authAdmin(standingHandler.HandleSet))
	mux.HandleFunc("POST /api/v1/admin/standing/clear", authAdmin(standingHandler.HandleClear))
	mux.HandleFunc("GET /api/v1/admin/standing/{volunteer_id}", authAdmin(standingHandler.HandleGet))
	mux.HandleFunc("GET /api/v1/admin/standing", authAdmin(standingHandler.HandleList))

	// --- Deprecated /api/v1/projects aliases (removed in v0.10) ---
	// Same handlers, same responses — allows existing clients to migrate gradually.
	mux.HandleFunc("GET /api/v1/projects/{leaf_id}", leafViewer(leafHandler.HandleGetDeprecated))
	mux.HandleFunc("GET /api/v1/projects", leafViewer(leafHandler.HandleListDeprecated))
	mux.HandleFunc("POST /api/v1/projects", authOnly(leafHandler.HandleCreate))
	mux.HandleFunc("PUT /api/v1/projects/{leaf_id}", authOwner(leafHandler.HandleUpdate))
	mux.HandleFunc("DELETE /api/v1/projects/{leaf_id}", authOwner(leafHandler.HandleDelete))
	mux.HandleFunc("POST /api/v1/projects/{leaf_id}/activate", authOwner(leafHandler.HandleActivate))
	mux.HandleFunc("POST /api/v1/projects/{leaf_id}/pause", authOwner(leafHandler.HandlePause))
	mux.HandleFunc("POST /api/v1/projects/{leaf_id}/resume", authOwner(leafHandler.HandleResume))
	mux.HandleFunc("POST /api/v1/projects/{leaf_id}/archive", authOwner(leafHandler.HandleArchive))
	mux.HandleFunc("POST /api/v1/projects/{leaf_id}/configure", authOwner(leafHandler.HandleConfigure))
	mux.HandleFunc("GET /api/v1/volunteers/{id}/credit/breakdown", authOnly(analysisHandler.HandleVolunteerBreakdown))

	// --- Browser volunteer endpoints (Ed25519 auth, not API key) ---
	// assignRepo declared above (shared with the work unit handler).

	maxInflight := 5 // default
	headName := ""
	var defaultWeights map[string]int32
	if deps.HeadConfig != nil {
		headName = deps.HeadConfig.Name
		if deps.HeadConfig.MaxInflightPerVolunteer > 0 {
			maxInflight = deps.HeadConfig.MaxInflightPerVolunteer
		}
		if len(deps.HeadConfig.DefaultLeafWeights) > 0 {
			defaultWeights = make(map[string]int32, len(deps.HeadConfig.DefaultLeafWeights))
			for k, v := range deps.HeadConfig.DefaultLeafWeights {
				defaultWeights[k] = int32(v)
			}
		}
	}

	// Head trust-gate policy (see internal/trust), shared by the browser submit path's
	// transitioner and its submit-time stamping. Reuses the trustRepo built above for the
	// admin handler so the router holds a single trust store instance.
	browserTrustPolicy := TrustPolicyFromHeadConfig(deps.HeadConfig)

	bvDeps := &browserVolunteerDeps{
		pool:             deps.Pool,
		volunteerRepo:    volunteerRepo,
		wuRepo:           wuRepo,
		leafRepo:         leafRepo,
		assignRepo:       assignRepo,
		resultRepo:       resultRepo,
		batchRepo:        batchRepo,
		validationEngine: deps.ValidationEngine,
		trustRepo:        trustRepo,
		now:              time.Now,
		// Route the browser/WASM submit path through the same single transitioner the gRPC
		// volunteer service uses (TODO #66) so it no longer bypasses it with a raw COMPLETED
		// write + legacy TryValidate. The head trust gate is overlaid identically to the gRPC path.
		transitioner:            newTransitioner(deps.Pool, wuRepo, leafRepo, resultRepo, deps.ValidationEngine, browserTrustPolicy, deps.Logger),
		logger:                  deps.Logger,
		headName:                headName,
		defaultWeights:          defaultWeights,
		maxInflightPerVolunteer: maxInflight,
		// Same head trust-gate policy the shared wuRepo carries, so the browser/WASM
		// request-work path resolves the trusted-corroborator reservation identically.
		trustDispatch: TrustDispatchFromHeadConfig(deps.HeadConfig),
	}

	mux.HandleFunc("POST /api/v1/volunteers/register", handleBrowserRegister(bvDeps))
	mux.HandleFunc("POST /api/v1/volunteers/request-work",
		ed25519AuthRequired(handleBrowserRequestWork(bvDeps)))
	mux.HandleFunc("POST /api/v1/volunteers/submit-result",
		ed25519AuthRequired(handleBrowserSubmitResult(bvDeps)))
	// Browser REST heartbeat removed: browser/WASM units run-start at assignment
	// time and liveness is deadline-based (see browser-volunteer-handlers.go).

	// --- Middleware chain ---
	// Execution order (outermost to innermost):
	//   RequestID → RequestLogging → CORS → Auth → RateLimit → Recovery → mux
	//
	// RateLimit runs AFTER Auth so it can read UserFromContext and apply the
	// per-user limit (100/min) for authenticated callers vs the per-IP limit
	// (20/min) for anonymous ones. Its per-IP branch uses the trust-aware client
	// IP so a spoofed X-Forwarded-For cannot mint a fresh token bucket.
	//
	// Middleware is applied by wrapping innermost-first, so the wrapping sequence
	// below is the reverse of the execution order above.

	var handler http.Handler = mux
	handler = recoveryMiddleware(handler, deps.Logger)
	rateLimited, rateLimitCleanup := rateLimitMiddleware(handler, deps.TrustedProxies)
	handler = rateLimited
	handler = authMiddleware(handler, deps.ApiKeyRepo, deps.AdminAPIKey, deps.Logger)
	handler = corsMiddleware(handler, deps.CORSOrigins, deps.Logger)
	handler = requestLoggingMiddleware(handler, deps.Logger, deps.TrustedProxies)
	handler = logging.RequestIDMiddleware(handler)

	return handler, rateLimitCleanup
}

// requestLoggingMiddleware logs method, path, status, and duration.
// trustedProxies controls trust-aware client-IP extraction for the audit
// remote_addr field so spoofed forwarding headers cannot poison the audit log.
func requestLoggingMiddleware(next http.Handler, logger *slog.Logger, trustedProxies []*net.IPNet) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(rw, r)

		l := logging.LoggerFromContext(r.Context(), logger)
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		// Audit trail: log user identity on mutation requests.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if user := UserFromContext(r.Context()); user != nil {
				attrs = append(attrs, "user_id", user.ID.String(), "role", user.Role)
			}
			attrs = append(attrs, "remote_addr", clientIPFromRequest(r, trustedProxies))
		}
		l.Info("http request", attrs...)
	})
}

// corsMiddleware adds CORS headers to responses.
//
// Origin policy (fail-closed by default):
//   - allowedOrigins == ""  → cross-origin sharing is DISABLED. No
//     Access-Control-Allow-Origin header is emitted. A startup WARNING is logged
//     so operators know how to enable CORS. (An operator who genuinely wants a
//     public wildcard must set LETTUCE_CORS_ORIGINS="*" explicitly.)
//   - allowedOrigins == "*" → public wildcard. Emits "*" and NEVER credentials.
//   - explicit allowlist (single origin or comma-separated) → reflects the
//     request Origin when it matches and sets Access-Control-Allow-Credentials.
//
// SAFETY INVARIANT: Access-Control-Allow-Credentials: true is never sent
// together with a wildcard origin. Credentials are only paired with a reflected,
// explicitly-allowlisted origin.
func corsMiddleware(next http.Handler, allowedOrigins string, logger *slog.Logger) http.Handler {
	disabled := allowedOrigins == ""
	if disabled && logger != nil {
		logger.Warn("CORS is unconfigured and cross-origin sharing is DISABLED; " +
			"set LETTUCE_CORS_ORIGINS to a comma-separated allowlist (e.g. https://your-domain.com) " +
			"to enable browser cross-origin access, or set LETTUCE_CORS_ORIGINS=* to allow any origin (public wildcard, no credentials)")
	}

	isWildcard := allowedOrigins == "*"
	var originSet map[string]bool
	if !disabled && !isWildcard {
		origins := strings.Split(allowedOrigins, ",")
		originSet = make(map[string]bool, len(origins))
		for _, o := range origins {
			originSet[strings.TrimSpace(o)] = true
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case disabled:
			// Cross-origin sharing disabled: emit no Access-Control-Allow-Origin
			// (and therefore no credentials). Other CORS headers below are inert
			// without an allowed origin.
		case isWildcard:
			// Public wildcard: never combine with credentials.
			w.Header().Set("Access-Control-Allow-Origin", "*")
		default:
			reqOrigin := r.Header.Get("Origin")
			if originSet[reqOrigin] {
				w.Header().Set("Access-Control-Allow-Origin", reqOrigin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			// Vary on Origin so caches don't serve wrong CORS headers.
			w.Header().Add("Vary", "Origin")
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID")
		w.Header().Set("Access-Control-Max-Age", "3600")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// recoveryMiddleware catches panics, logs the stack trace, and returns 500.
func recoveryMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				l := logging.LoggerFromContext(r.Context(), logger)
				l.Error("panic recovered",
					"error", fmt.Sprintf("%v", rec),
					"stack", string(debug.Stack()),
				)
				apierror.WriteError(w, apierror.Internal("internal server error", nil))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// adaptParamSweep wraps paramsweep.Generate as a workunit.GenerateFunc.
var adaptParamSweep workunit.GenerateFunc = paramsweep.Generate

// adaptMapReduce wraps mapreduce.Generate as a workunit.GenerateFunc.
var adaptMapReduce workunit.GenerateFunc = mapreduce.Generate

// adaptMonteCarlo wraps montecarlo.Generate as a workunit.GenerateFunc.
var adaptMonteCarlo workunit.GenerateFunc = montecarlo.Generate
