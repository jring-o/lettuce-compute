package aggregation

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// AggregationHandler handles aggregation HTTP requests.
type AggregationHandler struct {
	engine *Engine
	logger *slog.Logger
}

// NewAggregationHandler creates a new AggregationHandler.
func NewAggregationHandler(engine *Engine, logger *slog.Logger) *AggregationHandler {
	return &AggregationHandler{
		engine: engine,
		logger: logger,
	}
}

// RegisterRoutes is retained for test harnesses that drive the aggregation
// endpoints unauthenticated. Production does NOT call it: the router registers
// both the GET (read) and POST (recompute) under authOwner, because the
// aggregate is leaf CONTENTS — owner-only regardless of the leaf's visibility
// (BG-11a). Registering the GET here without a wrapper is what left it
// anonymous; the router now binds HandleGetAggregate under authOwner instead.
func (h *AggregationHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}/aggregate", h.handleGetAggregate)
	// Deprecated alias (removed in v0.10).
	mux.HandleFunc("GET /api/v1/projects/{leaf_id}/aggregate", h.handleGetAggregate)
}

// HandleAggregate handles POST /api/v1/leafs/{leaf_id}/aggregate (exported for auth wrapping).
func (h *AggregationHandler) HandleAggregate(w http.ResponseWriter, r *http.Request) {
	h.handleAggregate(w, r)
}

// HandleGetAggregate handles GET /api/v1/leafs/{leaf_id}/aggregate (exported for
// auth wrapping — the router binds it under authOwner, BG-11a).
func (h *AggregationHandler) HandleGetAggregate(w http.ResponseWriter, r *http.Request) {
	h.handleGetAggregate(w, r)
}

// aggregateRequest is the optional POST body.
type aggregateRequest struct {
	BatchID *string `json:"batch_id,omitempty"`
	Format  string  `json:"format,omitempty"`
	Force   bool    `json:"force,omitempty"`
}

// handleAggregate handles POST /api/v1/leafs/{leaf_id}/aggregate.
func (h *AggregationHandler) handleAggregate(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	var req aggregateRequest
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
			return
		}
	}

	if req.Format != "" && req.Format != "json" && req.Format != "csv" {
		apierror.WriteError(w, apierror.ValidationError("format must be 'json' or 'csv'", nil))
		return
	}

	opts := AggregateOptions{
		BatchID: req.BatchID,
		Format:  req.Format,
		Force:   req.Force,
	}

	result, err := h.engine.Aggregate(r.Context(), leafID, opts)
	if err != nil {
		l.Error("aggregation failed", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": result})
}

// handleGetAggregate handles GET /api/v1/leafs/{leaf_id}/aggregate.
func (h *AggregationHandler) handleGetAggregate(w http.ResponseWriter, r *http.Request) {
	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	cached := h.engine.GetCached(leafID)
	if cached == nil {
		apierror.WriteError(w, apierror.NotFound("aggregation result", leafID.String()))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": cached})
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
