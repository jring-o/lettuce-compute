package credit

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- Response types ---

type percentileStats struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
}

type taskPatternStats struct {
	Count         int     `json:"count"`
	AvgCPUSeconds float64 `json:"avg_cpu_seconds"`
}

type leafAnalysisResponse struct {
	LeafID       types.ID                    `json:"leaf_id"`
	WorkUnitsAnalyzed int                       `json:"work_units_analyzed"`
	CPUSecondsPerWU   percentileStats           `json:"cpu_seconds_per_wu"`
	GPUSecondsPerWU   percentileStats           `json:"gpu_seconds_per_wu"`
	WallClockPerWU    percentileStats           `json:"wall_clock_per_wu"`
	MemoryMBPerWU     percentileStats           `json:"memory_mb_per_wu"`
	ByTaskPattern     map[string]taskPatternStats `json:"by_task_pattern"`
}

type crossLeafEntry struct {
	LeafID              types.ID  `json:"leaf_id"`
	LeafName            string    `json:"leaf_name"`
	AvgCPUSecondsPerCredit float64   `json:"avg_cpu_seconds_per_credit"`
	AvgGPUSecondsPerCredit float64   `json:"avg_gpu_seconds_per_credit"`
	TotalCreditGranted     float64   `json:"total_credit_granted"`
	ActiveVolunteers       int       `json:"active_volunteers"`
}

type normalizationFactors struct {
	MaxCPUSecondsPerCredit float64 `json:"max_cpu_seconds_per_credit"`
	MinCPUSecondsPerCredit float64 `json:"min_cpu_seconds_per_credit"`
	Ratio                  float64 `json:"ratio"`
}

type crossLeafResponse struct {
	Leafs             []crossLeafEntry  `json:"leafs"`
	NormalizationFactors normalizationFactors `json:"normalization_factors"`
}

// The per-volunteer credit breakdown response types live in breakdown.go
// (VolunteerBreakdown, LeafCredit, ResourceTypeCredit, DailyCredit, WeeklyCredit,
// CreditTimeline) so the operator REST handler below and the volunteer
// self-service gRPC RPC share one definition.

// --- Handler ---

// AnalysisHandler serves credit analysis endpoints.
type AnalysisHandler struct {
	pool        *pgxpool.Pool
	leafRepo leaf.Repository
	logger      *slog.Logger
}

// NewAnalysisHandler creates a new AnalysisHandler.
func NewAnalysisHandler(pool *pgxpool.Pool, leafRepo leaf.Repository, logger *slog.Logger) *AnalysisHandler {
	return &AnalysisHandler{pool: pool, leafRepo: leafRepo, logger: logger}
}

