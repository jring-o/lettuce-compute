package credit

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// LeafWorkStatus counts one account's results on one leaf by validation state, and
// its copies of that leaf's work units by progress.
type LeafWorkStatus struct {
	LeafID   types.ID
	LeafName string

	ResultsPending                     int
	ResultsAgreed                      int
	ResultsDisagreed                   int
	ResultsAwaitingContentVerification int
	ResultsContentVerificationFailed   int
	ResultsSuperseded                  int

	// RunsStopped counts copies closed SUPERSEDED after they started: the unit
	// validated while the copy ran, and the late result was refused, so the run left
	// no result row and must be counted from the copy history.
	RunsStopped          int
	CopiesRunning        int
	CopiesWaitingToStart int
}

// ComputeVolunteerWorkStatus counts an account's results by validation state and
// its copies by progress, per leaf, ordered by leaf name.
//
// It serves only the account's own authenticated view (GetMyContribution), so it
// keeps every leaf the account worked on, PRIVATE ones included, and it is kept
// apart from VolunteerBreakdown, which the public per-account stats also read.
// Both queries are keyed by volunteer_id, which the per-volunteer indexes on
// results and work_unit_assignment_history cover.
func ComputeVolunteerWorkStatus(ctx context.Context, pool *pgxpool.Pool, volunteerID types.ID) ([]LeafWorkStatus, error) {
	byLeaf := make(map[types.ID]*LeafWorkStatus)
	entry := func(id types.ID, name string) *LeafWorkStatus {
		ls, ok := byLeaf[id]
		if !ok {
			ls = &LeafWorkStatus{LeafID: id, LeafName: name}
			byLeaf[id] = ls
		}
		return ls
	}

	// Results carry no leaf id, so the leaf comes through the work unit.
	rows, err := pool.Query(ctx, `
		SELECT l.id, l.name, r.validation_status::text, COUNT(*)
		FROM results r
		JOIN work_units wu ON wu.id = r.work_unit_id
		JOIN leafs l ON l.id = wu.leaf_id
		WHERE r.volunteer_id = $1
		GROUP BY l.id, l.name, r.validation_status`,
		volunteerID,
	)
	if err != nil {
		return nil, fmt.Errorf("query results by state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			leafID   types.ID
			leafName string
			state    string
			n        int
		)
		if scanErr := rows.Scan(&leafID, &leafName, &state, &n); scanErr != nil {
			return nil, fmt.Errorf("scan results by state: %w", scanErr)
		}
		ls := entry(leafID, leafName)
		switch state {
		case "PENDING":
			ls.ResultsPending = n
		case "AGREED":
			ls.ResultsAgreed = n
		case "DISAGREED":
			ls.ResultsDisagreed = n
		case "AWAITING_CONTENT_VERIFICATION":
			ls.ResultsAwaitingContentVerification = n
		case "CONTENT_VERIFICATION_FAILED":
			ls.ResultsContentVerificationFailed = n
		case "SUPERSEDED":
			ls.ResultsSuperseded = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate results by state: %w", err)
	}

	// Open copies (outcome IS NULL) are running once started_at is set and waiting
	// in a work buffer before that. A copy SUPERSEDED before it started never ran,
	// so only the ones stopped mid-run are counted.
	copyRows, err := pool.Query(ctx, `
		SELECT l.id, l.name,
			COUNT(*) FILTER (WHERE h.outcome = 'SUPERSEDED'),
			COUNT(*) FILTER (WHERE h.outcome IS NULL AND h.started_at IS NOT NULL),
			COUNT(*) FILTER (WHERE h.outcome IS NULL AND h.started_at IS NULL)
		FROM work_unit_assignment_history h
		JOIN work_units wu ON wu.id = h.work_unit_id
		JOIN leafs l ON l.id = wu.leaf_id
		WHERE h.volunteer_id = $1
		  AND (h.outcome IS NULL OR (h.outcome = 'SUPERSEDED' AND h.started_at IS NOT NULL))
		GROUP BY l.id, l.name`,
		volunteerID,
	)
	if err != nil {
		return nil, fmt.Errorf("query copies by progress: %w", err)
	}
	defer copyRows.Close()
	for copyRows.Next() {
		var (
			leafID                    types.ID
			leafName                  string
			stopped, running, waiting int
		)
		if scanErr := copyRows.Scan(&leafID, &leafName, &stopped, &running, &waiting); scanErr != nil {
			return nil, fmt.Errorf("scan copies by progress: %w", scanErr)
		}
		ls := entry(leafID, leafName)
		ls.RunsStopped = stopped
		ls.CopiesRunning = running
		ls.CopiesWaitingToStart = waiting
	}
	if err := copyRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate copies by progress: %w", err)
	}

	out := make([]LeafWorkStatus, 0, len(byLeaf))
	for _, ls := range byLeaf {
		out = append(out, *ls)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LeafName != out[j].LeafName {
			return out[i].LeafName < out[j].LeafName
		}
		return out[i].LeafID.String() < out[j].LeafID.String()
	})
	return out, nil
}
