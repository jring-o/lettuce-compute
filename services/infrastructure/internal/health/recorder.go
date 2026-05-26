package health

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/stats"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Recorder periodically computes and stores operator health metrics for all active leafs.
type Recorder struct {
	pool        *pgxpool.Pool
	statsEngine *stats.Engine
	leafRepo    leaf.Repository
	logger      *slog.Logger
}

// NewRecorder creates a new Recorder.
func NewRecorder(pool *pgxpool.Pool, statsEngine *stats.Engine, leafRepo leaf.Repository, logger *slog.Logger) *Recorder {
	return &Recorder{
		pool:        pool,
		statsEngine: statsEngine,
		leafRepo:    leafRepo,
		logger:      logger,
	}
}

// Start runs the recorder loop: records hourly, cleans up records older than 90 days.
// Returns when ctx is cancelled.
func (r *Recorder) Start(ctx context.Context) {
	r.logger.Info("health metrics recorder starting")

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("health metrics recorder stopping")
			return
		case <-ticker.C:
			r.record(ctx)
			r.cleanup(ctx)
		}
	}
}

func (r *Recorder) record(ctx context.Context) {
	leafs, err := listActiveLeafs(ctx, r.leafRepo)
	if err != nil {
		r.logger.Error("health recorder: failed to list leafs", "error", err)
		return
	}

	now := types.Now()
	var total int

	for _, lf := range leafs {
		metrics := r.computeMetrics(ctx, lf, now)
		for name, value := range metrics {
			_, err := r.pool.Exec(ctx,
				`INSERT INTO health_metrics_history (leaf_id, metric_name, metric_value, recorded_at)
				 VALUES ($1, $2, $3, $4)`,
				lf.ID, name, value, now,
			)
			if err != nil {
				r.logger.Error("health recorder: failed to insert metric",
					"error", err, "leaf_id", lf.ID, "metric", name)
			} else {
				total++
			}
		}
	}

	if total > 0 {
		r.logger.Info("health metrics recorded", "metrics_count", total, "leafs", len(leafs))
	}
}

func (r *Recorder) computeMetrics(ctx context.Context, lf *leaf.Leaf, now time.Time) map[string]float64 {
	metrics := make(map[string]float64)

	// Contribution flow: hours since last validated credit.
	var lastGrantedAt *time.Time
	err := r.pool.QueryRow(ctx,
		"SELECT MAX(granted_at) FROM credit_ledger WHERE leaf_id = $1",
		lf.ID,
	).Scan(&lastGrantedAt)
	if err == nil && lastGrantedAt != nil {
		metrics["contribution_flow_hours"] = math.Round(now.Sub(*lastGrantedAt).Hours()*100) / 100
	} else {
		metrics["contribution_flow_hours"] = 0
	}

	// Work availability: 7-day / 40-day ratio.
	sevenDayMean := meanValidatedWorkUnits(ctx, r.pool, lf.ID, now.Add(-7*24*time.Hour), now)
	fortyDayMean := meanValidatedWorkUnits(ctx, r.pool, lf.ID, now.Add(-40*24*time.Hour), now)
	var ratio float64
	if fortyDayMean > 0 {
		ratio = sevenDayMean / fortyDayMean
	}
	metrics["work_availability_ratio"] = math.Round(ratio*1000) / 1000

	// Volunteer activity: active count in last 24h.
	var count int
	err = r.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT cl.volunteer_id)
		FROM credit_ledger cl
		JOIN volunteers v ON v.id = cl.volunteer_id
		WHERE cl.leaf_id = $1
		  AND cl.granted_at >= $2
		  AND v.is_active = true`,
		lf.ID, now.Add(-24*time.Hour),
	).Scan(&count)
	if err != nil && err != pgx.ErrNoRows {
		count = 0
	}
	metrics["volunteer_activity_count"] = float64(count)

	return metrics
}

func (r *Recorder) cleanup(ctx context.Context) {
	tag, err := r.pool.Exec(ctx,
		"DELETE FROM health_metrics_history WHERE recorded_at < $1",
		time.Now().UTC().Add(-90*24*time.Hour),
	)
	if err != nil {
		r.logger.Error("health recorder: failed to cleanup old metrics", "error", err)
		return
	}
	if tag.RowsAffected() > 0 {
		r.logger.Info("health recorder: cleaned up old metrics", "count", tag.RowsAffected())
	}
}
