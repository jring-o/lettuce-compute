package credit

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// VolunteerStatsResponse is the JSON response for GET /api/v1/volunteers/{volunteer_id}/stats.
type VolunteerStatsResponse struct {
	VolunteerID             types.ID         `json:"volunteer_id"`
	PublicKey               string           `json:"public_key"`
	TotalCredit             float64          `json:"total_credit"`
	TotalWorkUnitsCompleted int              `json:"total_work_units_completed"`
	TotalWorkUnitsRejected  int              `json:"total_work_units_rejected"`
	Leafs                   []LeafStatsEntry `json:"leafs"`
}

// LeafStatsEntry is a per-project breakdown within volunteer stats.
type LeafStatsEntry struct {
	LeafID             types.ID `json:"leaf_id"`
	LeafName           string   `json:"leaf_name"`
	TotalCredit        float64  `json:"total_credit"`
	RAC                float64  `json:"rac"`
	WorkUnitsCompleted int      `json:"work_units_completed"`
}

// VolunteerStatsHandler handles volunteer statistics HTTP requests.
type VolunteerStatsHandler struct {
	pool          *pgxpool.Pool
	volunteerRepo volunteer.Repository
	racRepo       RACRepository
	creditRepo    Repository
	leafRepo      leaf.Repository
	logger        *slog.Logger
	// settlement is the OPTIONAL export-settlement configuration (kill switch, maturation
	// window, emission-anomaly halt). nil (the default — WithSettlement never called) makes
	// the whole settlement layer inert: every path below behaves exactly as it did before
	// this layer shipped. Wired by the orchestrator via WithSettlement.
	settlement *SettlementExportConfig
}

// SettlementExportConfig carries the resolved export-settlement knobs for the public
// credit-stats feeds. The orchestrator resolves the config.HeadConfig accessors at wiring
// and hands the concrete values here; the handler does no config resolution itself.
//
// It is passed as a POINTER on purpose: a nil config is the inert default (current
// behavior on every path), whereas a zero-VALUE struct would have ExportEnabled=false and
// therefore 503 the export — the opposite of inert. WithSettlement takes *SettlementExportConfig
// so "never configured" and "configured with the export disabled" stay distinguishable.
type SettlementExportConfig struct {
	// ExportEnabled is the kill switch. false freezes the public credit-stats feeds (503).
	ExportEnabled bool
	// MaturationDays > 0 makes the fleet feed serve per-entry adjustment-net sums over
	// entries at least this many days old; 0 serves raw lifetime sums (the default query).
	MaturationDays int
	// AnomalyHaltEnabled arms the emission circuit breaker on the feeds.
	AnomalyHaltEnabled bool
	// AnomalyFactor is the trip multiple, reported in the anomaly-halt response body.
	AnomalyFactor float64
	// AnomalyChecker is consulted when AnomalyHaltEnabled is set. Typed as the AnomalyCheck
	// interface so tests can stub the verdict; production wires a *AnomalyChecker.
	AnomalyChecker AnomalyCheck
}

// WithSettlement wires the OPTIONAL export-settlement configuration. Left unset the
// settlement layer is inert and every credit-stats path behaves exactly as before.
// Chainable; returns the handler so it can be composed onto NewVolunteerStatsHandler
// without widening its constructor signature.
func (h *VolunteerStatsHandler) WithSettlement(cfg *SettlementExportConfig) *VolunteerStatsHandler {
	h.settlement = cfg
	return h
}

// NewVolunteerStatsHandler creates a new VolunteerStatsHandler.
func NewVolunteerStatsHandler(
	pool *pgxpool.Pool,
	volunteerRepo volunteer.Repository,
	racRepo RACRepository,
	creditRepo Repository,
	leafRepo leaf.Repository,
	logger *slog.Logger,
) *VolunteerStatsHandler {
	return &VolunteerStatsHandler{
		pool:          pool,
		volunteerRepo: volunteerRepo,
		racRepo:       racRepo,
		creditRepo:    creditRepo,
		leafRepo:      leafRepo,
		logger:        logger,
	}
}

// RegisterRoutes registers volunteer stats routes on the given mux.
func (h *VolunteerStatsHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/volunteers/lookup", h.handleLookupVolunteer)
	mux.HandleFunc("GET /api/v1/volunteers/stats", h.handleGetAllVolunteerStats)
	mux.HandleFunc("GET /api/v1/volunteers/{volunteer_id}/stats", h.handleGetVolunteerStats)
}

const (
	// exportStatusHeader lets a consumer distinguish the two circuit-breaker 503s (an
	// operator kill switch vs a tripped anomaly halt) from a genuine 5xx, without parsing
	// the body.
	exportStatusHeader  = "X-Lettuce-Export-Status"
	exportStatusKilled  = "killed"
	exportStatusAnomaly = "anomaly-halt"
	// exportRetryAfterSeconds is the Retry-After hint on a frozen export. The knobs are
	// boot-time env, so this is only a hint; the freeze clears when the operator acts (kill
	// switch) or the trailing average catches up (anomaly halt).
	exportRetryAfterSeconds = "3600"
)

