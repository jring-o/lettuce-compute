package server

// The shared authorization contract for every HTTP route the head registers in
// router.go (cluster B, docs/gate/B-dashboard-authz/01-design.md §4.6/§6.3).
//
// This table is the single source of truth for two tests:
//
//   - authz_meta_test.go (unit, always-on CI) statically parses router.go and
//     fails when a registered route is missing from this table, is registered
//     under a different wrapper than the tier here demands, or when a tabled
//     route is not registered at all. A NEW data route cannot land without an
//     explicit, reviewable row here.
//   - authz_matrix_integration_test.go (integration, DB-backed) drives the rows
//     through the real router × {anonymous, non-owner, owner, admin} callers
//     and asserts the expected denials/permits.
//
// The table encodes the TARGET contract of the design (post-B1), so on pre-fix
// code both tests fail exactly on the known-open routes (BG-11a/b, ★BG-11c) —
// the PR #80 refutation style.

// authzTier names the authorization a route must enforce.
type authzTier string

const (
	// tierPublic: anonymous by design (health, head info, public stats,
	// volunteer registration).
	tierPublic authzTier = "public"
	// tierVisibility: leafViewer — leaf METADATA reads; PUBLIC/UNLISTED for
	// anyone, PRIVATE for owner/admin. Never used for leaf contents.
	tierVisibility authzTier = "visibility"
	// tierAuthed: requireAuth only (any valid API key).
	tierAuthed authzTier = "authed"
	// tierOwner: requireAuth + requireLeafOwnership on the path {leaf_id} —
	// the tier for leaf CONTENTS (results, work units, aggregate, analysis,
	// versions) and all leaf mutations.
	tierOwner authzTier = "owner"
	// tierAdminGate: the router's authAdmin wrapper — requireAuth plus
	// injected package Caller contexts; the handler's own requireAdmin
	// enforces the 403.
	tierAdminGate authzTier = "admin-caller"
	// tierAdminOnly: requireAuth + requireAdmin (server-side 403 for
	// non-ADMIN) — operator-only reads that need no injected caller.
	tierAdminOnly authzTier = "admin-only"
	// tierVolunteerKey: Ed25519 handler-level auth (browser volunteer paths).
	tierVolunteerKey authzTier = "volunteer-key"
)

// authzRoute is one route's authorization contract.
type authzRoute struct {
	method  string
	pattern string
	tier    authzTier
	// probeAllowed marks rows the integration matrix may safely call with an
	// AUTHORIZED caller (reads and side-effect-free probes). Denials (anon,
	// non-owner, non-admin) are probed on every row regardless; mutations keep
	// their allowed-path coverage in the handler test suites.
	probeAllowed bool
	// item is the beta-gate item the row closes (empty for already-correct rows).
	item string
}

