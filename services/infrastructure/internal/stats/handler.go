package stats

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

const defaultMaxAgeSeconds = 60

// StatsHandler handles HTTP requests for leaf statistics.
type StatsHandler struct {
	engine      *Engine
	leafRepo leaf.Repository
	logger      *slog.Logger
}

// NewStatsHandler creates a new StatsHandler.
func NewStatsHandler(engine *Engine, leafRepo leaf.Repository, logger *slog.Logger) *StatsHandler {
	return &StatsHandler{
		engine:      engine,
		leafRepo: leafRepo,
		logger:      logger,
	}
}

// RegisterRoutes registers stats routes on the given mux.
func (h *StatsHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/stats", h.handleGetStats)
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/stats/history", h.handleGetStatsHistory)
	// Deprecated aliases (removed in v0.10).
	mux.HandleFunc("GET /api/v1/projects/{leaf_id}/stats", h.handleGetStats)
	mux.HandleFunc("GET /api/v1/projects/{leaf_id}/stats/history", h.handleGetStatsHistory)
}

// handleGetStats returns the latest stats snapshot for a leaf.
// If no snapshot exists or the latest is older than the project's stats_cache_seconds, computes a fresh one.
func (h *StatsHandler) handleGetStats(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	// Verify project exists and read cache setting.
	proj, err := h.leafRepo.GetByID(r.Context(), leafID)
	if err != nil {
		l.Error("failed to get project for stats", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	cacheSeconds := proj.StatsCacheSeconds
	if cacheSeconds <= 0 {
		cacheSeconds = defaultMaxAgeSeconds
	}
	maxAge := time.Duration(cacheSeconds) * time.Second

	snap, err := h.engine.GetOrComputeSnapshot(r.Context(), leafID, maxAge)
	if err != nil {
		l.Error("failed to get or compute stats snapshot", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	writeJSON(w, http.StatusOK, snap)
}

// handleGetStatsHistory returns time-series snapshots for charts.
func (h *StatsHandler) handleGetStatsHistory(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	// Verify project exists.
	if _, err := h.leafRepo.GetByID(r.Context(), leafID); err != nil {
		l.Error("failed to get project for stats history", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	q := r.URL.Query()

	// Parse required "from" parameter.
	fromStr := q.Get("from")
	if fromStr == "" {
		apierror.WriteError(w, apierror.ValidationError("from parameter is required", nil))
		return
	}
	from, err := types.ParseTimestamp(fromStr)
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid from: must be RFC 3339 timestamp", nil))
		return
	}

	// Parse optional "to" parameter (default: now).
	to := types.Now()
	if toStr := q.Get("to"); toStr != "" {
		to, err = types.ParseTimestamp(toStr)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid to: must be RFC 3339 timestamp", nil))
			return
		}
	}

	// Parse optional "interval" parameter (default: raw).
	interval := q.Get("interval")
	if interval == "" {
		interval = "raw"
	}
	if interval != "raw" && interval != "hourly" && interval != "daily" {
		apierror.WriteError(w, apierror.ValidationError(
			"invalid interval: must be raw, hourly, or daily", nil))
		return
	}

	filters := StatsHistoryFilters{
		From:     from,
		To:       to,
		Interval: interval,
	}

	snapshots, err := h.engine.ListSnapshots(r.Context(), leafID, filters)
	if err != nil {
		l.Error("failed to list stats snapshots", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Ensure non-nil slice for JSON.
	if snapshots == nil {
		snapshots = []*LeafStatsSnapshot{}
	}

	resp := struct {
		Data []*LeafStatsSnapshot `json:"data"`
	}{Data: snapshots}

	writeJSON(w, http.StatusOK, resp)
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
