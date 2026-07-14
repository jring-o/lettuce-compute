//go:build integration

// Package transition_test drives the E1 recovery sweep against a real Postgres. These are the
// closeout refutation targets (design §9): each seeds a stranded finalization shape directly in
// SQL, then asserts one sweep (or one Evaluate) converges it. They are DB-gated: without
// LETTUCE_TEST_DB_URL they skip. Run with:
//
//	GOWORK=off go test -tags integration -p 1 ./internal/transition/
//
// The stack under test is production-shaped: a real validation Engine wired with the production
// FinalizationTxRunner (so accept/reject is atomic), a real Transitioner, and a real
// RecoverySweeper reading the real candidate finders off the work-unit repo.
package transition_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

const e1ExecMeta = `{"wall_clock_seconds":10,"cpu_seconds_user":8,"cpu_seconds_system":1,"cpu_cores_used":1,"peak_memory_mb":128}`

// e1Stack bundles the production-shaped components one sweep needs.
type e1Stack struct {
	pool         *pgxpool.Pool
	wuRepo       *workunit.PgxWorkUnitRepository
	transitioner *transition.Transitioner
	sweeper      *transition.RecoverySweeper
}

// e1SetupTestDB opens the test pool (house pattern) and returns a DELETE-clean teardown.
func e1SetupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	cleanup := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_attestations")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteer_rac")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}
	return pool, cleanup
}

// e1NewStack wires the real engine (+ production tx runner), transitioner, and sweeper. The
// sweeper's grace/interval/batch are caller-chosen per test; a NoopLocker stands in for the
// advisory lock (the tx's unit-row lock is the real serializer).
func e1NewStack(t *testing.T, pool *pgxpool.Pool, grace time.Duration, batch int) *e1Stack {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	leafRepo := leaf.NewPgxRepository(pool)
	resultRepo := result.NewPgxRepository(pool)

	engine := validation.NewEngine(
		resultRepo, wuRepo, leafRepo,
		credit.NewPgxRepository(pool), credit.NewPgxRACRepository(pool),
		volunteer.NewPgxRepository(pool), assignment.NewPgxRepository(pool),
		nil, nil, nil, logger, nil, transition.TrustPolicy{},
	).WithTxRunner(validation.NewPgxFinalizationTxRunner(pool))

	tr := transition.NewTransitioner(transition.NoopLocker{}, wuRepo, leafRepo, resultRepo, engine, transition.TrustPolicy{}, logger)
	sw := transition.NewRecoverySweeper(wuRepo, tr, time.Minute, grace, batch, logger)
	return &e1Stack{pool: pool, wuRepo: wuRepo, transitioner: tr, sweeper: sw}
}

// --- self-contained SQL seeding ---

func e1SeedUser(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		id, id.String()+"@test.example.com", "u"+id.String()[:8], "Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func e1SeedVolunteer(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	id := types.NewID()
	pub := make([]byte, 32)
	copy(pub, id[:])
	copy(pub[16:], id[:])
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO volunteers (id, public_key, hardware_capabilities, available_runtimes, scheduling_mode, is_active, last_seen_at)
		VALUES ($1, $2, $3, $4, 'ALWAYS', true, now())`,
		id, pub,
		json.RawMessage(`{"cpu_cores":8,"max_cpu_cores":4,"memory_total_mb":32768,"max_memory_mb":16384,"disk_available_mb":102400,"max_disk_mb":10240}`),
		[]string{"NATIVE", "CONTAINER"},
	); err != nil {
		t.Fatalf("seed volunteer: %v", err)
	}
	return id
}

// e1SeedLeaf inserts an ACTIVE EXACT leaf with the given validation_config JSON.
func e1SeedLeaf(t *testing.T, pool *pgxpool.Pool, creatorID types.ID, validationConfig string) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, 'recovery test leaf', 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false}',
			$4,
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576}',
			'{"credit_per_validated_work_unit":1.5}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false}',
			false, 'PUBLIC', $5
		)`,
		id, "Leaf "+id.String()[:8], "leaf-"+id.String()[:8], json.RawMessage(validationConfig), creatorID,
	); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}
	return id
}

