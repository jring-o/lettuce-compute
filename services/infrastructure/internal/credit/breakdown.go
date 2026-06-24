package credit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// LeafCredit is a volunteer's credit and resource usage on a single leaf.
type LeafCredit struct {
	LeafID     types.ID `json:"leaf_id"`
	LeafName   string   `json:"leaf_name"`
	Credit     float64  `json:"credit"`
	WorkUnits  int      `json:"work_units"`
	CPUSeconds float64  `json:"cpu_seconds"`
	GPUSeconds float64  `json:"gpu_seconds"`
}

// ResourceTypeCredit aggregates credit and work units for one resource class
// (cpu_only / gpu).
type ResourceTypeCredit struct {
	Credit    float64 `json:"credit"`
	WorkUnits int     `json:"work_units"`
}

// DailyCredit is one calendar day's credit total.
type DailyCredit struct {
	Date   string  `json:"date"`
	Credit float64 `json:"credit"`
}

// WeeklyCredit is one week's credit total, keyed by the week-start date.
type WeeklyCredit struct {
	WeekStart string  `json:"week_start"`
	Credit    float64 `json:"credit"`
}

// CreditTimeline holds the daily (last 30 days) and weekly (last 12 weeks)
// credit series.
type CreditTimeline struct {
	Daily  []DailyCredit  `json:"daily"`
	Weekly []WeeklyCredit `json:"weekly"`
}

// VolunteerBreakdown is a volunteer ACCOUNT's full credit breakdown across every
// leaf and every machine. Credit is keyed to the account (the Ed25519 identity
// key), not the host (the account<->host split, TODO #19), so this already
// aggregates a volunteer's machines into one account-wide picture.
//
// It is the single shared definition consumed by both the operator REST endpoint
// (GET /api/v1/volunteers/{id}/credit/breakdown) and the volunteer self-service
// gRPC RPC (GetMyContribution), so the two surfaces cannot drift apart. The JSON
// tags are the REST wire format.
type VolunteerBreakdown struct {
	VolunteerID    types.ID                      `json:"volunteer_id"`
	TotalCredit    float64                       `json:"total_credit"`
	ByLeaf         []LeafCredit                  `json:"by_leaf"`
	ByResourceType map[string]ResourceTypeCredit `json:"by_resource_type"`
	Timeline       CreditTimeline                `json:"timeline"`
}

// ComputeVolunteerBreakdown sums a volunteer's credit_ledger into a full
// breakdown: per-leaf credit + resource usage, a cpu_only/gpu split, and daily
// (30-day) / weekly (12-week) timelines.
//
// The timeline queries cast the Postgres date / week-start to text in SQL
// (DATE(...)::text, DATE_TRUNC('week',...)::date::text): pgx cannot scan a date
// or timestamptz straight into a Go string, so an uncast query fails every row
// scan. Errors are returned, never swallowed.
func ComputeVolunteerBreakdown(ctx context.Context, pool *pgxpool.Pool, volunteerID types.ID) (*VolunteerBreakdown, error) {
	bd := &VolunteerBreakdown{
		VolunteerID: volunteerID,
		ByLeaf:      make([]LeafCredit, 0),
	}

	// Per-leaf credit + resource usage. cpu/gpu seconds come from the AGREED
	// result tied to each credit row.
	rows, err := pool.Query(ctx, `
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
		return nil, fmt.Errorf("query per-leaf credit: %w", err)
	}
	defer rows.Close()

	cpuOnlyCredit, cpuOnlyWU := 0.0, 0
	gpuCredit, gpuWU := 0.0, 0

	for rows.Next() {
		var lc LeafCredit
		if scanErr := rows.Scan(&lc.LeafID, &lc.LeafName, &lc.Credit, &lc.WorkUnits, &lc.CPUSeconds, &lc.GPUSeconds); scanErr != nil {
			return nil, fmt.Errorf("scan per-leaf credit: %w", scanErr)
		}
		bd.TotalCredit += lc.Credit

		if lc.GPUSeconds > 0 {
			gpuCredit += lc.Credit
			gpuWU += lc.WorkUnits
		} else {
			cpuOnlyCredit += lc.Credit
			cpuOnlyWU += lc.WorkUnits
		}

		bd.ByLeaf = append(bd.ByLeaf, lc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate per-leaf credit: %w", err)
	}

	bd.ByResourceType = map[string]ResourceTypeCredit{
		"cpu_only": {Credit: cpuOnlyCredit, WorkUnits: cpuOnlyWU},
		"gpu":      {Credit: gpuCredit, WorkUnits: gpuWU},
	}

	// Daily timeline (last 30 days).
	bd.Timeline.Daily = make([]DailyCredit, 0)
	dayRows, err := pool.Query(ctx, `
		SELECT DATE(granted_at)::text AS day, SUM(credit_amount)
		FROM credit_ledger
		WHERE volunteer_id = $1 AND granted_at >= NOW() - INTERVAL '30 days'
		GROUP BY day ORDER BY day`,
		volunteerID,
	)
	if err != nil {
		return nil, fmt.Errorf("query daily timeline: %w", err)
	}
	defer dayRows.Close()
	for dayRows.Next() {
		var dc DailyCredit
		if scanErr := dayRows.Scan(&dc.Date, &dc.Credit); scanErr != nil {
			return nil, fmt.Errorf("scan daily timeline: %w", scanErr)
		}
		bd.Timeline.Daily = append(bd.Timeline.Daily, dc)
	}
	if err := dayRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daily timeline: %w", err)
	}

	// Weekly timeline (last 12 weeks).
	bd.Timeline.Weekly = make([]WeeklyCredit, 0)
	weekRows, err := pool.Query(ctx, `
		SELECT DATE_TRUNC('week', granted_at)::date::text AS week_start, SUM(credit_amount)
		FROM credit_ledger
		WHERE volunteer_id = $1 AND granted_at >= NOW() - INTERVAL '12 weeks'
		GROUP BY week_start ORDER BY week_start`,
		volunteerID,
	)
	if err != nil {
		return nil, fmt.Errorf("query weekly timeline: %w", err)
	}
	defer weekRows.Close()
	for weekRows.Next() {
		var wc WeeklyCredit
		if scanErr := weekRows.Scan(&wc.WeekStart, &wc.Credit); scanErr != nil {
			return nil, fmt.Errorf("scan weekly timeline: %w", scanErr)
		}
		bd.Timeline.Weekly = append(bd.Timeline.Weekly, wc)
	}
	if err := weekRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate weekly timeline: %w", err)
	}

	return bd, nil
}
