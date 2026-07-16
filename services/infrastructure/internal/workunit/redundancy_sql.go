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

// effMaxErrorSQL: the error-copy ceiling. Per-unit override (wu.max_error_copies > 0), else
// leaf config ((l.validation_config->>'max_error_copies')::int > 0), else 0 = unlimited.
// Mirrors RedundancyPolicy.MaxErrorCopies (transition/policy.go) field-for-field — note there
// is NO target-derived default (unlike effMaxTotalSQL): 0 means "only the total ceiling bounds
// errors," exactly as ResolvePolicy leaves it. The dead-letter executor guards the disjunct on
// `> 0` so an absent cap never dead-letters (a raw `errors >= 0` would fire immediately). This
// is the SQL twin whose ABSENCE was BG-27: MaxErrorCopies was resolved and decided in Go but
// executed by no SQL, so the executor could never enforce the owner's configured cap.
func effMaxErrorSQL(wu, l string) string {
	return `(CASE
		WHEN ` + wu + `.max_error_copies > 0 THEN ` + wu + `.max_error_copies
		WHEN COALESCE((` + l + `.validation_config->>'max_error_copies')::int, 0) > 0
			THEN (` + l + `.validation_config->>'max_error_copies')::int
		ELSE 0
	END)`
}

// errorCopiesSQL: the unit's wasted-work tally — the copies that ended EXPIRED or ABANDONED
// (history rows) PLUS the results marked DISAGREED. This is the EXACT expression
// CountErrorCopies runs, the max_error_copies probe (the RedundancyPolicy.MaxErrorCopies
// budget in UnitSnapshot.ErrorCopies). It is written ONCE here and embedded by
// CountErrorCopies, RefundCopyBudget's error-count arm, and DeadLetterIfExhausted's error
// disjunct so the count has a single definition (the same discipline countableCoverageSQL has:
// the drift BG-27 exposed came from an executor that never embedded a shared fragment at all).
//
// unitID is the work-unit id column/placeholder expression in scope ("wu.id", "$1", ...). The
// internal aliases are ec_-prefixed so an embedding never collides with a host query's aliases,
// following countableLiveCopiesSQL's prefix idiom.
func errorCopiesSQL(unitID string) string {
	return `(
		(SELECT COUNT(*) FROM work_unit_assignment_history ec_h
		 WHERE ec_h.work_unit_id = ` + unitID + ` AND ec_h.outcome IN ('EXPIRED', 'ABANDONED'))
		+ (SELECT COUNT(*) FROM results ec_r
		   WHERE ec_r.work_unit_id = ` + unitID + ` AND ec_r.validation_status = 'DISAGREED')
	)`
}