// exportGate applies the settlement circuit breakers before any credit figures are served:
// the operator kill switch first, then the emission-anomaly halt. It returns true once it
// has written a 503 response and the caller must return. A nil settlement config leaves
// every path exactly as before (the inert default), and the anomaly check FAILS OPEN — a
// checker error is logged and the export keeps serving, so an anomaly-check outage can
// never take the export down.
func (h *VolunteerStatsHandler) exportGate(w http.ResponseWriter, r *http.Request) bool {
	cfg := h.settlement
	if cfg == nil {
		return false
	}

	// (1) Kill switch: the operator has frozen the export. A consumer treats 503 as "halt
	// payouts" rather than ingesting figures that are under investigation.
	if !cfg.ExportEnabled {
		writeExportHalt(w, exportStatusKilled, map[string]any{
			"error": "credit stats export is disabled by the operator",
		})
		return true
	}

	// (2) Anomaly halt: today's global grant total is anomalously high. Fail OPEN on a
	// checker error (an anomaly-check outage must not freeze a healthy export).
	if cfg.AnomalyHaltEnabled && cfg.AnomalyChecker != nil {
		verdict, err := cfg.AnomalyChecker.Check(r.Context())
		if err != nil {
			l := logging.LoggerFromContext(r.Context(), h.logger)
			l.Warn("emission anomaly check failed; failing open and serving the export", "error", err)
		} else if verdict.Halted {
			writeExportHalt(w, exportStatusAnomaly, map[string]any{
				"error":    "credit stats export is frozen: today's granted credit is anomalously high",
				"today":    verdict.Today,
				"baseline": verdict.Baseline,
				"factor":   cfg.AnomalyFactor,
			})
			return true
		}
	}
	return false
}

// writeExportHalt writes the 503 circuit-breaker response: the machine-distinguishable
// export-status header (killed vs anomaly-halt) and a Retry-After hint, then the JSON body.
// Headers must be set before the status line, so they precede writeJSON's WriteHeader.
func writeExportHalt(w http.ResponseWriter, status string, body map[string]any) {
	w.Header().Set("Retry-After", exportRetryAfterSeconds)
	w.Header().Set(exportStatusHeader, status)
	writeJSON(w, http.StatusServiceUnavailable, body)
}

