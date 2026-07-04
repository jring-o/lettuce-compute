//go:build integration

package workunit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/dispatchparity"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// This is the SQL half of the dispatch-predicate parity suite. It drives the SHARED
// scenario table (internal/dispatchparity) through the three SQL predicate sites:
//
//   - FindNextAssignable — the authoritative read-side gate. It re-checks EVERY
//     dimension, so its verdict is asserted against the scenario's full expectation
//     for every scenario (this is the primary cross-layer parity assertion, paired
//     with the Go eligibleLocked half in
//     internal/server/dispatch_predicate_parity_test.go).
//   - FlushReservations / ReserveCopy — the landing-write gates. They re-check only
//     the subset of the rule they own (per-volunteer distinctness / cooldown /
//     feasibility, plus redundancy for FlushReservations). They are asserted only on
//     the scenarios whose dimension they enforce (Scenario.EnforcedBy), plus every
//     positive scenario, which all three gates must admit.
//
// Because both this file and the Go half consume the SAME dispatchparity.Scenarios(),
// a rule changed in one predicate but not mirrored in the others flips a shared
// expectation and fails here or in the Go half.
//
// Per repo convention this is a DB-backed integration test: build tag `integration`,
// SKIP unless LETTUCE_TEST_DB_URL is set (via the shared setupTestDB), and safe under
// `-p 1` (each scenario DELETE-cleans the shared tables before it seeds).

// parityTrustedScore is the current/stamped trust score a scenario's TRUSTED contributors
// (live holders, pending authors) are seeded with. It is deliberately far above every
// scenario's resolved floor so a "trusted" contributor is unambiguously trusted regardless
// of which floor the scenario resolves.
const parityTrustedScore = 1000000

// seededParity identifies the freshly seeded rows a single scenario needs.
type seededParity struct {
	leafID    types.ID
	unitID    types.ID
	requester types.ID
	hostID    *types.ID // the requester's reported host id, nil when it reports none
}

// cleanParityTables removes every row a scenario could have seeded, in FK-safe order.
// Called before each seed so scenarios (and the three gates within one scenario) do
// not contaminate one another.
func cleanParityTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{
		"volunteer_trust",
		"work_unit_assignment_history", "credit_ledger", "results",
		"work_units", "batches", "leafs", "volunteers", "users",
	} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clean %s: %v", table, err)
		}
	}
}

func parityExecConfig(s dispatchparity.Scenario) string {
	return fmt.Sprintf(
		`{"runtime":%q,"gpu_required":%t,"gpu_type":"","max_memory_mb":%d,"max_disk_mb":10240,"rsc_fpops_est":%s}`,
		s.LeafRuntime, s.LeafGPURequired, s.LeafMaxMemoryMB,
		strconv.FormatFloat(s.LeafRscFpopsEst, 'f', -1, 64),
	)
}

func parityResReq(s dispatchparity.Scenario) string {
	// resource_requirements.gpu_required is deliberately left false: GPU presence is
	// driven by the execution_config flag only, exercising the "either flag gates
	// presence" rule the same way both layers evaluate it.
	return fmt.Sprintf(
		`{"min_cpu_cores":%d,"min_disk_mb":0,"gpu_required":false,"min_gpu_vram_mb":0}`,
		s.LeafMinCPUCores,
	)
}

func parityValConfig(s dispatchparity.Scenario) string {
	// min_trusted_corroborators / trust_floor carry the per-leaf trust overrides. Both
	// resolve via COALESCE((... )::int, 0) > 0 in effTrustKSQL / effTrustFloorSQL, so a 0
	// here is indistinguishable from the field being absent — inheriting the head default,
	// exactly like a leaf that never set them. Emitting them unconditionally keeps the JSON
	// shape uniform across scenarios.
	return fmt.Sprintf(
		`{"redundancy_factor":%d,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3,"min_trusted_corroborators":%d,"trust_floor":%d}`,
		s.TargetCopies, s.LeafTrustK, s.LeafTrustFloor,
	)
}

