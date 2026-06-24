package leaf

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/logging"
)

// HeadHandler handles HTTP requests for head identity and leaf discovery.
type HeadHandler struct {
	headCfg *config.HeadConfig
	pool    *pgxpool.Pool
	logger  *slog.Logger
}

// NewHeadHandler creates a new HeadHandler.
func NewHeadHandler(headCfg *config.HeadConfig, pool *pgxpool.Pool, logger *slog.Logger) *HeadHandler {
	return &HeadHandler{
		headCfg: headCfg,
		pool:    pool,
		logger:  logger,
	}
}

// HeadInfoResponse is the response for GET /api/v1/head.
type HeadInfoResponse struct {
	Name               string         `json:"name"`
	Description        string         `json:"description"`
	URL                string         `json:"url"`
	Leafs              []LeafInfo     `json:"leafs"`
	DefaultLeafWeights map[string]int `json:"default_leaf_weights"`
}

// LeafExecutionSpec is a minimal execution spec exposed in the head info response.
// Browser volunteers need this to determine if a leaf has WASM binaries and GPU requirements.
type LeafExecutionSpec struct {
	Binaries    map[string]string `json:"binaries,omitempty"`
	GPURequired bool              `json:"gpu_required"`
	GPUType     string            `json:"gpu_type,omitempty"`
}

// LeafInfo is a summary of an active leaf for discovery.
type LeafInfo struct {
	ID               string             `json:"id"`
	Slug             string             `json:"slug"`
	Name             string             `json:"name"`
	Description      string             `json:"description"`
	ResearchArea     []string           `json:"research_area"`
	TaskPattern      string             `json:"task_pattern"`
	State            string             `json:"state"`
	QueuedWorkUnits  int                `json:"queued_work_units"`
	ActiveVolunteers int                `json:"active_volunteers"`
	ExecutionSpec    *LeafExecutionSpec `json:"execution_spec,omitempty"`
}

// HandleGetHeadInfo handles GET /api/v1/head. No authentication required.
func (h *HeadHandler) HandleGetHeadInfo(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)
	ctx := r.Context()

	// Query active, public leafs with queued WU counts, active volunteer counts, and execution config.
	// Uses LEFT JOINs with pre-aggregated subqueries instead of correlated subqueries to avoid
	// O(N) sequential scans per leaf when work_units table is large.
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT
			l.id, l.slug, l.name, l.description, l.research_area,
			l.task_pattern, l.state, l.execution_config,
			COALESCE(q.cnt, 0) AS queued_work_units,
			COALESCE(a.cnt, 0) AS active_volunteers
		FROM leafs l
		LEFT JOIN (
			SELECT leaf_id, COUNT(*) AS cnt
			FROM work_units
			WHERE state IN ('QUEUED', 'CREATED')
			GROUP BY leaf_id
		) q ON q.leaf_id = l.id
		LEFT JOIN (%s) a ON a.leaf_id = l.id
		WHERE l.state = 'ACTIVE' AND l.visibility = 'PUBLIC'
		ORDER BY l.name ASC
	`, ActiveVolunteerSubquery()))
	if err != nil {
		l.Error("failed to query leafs for head info", "error", err)
		apierror.WriteError(w, apierror.Internal("failed to get head info", err))
		return
	}
	defer rows.Close()

	var leafs []LeafInfo
	for rows.Next() {
		var li LeafInfo
		var researchArea []string
		var execConfig ExecutionConfig
		if err := rows.Scan(
			&li.ID, &li.Slug, &li.Name, &li.Description, &researchArea,
			&li.TaskPattern, &li.State, &execConfig,
			&li.QueuedWorkUnits, &li.ActiveVolunteers,
		); err != nil {
			l.Error("failed to scan leaf info", "error", err)
			apierror.WriteError(w, apierror.Internal("failed to get head info", err))
			return
		}
		li.ResearchArea = researchArea
		if li.ResearchArea == nil {
			li.ResearchArea = []string{}
		}
		// Expose minimal execution spec for browser volunteer leaf selection.
		li.ExecutionSpec = &LeafExecutionSpec{
			Binaries:    execConfig.Binaries,
			GPURequired: execConfig.GPURequired,
			GPUType:     execConfig.GPUType,
		}
		leafs = append(leafs, li)
	}
	if err := rows.Err(); err != nil {
		l.Error("failed to iterate leaf info", "error", err)
		apierror.WriteError(w, apierror.Internal("failed to get head info", err))
		return
	}
	if leafs == nil {
		leafs = []LeafInfo{}
	}

	weights := h.headCfg.DefaultLeafWeights
	if weights == nil {
		weights = map[string]int{}
	}

	resp := HeadInfoResponse{
		Name:               h.headCfg.Name,
		Description:        h.headCfg.Description,
		URL:                h.headCfg.URL,
		Leafs:              leafs,
		DefaultLeafWeights: weights,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_ = json.NewEncoder(w).Encode(resp)
}
