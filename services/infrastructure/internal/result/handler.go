package result

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// ResultHandler handles HTTP requests for result queries.
type ResultHandler struct {
	repo        Repository
	leafRepo leaf.Repository
	logger      *slog.Logger
}

// NewResultHandler creates a new ResultHandler.
func NewResultHandler(repo Repository, leafRepo leaf.Repository, logger *slog.Logger) *ResultHandler {
	return &ResultHandler{
		repo:        repo,
		leafRepo: leafRepo,
		logger:      logger,
	}
}

// RegisterRoutes is a no-op; result routes require auth and are
// registered in the router with middleware wrappers.
func (h *ResultHandler) RegisterRoutes(mux *http.ServeMux) {}

// HandleListByLeaf handles GET /api/v1/leafs/{leaf_id}/results (exported for auth wrapping).
func (h *ResultHandler) HandleListByLeaf(w http.ResponseWriter, r *http.Request) {
	h.handleListByLeaf(w, r)
}

// handleListByLeaf handles GET /api/v1/leafs/{leaf_id}/results.
func (h *ResultHandler) handleListByLeaf(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	// Parse leaf ID.
	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id", nil))
		return
	}

	// Verify leaf exists.
	_, err = h.leafRepo.GetByID(r.Context(), leafID)
	if err != nil {
		apiErr := apierror.FromError(err)
		apierror.WriteError(w, apiErr)
		return
	}

	// Parse filters.
	var filters ResultFilters
	if vs := r.URL.Query().Get("validation_status"); vs != "" {
		status := ValidationStatus(vs)
		switch status {
		case ValidationPending, ValidationAgreed, ValidationDisagreed:
			filters.ValidationStatus = &status
		default:
			apierror.WriteError(w, apierror.ValidationError(
				"validation_status must be PENDING, AGREED, or DISAGREED",
				nil,
			))
			return
		}
	}
	if wuID := r.URL.Query().Get("work_unit_id"); wuID != "" {
		id, err := types.ParseID(wuID)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid work_unit_id", nil))
			return
		}
		filters.WorkUnitID = &id
	}
	if vid := r.URL.Query().Get("volunteer_id"); vid != "" {
		id, err := types.ParseID(vid)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid volunteer_id", nil))
			return
		}
		filters.VolunteerID = &id
	}

	// Parse pagination.
	page := types.PaginationRequest{
		Cursor: r.URL.Query().Get("cursor"),
	}
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit < 1 {
			apierror.WriteError(w, apierror.ValidationError("limit must be a positive integer", nil))
			return
		}
		page.PageSize = limit
	}

	// Query results.
	results, pagination, err := h.repo.ListByLeaf(r.Context(), leafID, filters, page)
	if err != nil {
		l.Error("failed to list results", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	writeJSON(w, http.StatusOK, types.NewListResponse(results, pagination))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
