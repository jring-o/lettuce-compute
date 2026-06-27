package leaf

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// ActiveVolunteerWindowMinutes is the rolling window over which a volunteer is
// counted as "active" on a leaf.
//
// A volunteer counts as active if it holds a LIVE copy (RESERVED or RUNNING)
// right now OR it closed a copy within this window. Counting only live copies
// (the pre-window behaviour) made the metric flicker to zero between hand-outs
// for leaves whose work units run for many minutes and whose volunteer pool is
// thin or dominated by one contributor: a volunteer that had just submitted a
// long-running container unit held no live copy, and per-work-unit distinctness
// prevents it from being re-handed its own unit, so the leaf read "0 active
// volunteers" even though that volunteer was productively working it. The window
// keeps a recently-active volunteer counted until it has genuinely stopped.
const ActiveVolunteerWindowMinutes = 15

// activeVolunteerPredicate returns the SQL boolean predicate, over a
// work_unit_assignment_history row aliased as `alias`, that selects the copies
// whose volunteer counts as active on the leaf.
//
// This is the single source of truth for "active volunteer". Every surface that
// reports active_volunteers — the gRPC GetHeadInfo RPC, REST GET /api/v1/head,
// REST GET /api/v1/leafs, and the leaf stats snapshot — derives its count from
// this predicate so the surfaces cannot drift apart (they did before: two paths
// computed live-copy counts while the list endpoint and the stats snapshot were
// hardcoded to zero).
func activeVolunteerPredicate(alias string) string {
	return fmt.Sprintf(
		"%[1]s.volunteer_id IS NOT NULL AND (%[1]s.outcome IS NULL OR %[1]s.outcome_at >= now() - make_interval(mins => %[2]d))",
		alias, ActiveVolunteerWindowMinutes,
	)
}

// ActiveVolunteerSubquery returns a SQL subquery that yields (leaf_id, cnt) rows,
// where cnt is the number of DISTINCT volunteers active (per
// activeVolunteerPredicate) on each leaf. It is meant to be LEFT JOINed against
// the leafs table, e.g.:
//
//	LEFT JOIN ( <ActiveVolunteerSubquery()> ) a ON a.leaf_id = l.id
//
// and read with COALESCE(a.cnt, 0).
func ActiveVolunteerSubquery() string {
	return fmt.Sprintf(`
		SELECT wu.leaf_id, COUNT(DISTINCT h.volunteer_id) AS cnt
		FROM work_unit_assignment_history h
		JOIN work_units wu ON wu.id = h.work_unit_id
		WHERE %s
		GROUP BY wu.leaf_id`, activeVolunteerPredicate("h"))
}

// CountActiveVolunteersByLeaf returns the number of distinct active volunteers
// for every leaf that currently has any. Leaves with no active volunteers are
// absent from the map; callers should treat a missing key as zero. It is used by
// the leaf list endpoint, which holds only a page of leaves but pays the same
// (cheap, pre-aggregated) scan the head endpoints already do.
func CountActiveVolunteersByLeaf(ctx context.Context, pool *pgxpool.Pool) (map[types.ID]int, error) {
	rows, err := pool.Query(ctx, ActiveVolunteerSubquery())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[types.ID]int)
	for rows.Next() {
		var id types.ID
		var cnt int
		if err := rows.Scan(&id, &cnt); err != nil {
			return nil, err
		}
		counts[id] = cnt
	}
	return counts, rows.Err()
}

// CountActiveVolunteersForLeaf returns the number of distinct active volunteers
// for a single leaf. It is used by the stats snapshot, which is computed per
// leaf by a low-frequency background job, so a dedicated query is fine.
func CountActiveVolunteersForLeaf(ctx context.Context, pool *pgxpool.Pool, leafID types.ID) (int, error) {
	var cnt int
	err := pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT COUNT(DISTINCT h.volunteer_id)
		FROM work_unit_assignment_history h
		JOIN work_units wu ON wu.id = h.work_unit_id
		WHERE wu.leaf_id = $1 AND %s`, activeVolunteerPredicate("h")), leafID).Scan(&cnt)
	if err != nil {
		return 0, err
	}
	return cnt, nil
}

// activeHostCountExpr is the COUNT expression for distinct active HOSTS (machines)
// on a leaf, over a work_unit_assignment_history row aliased as `alias`. A host is
// a single machine under an account (the account<->host split, TODO #19): two
// machines sharing one identity key count as two hosts but one volunteer, which is
// why a one-account/many-machine contributor reads "1 active volunteer" but is
// usefully shown as N hosts. A NULL host_id (a client that doesn't report a host
// key, or pre-#19 data) falls back to the account so it still counts as one host.
func activeHostCountExpr(alias string) string {
	return fmt.Sprintf("COUNT(DISTINCT COALESCE(%[1]s.host_id::text, %[1]s.volunteer_id::text))", alias)
}

// ActiveHostSubquery mirrors ActiveVolunteerSubquery but counts distinct active
// hosts (machines) per leaf, using the same active predicate.
func ActiveHostSubquery() string {
	return fmt.Sprintf(`
		SELECT wu.leaf_id, %s AS cnt
		FROM work_unit_assignment_history h
		JOIN work_units wu ON wu.id = h.work_unit_id
		WHERE %s
		GROUP BY wu.leaf_id`, activeHostCountExpr("h"), activeVolunteerPredicate("h"))
}

// CountActiveHostsByLeaf returns the number of distinct active hosts (machines)
// for every leaf that currently has any. Leaves with none are absent; treat a
// missing key as zero.
func CountActiveHostsByLeaf(ctx context.Context, pool *pgxpool.Pool) (map[types.ID]int, error) {
	rows, err := pool.Query(ctx, ActiveHostSubquery())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[types.ID]int)
	for rows.Next() {
		var id types.ID
		var cnt int
		if err := rows.Scan(&id, &cnt); err != nil {
			return nil, err
		}
		counts[id] = cnt
	}
	return counts, rows.Err()
}