// insertLiveCopy writes a live (outcome NULL, un-started) copy for (wuID, vol),
// optionally attributed to a machine (host). A live copy consumes redundancy
// headroom and, via COALESCE(host_id, volunteer_id), counts toward the per-machine
// in-flight cap.
func insertLiveCopy(t *testing.T, pool *pgxpool.Pool, wuID, vol types.ID, host *types.ID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, host_id, assigned_at)
		VALUES ($1, $2, $3, NOW())`, wuID, vol, host); err != nil {
		t.Fatalf("insert live copy: %v", err)
	}
}

// insertCooldownCopy writes a CLOSED copy for the requester on the target unit that
// drives the post-failure cooldown. started controls started_at (NULL vs set);
// outcomeAgoSecs places outcome_at that many seconds in the past (0 = now).
func insertCooldownCopy(t *testing.T, pool *pgxpool.Pool, wuID, vol types.ID, outcome string, started bool, outcomeAgoSecs int) {
	t.Helper()
	startedExpr := "NULL"
	if started {
		startedExpr = "NOW()"
	}
	sql := fmt.Sprintf(`
		INSERT INTO work_unit_assignment_history
			(work_unit_id, volunteer_id, assigned_at, started_at, outcome, outcome_at)
		VALUES ($1, $2, NOW(), %s, $3::assignment_outcome, NOW() - make_interval(secs => $4))`, startedExpr)
	if _, err := pool.Exec(context.Background(), sql, wuID, vol, outcome, outcomeAgoSecs); err != nil {
		t.Fatalf("insert cooldown copy: %v", err)
	}
}

// setVolunteerDID binds a volunteer row to a DID with the given binding status via a
// direct UPDATE (tests may write the DID columns directly). The trust subject is then
// computed by the production SQL expression at query time, exactly as in production —
// the seed never re-implements trust.SubjectForVolunteer. status must be one of the
// did_binding_status domain values (OK / STALE / REVOKED).
func setVolunteerDID(t *testing.T, pool *pgxpool.Pool, vol types.ID, did, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE volunteers
		SET did = $2, did_binding_status = $3, did_bound_at = NOW(), did_binding_checked_at = NOW()
		WHERE id = $1`, vol, did, status); err != nil {
		t.Fatalf("set volunteer DID: %v", err)
	}
}

// setTrustScore upserts a volunteer_trust row for vol's subject with the given current
// score. The subject is computed by the PRODUCTION SQL expression (subjectExprSQL) from the
// volunteer row, never re-implemented here — so the seeded key matches the subject the
// dispatch queries resolve for the same volunteer. Used to make a live-copy holder (or the
// requester) count TRUSTED by its CURRENT score (the live arm of the trusted-present count,
// and the requester-trusted bypass).
func setTrustScore(t *testing.T, pool *pgxpool.Pool, vol types.ID, score int) {
	t.Helper()
	sql := `INSERT INTO volunteer_trust (subject, score)
		SELECT ` + subjectExprSQL("v") + `, $2 FROM volunteers v WHERE v.id = $1
		ON CONFLICT (subject) DO UPDATE SET score = EXCLUDED.score, updated_at = NOW()`
	if _, err := pool.Exec(context.Background(), sql, vol, score); err != nil {
		t.Fatalf("set trust score: %v", err)
	}
}

// stampResultTrust stamps a PENDING result's submission-time trust snapshot
// (results.trust_subject + results.trust_score_at_submit) for author vol on wuID, so the
// result counts TRUSTED via the STAMP arm of the trusted-present count — exactly the number
// the validation verdict will credit, not a later current score. The stamped subject is the
// author's subject via the production expression (subjectExprSQL). The base
// insertPendingResult leaves both columns NULL, i.e. untrusted; this promotes a chosen
// author to trusted.
func stampResultTrust(t *testing.T, pool *pgxpool.Pool, wuID, vol types.ID, score int) {
	t.Helper()
	sql := `UPDATE results r
		SET trust_subject = (` + subjectExprSQL("v") + `), trust_score_at_submit = $3
		FROM volunteers v
		WHERE r.work_unit_id = $1 AND r.volunteer_id = $2 AND v.id = $2`
	if _, err := pool.Exec(context.Background(), sql, wuID, vol, score); err != nil {
		t.Fatalf("stamp result trust: %v", err)
	}
}

