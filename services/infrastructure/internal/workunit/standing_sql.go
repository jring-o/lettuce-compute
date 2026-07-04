package workunit

// standingExprSQL is the SQL twin of volunteer.EffectiveStanding — the ONE
// resolution rule for the standing that enforcement sees (BG-24b), written once
// and embedded wherever a dispatch or counting query needs a volunteer's
// EFFECTIVE standing (the subjectExprSQL discipline; a golden test pins this
// fragment to the Go function so the two can never drift):
//
//   - BENCHED while the stored standing is BENCHED and benched_until is NULL
//     (indefinite) or in the future;
//   - an EXPIRED bench resolves to PROBATION, never straight to OK;
//   - PROBATION as stored; anything else is OK.
//
// v is the volunteers table alias in scope. Countability contract (what the
// redundancy/quorum arithmetic consumes): a LIVE COPY is countable iff its
// holder's CURRENT effective standing is 'OK'; a PENDING RESULT is countable
// iff its submit-time stamp (results.standing_at_submit) is NULL or 'OK' — the
// same live-by-current / pending-by-stamped split the #87 trusted_present
// counting uses.
func standingExprSQL(v string) string {
	return `(CASE
		WHEN ` + v + `.standing = 'BENCHED'
			AND (` + v + `.benched_until IS NULL OR ` + v + `.benched_until > NOW())
			THEN 'BENCHED'
		WHEN ` + v + `.standing IN ('PROBATION', 'BENCHED') THEN 'PROBATION'
		ELSE 'OK'
	END)`
}
