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

// --- Trust-gate dispatch (trusted-corroborator reservation) ---
//
// These two builders are the SQL twins of transition.TrustPolicy.ResolveTrust
// (internal/transition/trust.go): the resolved trust FLOOR (the score at or above which an
// agreeing subject counts as trusted) and the resolved trusted-corroborator requirement K
// (how many DISTINCT trusted subjects a unit's leaf demands). The dispatch reservation
// embeds these EXACT expressions instead of re-deriving the resolution, so the SQL and Go
// never drift; the golden parity test (trust_resolve_parity_test.go) pins them to
// ResolveTrust across a grid of gate / leaf-override / default / min-quorum inputs — the
// same structural guard subjectExprSQL has against a silent Go<->SQL divergence.
//
// Each builder takes the leafs (and, for K, the work_units) alias in scope plus the $N
// placeholders carrying the head TrustDispatchPolicy fields (gate-enabled, default K,
// default floor). Resolution mirrors ResolveTrust field-for-field: per-leaf override
// (validation_config, 0 = none) -> head default. Because the gate-off branch resolves K to
// a constant 0, the reservation clause that embeds effTrustKSQL collapses to the
// pre-existing redundancy headroom check when the gate is off — provably inert, exactly as
// ResolveTrust returns K == 0 with the gate disabled.

// effTrustFloorSQL: the resolved trust floor W. Mirrors ResolveTrust's floor branch — the
// leaf override validation_config.trust_floor when > 0, else the head-default floor
// (floorParam). Resolved REGARDLESS of the gate switch, exactly as ResolveTrust resolves
// the floor even when disabled (accrual needs the real floor before enforcement is ever
// turned on, so a subject can earn a score first). floorParam is the $N placeholder
// carrying TrustDispatchPolicy.DefaultFloor.
func effTrustFloorSQL(l, floorParam string) string {
	return `(CASE
		WHEN COALESCE((` + l + `.validation_config->>'trust_floor')::int, 0) > 0
			THEN (` + l + `.validation_config->>'trust_floor')::int
		ELSE ` + floorParam + `::int
	END)`
}

// effTrustKSQL: the resolved trusted-corroborator requirement K. Mirrors ResolveTrust's K
// branch exactly:
//   - gate disabled (gateParam false) -> 0 (the gate then never withholds a slot), else
//   - the leaf override validation_config.min_trusted_corroborators when > 0, else the
//     head-default K (kParam), CLAMPED down to the effective min quorum (effQuorumSQL): a
//     quorum-sized agreeing group holds at most min_quorum distinct subjects, so a K above
//     it could never be satisfied. The clamp is gated on quorum >= 1, mirroring the Go
//     guard `minQuorum >= 1 && k > minQuorum`, so a non-positive quorum never clamps.
//
// gateParam is the $N bool placeholder (TrustDispatchPolicy.GateEnabled); kParam the $N
// default-K placeholder (TrustDispatchPolicy.DefaultMinCorroborators). The clamp target is
// the existing effQuorumSQL fragment for the same wu/l aliases, so the SQL clamp uses the
// very expression the redundancy SQL<->Go parity already pins to the Go MinQuorum.
func effTrustKSQL(wu, l, gateParam, kParam string) string {
	quorum := effQuorumSQL(wu, l)
	baseK := `(CASE
		WHEN COALESCE((` + l + `.validation_config->>'min_trusted_corroborators')::int, 0) > 0
			THEN (` + l + `.validation_config->>'min_trusted_corroborators')::int
		ELSE ` + kParam + `::int
	END)`
	return `(CASE
		WHEN NOT ` + gateParam + `::boolean THEN 0
		WHEN ` + quorum + ` >= 1 AND ` + baseK + ` > ` + quorum + ` THEN ` + quorum + `
		ELSE ` + baseK + `
	END)`
}