// seedParity materializes one scenario into Postgres and returns the ids the gates
// need. It is the SQL counterpart of the Go half's projectGo: the same abstract
// scenario, rendered as rows.
func seedParity(t *testing.T, pool *pgxpool.Pool, repo *PgxWorkUnitRepository, s dispatchparity.Scenario) seededParity {
	t.Helper()
	ctx := context.Background()

	userID := createTestUser(t, pool, "parity-"+uuid.New().String()[:8])
	leafID := createActiveTestLeaf(t, pool, &userID, parityResReq(s), parityExecConfig(s), parityValConfig(s))

	requester := createTestVolunteer(t, pool)
	// The landing gates (FlushReservations / ReserveCopy) read the requester's STORED
	// benchmark for the feasibility check; FindNextAssignable takes it from opts. Set
	// both from the same scenario value so the two feasibility paths see one input.
	if _, err := pool.Exec(ctx,
		`UPDATE volunteers SET hardware_capabilities = $1::jsonb WHERE id = $2`,
		fmt.Sprintf(`{"benchmark_fpops":%s}`, strconv.FormatFloat(s.RequesterBenchmarkFPOPS, 'f', -1, 64)),
		requester); err != nil {
		t.Fatalf("set requester benchmark: %v", err)
	}

	// Target unit.
	wu := newTestWorkUnit(leafID, nil)
	wu.State = WorkUnitStateQueued
	wu.DeadlineSeconds = s.DeadlineSeconds
	if err := repo.Create(ctx, wu); err != nil {
		t.Fatalf("create target unit: %v", err)
	}
	if s.UnitHRClass != "" {
		if _, err := pool.Exec(ctx, `UPDATE work_units SET hr_class = $1 WHERE id = $2`, s.UnitHRClass, wu.ID); err != nil {
			t.Fatalf("pin unit hr_class: %v", err)
		}
	}

	// Live copies + PENDING results by OTHER, distinct volunteers (redundancy coverage).
	// The first TrustedOtherLiveCopies live holders and the first TrustedOtherPendingResults
	// pending authors are made TRUSTED so they count toward the trusted-corroborator
	// reservation's trusted-present total — a live holder via a high CURRENT volunteer_trust
	// score, a pending author via a high STAMPED submission-time score (the two arms of
	// trustedPresentCountSQL). parityTrustedScore is far above every scenario floor.
	for i := 0; i < s.OtherLiveCopies; i++ {
		holder := createTestVolunteer(t, pool)
		insertLiveCopy(t, pool, wu.ID, holder, nil)
		if i < s.TrustedOtherLiveCopies {
			setTrustScore(t, pool, holder, parityTrustedScore)
		}
	}
	for i := 0; i < s.OtherPendingResults; i++ {
		author := createTestVolunteer(t, pool)
		insertPendingResult(t, pool, wu.ID, author)
		if i < s.TrustedOtherPendingResults {
			stampResultTrust(t, pool, wu.ID, author, parityTrustedScore)
		}
	}
	// The requester's own current trust score (0 = no row = untrusted). A score at or above
	// the resolved floor makes the requester TRUSTED, bypassing the reservation.
	if s.RequesterTrustScore > 0 {
		setTrustScore(t, pool, requester, s.RequesterTrustScore)
	}

	// The requester's own relationship to the target unit.
	if s.SelfLiveCopy {
		insertLiveCopy(t, pool, wu.ID, requester, nil)
	}
	if s.SelfPendingResult {
		insertPendingResult(t, pool, wu.ID, requester)
	}
	switch s.Cooldown {
	case dispatchparity.CooldownExpiredRecent:
		insertCooldownCopy(t, pool, wu.ID, requester, "EXPIRED", true, 0)
	case dispatchparity.CooldownStartedAbandon:
		insertCooldownCopy(t, pool, wu.ID, requester, "ABANDONED", true, 0)
	case dispatchparity.CooldownUnstartedAbandon:
		insertCooldownCopy(t, pool, wu.ID, requester, "ABANDONED", false, 0)
	case dispatchparity.CooldownExpiredStale:
		// outcome_at older than the cooldown window (GREATEST(deadline,1) seconds).
		insertCooldownCopy(t, pool, wu.ID, requester, "EXPIRED", true, s.DeadlineSeconds+120)
	}

	// Subject (DID) distinctness. Mint ONE DID per scenario and reuse it for the
	// requester and every same-DID "other" row so their trust subjects coincide
	// (trust.SubjectForVolunteer); a different-DID row gets its own DID. The subjects
	// are derived by the production SQL expression at query time, never re-implemented here.
	scenarioDID := "did:plc:parity" + uuid.New().String()[:12]
	if s.RequesterBinding != "" {
		setVolunteerDID(t, pool, requester, scenarioDID, s.RequesterBinding)
	}
	// A SECOND device bound to the SAME DID as the requester, holding a live copy or a
	// PENDING result on the target unit. Like OtherLiveCopies / OtherPendingResults it
	// consumes one unit of redundancy headroom, so with the baseline TargetCopies=2 any
	// refusal is attributable to the subject rule alone.
	if s.OtherSameDIDLiveCopy {
		other := createTestVolunteer(t, pool)
		setVolunteerDID(t, pool, other, scenarioDID, s.OtherBinding)
		insertLiveCopy(t, pool, wu.ID, other, nil)
	}
	if s.OtherSameDIDPendingResult {
		other := createTestVolunteer(t, pool)
		setVolunteerDID(t, pool, other, scenarioDID, s.OtherBinding)
		insertPendingResult(t, pool, wu.ID, other)
	}
	// Control: a distinct bound principal (a DIFFERENT DID) holding a live copy. Its
	// subject differs from the requester's, so it must NOT exclude — proving the rule
	// keys on subject equality, not merely on "some other bound row exists".
	if s.OtherDifferentDIDLiveCopy {
		other := createTestVolunteer(t, pool)
		setVolunteerDID(t, pool, other, "did:plc:parityalt"+uuid.New().String()[:12], dispatchparity.BindingOK)
		insertLiveCopy(t, pool, wu.ID, other, nil)
	}

	// Per-machine in-flight: live copies of OTHER units attributed to the requester's
	// effective host key. Kept on a SEPARATE leaf so FindNextAssignable (scoped to the
	// target leaf) never offers them, while the cap subquery — which has no leaf
	// filter — still counts them.
	var hostPtr *types.ID
	if s.ReportsHost {
		h := types.NewID()
		hostPtr = &h
	}
	if s.HostOtherInflight > 0 {
		fillerLeaf := createActiveTestLeaf(t, pool, &userID, "", "", "")
		for i := 0; i < s.HostOtherInflight; i++ {
			fu := newTestWorkUnit(fillerLeaf, nil)
			fu.State = WorkUnitStateQueued
			if err := repo.Create(ctx, fu); err != nil {
				t.Fatalf("create filler unit: %v", err)
			}
			if s.ReportsHost {
				// Attributed to the reported host id (COALESCE(host_id, vol) = host id).
				insertLiveCopy(t, pool, fu.ID, createTestVolunteer(t, pool), hostPtr)
			} else {
				// No reported host: the requester itself holds the copy, so
				// COALESCE(host_id, vol) = the requester's account id.
				insertLiveCopy(t, pool, fu.ID, requester, nil)
			}
		}
	}

	return seededParity{leafID: leafID, unitID: wu.ID, requester: requester, hostID: hostPtr}
}

