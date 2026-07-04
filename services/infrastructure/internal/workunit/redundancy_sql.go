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