// Precomputed fragments for the common wu/l alias pair (used by every query except
// ClaimDispatchableBatch's inner select, which builds its own with effTargetSQL("wu2","l2")).
var (
	effTargetWuL   = effTargetSQL("wu", "l")
	effQuorumWuL   = effQuorumSQL("wu", "l")
	effMaxTotalWuL = effMaxTotalSQL("wu", "l")
	effMaxErrorWuL = effMaxErrorSQL("wu", "l")
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

// effTrustFloorSQL: the resolved trust floor W. Mirrors ResolveTrust's floor branch
// (BG-01a): floor = GREATEST(1, GREATEST(leaf override, head default)) — the TIGHTEN-ONLY
// leaf override (F-H5: a leaf may only RAISE the floor, so the effective value is the max of
// its trust_floor and the head default, never the leaf's value outright) plus the
// unconditional >= 1 clamp (a floor of 0 would make every score-0 account a trusted witness
// for accrual). The old "leaf when > 0 else default" CASE collapses into GREATEST because
// both branches are now max'd: COALESCE(..., 0) supplies a stored leaf floor (0 when absent),
// and GREATEST with the head default handles negative stored values IDENTICALLY to the Go
// max() (validation pins trust_floor >= 0 at leaf create, but this SQL stays safe for any
// stored value). Resolved REGARDLESS of the gate switch, exactly as ResolveTrust resolves the
// floor even when disabled (accrual needs the real floor before enforcement is ever turned on,
// so a subject can earn a score first). floorParam is the $N placeholder carrying
// TrustDispatchPolicy.DefaultFloor. The golden parity test pins this to ResolveTrust in
// lockstep — a change to EITHER side that drifts fails there.
func effTrustFloorSQL(l, floorParam string) string {
	return `GREATEST(1, GREATEST(
		COALESCE((` + l + `.validation_config->>'trust_floor')::int, 0),
		` + floorParam + `::int))`
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

// versionHomogeneousPendingSQL is the VERSION-HOMOGENEOUS pending count — the SQL twin of the
// filter the transitioner applies before Decide sees a pending count (validation.FilterPending /
// versionHomogeneousGroup: results are never compared across artifact versions, so the count
// that feeds every quorum/coverage decision is the size of the LARGEST single-version group,
// with NULL/legacy versions forming their own group and ties broken by the smallest version
// key, NULL first). Every SQL consumer of "how many PENDING results does this unit hold" —
// the dead-letter executor's quorum probe, the recovery sweep's shape-2 selector, and (via
// countablePendingResultsSQL) the dispatch coverage — embeds this expression, NOT a raw
// COUNT(*): a raw count over a version-heterogeneous pending set reads HIGHER than the count
// Decide acts on, and the divergence livelocks the unit (★BG-21g: Decide WAITs on the filtered
// count, dispatch sees raw coverage met so never sends the missing corroborator, the sweep
// re-selects on the raw count forever, and dead-letter's raw probe refuses the very unit
// Decide told it to fail). COLLATE "C" pins the tie-break to byte order, exactly Go's k < bestKey.
func versionHomogeneousPendingSQL(unitID string) string {
	return `(SELECT COALESCE((
		SELECT COUNT(*)
		FROM results vh_r
		WHERE vh_r.work_unit_id = ` + unitID + ` AND vh_r.validation_status = 'PENDING'
		GROUP BY vh_r.artifact_version_id
		ORDER BY COUNT(*) DESC, COALESCE(vh_r.artifact_version_id::text, '') COLLATE "C" ASC
		LIMIT 1), 0))`
}

// countablePendingResultsSQL counts the unit's PENDING results that COUNT toward coverage:
// the rows of the unit's version-homogeneous group (the SAME largest-raw-group selection as
// versionHomogeneousPendingSQL — chosen by RAW size, mirroring how the transitioner filters
// by version FIRST and discounts standing WITHIN the filtered slice) that were stamped OK at
// submit (standing_at_submit NULL [legacy = OK] or 'OK'). Version-excluded rows cover nothing:
// they can never corroborate the group that will decide the unit, so counting them (the ★BG-21g
// pre-fix raw count) made dispatch believe a heterogeneous unit was covered and starved it of
// the copies Decide was waiting for.
func countablePendingResultsSQL(unitID string) string {
	return `(SELECT COALESCE((
		SELECT COUNT(*) FILTER (WHERE cpr_r.standing_at_submit IS NULL OR cpr_r.standing_at_submit = 'OK')
		FROM results cpr_r
		WHERE cpr_r.work_unit_id = ` + unitID + ` AND cpr_r.validation_status = 'PENDING'
		GROUP BY cpr_r.artifact_version_id
		ORDER BY COUNT(*) DESC, COALESCE(cpr_r.artifact_version_id::text, '') COLLATE "C" ASC
		LIMIT 1), 0))`
}

// countableCoverageSQL is the single coverage number every redundancy headroom comparison
// uses: countable live copies + countable pending results.
func countableCoverageSQL(unitID string) string {
	return `(` + countableLiveCopiesSQL(unitID) + ` + ` + countablePendingResultsSQL(unitID) + `)`
}

// nonCountableCoverageSQL is the COMPLEMENT of countableCoverageSQL over the same rows: the
// unit's live copies held by a non-OK account PLUS its PENDING results that do not count —
// non-OK stamps within the version-homogeneous group AND every version-excluded row. The
// two volunteer-agnostic refill queries emit it per candidate (DispatchCandidate.
// ProbationCoverage) so the in-memory dispatch cache can subtract it from its RAW
// live+pending seed and reach the SAME countable coverage the SQL headroom uses — the
// cache's forced-replication parity. Its live arm INNER-joins volunteers (a NULL-holder row
// has no standing and is NOT non-OK, so it is excluded here exactly as it is INCLUDED as
// countable above); its pending arm is written as raw-minus-countable so the identity
// countableCoverageSQL(u) + nonCountableCoverageSQL(u) = raw live+pending holds BY
// CONSTRUCTION for every unit, version-heterogeneous or not (★BG-21g).
func nonCountableCoverageSQL(unitID string) string {
	return `(
		(SELECT COUNT(*)
		 FROM work_unit_assignment_history ncl_h
		 JOIN volunteers ncl_v ON ncl_v.id = ncl_h.volunteer_id
		 WHERE ncl_h.work_unit_id = ` + unitID + ` AND ncl_h.outcome IS NULL
		   AND ` + standingExprSQL("ncl_v") + ` <> 'OK')
		+ ((SELECT COUNT(*) FROM results ncp_r
		    WHERE ncp_r.work_unit_id = ` + unitID + ` AND ncp_r.validation_status = 'PENDING')
		   - ` + countablePendingResultsSQL(unitID) + `)
	)`
}
