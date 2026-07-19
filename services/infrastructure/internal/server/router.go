package server

import (
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	// Imported for its handler funcs only (pprof.Index etc., wired admin-gated
	// below). Its init() registers on http.DefaultServeMux, which this server
	// never serves, so that side effect is inert.
	"net/http/pprof"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lettuce-compute/infrastructure/internal/aggregation"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/atproto"
	"github.com/lettuce-compute/infrastructure/internal/attestation"
	"github.com/lettuce-compute/infrastructure/internal/audit"
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
	// AnomalyChecker backs the export emission-anomaly circuit breaker. nil unless
	// the operator enabled the emission anomaly halt; when set, the router wires it
	// into the public credit-stats settlement gates. The SAME instance also feeds
	// the fault monitor's WARN sweep, so the 503 gate and the operator page consult
	// one cached verdict and cannot disagree.
	AnomalyChecker *credit.AnomalyChecker
	// TrustedProxies is the set of reverse-proxy networks whose X-Forwarded-For /
	// X-Real-IP headers may be trusted for client-IP extraction (rate limiting and
	// audit logging). EMPTY (nil) by default: forwarding headers are not trusted and
	// the direct peer IP is always used. Populated from config.Server.TrustedProxies.
	TrustedProxies []*net.IPNet
	// RevocationEmitter writes the signed revocation attestation after a manual
	// clawback commits (attestation v2). Built in main next to the signer; attached
	// to the credit admin handler here. Best-effort at the handler; the leader-gated
	// reconciliation sweep recovers lost emissions.
	RevocationEmitter credit.RevocationEmitter
	// DispatchCacheRef is the late-bound handle to the in-process dispatch cache
	// (PB-9): the router is built before StartDispatchCache runs, so the operator
	// requeue handler holds this ref (a no-op until the cache is bound) to drop a
	// requeued unit's stale in-memory dispatch state. nil disables the hook (tests,
	// deployments that never start the cache).
	DispatchCacheRef *DispatchCacheRef
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
	// The eager /generate path persists each batch atomically through the transactional sink
	// (design §4.8): a crashed multi-batch run leaves earlier complete batches QUEUED, not
	// stranded CREATED.
	genSink := generate.NewPgxBatchSink(deps.Pool, deps.Logger)
	wuHandler := workunit.NewWorkUnitHandler(wuRepo, batchRepo, leafRepo, patternRouter.Generate, genSink, deps.Logger)
	// Enables operator requeue to close the prior volunteer's assignment outcome.
	wuHandler.SetAssignmentRepo(assignRepo)
	// Enables operator requeue to drop the unit's stale in-memory dispatch state
	// (PB-9); nil-safe no-op until/unless the dispatch cache is started.
	if deps.DispatchCacheRef != nil {
		wuHandler.SetDispatchInvalidator(deps.DispatchCacheRef)
	}

	// Result handler (RegisterRoutes is no-op; all routes are protected).
	resultRepo := result.NewPgxRepository(deps.Pool)
	resultHandler := result.NewResultHandler(resultRepo, leafRepo, deps.Logger)

	// Aggregation handler. Both the GET (read) and POST (recompute) routes are
	// registered below under authOwner — the aggregate is leaf CONTENTS, owner-
	// only regardless of the leaf's visibility (BG-11a). The handler's
	// RegisterRoutes (which bound the GET anonymously) is deliberately NOT called.
	aggEngine := aggregation.NewEngine(resultRepo, wuRepo, leafRepo, deps.Logger)
	aggHandler := aggregation.NewAggregationHandler(aggEngine, deps.Logger)

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
	// Settlement gates on the public credit-stats feeds (kill switch, maturation
	// window, emission-anomaly halt). With every knob at its default this config is
	// behaviorally identical to no config (export on, maturation 0, halt off).
	// AnomalyChecker is assigned only when non-nil: a typed-nil pointer stuffed into
	// the interface field would defeat the handler's nil guard.
	if deps.HeadConfig != nil {
		settlementCfg := &credit.SettlementExportConfig{
			ExportEnabled:      deps.HeadConfig.EffectiveStatsExportEnabled(),
			MaturationDays:     deps.HeadConfig.CreditMaturationDays,
			AnomalyHaltEnabled: deps.HeadConfig.EmissionAnomalyHaltEnabled,
			AnomalyFactor:      deps.HeadConfig.EffectiveEmissionAnomalyFactor(),
		}
		if deps.AnomalyChecker != nil {
			settlementCfg.AnomalyChecker = deps.AnomalyChecker
		}
		volunteerStatsHandler = volunteerStatsHandler.WithSettlement(settlementCfg)
	}
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
			ctx = credit.WithCaller(ctx, credit.Caller{IsAdmin: isAdmin})
			ctx = audit.WithCaller(ctx, audit.Caller{IsAdmin: isAdmin})
			h(w, r.WithContext(ctx))
		})
	}

	// Helper: requireAuth + requireAdmin. For operator-only READ surfaces whose
	// handlers need no injected package Caller (cross-leaf credit analysis and
	// the volunteer credit breakdown) — a plain ADMIN-role gate that fails
	// closed with 403 for any non-admin (BG-11b cross-leaf, ★BG-11c).
	authAdminOnly := func(h http.HandlerFunc) http.HandlerFunc {
		return requireAuth(requireAdmin(h))
	}

	// Detailed health (auth required — exposes uptime + DB status).
	mux.HandleFunc("GET /api/v1/health/detailed", authOnly(HealthDetailedHandler(deps.Pool, deps.StartTime)))

	// Operator observability: Prometheus scrape + pprof (BG-29; both ADMIN-only).
	//
	// TOPOLOGY — these routes are deliberately registered OUTSIDE /api/v1/. The
	// shipped Caddyfile proxies ONLY /api/v1/* (plus gRPC and a few static
	// prefixes) to the head; every other path falls through to the dashboard,
	// and the infrastructure service publishes no host ports. So /metrics and
	// /debug/pprof/ are unreachable from the internet in the shipped topology —
	// only processes on the compose-internal network (e.g. a Prometheus scraper
	// sidecar) reach them, at http://infrastructure:8080/metrics. They are STILL
	// admin-gated (ADMIN role / admin API key) so a deploy that exposes the
	// head's HTTP port directly does not silently expose runtime internals.
	metricsHandler := MetricsHandler()
	mux.HandleFunc("GET /metrics", authAdminOnly(metricsHandler.ServeHTTP))

	// Runtime profiling, same non-proxied path space and the same admin gate.
	// The subtree route serves the named profiles (heap, goroutine, allocs, …)
	// through pprof.Index; the four fixed handlers are not name-addressable
	// through Index and need their own routes (more-specific patterns win).
	// The exact no-slash pattern exists so ServeMux's automatic 301 →
	// /debug/pprof/ (which would fire BEFORE the admin gate) never answers an
	// anonymous probe.
	mux.HandleFunc("GET /debug/pprof", authAdminOnly(pprof.Index))
	mux.HandleFunc("GET /debug/pprof/", authAdminOnly(pprof.Index))
	mux.HandleFunc("GET /debug/pprof/cmdline", authAdminOnly(pprof.Cmdline))
	mux.HandleFunc("GET /debug/pprof/profile", authAdminOnly(pprof.Profile))
	mux.HandleFunc("GET /debug/pprof/symbol", authAdminOnly(pprof.Symbol))
	mux.HandleFunc("GET /debug/pprof/trace", authAdminOnly(pprof.Trace))

	// Leaf mutations. Create is authOnly + leafViewer: requireAuth guarantees a
	// caller, and leafViewer injects that caller's identity so handleCreate can
	// bind creator_id to it (★BG-11d-write / R1.5) without importing the server
	// package.
	mux.HandleFunc("POST /api/v1/leafs", authOnly(leafViewer(leafHandler.HandleCreate)))
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

	// Aggregation read + recompute. Both are owner-only: the aggregate is leaf
	// contents. The GET read (BG-11a) joins the POST's tier; the deprecated
	// /projects alias gets the identical treatment below.
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/aggregate", authOwner(aggHandler.HandleGetAggregate))
	mux.HandleFunc("POST /api/v1/leafs/{leaf_id}/aggregate", authOwner(aggHandler.HandleAggregate))

	// Credit analysis routes. Per-leaf analysis is leaf contents → authOwner
	// (BG-11b). Cross-leaf whole-head economics is an operator concern →
	// authAdminOnly (BG-11b). The volunteer breakdown alias below is likewise
	// authAdminOnly (★BG-11c).
	mux.HandleFunc("GET /api/v1/credit/analysis/cross-leaf", authAdminOnly(analysisHandler.HandleCrossLeaf))
	mux.HandleFunc("GET /api/v1/credit/analysis/{leaf_id}", authOwner(analysisHandler.HandleLeafAnalysis))

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

	// Credit settlement administration (operator-only — admin API key). The manual
	// clawback appends a compensating negative credit_adjustments row against one
	// credit_ledger entry (the ledger stays append-only); the incident-runbook lever
	// until automated clawback ships. Shares the authAdmin wrapper, which injects
	// credit.Caller alongside trust.Caller and standing.Caller — the handler's own
	// requireAdmin enforces the 403 (fail-closed without the injection).
	adjustmentsRepo := credit.NewPgxAdjustmentsRepository(deps.Pool)
	creditAdminHandler := credit.NewAdminHandler(adjustmentsRepo, creditRepo, deps.Logger)
	if deps.RevocationEmitter != nil {
		creditAdminHandler.WithRevocationEmitter(deps.RevocationEmitter)
	}
	mux.HandleFunc("POST /api/v1/admin/credit/adjustments", authAdmin(creditAdminHandler.HandleClawback))
	mux.HandleFunc("GET /api/v1/admin/credit/adjustments", authAdmin(creditAdminHandler.HandleListAdjustments))

	// Result-audit administration (operator-only — admin API key). The trusted-runner
	// registry (register / deactivate / list — registry membership is what authorizes the
	// AuditService claim/submit surface AND upgrades the trust-accrual witness rule) plus
	// the observe-only verdict read surface. Shares the authAdmin wrapper, which injects
	// audit.Caller alongside the other admin callers — the handler's own requireAdmin
	// enforces the 403 (fail-closed without the injection).
	auditRunnersRepo := audit.NewPgxRunnersRepository(deps.Pool)
	auditsRepo := audit.NewPgxAuditsRepository(deps.Pool)
	auditAdminHandler := audit.NewAdminHandler(auditRunnersRepo, auditsRepo, deps.Logger)
	mux.HandleFunc("POST /api/v1/admin/audit/runners", authAdmin(auditAdminHandler.HandleRegisterRunner))
	mux.HandleFunc("POST /api/v1/admin/audit/runners/deactivate", authAdmin(auditAdminHandler.HandleDeactivateRunner))
	mux.HandleFunc("GET /api/v1/admin/audit/runners", authAdmin(auditAdminHandler.HandleListRunners))
	mux.HandleFunc("GET /api/v1/admin/audit/results", authAdmin(auditAdminHandler.HandleListAudits))
	// Slice-3 leaf-owner flag surface (design §9.8): leaves with enforcement history
	// (ENFORCED / CONTRADICTED / STALLED root audits), derived — no flag schema exists.
	mux.HandleFunc("GET /api/v1/admin/audit/flagged-leaves", authAdmin(auditAdminHandler.HandleFlaggedLeaves))

	// --- Deprecated /api/v1/projects aliases (removed in v0.10) ---
	// Same handlers, same responses — allows existing clients to migrate gradually.
	mux.HandleFunc("GET /api/v1/projects/{leaf_id}", leafViewer(leafHandler.HandleGetDeprecated))
	mux.HandleFunc("GET /api/v1/projects", leafViewer(leafHandler.HandleListDeprecated))
	mux.HandleFunc("POST /api/v1/projects", authOnly(leafViewer(leafHandler.HandleCreate)))
	mux.HandleFunc("PUT /api/v1/projects/{leaf_id}", authOwner(leafHandler.HandleUpdate))
	mux.HandleFunc("DELETE /api/v1/projects/{leaf_id}", authOwner(leafHandler.HandleDelete))
	mux.HandleFunc("POST /api/v1/projects/{leaf_id}/activate", authOwner(leafHandler.HandleActivate))
	mux.HandleFunc("POST /api/v1/projects/{leaf_id}/pause", authOwner(leafHandler.HandlePause))
	mux.HandleFunc("POST /api/v1/projects/{leaf_id}/resume", authOwner(leafHandler.HandleResume))
	mux.HandleFunc("POST /api/v1/projects/{leaf_id}/archive", authOwner(leafHandler.HandleArchive))
	mux.HandleFunc("POST /api/v1/projects/{leaf_id}/configure", authOwner(leafHandler.HandleConfigure))
	// Deprecated aggregate-read alias — same owner-only tier as the canonical
	// /leafs route (BG-11a).
	mux.HandleFunc("GET /api/v1/projects/{leaf_id}/aggregate", authOwner(aggHandler.HandleGetAggregate))
	// Operator-only per-volunteer credit breakdown: per-machine hostnames,
	// last-seen, and credit timelines — strictly more than the public stats
	// feed, so ADMIN-only (★BG-11c). The volunteer's own self-view is the
	// separate Ed25519-authed gRPC GetMyContribution.
	mux.HandleFunc("GET /api/v1/volunteers/{id}/credit/breakdown", authAdminOnly(analysisHandler.HandleVolunteerBreakdown))

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
		// PB-17: pin-and-serve per-unit artifact versions on the browser dispatch
		// path (the same *leaf.PgxRepository implements both leaf interfaces).
		artifactVersionRepo: leafRepo,
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
		// Registration admission cap (design §4.1), enforced identically to the gRPC
		// register path; zero value (knob off) leaves browser registration unchanged.
		registrationCap: RegistrationCapFromHeadConfig(deps.HeadConfig),
		// Registration proof-of-work policy (design §4.1); enforcement off by default,
		// but effective difficulty/TTL always populated so challenge issuance works.
		registrationPow: RegistrationPowFromHeadConfig(deps.HeadConfig),
		trustedProxies:  deps.TrustedProxies,
	}

	mux.HandleFunc("POST /api/v1/volunteers/register", handleBrowserRegister(bvDeps))
	// Registration proof-of-work challenge issuance (design §4.1). Unauthenticated
	// like register; rate-limited by the global per-IP chain; challenges expire and
	// are swept, so the surface is bounded.
	mux.HandleFunc("POST /api/v1/volunteers/register-challenge", handleBrowserRegisterChallenge(bvDeps))
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

		elapsed := time.Since(start)
		// BG-29: this middleware already observes method, duration, and status
		// for every HTTP request, so the Prometheus request metrics feed here —
		// the gRPC twin lives in loggingInterceptor. No path label, and the
		// method is folded to the standard set: see the cardinality notes in
		// metrics.go.
		methodLabel := httpMethodLabel(r.Method)
		httpRequestsTotal.WithLabelValues(methodLabel, strconv.Itoa(rw.statusCode)).Inc()
		httpRequestDuration.WithLabelValues(methodLabel).Observe(elapsed.Seconds())

		l := logging.LoggerFromContext(r.Context(), logger)
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration_ms", elapsed.Milliseconds(),
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