// --- Countable redundancy coverage (account standing, BG-24b) ---
//
// A copy or result only CORROBORATES — only closes a unit's redundancy need — when it
// COUNTS under account standing. The countability contract every redundancy/quorum
// comparison embeds: a LIVE copy counts iff its holder's CURRENT effective standing is 'OK'
// (join volunteers + standingExprSQL); a PENDING result counts iff its submit-time stamp
// results.standing_at_submit is NULL (legacy = OK) or 'OK'. This is the same
// live-by-current / pending-by-stamped split trusted_present already uses. These builders
// are the ONE place it is written; every redundancy HEADROOM comparison
// (FindNextAssignable, FindDispatchableBatch, ClaimDispatchableBatch, FlushReservations)
// embeds countableCoverageSQL in place of the raw live+pending count, so a copy held by a
// PROBATION/BENCHED account — or a result submitted while non-OK — never covers redundancy
// and full replication is FORCED around it. ReserveCopy deliberately re-checks NO redundancy
// headroom (it owns only the per-volunteer distinctness/cooldown/feasibility landing
// invariants; the read-side gate owns coverage), so it embeds none of these.
//
// unitID is the work-unit id column expression in scope ("wu.id", "wu2.id", "v.id", ...).
// Each builder's internal aliases are distinctively prefixed so an embedding never collides
// with a host query's aliases; sibling embeddings in one query are independent scopes and so
// may safely reuse the same internal alias names.
//
// Zero-value safety: with an all-'OK' population and only NULL/'OK' stamps the live LEFT
// JOIN keeps every history row (standingExprSQL resolves a NULL volunteers row to 'OK' too)
// and the pending filter admits every PENDING result, so countableCoverageSQL reduces
// EXACTLY to the pre-standing `live + pending` count and dispatch is byte-for-byte unchanged.

// countableLiveCopiesSQL counts the unit's LIVE copies (outcome IS NULL) that COUNT toward
// coverage — those whose holder's CURRENT effective standing is 'OK'. The LEFT JOIN keeps a
// history row with a NULL/absent volunteer_id (no holder): standingExprSQL resolves a NULL
// standing to 'OK', so such a row counts exactly as it did before standing existed (the raw
// count included it too).
func countableLiveCopiesSQL(unitID string) string {
	return `(SELECT COUNT(*)
		FROM work_unit_assignment_history clc_h
		LEFT JOIN volunteers clc_v ON clc_v.id = clc_h.volunteer_id
		WHERE clc_h.work_unit_id = ` + unitID + ` AND clc_h.outcome IS NULL
		  AND ` + standingExprSQL("clc_v") + ` = 'OK')`
}

// countablePendingResultsSQL counts the unit's PENDING results that COUNT toward coverage —
// those stamped OK at submit (standing_at_submit NULL [legacy = OK] or 'OK').
func countablePendingResultsSQL(unitID string) string {
	return `(SELECT COUNT(*) FROM results cpr_r
		WHERE cpr_r.work_unit_id = ` + unitID + ` AND cpr_r.validation_status = 'PENDING'
		  AND (cpr_r.standing_at_submit IS NULL OR cpr_r.standing_at_submit = 'OK'))`
}

// countableCoverageSQL is the single coverage number every redundancy headroom comparison
// uses: countable live copies + countable pending results.
func countableCoverageSQL(unitID string) string {
	return `(` + countableLiveCopiesSQL(unitID) + ` + ` + countablePendingResultsSQL(unitID) + `)`
}

// nonCountableCoverageSQL is the COMPLEMENT of countableCoverageSQL over the same rows: the
// unit's live copies held by a non-OK account PLUS its PENDING results stamped non-OK. The
// two volunteer-agnostic refill queries emit it per candidate (DispatchCandidate.
// ProbationCoverage) so the in-memory dispatch cache can subtract it from its RAW
// live+pending seed and reach the SAME countable coverage the SQL headroom uses — the
// cache's forced-replication parity. Its live arm INNER-joins volunteers (a NULL-holder row
// has no standing and is NOT non-OK, so it is excluded here exactly as it is INCLUDED as
// countable above), so countableCoverageSQL(u) + nonCountableCoverageSQL(u) equals the raw
// live+pending count for every unit.
func nonCountableCoverageSQL(unitID string) string {
	return `(
		(SELECT COUNT(*)
		 FROM work_unit_assignment_history ncl_h
		 JOIN volunteers ncl_v ON ncl_v.id = ncl_h.volunteer_id
		 WHERE ncl_h.work_unit_id = ` + unitID + ` AND ncl_h.outcome IS NULL
		   AND ` + standingExprSQL("ncl_v") + ` <> 'OK')
		+ (SELECT COUNT(*) FROM results ncp_r
		   WHERE ncp_r.work_unit_id = ` + unitID + ` AND ncp_r.validation_status = 'PENDING'
		     AND ncp_r.standing_at_submit IS NOT NULL AND ncp_r.standing_at_submit <> 'OK')
	)`
}