// parityOpts builds the AssignmentOptions FindNextAssignable is called with. It is
// scoped to the target leaf so ONLY the target unit is a selection candidate — any
// in-flight filler units on other leafs are excluded, making the returned unit
// unambiguously "the target, or nothing".
func parityOpts(s dispatchparity.Scenario, seed seededParity) AssignmentOptions {
	return AssignmentOptions{
		VolunteerID:             seed.requester,
		LeafIDs:                 []types.ID{seed.leafID},
		MaxCPUCores:             s.RequesterMaxCPUCores,
		MaxMemoryMB:             s.RequesterMaxMemoryMB,
		MaxDiskMB:               1 << 40,
		HasGPU:                  s.RequesterHasGPU,
		AvailableRuntimes:       s.RequesterRuntimes,
		MaxInflightPerVolunteer: s.MaxInflight,
		HRClass:                 s.RequesterHRClass,
		HostID:                  seed.hostID,
		BenchmarkFPOPS:          s.RequesterBenchmarkFPOPS,
	}
}

func TestDispatchPredicateParity_SQL(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxWorkUnitRepository(pool)
	ctx := context.Background()

	for _, s := range dispatchparity.Scenarios() {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			// Feed the scenario's head trust-gate policy into every SQL predicate site for
			// this scenario (mutates the shared repo in place; each scenario resets it, and
			// the zero policy every non-trust scenario carries keeps the reservation inert).
			// The per-leaf (K, floor) overrides ride on validation_config (parityValConfig).
			repo.WithTrustDispatch(TrustDispatchPolicy{
				GateEnabled:             s.TrustGateEnabled,
				DefaultMinCorroborators: s.TrustDefaultK,
				DefaultFloor:            s.TrustDefaultFloor,
			})

			// --- FindNextAssignable: the full read-side predicate ---
			cleanParityTables(t, pool)
			seed := seedParity(t, pool, repo, s)
			found, err := repo.FindNextAssignable(ctx, parityOpts(s, seed))
			if err != nil {
				t.Fatalf("FindNextAssignable: %v", err)
			}
			offered := found != nil && found.ID == seed.unitID
			if offered != s.SQLWant() {
				t.Fatalf("FindNextAssignable verdict mismatch\n"+
					"  scenario:  %s (dimension %s)\n"+
					"  offered:   %v\n"+
					"  want:      %v\n"+
					"  divergence:%v\n"+
					"If you changed the dispatch eligibility rule, update the shared table in\n"+
					"internal/dispatchparity AND the Go layer (eligibleLocked) to match.",
					s.Name, s.Dimension, offered, s.SQLWant(), s.Divergence != nil)
			}

			// --- FlushReservations + ReserveCopy: the landing-write subsets ---
			assertLandingGate(t, pool, repo, s, dispatchparity.GateFlushReservations)
			assertLandingGate(t, pool, repo, s, dispatchparity.GateReserveCopy)
		})
	}
}

