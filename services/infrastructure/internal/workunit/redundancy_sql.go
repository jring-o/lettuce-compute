package workunit

// Shared SQL fragments for the redundancy decision (TODO #50). These are the SQL mirror of
// transition.ResolvePolicy: the dispatch predicate, the reservation headroom guard, and the
// dead-letter ceiling all embed these EXACT expressions instead of hand-copying the old
// `CASE WHEN spot_check THEN 2 ELSE COALESCE(redundancy_factor,2)` arithmetic in five places.
// Centralising them here is half of the eligibility<->transitioner agreement; the other half
// is the Go transition.Dispatchable/Decide, and the SQL<->Go parity property test asserts the
// two never diverge (the structural guard against another #49).
//
// Each builder takes the work_units and leafs table aliases in scope (most queries use
// "wu"/"l"; ClaimDispatchableBatch's inner select uses "wu2"/"l2"). They resolve the same way
// ResolvePolicy does: per-unit stamped override (0 = none) -> leaf validation_config (0 =
// none) -> redundancy_factor. For an existing leaf that only sets redundancy_factor, each
// fragment evaluates IDENTICALLY to the pre-#50 expression, so dispatch / dead-letter behavior
// is byte-for-byte unchanged.

// effTargetSQL: how many copies to dispatch. spot_check forces 2 (the old
// effectiveRedundancy=2 override). Mirrors RedundancyPolicy.TargetCopies.
func effTargetSQL(wu, l string) string {
	return `(CASE
		WHEN ` + wu + `.spot_check THEN 2
		WHEN ` + wu + `.target_copies > 0 THEN ` + wu + `.target_copies
		WHEN COALESCE((` + l + `.validation_config->>'target_copies')::int, 0) > 0
			THEN (` + l + `.validation_config->>'target_copies')::int
		ELSE COALESCE((` + l + `.validation_config->>'redundancy_factor')::int, 2)
	END)`
}

// effQuorumSQL: how many agreeing results validate. spot_check forces 2. The validator
// guarantees min_quorum <= target_copies, so no clamp is needed. Mirrors
// RedundancyPolicy.MinQuorum.
func effQuorumSQL(wu, l string) string {
	return `(CASE
		WHEN ` + wu + `.spot_check THEN 2
		WHEN ` + wu + `.min_quorum > 0 THEN ` + wu + `.min_quorum
		WHEN COALESCE((` + l + `.validation_config->>'min_quorum')::int, 0) > 0
			THEN (` + l + `.validation_config->>'min_quorum')::int
		ELSE COALESCE((` + l + `.validation_config->>'redundancy_factor')::int, 2)
	END)`
}

// effMaxTotalSQL: the dead-letter ceiling. Per-unit override, else leaf config, else the
// effective target + the historical retry margin (redundancy + 6). Mirrors
// RedundancyPolicy.MaxTotalCopies. The +6 mirrors defaultCopyRetryMargin (workunit/model.go).
func effMaxTotalSQL(wu, l string) string {
	return `(CASE
		WHEN ` + wu + `.max_total_copies > 0 THEN ` + wu + `.max_total_copies
		WHEN COALESCE((` + l + `.validation_config->>'max_total_copies')::int, 0) > 0
			THEN (` + l + `.validation_config->>'max_total_copies')::int
		ELSE ` + effTargetSQL(wu, l) + ` + 6
	END)`
}

// Precomputed fragments for the common wu/l alias pair (used by every query except
// ClaimDispatchableBatch's inner select, which builds its own with effTargetSQL("wu2","l2")).
var (
	effTargetWuL   = effTargetSQL("wu", "l")
	effQuorumWuL   = effQuorumSQL("wu", "l")
	effMaxTotalWuL = effMaxTotalSQL("wu", "l")
)