func (h *AnalysisHandler) HandleLeafAnalysis(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id", nil))
		return
	}

	// Percentile distributions from execution_metadata JSONB.
	var wuCount int
	var cpuP50, cpuP90, cpuP99 float64
	var gpuP50, gpuP90, gpuP99 float64
	var wallP50, wallP90, wallP99 float64
	var memP50, memP90, memP99 float64

	err = h.pool.QueryRow(r.Context(), `
		SELECT
			COUNT(*),
			COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY (execution_metadata->>'cpu_seconds_user')::float), 0),
			COALESCE(percentile_cont(0.9) WITHIN GROUP (ORDER BY (execution_metadata->>'cpu_seconds_user')::float), 0),
			COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY (execution_metadata->>'cpu_seconds_user')::float), 0),
			COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY (execution_metadata->>'gpu_seconds')::float), 0),
			COALESCE(percentile_cont(0.9) WITHIN GROUP (ORDER BY (execution_metadata->>'gpu_seconds')::float), 0),
			COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY (execution_metadata->>'gpu_seconds')::float), 0),
			COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY (execution_metadata->>'wall_clock_seconds')::float), 0),
			COALESCE(percentile_cont(0.9) WITHIN GROUP (ORDER BY (execution_metadata->>'wall_clock_seconds')::float), 0),
			COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY (execution_metadata->>'wall_clock_seconds')::float), 0),
			COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY (execution_metadata->>'peak_memory_mb')::float), 0),
			COALESCE(percentile_cont(0.9) WITHIN GROUP (ORDER BY (execution_metadata->>'peak_memory_mb')::float), 0),
			COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY (execution_metadata->>'peak_memory_mb')::float), 0)
		FROM results
		WHERE work_unit_id IN (SELECT id FROM work_units WHERE leaf_id = $1)
		  AND validation_status = 'AGREED'`,
		leafID,
	).Scan(
		&wuCount,
		&cpuP50, &cpuP90, &cpuP99,
		&gpuP50, &gpuP90, &gpuP99,
		&wallP50, &wallP90, &wallP99,
		&memP50, &memP90, &memP99,
	)
	if err != nil {
		l.Error("failed to compute leaf analysis", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.Internal("failed to compute analysis", err))
		return
	}

	// Task pattern breakdown.
	byPattern := make(map[string]taskPatternStats)
	rows, err := h.pool.Query(r.Context(), `
		SELECT l.task_pattern, COUNT(*), COALESCE(AVG((r.execution_metadata->>'cpu_seconds_user')::float), 0)
		FROM results r
		JOIN work_units wu ON wu.id = r.work_unit_id
		JOIN leafs l ON l.id = wu.leaf_id
		WHERE wu.leaf_id = $1 AND r.validation_status = 'AGREED'
		GROUP BY l.task_pattern`,
		leafID,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var pattern string
			var cnt int
			var avgCPU float64
			if scanErr := rows.Scan(&pattern, &cnt, &avgCPU); scanErr == nil {
				byPattern[pattern] = taskPatternStats{Count: cnt, AvgCPUSeconds: avgCPU}
			}
		}
	}

	resp := leafAnalysisResponse{
		LeafID:         leafID,
		WorkUnitsAnalyzed: wuCount,
		CPUSecondsPerWU:   percentileStats{P50: cpuP50, P90: cpuP90, P99: cpuP99},
		GPUSecondsPerWU:   percentileStats{P50: gpuP50, P90: gpuP90, P99: gpuP99},
		WallClockPerWU:    percentileStats{P50: wallP50, P90: wallP90, P99: wallP99},
		MemoryMBPerWU:     percentileStats{P50: memP50, P90: memP90, P99: memP99},
		ByTaskPattern:     byPattern,
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *AnalysisHandler) HandleCrossLeaf(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	rows, err := h.pool.Query(r.Context(), `
		SELECT
			lf.id, lf.name,
			COALESCE(SUM(cl.credit_amount), 0) as total_credit,
			COALESCE(AVG((r.execution_metadata->>'cpu_seconds_user')::float), 0) as avg_cpu,
			COALESCE(AVG((r.execution_metadata->>'gpu_seconds')::float), 0) as avg_gpu,
			COUNT(DISTINCT r.volunteer_id) as active_volunteers
		FROM leafs lf
		LEFT JOIN credit_ledger cl ON cl.leaf_id = lf.id
		LEFT JOIN results r ON r.work_unit_id IN (SELECT id FROM work_units WHERE leaf_id = lf.id) AND r.validation_status = 'AGREED'
		WHERE lf.state = 'ACTIVE'
		GROUP BY lf.id, lf.name`)
	if err != nil {
		l.Error("failed to compute cross-leaf analysis", "error", err)
		apierror.WriteError(w, apierror.Internal("failed to compute analysis", err))
		return
	}
	defer rows.Close()

	entries := make([]crossLeafEntry, 0)
	var maxCPUPerCredit, minCPUPerCredit float64
	minCPUPerCredit = -1

	for rows.Next() {
		var e crossLeafEntry
		var totalCredit, avgCPU, avgGPU float64
		if scanErr := rows.Scan(&e.LeafID, &e.LeafName, &totalCredit, &avgCPU, &avgGPU, &e.ActiveVolunteers); scanErr != nil {
			continue
		}
		e.TotalCreditGranted = totalCredit

		if totalCredit > 0 {
			e.AvgCPUSecondsPerCredit = avgCPU
			e.AvgGPUSecondsPerCredit = avgGPU

			if avgCPU > maxCPUPerCredit {
				maxCPUPerCredit = avgCPU
			}
			if minCPUPerCredit < 0 || avgCPU < minCPUPerCredit {
				minCPUPerCredit = avgCPU
			}
		}

		entries = append(entries, e)
	}

	if minCPUPerCredit < 0 {
		minCPUPerCredit = 0
	}

	var ratio float64
	if minCPUPerCredit > 0 {
		ratio = maxCPUPerCredit / minCPUPerCredit
	}

	resp := crossLeafResponse{
		Leafs: entries,
		NormalizationFactors: normalizationFactors{
			MaxCPUSecondsPerCredit: maxCPUPerCredit,
			MinCPUSecondsPerCredit: minCPUPerCredit,
			Ratio:                  ratio,
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *AnalysisHandler) HandleVolunteerBreakdown(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	volunteerID, err := types.ParseID(r.PathValue("id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid volunteer id", nil))
		return
	}

	bd, err := ComputeVolunteerBreakdown(r.Context(), h.pool, volunteerID)
	if err != nil {
		l.Error("failed to compute volunteer credit breakdown", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.Internal("failed to compute breakdown", err))
		return
	}

	writeJSON(w, http.StatusOK, bd)
}