// assertLandingGate re-seeds the scenario and asserts the given landing-write gate's
// admit/refuse verdict. The canonical verdict is the full-predicate one (SQLWant):
//   - a POSITIVE scenario must be admitted by the gate (it checks a subset, all of
//     which pass);
//   - a NEGATIVE scenario whose dimension the gate ENFORCES must be refused;
//   - a NEGATIVE scenario whose dimension the gate does NOT enforce is deliberately
//     delegated to the hand-out (the gate would admit); it is skipped, not asserted,
//     so the test pins only what each gate actually owns.
func assertLandingGate(t *testing.T, pool *pgxpool.Pool, repo *PgxWorkUnitRepository, s dispatchparity.Scenario, gate dispatchparity.Gate) {
	t.Helper()
	canonical := s.SQLWant()
	if !canonical && !s.EnforcedBy(gate) {
		t.Logf("%s: dimension %s is delegated to the hand-out (not re-checked here) — skipping", gateName(gate), s.Dimension)
		return
	}
	cleanParityTables(t, pool)
	seed := seedParity(t, pool, repo, s)
	admitted := runLandingGate(t, pool, repo, s, seed, gate)
	if admitted != canonical {
		t.Fatalf("%s verdict mismatch\n"+
			"  scenario: %s (dimension %s)\n"+
			"  admitted: %v\n"+
			"  want:     %v\n"+
			"This landing gate re-checks the %s dimension; its verdict must match the\n"+
			"read-side gate. Update internal/dispatchparity and all four predicate sites together.",
			gateName(gate), s.Name, s.Dimension, admitted, canonical, s.Dimension)
	}
}

// runLandingGate exercises one landing-write gate against the freshly seeded target
// unit and reports whether the requester's copy was admitted (landed / created).
func runLandingGate(t *testing.T, pool *pgxpool.Pool, repo *PgxWorkUnitRepository, s dispatchparity.Scenario, seed seededParity, gate dispatchparity.Gate) bool {
	t.Helper()
	ctx := context.Background()
	until := time.Now().UTC().Add(time.Hour)

	switch gate {
	case dispatchparity.GateFlushReservations:
		landed, err := repo.FlushReservations(ctx, []FlushReservation{{
			WorkUnitID:      seed.unitID,
			VolunteerID:     seed.requester,
			HostID:          seed.hostID,
			ReservedUntil:   until,
			DeadlineSeconds: s.DeadlineSeconds,
		}}, types.ID{}, 0)
		if err != nil {
			t.Fatalf("FlushReservations: %v", err)
		}
		return containsFlushedPair(landed, seed.unitID, seed.requester)

	case dispatchparity.GateReserveCopy:
		cp, err := repo.ReserveCopy(ctx, seed.unitID, seed.requester, seed.hostID, until, s.DeadlineSeconds)
		if err == nil {
			return cp != nil
		}
		// A refusal is a 409 Conflict; anything else is a real failure to surface.
		var apiErr *apierror.APIError
		if errors.As(err, &apiErr) && apiErr.HTTPStatus == 409 {
			return false
		}
		t.Fatalf("ReserveCopy: unexpected error: %v", err)
		return false
	}
	t.Fatalf("unknown gate %v", gate)
	return false
}

func gateName(g dispatchparity.Gate) string {
	switch g {
	case dispatchparity.GateFlushReservations:
		return "FlushReservations"
	case dispatchparity.GateReserveCopy:
		return "ReserveCopy"
	case dispatchparity.GateFindNextAssignable:
		return "FindNextAssignable"
	default:
		return "eligibleLocked"
	}
}
