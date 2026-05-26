package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Trend represents a metric's direction over 24h.
type Trend string

const (
	TrendImproving Trend = "improving"
	TrendDeclining Trend = "declining"
	TrendStable    Trend = "stable"
)

// Status represents the health status of a metric.
type Status string

const (
	StatusHealthy  Status = "healthy"
	StatusWarning  Status = "warning"
	StatusCritical Status = "critical"
)

// metricDetailJSON is the enhanced per-metric response.
type metricDetailJSON struct {
	Value          float64 `json:"value"`
	Unit           string  `json:"unit"`
	Status         Status  `json:"status"`
	AlertThreshold float64 `json:"alert_threshold"`
	Trend24h       Trend   `json:"trend_24h"`
	Avg7d          float64 `json:"avg_7d"`
	Avg30d         float64 `json:"avg_30d"`
}

// leafHealthJSON is the per-leaf health entry.
type leafHealthJSON struct {
	LeafID            types.ID         `json:"leaf_id"`
	LeafName          string           `json:"leaf_name"`
	ContributionFlow  metricDetailJSON `json:"contribution_flow"`
	WorkAvailability  metricDetailJSON `json:"work_availability"`
	VolunteerActivity metricDetailJSON `json:"volunteer_activity"`
}

// healthResponse is the JSON response for GET /api/v1/health/leafs.
type healthResponse struct {
	HeadName          string           `json:"head_name"`
	ContributionFlow  metricDetailJSON `json:"contribution_flow"`
	WorkAvailability  metricDetailJSON `json:"work_availability"`
	VolunteerActivity metricDetailJSON `json:"volunteer_activity"`
	OverallStatus     Status           `json:"overall_status"`
	LeafCount         int              `json:"leaf_count"`
	Leafs             []leafHealthJSON `json:"leafs"`
	RecordedAt        string           `json:"recorded_at"`
}

// Handler serves operator health metrics requests.
type Handler struct {
	pool        *pgxpool.Pool
	statsEngine *stats.Engine
	leafRepo    leaf.Repository
	logger      *slog.Logger
	headName    string
}

// NewHandler creates a new Handler.
func NewHandler(
	pool *pgxpool.Pool,
	statsEngine *stats.Engine,
	leafRepo leaf.Repository,
	logger *slog.Logger,
	headName string,
) *Handler {
	return &Handler{
		pool:        pool,
		statsEngine: statsEngine,
		leafRepo:    leafRepo,
		logger:      logger,
		headName:    headName,
	}
}

