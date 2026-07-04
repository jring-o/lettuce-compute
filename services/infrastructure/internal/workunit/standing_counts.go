package workunit

import (
	"context"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// CountProbationLiveCopies returns the number of LIVE copies (work_unit_assignment_history
// rows with no outcome yet — RESERVED or RUNNING) of a unit whose HOLDER's CURRENT effective
// standing is not OK (BG-24b). These are the probation-held copies the redundancy arithmetic
// EXCLUDES from coverage: a probation account's live copy forces full replication around
// itself rather than covering a target slot, so the transitioner subtracts this count from
// LiveCopies when it sizes coverage.
//
// The live arm reads the holder's CURRENT effective standing (via standingExprSQL), not a
// stamp — a live copy has submitted nothing yet, so there is no as-of-submit standing to read;
// this mirrors the live-by-current / pending-by-stamped split the #87 trusted-present counting
// uses (the pending arm is counted in-memory by the transitioner off results.standing_at_submit).
// It mirrors CountLiveCopies, joined to volunteers and filtered by the effective-standing twin.
// An INNER JOIN drops a history row with a NULL volunteer_id (no resolvable holder, so no
// standing) — such a copy is treated as countable coverage, exactly as CountLiveCopies counts it.
func (r *PgxWorkUnitRepository) CountProbationLiveCopies(ctx context.Context, workUnitID types.ID) (int, error) {
	var n int
	if err := r.db.QueryRow(ctx,
		`SELECT COUNT(*)
		   FROM work_unit_assignment_history h
		   JOIN volunteers v ON v.id = h.volunteer_id
		  WHERE h.work_unit_id = $1 AND h.outcome IS NULL
		    AND `+standingExprSQL("v")+` <> 'OK'`,
		workUnitID,
	).Scan(&n); err != nil {
		return 0, apierror.Internal("failed to count probation live copies", err)
	}
	return n, nil
}
