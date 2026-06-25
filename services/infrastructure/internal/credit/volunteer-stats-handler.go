package credit

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// VolunteerStatsResponse is the JSON response for GET /api/v1/volunteers/{volunteer_id}/stats.
type VolunteerStatsResponse struct {
	VolunteerID            types.ID               `json:"volunteer_id"`
	PublicKey              string                 `json:"public_key"`
	TotalCredit            float64                `json:"total_credit"`
	TotalWorkUnitsCompleted int                   `json:"total_work_units_completed"`
	TotalWorkUnitsRejected  int                   `json:"total_work_units_rejected"`
	Leafs               []LeafStatsEntry    `json:"leafs"`
}

// LeafStatsEntry is a per-project breakdown within volunteer stats.
type LeafStatsEntry struct {
	LeafID          types.ID `json:"leaf_id"`
	LeafName        string   `json:"leaf_name"`
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
	leafRepo   leaf.Repository
	logger        *slog.Logger
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
		leafRepo:   leafRepo,
		logger:        logger,
	}
}

// RegisterRoutes registers volunteer stats routes on the given mux.
func (h *VolunteerStatsHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/volunteers/lookup", h.handleLookupVolunteer)
	mux.HandleFunc("GET /api/v1/volunteers/stats", h.handleGetAllVolunteerStats)
	mux.HandleFunc("GET /api/v1/volunteers/{volunteer_id}/stats", h.handleGetVolunteerStats)
}

func (h *VolunteerStatsHandler) handleGetVolunteerStats(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

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
}

// handleGetAllVolunteerStats returns per-volunteer RAC and total credit aggregated
// across all ACTIVE leafs. Public feed; deterministic ordering by public key.
func (h *VolunteerStatsHandler) handleGetAllVolunteerStats(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	// total_credit is summed from the authoritative append-only credit ledger
	// (consistent with the per-volunteer stats endpoint and GetMyContribution),
	// while rac stays sourced from volunteer_rac — rac is a decaying metric that
	// has no ledger equivalent. Both are restricted to ACTIVE leafs. Driving the
	// outer query off the ledger means a volunteer appears iff it has credit on an
	// ACTIVE leaf, matching the prior inclusion semantics.
	rows, err := h.pool.Query(r.Context(), `
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
		) rr ON rr.volunteer_id = c.volunteer_id`)
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

	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