// authzRouteTable enumerates every route registered in router.go.
var authzRouteTable = []authzRoute{
	// --- Public by design ---
	{method: "GET", pattern: "/api/v1/health", tier: tierPublic, probeAllowed: true},
	{method: "GET", pattern: "/api/v1/head", tier: tierPublic, probeAllowed: true},
	{method: "GET", pattern: "/api/v1/leafs/stats/batch", tier: tierPublic, probeAllowed: true},
	{method: "POST", pattern: "/api/v1/volunteers/register", tier: tierPublic},
	{method: "POST", pattern: "/api/v1/volunteers/register-challenge", tier: tierPublic},

	// --- Leaf metadata (visibility tier — PRIVATE hidden from non-owners) ---
	{method: "GET", pattern: "/api/v1/leafs/{leaf_id}", tier: tierVisibility, probeAllowed: true},
	{method: "GET", pattern: "/api/v1/leafs", tier: tierVisibility, probeAllowed: true},
	{method: "GET", pattern: "/api/v1/projects/{leaf_id}", tier: tierVisibility, probeAllowed: true},
	{method: "GET", pattern: "/api/v1/projects", tier: tierVisibility, probeAllowed: true},

	// --- Authenticated (any key) ---
	{method: "GET", pattern: "/api/v1/health/detailed", tier: tierAuthed, probeAllowed: true},
	{method: "POST", pattern: "/api/v1/leafs", tier: tierAuthed},
	{method: "POST", pattern: "/api/v1/projects", tier: tierAuthed},

	// --- Leaf contents + mutations (owner tier) ---
	{method: "PUT", pattern: "/api/v1/leafs/{leaf_id}", tier: tierOwner},
	{method: "DELETE", pattern: "/api/v1/leafs/{leaf_id}", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/activate", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/pause", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/resume", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/archive", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/configure", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/versions", tier: tierOwner},
	{method: "GET", pattern: "/api/v1/leafs/{leaf_id}/versions", tier: tierOwner, probeAllowed: true},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/versions/{version_id}/activate", tier: tierOwner},
	{method: "DELETE", pattern: "/api/v1/leafs/{leaf_id}/versions/{version_id}", tier: tierOwner, item: "BG-11c"},
	{method: "GET", pattern: "/api/v1/leafs/{leaf_id}/work-units", tier: tierOwner, probeAllowed: true},
	{method: "GET", pattern: "/api/v1/leafs/{leaf_id}/work-units/{work_unit_id}", tier: tierOwner, probeAllowed: true},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/work-units/generate", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/work-units/{work_unit_id}/requeue", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/work-units/bulk", tier: tierOwner},
	{method: "GET", pattern: "/api/v1/leafs/{leaf_id}/results", tier: tierOwner, probeAllowed: true},
	{method: "POST", pattern: "/api/v1/leafs/{leaf_id}/aggregate", tier: tierOwner},
	// BG-11a: the aggregate READ joins the write's tier (was public via
	// aggregation.RegisterRoutes; both aliases move under authOwner).
	{method: "GET", pattern: "/api/v1/leafs/{leaf_id}/aggregate", tier: tierOwner, probeAllowed: true, item: "BG-11a"},
	{method: "GET", pattern: "/api/v1/projects/{leaf_id}/aggregate", tier: tierOwner, probeAllowed: true, item: "BG-11a"},
	// BG-11b (per-leaf): any authed caller could read any leaf's credit and
	// resource analytics; the read joins the owner tier.
	{method: "GET", pattern: "/api/v1/credit/analysis/{leaf_id}", tier: tierOwner, probeAllowed: true, item: "BG-11b"},

	// --- Deprecated /projects aliases (same handlers, same tiers) ---
	{method: "PUT", pattern: "/api/v1/projects/{leaf_id}", tier: tierOwner},
	{method: "DELETE", pattern: "/api/v1/projects/{leaf_id}", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/projects/{leaf_id}/activate", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/projects/{leaf_id}/pause", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/projects/{leaf_id}/resume", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/projects/{leaf_id}/archive", tier: tierOwner},
	{method: "POST", pattern: "/api/v1/projects/{leaf_id}/configure", tier: tierOwner},

	// --- Operator-only analysis reads (admin-only tier) ---
	// BG-11b (cross-leaf): whole-head economics — ADMIN only.
	{method: "GET", pattern: "/api/v1/credit/analysis/cross-leaf", tier: tierAdminOnly, probeAllowed: true, item: "BG-11b"},
	// ★BG-11c: per-volunteer machine fingerprint + credit timelines — ADMIN
	// only (the volunteer's own self-view is the Ed25519 gRPC path).
	{method: "GET", pattern: "/api/v1/volunteers/{id}/credit/breakdown", tier: tierAdminOnly, probeAllowed: true, item: "BG-11c"},

	// --- Operator observability (admin-only tier; BG-29) ---
	// Registered OUTSIDE /api/v1/ so the shipped Caddy topology never proxies
	// them (see the topology note in router.go); the admin gate is the backstop
	// for a deploy that exposes the head's HTTP port directly.
	{method: "GET", pattern: "/metrics", tier: tierAdminOnly, probeAllowed: true, item: "BG-29"},
	// No-slash twin exists so the mux's automatic 301 → /debug/pprof/ can never
	// answer an anonymous probe ahead of the admin gate.
	{method: "GET", pattern: "/debug/pprof", tier: tierAdminOnly, probeAllowed: true, item: "BG-29"},
	{method: "GET", pattern: "/debug/pprof/", tier: tierAdminOnly, probeAllowed: true, item: "BG-29"},
	{method: "GET", pattern: "/debug/pprof/cmdline", tier: tierAdminOnly, probeAllowed: true, item: "BG-29"},
	// profile and trace BLOCK for their sampling window (30s / 1s defaults) —
	// denials are still probed on every row; only the authorized probe is off.
	{method: "GET", pattern: "/debug/pprof/profile", tier: tierAdminOnly, item: "BG-29"},
	{method: "GET", pattern: "/debug/pprof/symbol", tier: tierAdminOnly, probeAllowed: true, item: "BG-29"},
	{method: "GET", pattern: "/debug/pprof/trace", tier: tierAdminOnly, item: "BG-29"},

	// --- Operator administration (authAdmin wrapper + handler requireAdmin) ---
	{method: "POST", pattern: "/api/v1/admin/trust", tier: tierAdminGate},
	{method: "POST", pattern: "/api/v1/admin/trust/slash", tier: tierAdminGate},
	{method: "GET", pattern: "/api/v1/admin/trust/{subject}", tier: tierAdminGate, probeAllowed: true},
	{method: "GET", pattern: "/api/v1/admin/trust", tier: tierAdminGate, probeAllowed: true},
	{method: "POST", pattern: "/api/v1/admin/standing", tier: tierAdminGate},
	{method: "POST", pattern: "/api/v1/admin/standing/clear", tier: tierAdminGate},
	{method: "GET", pattern: "/api/v1/admin/standing/{volunteer_id}", tier: tierAdminGate, probeAllowed: true},
	{method: "GET", pattern: "/api/v1/admin/standing", tier: tierAdminGate, probeAllowed: true},
	{method: "POST", pattern: "/api/v1/admin/credit/adjustments", tier: tierAdminGate},
	{method: "GET", pattern: "/api/v1/admin/credit/adjustments", tier: tierAdminGate, probeAllowed: true},
	{method: "POST", pattern: "/api/v1/admin/audit/runners", tier: tierAdminGate},
	{method: "POST", pattern: "/api/v1/admin/audit/runners/deactivate", tier: tierAdminGate},
	{method: "GET", pattern: "/api/v1/admin/audit/runners", tier: tierAdminGate, probeAllowed: true},
	{method: "GET", pattern: "/api/v1/admin/audit/results", tier: tierAdminGate, probeAllowed: true},
	{method: "GET", pattern: "/api/v1/admin/audit/flagged-leaves", tier: tierAdminGate, probeAllowed: true},

	// --- Browser volunteer endpoints (Ed25519 handler-level auth) ---
	{method: "POST", pattern: "/api/v1/identity/bind-did", tier: tierVolunteerKey},
	{method: "POST", pattern: "/api/v1/volunteers/request-work", tier: tierVolunteerKey},
	{method: "POST", pattern: "/api/v1/volunteers/submit-result", tier: tierVolunteerKey},
}

// publicRegistrars are the RegisterRoutes(mux) call sites in router.go whose
// routes are public BY DESIGN (see §6.3 boundary note: the public stats /
// attestation / identity feeds belong to the A/E settlement layer, not B).
// Any other registrar — e.g. the aggregation handler's, which used to expose
// the aggregate GET anonymously (BG-11a) — must not appear in router.go.
var publicRegistrars = []string{
	"statsHandler",
	"volunteerStatsHandler",
	"attestationHandler",
	"identityHandler",
	"health.NewHandler",
}

// findAuthzRoute returns the table row for a "METHOD PATTERN" registration.
func findAuthzRoute(method, pattern string) *authzRoute {
	for i := range authzRouteTable {
		if authzRouteTable[i].method == method && authzRouteTable[i].pattern == pattern {
			return &authzRouteTable[i]
		}
	}
	return nil
}
