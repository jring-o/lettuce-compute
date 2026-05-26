package stats

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// LeafStatsSnapshot captures point-in-time aggregate statistics
// for a leaf's work unit distribution.
type LeafStatsSnapshot struct {
	ID                   types.ID `json:"id"`
	LeafID               types.ID `json:"leaf_id"`
	SnapshotAt           time.Time `json:"snapshot_at"`
	TotalWorkUnits       int      `json:"total_work_units"`
	WorkUnitsQueued      int      `json:"work_units_queued"`
	WorkUnitsAssigned    int      `json:"work_units_assigned"`
	WorkUnitsRunning     int      `json:"work_units_running"`
	WorkUnitsCompleted   int      `json:"work_units_completed"`
	WorkUnitsValidated   int      `json:"work_units_validated"`
	WorkUnitsFailed      int      `json:"work_units_failed"`
	ActiveVolunteers     int      `json:"active_volunteers"`
	TotalCreditGranted   float64  `json:"total_credit_granted"`
	AvgCompletionSeconds *float64 `json:"avg_completion_seconds"`
	AgreementRate        *float64 `json:"agreement_rate"`
	ThroughputPerHour    *float64 `json:"throughput_per_hour"`
	SpotChecksTotal      int      `json:"spot_checks_total"`
	SpotChecksPassed     int      `json:"spot_checks_passed"`
	SpotChecksFailed     int      `json:"spot_checks_failed"`
	SpotCheckPassRate    *float64 `json:"spot_check_pass_rate"`
	CreatedAt            time.Time `json:"created_at"`
}

// StatsHistoryFilters controls time-range and downsampling for history queries.
type StatsHistoryFilters struct {
	From     time.Time
	To       time.Time
	Interval string // "raw", "hourly", "daily"
}