func (h *VolunteerStatsHandler) handleGetVolunteerStats(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	// The kill switch and anomaly halt gate this per-account public surface too — it is a
	// scraper bypass of the fleet-feed circuit breaker otherwise (audit F5). This surface
	// stays UNMATURED (a recorded design deviation, D3): only the fleet feed nets/maturates;
	// the per-account view keeps full-ledger figures for volunteer UX.
	if h.exportGate(w, r) {
		return
	}

	volunteerID, err := types.ParseID(r.PathValue("volunteer_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid volunteer_id: must be a valid UUID", nil))
		return
	}

	// Get the volunteer record (for the public key).
	vol, err := h.volunteerRepo.GetByID(r.Context(), volunteerID)
	if err != nil {
		l.Error("failed to get volunteer", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Credit and work-units-completed are sourced from the authoritative,
	// append-only credit ledger (ComputeVolunteerBreakdown — the same computation
	// behind GetMyContribution), NOT from the per-volunteer running counters
	// (volunteers.total_work_units_completed, volunteer_rac.total_credit). Those
	// counters are best-effort and never reconciled, so they drift above the ledger
	// after work-unit regenerations or counting-logic changes. Deriving display from
	// the ledger keeps this endpoint consistent with every other credit surface.
	bd, err := ComputeVolunteerBreakdown(r.Context(), h.pool, volunteerID)
	if err != nil {
		l.Error("failed to compute credit breakdown", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.Internal("failed to compute volunteer stats", err))
		return
	}

	// RAC is a distinct, decaying metric kept per (volunteer, leaf); it is the one
	// value here that legitimately lives in volunteer_rac rather than the ledger.
	racEntries, err := h.racRepo.ListByVolunteer(r.Context(), volunteerID)
	if err != nil {
		l.Error("failed to list rac entries", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	racByLeaf := make(map[types.ID]float64, len(racEntries))
	for _, rac := range racEntries {
		racByLeaf[rac.LeafID] = rac.RAC
	}

	// Rejected work units: count DISAGREED results directly (the authoritative
	// record) rather than the volunteers.total_work_units_rejected running counter.
	var rejected int
	if err := h.pool.QueryRow(r.Context(),
		"SELECT COUNT(*)::int FROM results WHERE volunteer_id = $1 AND validation_status = 'DISAGREED'",
		volunteerID,
	).Scan(&rejected); err != nil {
		l.Error("failed to count rejected results", "error", err, "volunteer_id", volunteerID)
		rejected = 0
	}

	// Build the per-leaf breakdown from the ledger, attaching RAC per leaf.
	leafs := make([]LeafStatsEntry, 0, len(bd.ByLeaf))
	totalWorkUnits := 0
	for _, lc := range bd.ByLeaf {
		leafs = append(leafs, LeafStatsEntry{
			LeafID:             lc.LeafID,
			LeafName:           lc.LeafName,
			TotalCredit:        lc.Credit,
			RAC:                racByLeaf[lc.LeafID],
			WorkUnitsCompleted: lc.WorkUnits,
		})
		totalWorkUnits += lc.WorkUnits
	}

	resp := VolunteerStatsResponse{
		VolunteerID:             volunteerID,
		PublicKey:               base64.RawURLEncoding.EncodeToString(vol.PublicKey),
		TotalCredit:             bd.TotalCredit,
		TotalWorkUnitsCompleted: totalWorkUnits,
		TotalWorkUnitsRejected:  rejected,
		Leafs:                   leafs,
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *VolunteerStatsHandler) handleLookupVolunteer(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	userIDStr := r.URL.Query().Get("user_id")
	if userIDStr == "" {
		apierror.WriteError(w, apierror.ValidationError("user_id query parameter is required", nil))
		return
	}

	userID, err := types.ParseID(userIDStr)
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid user_id: must be a valid UUID", nil))
		return
	}

	vol, err := h.volunteerRepo.GetByUserID(r.Context(), userID)
	if err != nil {
		l.Error("failed to lookup volunteer by user_id", "error", err, "user_id", userID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	resp := struct {
		ID        types.ID `json:"id"`
		PublicKey string   `json:"public_key"`
		UserID    string   `json:"user_id"`
	}{
		ID:        vol.ID,
		PublicKey: base64.RawURLEncoding.EncodeToString(vol.PublicKey),
		UserID:    userID.String(),
	}

	writeJSON(w, http.StatusOK, resp)
}

// AllVolunteerStatsEntry is a per-volunteer entry in the cross-leaf stats feed.
type AllVolunteerStatsEntry struct {
	VolunteerID types.ID `json:"volunteer_id"`
	NumericID   int      `json:"numeric_id"`
	PublicKey   string   `json:"public_key"`
	TotalCredit float64  `json:"total_credit"`
	RAC         float64  `json:"rac"`
}

// AllVolunteerStatsResponse is the JSON response for GET /api/v1/volunteers/stats.
type AllVolunteerStatsResponse struct {
	TotalCredit float64                  `json:"total_credit"`
	TotalUsers  int                      `json:"total_users"`
	Volunteers  []AllVolunteerStatsEntry `json:"volunteers"`
	GeneratedAt string                   `json:"generated_at"`
	// MaturationDays is present ONLY when the maturation window is active (> 0), signalling
	// that TotalCredit figures are matured per-entry nets rather than raw lifetime sums. The
	// key is ABSENT when maturation is off (omitempty + set-only-when-positive) so the
	// default response is byte-shape-stable against the pre-settlement feed (audit F4).
	MaturationDays int `json:"maturation_days,omitempty"`
}

// fleetStatsSQL is the DEFAULT (maturation-off) fleet-feed query: raw lifetime credit sums
// over ACTIVE leafs, RAC from volunteer_rac. It is the pre-settlement query verbatim, run
// unchanged whenever the maturation window is 0 so the default path is untouched (F4).
const fleetStatsSQL = `
		SELECT c.volunteer_id, v.numeric_id, v.public_key,
		       c.total_credit,
		       COALESCE(rr.rac, 0) AS rac
		FROM (
			SELECT cl.volunteer_id, SUM(cl.credit_amount)::float8 AS total_credit
			FROM credit_ledger cl
			JOIN leafs lf ON lf.id = cl.leaf_id
			WHERE lf.state = 'ACTIVE'
			GROUP BY cl.volunteer_id
		) c
		JOIN volunteers v ON v.id = c.volunteer_id
		LEFT JOIN (
			SELECT vr.volunteer_id, SUM(vr.rac)::float8 AS rac
			FROM volunteer_rac vr
			JOIN leafs lf ON lf.id = vr.leaf_id
			WHERE lf.state = 'ACTIVE'
			GROUP BY vr.volunteer_id
		) rr ON rr.volunteer_id = c.volunteer_id`

// maturedFleetStatsSQL is the maturation-on fleet-feed query ($1 = maturation days). Credit
// is the PER-ENTRY adjustment net — credit_ledger LEFT JOINed to adjustments summed by
// ledger_entry_id — restricted to entries at least $1 days old on ACTIVE leafs, with
// HAVING SUM(net) > 0 so a fully-clawed volunteer drops out of the feed (audit F16). The
// netting is strictly PER-ENTRY via ledger_entry_id and NEVER a per-account adjustment sum:
// a per-account subtraction would wrongly net clawbacks of immature or inactive-leaf
// entries into matured totals (audit F6). The RAC subquery is identical to the default
// path (RAC correction on clawback is a later slice).
const maturedFleetStatsSQL = `
		SELECT c.volunteer_id, v.numeric_id, v.public_key,
		       c.total_credit,
		       COALESCE(rr.rac, 0) AS rac
		FROM (
			SELECT cl.volunteer_id, SUM(cl.credit_amount + COALESCE(a.adj, 0))::float8 AS total_credit
			FROM credit_ledger cl
			JOIN leafs lf ON lf.id = cl.leaf_id AND lf.state = 'ACTIVE'
			LEFT JOIN (
				SELECT ledger_entry_id, SUM(amount) AS adj
				FROM credit_adjustments
				GROUP BY ledger_entry_id
			) a ON a.ledger_entry_id = cl.id
			WHERE cl.granted_at <= now() - ($1::int * interval '1 day')
			GROUP BY cl.volunteer_id
			HAVING SUM(cl.credit_amount + COALESCE(a.adj, 0)) > 0
		) c
		JOIN volunteers v ON v.id = c.volunteer_id
		LEFT JOIN (
			SELECT vr.volunteer_id, SUM(vr.rac)::float8 AS rac
			FROM volunteer_rac vr
			JOIN leafs lf ON lf.id = vr.leaf_id
			WHERE lf.state = 'ACTIVE'
			GROUP BY vr.volunteer_id
		) rr ON rr.volunteer_id = c.volunteer_id`

// handleGetAllVolunteerStats returns per-volunteer RAC and total credit aggregated
// across all ACTIVE leafs. Public feed; deterministic ordering by public key.
func (h *VolunteerStatsHandler) handleGetAllVolunteerStats(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	// Kill switch + emission-anomaly halt (inert when no settlement config is wired).
	if h.exportGate(w, r) {
		return
	}

	// total_credit is summed from the authoritative append-only credit ledger
	// (consistent with the per-volunteer stats endpoint and GetMyContribution),
	// while rac stays sourced from volunteer_rac — rac is a decaying metric that
	// has no ledger equivalent. Both are restricted to ACTIVE leafs. Driving the
	// outer query off the ledger means a volunteer appears iff it has credit on an
	// ACTIVE leaf, matching the prior inclusion semantics.
	//
	// When the maturation window is active the fleet feed instead serves per-entry
	// adjustment-net sums over matured entries (maturedFleetStatsSQL); when it is off the
	// pre-settlement query runs verbatim (fleetStatsSQL), never routed through the netting
	// SQL with a 0 window (audit F4).
	query := fleetStatsSQL
	var args []any
	if h.settlement != nil && h.settlement.MaturationDays > 0 {
		query = maturedFleetStatsSQL
		args = []any{h.settlement.MaturationDays}
	}
	rows, err := h.pool.Query(r.Context(), query, args...)
	if err != nil {
		l.Error("failed to query cross-leaf volunteer stats", "error", err)
		apierror.WriteError(w, apierror.Internal("failed to compute volunteer stats", err))
		return
	}
	defer rows.Close()

	entries := make([]AllVolunteerStatsEntry, 0)
	var totalCredit float64
	for rows.Next() {
		var (
			volID  types.ID
			numID  int
			pubKey []byte
			credit float64
			rac    float64
		)
		if scanErr := rows.Scan(&volID, &numID, &pubKey, &credit, &rac); scanErr != nil {
			l.Error("failed to scan volunteer stats row", "error", scanErr)
			continue
		}
		entries = append(entries, AllVolunteerStatsEntry{
			VolunteerID: volID,
			NumericID:   numID,
			PublicKey:   base64.RawURLEncoding.EncodeToString(pubKey),
			TotalCredit: credit,
			RAC:         rac,
		})
		totalCredit += credit
	}
	if err := rows.Err(); err != nil {
		l.Error("failed to iterate volunteer stats rows", "error", err)
		apierror.WriteError(w, apierror.Internal("failed to compute volunteer stats", err))
		return
	}

	// Deterministic ordering by public key.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].PublicKey < entries[j].PublicKey
	})

	resp := AllVolunteerStatsResponse{
		TotalCredit: totalCredit,
		TotalUsers:  len(entries),
		Volunteers:  entries,
		GeneratedAt: types.FormatTimestamp(types.Now()),
	}
	// Set only when maturation is active, so the key is absent on the default feed (F4).
	if h.settlement != nil && h.settlement.MaturationDays > 0 {
		resp.MaturationDays = h.settlement.MaturationDays
	}

	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