// e1SeedWorkUnit inserts a work unit in the given state with per-unit redundancy overrides.
// A COMPLETED unit's completed_at is backdated so the shape-1 age filter selects it under any
// non-negative grace.
func e1SeedWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID types.ID, state string, target, quorum, maxError, maxTotal int, spotCheck bool) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO work_units (
			id, leaf_id, state, priority, input_data, code_artifact_ref, parameters,
			deadline_seconds, max_reassignments,
			target_copies, min_quorum, max_error_copies, max_total_copies, spot_check,
			completed_at
		) VALUES (
			$1, $2, $3, 'NORMAL', $4, $5, $6, 3600, 3,
			$7, $8, $9, $10, $11,
			CASE WHEN $12 THEN now() - make_interval(secs => 120) ELSE NULL END
		)`,
		id, leafID, state, json.RawMessage(`{"x":1}`), "ref://bin", json.RawMessage(`{"i":1}`),
		target, quorum, maxError, maxTotal, spotCheck, state == "COMPLETED",
	); err != nil {
		t.Fatalf("seed work unit: %v", err)
	}
	return id
}

// e1SeedResult inserts a result with the given checksum, validation status, and age (created_at
// backdated by agoSecs so the shape-2 evidence-age filter selects it). Distinct checksums do not
// corroborate under the EXACT comparator; identical checksums agree.
func e1SeedResult(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, checksum, status string, agoSecs int) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO results (
			id, work_unit_id, volunteer_id, output_data, output_checksum,
			execution_metadata, validation_status, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, now() - make_interval(secs => $8))`,
		id, wuID, volID, json.RawMessage(`{"answer":42}`), checksum,
		json.RawMessage(e1ExecMeta), status, agoSecs,
	); err != nil {
		t.Fatalf("seed result: %v", err)
	}
	return id
}

// e1SeedHistory inserts an assignment-history copy row. outcome "" is a live copy (outcome NULL);
// a non-empty outcome (EXPIRED/ABANDONED) is a closed error copy.
func e1SeedHistory(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, outcome string) {
	t.Helper()
	var err error
	if outcome == "" {
		_, err = pool.Exec(context.Background(),
			`INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at) VALUES ($1, $2, now())`,
			wuID, volID)
	} else {
		_, err = pool.Exec(context.Background(),
			`INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at, outcome, outcome_at) VALUES ($1, $2, now(), $3, now())`,
			wuID, volID, outcome)
	}
	if err != nil {
		t.Fatalf("seed history (%q): %v", outcome, err)
	}
}

// --- assertions ---

func e1UnitState(t *testing.T, pool *pgxpool.Pool, id types.ID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM work_units WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read unit state: %v", err)
	}
	return s
}

func e1UnitFlagged(t *testing.T, pool *pgxpool.Pool, id types.ID) bool {
	t.Helper()
	var b bool
	if err := pool.QueryRow(context.Background(), `SELECT flagged_for_review FROM work_units WHERE id = $1`, id).Scan(&b); err != nil {
		t.Fatalf("read flagged: %v", err)
	}
	return b
}

func e1ReassignCount(t *testing.T, pool *pgxpool.Pool, id types.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT reassignment_count FROM work_units WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read reassignment_count: %v", err)
	}
	return n
}

