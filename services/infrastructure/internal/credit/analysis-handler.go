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

type volunteerLeafCredit struct {
	LeafID   types.ID `json:"leaf_id"`
	LeafName string   `json:"leaf_name"`
	Credit      float64  `json:"credit"`
	WorkUnits   int      `json:"work_units"`
	CPUSeconds  float64  `json:"cpu_seconds"`
	GPUSeconds  float64  `json:"gpu_seconds"`
}

type resourceTypeBreakdown struct {
	Credit    float64 `json:"credit"`
	WorkUnits int     `json:"work_units"`
}

type dailyCredit struct {
	Date   string  `json:"date"`
	Credit float64 `json:"credit"`
}

type weeklyCredit struct {
	WeekStart string  `json:"week_start"`
	Credit    float64 `json:"credit"`
}

type volunteerBreakdownResponse struct {
	VolunteerID    types.ID                          `json:"volunteer_id"`
	TotalCredit    float64                           `json:"total_credit"`
	ByLeaf      []volunteerLeafCredit          `json:"by_leaf"`
	ByResourceType map[string]resourceTypeBreakdown  `json:"by_resource_type"`
	Timeline       struct {
		Daily  []dailyCredit  `json:"daily"`
		Weekly []weeklyCredit `json:"weekly"`
	} `json:"timeline"`
}

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

	// Per-leaf credit + resource usage.
	rows, err := h.pool.Query(r.Context(), `
		SELECT
			l.id, l.name,
			COALESCE(SUM(cl.credit_amount), 0),
			COUNT(cl.id),
			COALESCE(SUM((r.execution_metadata->>'cpu_seconds_user')::float), 0),
			COALESCE(SUM((r.execution_metadata->>'gpu_seconds')::float), 0)
		FROM credit_ledger cl
		JOIN leafs l ON l.id = cl.leaf_id
		LEFT JOIN results r ON r.id = cl.result_id AND r.validation_status = 'AGREED'
		WHERE cl.volunteer_id = $1
		GROUP BY l.id, l.name`,
		volunteerID,
	)
	if err != nil {
		l.Error("failed to get volunteer credit breakdown", "error", err)
		apierror.WriteError(w, apierror.Internal("failed to compute breakdown", err))
		return
	}
	defer rows.Close()

	var totalCredit float64
	byLeaf := make([]volunteerLeafCredit, 0)
	cpuOnlyCredit, cpuOnlyWU := 0.0, 0
	gpuCredit, gpuWU := 0.0, 0

	for rows.Next() {
		var vpc volunteerLeafCredit
		if scanErr := rows.Scan(&vpc.LeafID, &vpc.LeafName, &vpc.Credit, &vpc.WorkUnits, &vpc.CPUSeconds, &vpc.GPUSeconds); scanErr != nil {
			continue
		}
		totalCredit += vpc.Credit

		if vpc.GPUSeconds > 0 {
			gpuCredit += vpc.Credit
			gpuWU += vpc.WorkUnits
		} else {
			cpuOnlyCredit += vpc.Credit
			cpuOnlyWU += vpc.WorkUnits
		}

		byLeaf = append(byLeaf, vpc)
	}

	// Daily credit timeline (last 30 days). DATE(granted_at) is a Postgres date;
	// cast it to text so it scans cleanly into the Go string field. pgx cannot scan
	// a date straight into a *string, so without the cast every Scan here failed —
	// and because the error was swallowed (only appending on scanErr == nil) the
	// timeline came back silently empty. Surface query/scan errors instead.
	dailyTimeline := make([]dailyCredit, 0)
	dayRows, err := h.pool.Query(r.Context(), `
		SELECT DATE(granted_at)::text AS day, SUM(credit_amount)
		FROM credit_ledger
		WHERE volunteer_id = $1 AND granted_at >= NOW() - INTERVAL '30 days'
		GROUP BY day ORDER BY day`,
		volunteerID,
	)
	if err != nil {
		l.Error("failed to query daily credit timeline", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.Internal("failed to compute breakdown", err))
		return
	}
	defer dayRows.Close()
	for dayRows.Next() {
		var dc dailyCredit
		var credit float64
		if scanErr := dayRows.Scan(&dc.Date, &credit); scanErr != nil {
			l.Error("failed to scan daily credit row", "error", scanErr, "volunteer_id", volunteerID)
			apierror.WriteError(w, apierror.Internal("failed to compute breakdown", scanErr))
			return
		}
		dc.Credit = credit
		dailyTimeline = append(dailyTimeline, dc)
	}
	if err := dayRows.Err(); err != nil {
		l.Error("failed to iterate daily credit rows", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.Internal("failed to compute breakdown", err))
		return
	}

	// Weekly credit timeline (last 12 weeks). DATE_TRUNC returns a timestamptz;
	// cast it to date then text (YYYY-MM-DD week start) for the same reason as the
	// daily timeline above — pgx cannot scan a timestamptz into a *string, so the
	// uncast query failed every Scan and the swallowed error left this empty.
	weeklyTimeline := make([]weeklyCredit, 0)
	weekRows, err := h.pool.Query(r.Context(), `
		SELECT DATE_TRUNC('week', granted_at)::date::text AS week_start, SUM(credit_amount)
		FROM credit_ledger
		WHERE volunteer_id = $1 AND granted_at >= NOW() - INTERVAL '12 weeks'
		GROUP BY week_start ORDER BY week_start`,
		volunteerID,
	)
	if err != nil {
		l.Error("failed to query weekly credit timeline", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.Internal("failed to compute breakdown", err))
		return
	}
	defer weekRows.Close()
	for weekRows.Next() {
		var wc weeklyCredit
		var credit float64
		if scanErr := weekRows.Scan(&wc.WeekStart, &credit); scanErr != nil {
			l.Error("failed to scan weekly credit row", "error", scanErr, "volunteer_id", volunteerID)
			apierror.WriteError(w, apierror.Internal("failed to compute breakdown", scanErr))
			return
		}
		wc.Credit = credit
		weeklyTimeline = append(weeklyTimeline, wc)
	}
	if err := weekRows.Err(); err != nil {
		l.Error("failed to iterate weekly credit rows", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.Internal("failed to compute breakdown", err))
		return
	}

	resp := volunteerBreakdownResponse{
		VolunteerID: volunteerID,
		TotalCredit: totalCredit,
		ByLeaf:   byLeaf,
		ByResourceType: map[string]resourceTypeBreakdown{
			"cpu_only": {Credit: cpuOnlyCredit, WorkUnits: cpuOnlyWU},
			"gpu":      {Credit: gpuCredit, WorkUnits: gpuWU},
		},
	}
	resp.Timeline.Daily = dailyTimeline
	resp.Timeline.Weekly = weeklyTimeline

	writeJSON(w, http.StatusOK, resp)
}

