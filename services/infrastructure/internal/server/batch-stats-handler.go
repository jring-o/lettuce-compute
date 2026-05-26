package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

const maxBatchStatsIDs = 200

// handleBatchStats returns stats for multiple projects in a single call.
// GET /api/v1/projects/stats/batch?ids=uuid1,uuid2,...
func handleBatchStats(engine *stats.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idsParam := r.URL.Query().Get("ids")
		if idsParam == "" {
			apierror.WriteError(w, apierror.ValidationError("ids parameter is required",
				map[string]string{"field": "ids", "reason": "required"}))
			return
		}

		rawIDs := strings.Split(idsParam, ",")
		if len(rawIDs) > maxBatchStatsIDs {
			apierror.WriteError(w, apierror.ValidationError(
				"maximum 200 project IDs allowed per batch request",
				map[string]string{"field": "ids", "reason": "too_many"}))
			return
		}

		projectIDs := make([]types.ID, 0, len(rawIDs))
		for _, raw := range rawIDs {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			id, err := types.ParseID(raw)
			if err != nil {
				apierror.WriteError(w, apierror.ValidationError(
					"invalid UUID in ids: "+raw,
					map[string]string{"field": "ids", "reason": "invalid_uuid"}))
				return
			}
			projectIDs = append(projectIDs, id)
		}

		if len(projectIDs) == 0 {
			apierror.WriteError(w, apierror.ValidationError("ids parameter must contain at least one valid UUID",
				map[string]string{"field": "ids", "reason": "required"}))
			return
		}

		statsMap, err := engine.ComputeLeafStatsBatch(r.Context(), projectIDs)
		if err != nil {
			apierror.WriteError(w, apierror.FromError(err))
			return
		}

		// Convert types.ID keys to strings for JSON.
		data := make(map[string]*stats.LeafStatsSnapshot, len(statsMap))
		for id, snap := range statsMap {
			data[id.String()] = snap
		}

		resp := struct {
			Data map[string]*stats.LeafStatsSnapshot `json:"data"`
		}{Data: data}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