func e1CreditRows(t *testing.T, pool *pgxpool.Pool, wuID types.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM credit_ledger WHERE work_unit_id = $1`, wuID).Scan(&n); err != nil {
		t.Fatalf("count credit rows: %v", err)
	}
	return n
}

func e1Checksum(a byte) string {
	// 64-hex output checksum; the leading byte differentiates agreeing vs conflicting sets.
	s := make([]byte, 64)
	for i := range s {
		s[i] = 'a'
	}
	s[0] = a
	return string(s)
}

// TestRecoverySweep_FinalizesStrandShapes (BG-21b/★E1-b′): every stranded finalization shape
// converges in one sweep. Seeds four strands and asserts each validates with credit rows.
func TestRecoverySweep_FinalizesStrandShapes(t *testing.T) {
	pool, cleanup := e1SetupTestDB(t)
	defer cleanup()
	st := e1NewStack(t, pool, 0, 100)
	ctx := context.Background()
	user := e1SeedUser(t, pool)
	lf := e1SeedLeaf(t, pool, user, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)

	agree := e1Checksum('b')

	// (a) gRPC shape: COMPLETED unit + quorum agreeing PENDING results.
	a := e1SeedWorkUnit(t, pool, lf, "COMPLETED", 2, 2, 0, 8, false)
	e1SeedResult(t, pool, a, e1SeedVolunteer(t, pool), agree, "PENDING", 120)
	e1SeedResult(t, pool, a, e1SeedVolunteer(t, pool), agree, "PENDING", 120)

	// (b) browser shape: QUEUED unit + quorum agreeing PENDING results.
	b := e1SeedWorkUnit(t, pool, lf, "QUEUED", 2, 2, 0, 8, false)
	e1SeedResult(t, pool, b, e1SeedVolunteer(t, pool), agree, "PENDING", 120)
	e1SeedResult(t, pool, b, e1SeedVolunteer(t, pool), agree, "PENDING", 120)

	// (c) abandon shape: like (b) plus a closed ABANDONED copy.
	c := e1SeedWorkUnit(t, pool, lf, "QUEUED", 2, 2, 0, 8, false)
	e1SeedResult(t, pool, c, e1SeedVolunteer(t, pool), agree, "PENDING", 120)
	e1SeedResult(t, pool, c, e1SeedVolunteer(t, pool), agree, "PENDING", 120)
	e1SeedHistory(t, pool, c, e1SeedVolunteer(t, pool), "ABANDONED")

	// (d) spot-check-reclaim shape: QUEUED, one PENDING, quorum resolves to 1.
	d := e1SeedWorkUnit(t, pool, lf, "QUEUED", 1, 1, 0, 8, false)
	e1SeedResult(t, pool, d, e1SeedVolunteer(t, pool), agree, "PENDING", 120)

	st.sweeper.RunOnce(ctx)

	for name, id := range map[string]types.ID{"grpc": a, "browser": b, "abandon": c, "spotcheck": d} {
		if got := e1UnitState(t, pool, id); got != "VALIDATED" {
			t.Errorf("%s strand: state = %s, want VALIDATED", name, got)
		}
		if e1CreditRows(t, pool, id) == 0 {
			t.Errorf("%s strand: no credit rows written", name)
		}
	}
}

// TestRecoverySweep_ReopenConverges (review #1/★E1-5): target 3 / quorum 2, two conflicting
// PENDING results, the third copy EXPIRED without submitting, zero live. Pre-arm the sweep
// re-drives this into WAIT forever; with the reopen arm it demotes to QUEUED. A fresh agreeing
// result then validates the majority.
func TestRecoverySweep_ReopenConverges(t *testing.T) {
	pool, cleanup := e1SetupTestDB(t)
	defer cleanup()
	st := e1NewStack(t, pool, 0, 100)
	ctx := context.Background()
	user := e1SeedUser(t, pool)
	// agreement_threshold 0.6 so a 2-of-3 majority can validate around the one dissenter.
	lf := e1SeedLeaf(t, pool, user, `{"redundancy_factor":2,"agreement_threshold":0.6,"comparison_mode":"EXACT","max_retries":3}`)

	wu := e1SeedWorkUnit(t, pool, lf, "COMPLETED", 3, 2, 0, 9, false)
	e1SeedResult(t, pool, wu, e1SeedVolunteer(t, pool), e1Checksum('b'), "PENDING", 120) // group A
	e1SeedResult(t, pool, wu, e1SeedVolunteer(t, pool), e1Checksum('c'), "PENDING", 120) // conflicting
	e1SeedHistory(t, pool, wu, e1SeedVolunteer(t, pool), "EXPIRED")                      // straggler died

	st.sweeper.RunOnce(ctx)

	if got := e1UnitState(t, pool, wu); got != "QUEUED" {
		t.Fatalf("after reopen sweep: state = %s, want QUEUED", got)
	}

	// A fresh corroborator agrees with group A; Evaluate now validates the majority.
	e1SeedResult(t, pool, wu, e1SeedVolunteer(t, pool), e1Checksum('b'), "PENDING", 1)
	if _, err := st.transitioner.Evaluate(ctx, wu); err != nil {
		t.Fatalf("Evaluate after fresh corroborator: %v", err)
	}
	if got := e1UnitState(t, pool, wu); got != "VALIDATED" {
		t.Fatalf("after fresh corroborator: state = %s, want VALIDATED", got)
	}
}

// TestRecoverySweep_StrandedRejectedRequeues: a pre-fix stranded REJECTED unit (0 pending,
// headroom) is requeued by the reopen arm's Reassign, bumping reassignment_count.
func TestRecoverySweep_StrandedRejectedRequeues(t *testing.T) {
	pool, cleanup := e1SetupTestDB(t)
	defer cleanup()
	st := e1NewStack(t, pool, 0, 100)
	ctx := context.Background()
	user := e1SeedUser(t, pool)
	lf := e1SeedLeaf(t, pool, user, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)

	wu := e1SeedWorkUnit(t, pool, lf, "REJECTED", 2, 2, 0, 8, false)
	before := e1ReassignCount(t, pool, wu)

	st.sweeper.RunOnce(ctx)

	if got := e1UnitState(t, pool, wu); got != "QUEUED" {
		t.Fatalf("after sweep: state = %s, want QUEUED", got)
	}
	if after := e1ReassignCount(t, pool, wu); after != before+1 {
		t.Errorf("reassignment_count = %d, want %d", after, before+1)
	}
}

// TestSweepShape2_ImmuneToClaimChurn (review #3): a QUEUED-at-quorum strand under continuous
// dispatch-claim churn (which bumps work_units.updated_at) is still selected by the shape-2
// finder, because that finder ages on the newest PENDING result, not on updated_at.
func TestSweepShape2_ImmuneToClaimChurn(t *testing.T) {
	pool, cleanup := e1SetupTestDB(t)
	defer cleanup()
	st := e1NewStack(t, pool, 5*time.Minute, 100)
	ctx := context.Background()
	user := e1SeedUser(t, pool)
	lf := e1SeedLeaf(t, pool, user, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)

	wu := e1SeedWorkUnit(t, pool, lf, "QUEUED", 2, 2, 0, 8, false)
	agree := e1Checksum('b')
	// Evidence aged 10 minutes — older than the 5-minute grace.
	e1SeedResult(t, pool, wu, e1SeedVolunteer(t, pool), agree, "PENDING", 600)
	e1SeedResult(t, pool, wu, e1SeedVolunteer(t, pool), agree, "PENDING", 600)

	// Stamp/renew a dispatch claim repeatedly — each write bumps updated_at to ~now(), which an
	// updated_at-anchored predicate would (wrongly) treat as fresh and exclude.
	claimer := e1SeedVolunteer(t, pool)
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx,
			`UPDATE work_units SET dispatch_claimed_by = $1, dispatch_claim_expires_at = now() + make_interval(secs => 120) WHERE id = $2`,
			claimer, wu); err != nil {
			t.Fatalf("stamp claim: %v", err)
		}
	}

	ids, err := st.wuRepo.FindStalledQueuedAtQuorum(ctx, 5*time.Minute, 100)
	if err != nil {
		t.Fatalf("FindStalledQueuedAtQuorum: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == wu {
			found = true
		}
	}
	if !found {
		t.Fatalf("shape-2 finder did not select the claim-churned unit %s (ids=%v)", wu, ids)
	}
}

// TestDeadLetterCompletedState: a COMPLETED unit with zero PENDING, >= 1 DISAGREED (so NOT the
// residue shape), no live copy, and an exhausted copy budget dead-letters in one Evaluate —
// red on the old QUEUED-only DeadLetterIfExhausted guard.
func TestDeadLetterCompletedState(t *testing.T) {
	pool, cleanup := e1SetupTestDB(t)
	defer cleanup()
	st := e1NewStack(t, pool, 0, 100)
	ctx := context.Background()
	user := e1SeedUser(t, pool)
	lf := e1SeedLeaf(t, pool, user, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)

	// max_total_copies = 2, two closed EXPIRED copies -> budget exhausted; one DISAGREED result
	// (not AGREED) keeps this off the residue-report shape and below quorum.
	wu := e1SeedWorkUnit(t, pool, lf, "COMPLETED", 2, 2, 0, 2, false)
	e1SeedResult(t, pool, wu, e1SeedVolunteer(t, pool), e1Checksum('b'), "DISAGREED", 120)
	e1SeedHistory(t, pool, wu, e1SeedVolunteer(t, pool), "EXPIRED")
	e1SeedHistory(t, pool, wu, e1SeedVolunteer(t, pool), "EXPIRED")

	if _, err := st.transitioner.Evaluate(ctx, wu); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := e1UnitState(t, pool, wu); got != "FAILED" {
		t.Fatalf("state = %s, want FAILED", got)
	}
	if !e1UnitFlagged(t, pool, wu) {
		t.Error("dead-lettered unit not flagged_for_review")
	}
}