// RegisterRoutes registers health routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/health/leafs", h.handleHealth)
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	leafs, err := listActiveLeafs(r.Context(), h.leafRepo)
	if err != nil {
		l.Error("failed to list leafs for health", "error", err)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Build per-leaf health details.
	leafDetails := make([]leafHealthJSON, 0, len(leafs))
	for _, lf := range leafs {
		lh, buildErr := h.buildLeafHealth(r.Context(), lf)
		if buildErr != nil {
			l.Error("failed to build leaf health", "error", buildErr, "leaf_id", lf.ID)
			continue
		}
		leafDetails = append(leafDetails, *lh)
	}

	// Compute head-level aggregated metrics.
	now := types.Now()
	hc := leaf.DefaultHealthConfig()
	headCF := h.computeHeadContributionFlow(r.Context(), now, hc)
	headWA := h.computeHeadWorkAvailability(r.Context(), now, hc)
	headVA := h.computeHeadVolunteerActivity(r.Context(), now, hc)

	// Determine overall status from head-level metrics.
	worstStatus := StatusHealthy
	for _, s := range []Status{headCF.Status, headWA.Status, headVA.Status} {
		if s == StatusCritical {
			worstStatus = StatusCritical
		} else if s == StatusWarning && worstStatus == StatusHealthy {
			worstStatus = StatusWarning
		}
	}

	resp := healthResponse{
		HeadName:          h.headName,
		ContributionFlow:  headCF,
		WorkAvailability:  headWA,
		VolunteerActivity: headVA,
		OverallStatus:     worstStatus,
		LeafCount:         len(leafs),
		Leafs:             leafDetails,
		RecordedAt:        types.FormatTimestamp(types.Now()),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) buildLeafHealth(ctx context.Context, lf *leaf.Leaf) (*leafHealthJSON, error) {
	now := types.Now()
	hc := lf.HealthConfig
	if hc.ContributionFlowAlertHours == 0 {
		hc = leaf.DefaultHealthConfig()
	}

	cf := h.computeContributionFlow(ctx, lf.ID, now, hc)
	wa := h.computeWorkAvailability(ctx, lf.ID, now, hc)
	va := h.computeVolunteerActivity(ctx, lf.ID, now, hc)

	return &leafHealthJSON{
		LeafID:            lf.ID,
		LeafName:          lf.Name,
		ContributionFlow:  cf,
		WorkAvailability:  wa,
		VolunteerActivity: va,
	}, nil
}

// listActiveLeafs returns all ACTIVE leafs across the head.
func listActiveLeafs(ctx context.Context, repo leaf.Repository) ([]*leaf.Leaf, error) {
	state := leaf.StateActive
	filters := leaf.LeafListFilters{State: &state}
	var all []*leaf.Leaf
	page := types.PaginationRequest{PageSize: 200}

	for {
		leafs, pagination, err := repo.List(ctx, filters, page)
		if err != nil {
			return nil, apierror.Internal("failed to list leafs", err)
		}
		all = append(all, leafs...)
		if !pagination.HasMore {
			break
		}
		page.Cursor = pagination.NextCursor
	}
	return all, nil
}

// --- Head-level aggregated metric computations ---

// computeHeadContributionFlow: hours since last validation across ALL leafs (minimum hours = most recent).
func (h *Handler) computeHeadContributionFlow(ctx context.Context, now time.Time, hc leaf.HealthConfig) metricDetailJSON {
	var lastGrantedAt *time.Time
	if h.pool != nil {
		_ = h.pool.QueryRow(ctx,
			"SELECT MAX(granted_at) FROM credit_ledger",
		).Scan(&lastGrantedAt)
	}

	var hours float64
	if lastGrantedAt == nil {
		hours = 0
	} else {
		hours = math.Round(now.Sub(*lastGrantedAt).Hours()*100) / 100
	}

	threshold := float64(hc.ContributionFlowAlertHours)

	return metricDetailJSON{
		Value:          hours,
		Unit:           "hours_since_last_validation",
		Status:         statusForLowerIsBetter(hours, threshold),
		AlertThreshold: threshold,
		Trend24h:       TrendStable,
		Avg7d:          0,
		Avg30d:         0,
	}
}

// computeHeadWorkAvailability: aggregate 7d/40d ratio across all leafs.
func (h *Handler) computeHeadWorkAvailability(ctx context.Context, now time.Time, hc leaf.HealthConfig) metricDetailJSON {
	sevenDayMean := meanValidatedWorkUnitsAll(ctx, h.pool, now.Add(-7*24*time.Hour), now)
	fortyDayMean := meanValidatedWorkUnitsAll(ctx, h.pool, now.Add(-40*24*time.Hour), now)

	var score float64
	if fortyDayMean > 0 {
		score = sevenDayMean / fortyDayMean
	}
	score = math.Round(score*1000) / 1000

	threshold := hc.WorkAvailabilityAlertRatio

	return metricDetailJSON{
		Value:          score,
		Unit:           "7d_40d_ratio",
		Status:         statusForHigherIsBetter(score, threshold),
		AlertThreshold: threshold,
		Trend24h:       TrendStable,
		Avg7d:          0,
		Avg30d:         0,
	}
}

// computeHeadVolunteerActivity: unique active volunteers across all leafs in last 24h.
func (h *Handler) computeHeadVolunteerActivity(ctx context.Context, now time.Time, hc leaf.HealthConfig) metricDetailJSON {
	count := countActiveVolunteers(ctx, h.pool, now)

	threshold := float64(hc.VolunteerActivityAlertCount)

	return metricDetailJSON{
		Value:          float64(count),
		Unit:           "active_24h",
		Status:         statusForHigherIsBetter(float64(count), threshold),
		AlertThreshold: threshold,
		Trend24h:       TrendStable,
		Avg7d:          0,
		Avg30d:         0,
	}
}

// --- Per-leaf metric computations ---

func (h *Handler) computeContributionFlow(ctx context.Context, leafID types.ID, now time.Time, hc leaf.HealthConfig) metricDetailJSON {
	var lastGrantedAt *time.Time
	if h.pool != nil {
		_ = h.pool.QueryRow(ctx,
			"SELECT MAX(granted_at) FROM credit_ledger WHERE leaf_id = $1",
			leafID,
		).Scan(&lastGrantedAt)
	}

	var hours float64
	if lastGrantedAt == nil {
		hours = 0
	} else {
		hours = math.Round(now.Sub(*lastGrantedAt).Hours()*100) / 100
	}

	threshold := float64(hc.ContributionFlowAlertHours)
	avg7d := h.historyAvg(ctx, leafID, "contribution_flow_hours", now, 7)
	avg30d := h.historyAvg(ctx, leafID, "contribution_flow_hours", now, 30)
	trend := h.computeTrend(ctx, leafID, "contribution_flow_hours", hours, true, now)

	return metricDetailJSON{
		Value:          hours,
		Unit:           "hours_since_last_validation",
		Status:         statusForLowerIsBetter(hours, threshold),
		AlertThreshold: threshold,
		Trend24h:       trend,
		Avg7d:          avg7d,
		Avg30d:         avg30d,
	}
}

func (h *Handler) computeWorkAvailability(ctx context.Context, leafID types.ID, now time.Time, hc leaf.HealthConfig) metricDetailJSON {
	sevenDayMean := meanValidatedWorkUnits(ctx, h.pool, leafID, now.Add(-7*24*time.Hour), now)
	fortyDayMean := meanValidatedWorkUnits(ctx, h.pool, leafID, now.Add(-40*24*time.Hour), now)

	var score float64
	if fortyDayMean > 0 {
		score = sevenDayMean / fortyDayMean
	}
	score = math.Round(score*1000) / 1000

	threshold := hc.WorkAvailabilityAlertRatio
	avg7d := h.historyAvg(ctx, leafID, "work_availability_ratio", now, 7)
	avg30d := h.historyAvg(ctx, leafID, "work_availability_ratio", now, 30)
	trend := h.computeTrend(ctx, leafID, "work_availability_ratio", score, false, now)

	return metricDetailJSON{
		Value:          score,
		Unit:           "7d_40d_ratio",
		Status:         statusForHigherIsBetter(score, threshold),
		AlertThreshold: threshold,
		Trend24h:       trend,
		Avg7d:          avg7d,
		Avg30d:         avg30d,
	}
}

func (h *Handler) computeVolunteerActivity(ctx context.Context, leafID types.ID, now time.Time, hc leaf.HealthConfig) metricDetailJSON {
	var count int
	if h.pool != nil {
		err := h.pool.QueryRow(ctx, `
			SELECT COUNT(DISTINCT cl.volunteer_id)
			FROM credit_ledger cl
			JOIN volunteers v ON v.id = cl.volunteer_id
			WHERE cl.leaf_id = $1
			  AND cl.granted_at >= $2
			  AND v.is_active = true`,
			leafID, now.Add(-24*time.Hour),
		).Scan(&count)

		if err != nil && err != pgx.ErrNoRows {
			count = 0
		}
	}

	threshold := float64(hc.VolunteerActivityAlertCount)
	avg7d := h.historyAvg(ctx, leafID, "volunteer_activity_count", now, 7)
	avg30d := h.historyAvg(ctx, leafID, "volunteer_activity_count", now, 30)
	trend := h.computeTrend(ctx, leafID, "volunteer_activity_count", float64(count), false, now)

	return metricDetailJSON{
		Value:          float64(count),
		Unit:           "active_24h",
		Status:         statusForHigherIsBetter(float64(count), threshold),
		AlertThreshold: threshold,
		Trend24h:       trend,
		Avg7d:          avg7d,
		Avg30d:         avg30d,
	}
}

// countActiveVolunteers returns the number of unique active volunteers with credit in the last 24h.
func countActiveVolunteers(ctx context.Context, pool *pgxpool.Pool, now time.Time) int {
	if pool == nil {
		return 0
	}
	var count int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(DISTINCT cl.volunteer_id)
		 FROM credit_ledger cl
		 JOIN volunteers v ON v.id = cl.volunteer_id
		 WHERE cl.granted_at >= $1 AND v.is_active = true`,
		now.Add(-24*time.Hour),
	).Scan(&count)
	return count
}

// meanValidatedWorkUnits returns the average of work_units_validated across snapshots for a specific leaf.
func meanValidatedWorkUnits(ctx context.Context, pool *pgxpool.Pool, leafID types.ID, from, to time.Time) float64 {
	if pool == nil {
		return 0
	}
	var avg *float64
	err := pool.QueryRow(ctx,
		"SELECT AVG(work_units_validated) FROM leaf_stats_snapshots WHERE leaf_id = $1 AND snapshot_at >= $2 AND snapshot_at <= $3",
		leafID, from, to,
	).Scan(&avg)
	if err != nil || avg == nil {
		return 0
	}
	return *avg
}

// meanValidatedWorkUnitsAll returns the average of work_units_validated across all leafs.
func meanValidatedWorkUnitsAll(ctx context.Context, pool *pgxpool.Pool, from, to time.Time) float64 {
	if pool == nil {
		return 0
	}
	var avg *float64
	err := pool.QueryRow(ctx,
		"SELECT AVG(work_units_validated) FROM leaf_stats_snapshots WHERE snapshot_at >= $1 AND snapshot_at <= $2",
		from, to,
	).Scan(&avg)
	if err != nil || avg == nil {
		return 0
	}
	return *avg
}

// historyAvg returns the average metric value from health_metrics_history for the last N days.
func (h *Handler) historyAvg(ctx context.Context, leafID types.ID, metricName string, now time.Time, days int) float64 {
	if h.pool == nil {
		return 0
	}
	var avg *float64
	err := h.pool.QueryRow(ctx,
		`SELECT AVG(metric_value) FROM health_metrics_history
		 WHERE leaf_id = $1 AND metric_name = $2 AND recorded_at >= $3`,
		leafID, metricName, now.Add(-time.Duration(days)*24*time.Hour),
	).Scan(&avg)
	if err != nil || avg == nil {
		return 0
	}
	return math.Round(*avg*100) / 100
}

// computeTrend compares current value to the value recorded ~24h ago.
func (h *Handler) computeTrend(ctx context.Context, leafID types.ID, metricName string, current float64, lowerIsBetter bool, now time.Time) Trend {
	if h.pool == nil {
		return TrendStable
	}
	var prev *float64
	err := h.pool.QueryRow(ctx,
		`SELECT metric_value FROM health_metrics_history
		 WHERE leaf_id = $1 AND metric_name = $2 AND recorded_at <= $3
		 ORDER BY recorded_at DESC LIMIT 1`,
		leafID, metricName, now.Add(-23*time.Hour),
	).Scan(&prev)

	if err != nil || prev == nil {
		return TrendStable
	}

	if *prev == 0 {
		return TrendStable
	}

	ratio := math.Abs(current-*prev) / math.Abs(*prev)
	if ratio < 0.1 {
		return TrendStable
	}

	if lowerIsBetter {
		if current < *prev {
			return TrendImproving
		}
		return TrendDeclining
	}

	if current > *prev {
		return TrendImproving
	}
	return TrendDeclining
}

// statusForLowerIsBetter determines health status where lower values are better.
func statusForLowerIsBetter(value, threshold float64) Status {
	if value >= threshold {
		return StatusCritical
	}
	if threshold > 0 && value >= threshold*0.8 {
		return StatusWarning
	}
	return StatusHealthy
}

// statusForHigherIsBetter determines health status where higher values are better.
func statusForHigherIsBetter(value, threshold float64) Status {
	if value <= threshold {
		return StatusCritical
	}
	if threshold >= 0 && value <= threshold*1.2+0.01 {
		return StatusWarning
	}
	return StatusHealthy
}
