//go:build integration

// Package validation_test drives the E1 finalization transaction against a real Postgres. These
// are the closeout refutation targets (design §9): each seeds a finalization scenario directly in
// SQL and asserts the atomic accept/reject transaction (§4.1) holds under fault injection,
// stale-snapshot races, concurrent duplicate Evaluates, and the reject+requeue path. They are
// DB-gated: without LETTUCE_TEST_DB_URL they skip. Run with:
//
//	GOWORK=off go test -tags integration -p 1 ./internal/validation/
//
// The stack under test is production-shaped: a real validation Engine wired with the production
// FinalizationTxRunner (so accept/reject is atomic), driven through a real Transitioner. The
// failing-credits fault is injected INSIDE the real transaction via the test-only decorate hook
// (SetFinalizationDecorateHook), so a mid-accept credit failure exercises a genuine rollback.
package validation_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"

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

const finExecMeta = `{"wall_clock_seconds":10,"cpu_seconds_user":8,"cpu_seconds_system":1,"cpu_cores_used":1,"peak_memory_mb":128}`

// finStack bundles the production-shaped components the finalization tests drive.
type finStack struct {
	pool         *pgxpool.Pool
	wuRepo       *workunit.PgxWorkUnitRepository
	leafRepo     *leaf.PgxRepository
	resultRepo   *result.PgxRepository
	engine       *validation.Engine
	runner       validation.FinalizationTxRunner
	transitioner *transition.Transitioner
}

func finSetupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
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

// finNewStack wires the real engine (+ production tx runner) and a real transitioner. A
// NoopLocker stands in for the advisory lock, so the tx's own unit-row lock is the only
// serializer — exactly the degraded-lock regime the design must be correct under (★E1-4).
func finNewStack(t *testing.T, pool *pgxpool.Pool) *finStack {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	leafRepo := leaf.NewPgxRepository(pool)
	resultRepo := result.NewPgxRepository(pool)
	runner := validation.NewPgxFinalizationTxRunner(pool)

	engine := validation.NewEngine(
		resultRepo, wuRepo, leafRepo,
		credit.NewPgxRepository(pool), credit.NewPgxRACRepository(pool),
		volunteer.NewPgxRepository(pool), assignment.NewPgxRepository(pool),
		nil, nil, nil, logger, nil, transition.TrustPolicy{},
	).WithTxRunner(runner)

	tr := transition.NewTransitioner(transition.NoopLocker{}, wuRepo, leafRepo, resultRepo, engine, transition.TrustPolicy{}, logger)
	return &finStack{pool: pool, wuRepo: wuRepo, leafRepo: leafRepo, resultRepo: resultRepo, engine: engine, runner: runner, transitioner: tr}
}

// --- self-contained SQL seeding (prefixed fin* to avoid the internal test helpers) ---

func finSeedUser(t *testing.T, pool *pgxpool.Pool) types.ID {
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

func finSeedVolunteer(t *testing.T, pool *pgxpool.Pool) types.ID {
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

func finSeedLeaf(t *testing.T, pool *pgxpool.Pool, creatorID types.ID, validationConfig string) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, 'finalization test leaf', 'ACTIVE', 'PARAMETER_SWEEP',
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

func finSeedWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID types.ID, state string, target, quorum, maxTotal int) types.ID {
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
			$7, $8, 0, $9, false,
			CASE WHEN $10 THEN now() - make_interval(secs => 120) ELSE NULL END
		)`,
		id, leafID, state, json.RawMessage(`{"x":1}`), "ref://bin", json.RawMessage(`{"i":1}`),
		target, quorum, maxTotal, state == "COMPLETED",
	); err != nil {
		t.Fatalf("seed work unit: %v", err)
	}
	return id
}

func finSeedResult(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, checksum, status string) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO results (
			id, work_unit_id, volunteer_id, output_data, output_checksum,
			execution_metadata, validation_status, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, now())`,
		id, wuID, volID, json.RawMessage(`{"answer":42}`), checksum,
		json.RawMessage(finExecMeta), status,
	); err != nil {
		t.Fatalf("seed result: %v", err)
	}
	return id
}

// finChecksum builds a 64-hex output checksum; the leading byte differentiates agreeing sets.
func finChecksum(a byte) string {
	s := make([]byte, 64)
	for i := range s {
		s[i] = 'a'
	}
	s[0] = a
	return string(s)
}

// finSeedHistory inserts an assignment-history copy row: outcome "" is a live copy (outcome
// NULL); a non-empty outcome (EXPIRED/ABANDONED) is a closed error copy.
func finSeedHistory(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, outcome string) {
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

func finUnitState(t *testing.T, pool *pgxpool.Pool, id types.ID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM work_units WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read unit state: %v", err)
	}
	return s
}

func finReassignCount(t *testing.T, pool *pgxpool.Pool, id types.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT reassignment_count FROM work_units WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read reassignment_count: %v", err)
	}
	return n
}

func finStatusCount(t *testing.T, pool *pgxpool.Pool, wuID types.ID, status string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM results WHERE work_unit_id = $1 AND validation_status = $2`, wuID, status).Scan(&n); err != nil {
		t.Fatalf("count results (%s): %v", status, err)
	}
	return n
}

func finCreditRows(t *testing.T, pool *pgxpool.Pool, wuID types.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM credit_ledger WHERE work_unit_id = $1`, wuID).Scan(&n); err != nil {
		t.Fatalf("count credit rows: %v", err)
	}
	return n
}

// finAssertE1S asserts the whole-DB E1-S safety invariant: no VALIDATED unit has an AGREED result
// without a ledger row, and no VALIDATED/FAILED unit has a PENDING result (design §9).
func finAssertE1S(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var uncredited int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM results r
		JOIN work_units wu ON wu.id = r.work_unit_id
		WHERE wu.state = 'VALIDATED' AND r.validation_status = 'AGREED'
		AND NOT EXISTS (SELECT 1 FROM credit_ledger cl WHERE cl.result_id = r.id)`).Scan(&uncredited); err != nil {
		t.Fatalf("E1-S uncredited query: %v", err)
	}
	if uncredited != 0 {
		t.Errorf("E1-S violated: %d VALIDATED+AGREED results without a ledger row", uncredited)
	}
	var pendingUnderTerminal int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM results r
		JOIN work_units wu ON wu.id = r.work_unit_id
		WHERE wu.state IN ('VALIDATED','FAILED') AND r.validation_status = 'PENDING'`).Scan(&pendingUnderTerminal); err != nil {
		t.Fatalf("E1-S pending-under-terminal query: %v", err)
	}
	if pendingUnderTerminal != 0 {
		t.Errorf("E1-S violated: %d PENDING results under a terminal unit", pendingUnderTerminal)
	}
}

// failingCredits wraps a credit.Repository so Create/CreateCapped always fail — injected INSIDE
// the finalization transaction to prove a mid-accept credit failure rolls the whole tx back.
type failingCredits struct {
	credit.Repository
	err error
}

func (f failingCredits) Create(context.Context, *credit.LedgerEntry) error { return f.err }
func (f failingCredits) CreateCapped(context.Context, *credit.LedgerEntry, float64) (bool, error) {
	return false, f.err
}

// TestFinalizationAtomicity_CreditFailureRollsBackMarks (BG-21c/★E1-1): a credit write failing
// mid-accept rolls back the entire finalization transaction — the unit stays COMPLETED, every
// result stays PENDING, and no ledger row exists. Healing the fault and re-driving then validates
// with exactly one ledger row per AGREED result.
func TestFinalizationAtomicity_CreditFailureRollsBackMarks(t *testing.T) {
	pool, cleanup := finSetupTestDB(t)
	defer cleanup()
	st := finNewStack(t, pool)
	ctx := context.Background()

	user := finSeedUser(t, pool)
	lf := finSeedLeaf(t, pool, user, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)
	wu := finSeedWorkUnit(t, pool, lf, "COMPLETED", 2, 2, 8)
	agree := finChecksum('b')
	finSeedResult(t, pool, wu, finSeedVolunteer(t, pool), agree, "PENDING")
	finSeedResult(t, pool, wu, finSeedVolunteer(t, pool), agree, "PENDING")

	// Inject the failing-credits fault inside the real transaction.
	validation.SetFinalizationDecorateHook(st.runner, func(s validation.FinalizationStores) validation.FinalizationStores {
		s.Credits = failingCredits{Repository: s.Credits, err: errors.New("credit write failed")}
		return s
	})

	if _, err := st.transitioner.Evaluate(ctx, wu); err == nil {
		t.Fatal("expected Evaluate to fail while credit writes fail")
	}

	// Everything rolled back: COMPLETED, both PENDING, zero ledger rows.
	if got := finUnitState(t, pool, wu); got != "COMPLETED" {
		t.Fatalf("after failed accept: state = %s, want COMPLETED", got)
	}
	if got := finStatusCount(t, pool, wu, "PENDING"); got != 2 {
		t.Fatalf("after failed accept: PENDING results = %d, want 2 (marks rolled back)", got)
	}
	if got := finCreditRows(t, pool, wu); got != 0 {
		t.Fatalf("after failed accept: credit rows = %d, want 0", got)
	}
	finAssertE1S(t, pool)

	// Heal the fault and re-drive: the unit validates with exactly one ledger row per AGREED result.
	validation.SetFinalizationDecorateHook(st.runner, nil)
	if _, err := st.transitioner.Evaluate(ctx, wu); err != nil {
		t.Fatalf("Evaluate after healing: %v", err)
	}
	if got := finUnitState(t, pool, wu); got != "VALIDATED" {
		t.Fatalf("after healing: state = %s, want VALIDATED", got)
	}
	if got := finStatusCount(t, pool, wu, "AGREED"); got != 2 {
		t.Errorf("AGREED results = %d, want 2", got)
	}
	if got := finCreditRows(t, pool, wu); got != 2 {
		t.Errorf("credit rows = %d, want 2 (one per AGREED result)", got)
	}
	finAssertE1S(t, pool)
}

// TestFinalizationStaleSnapshot_AbortsAndRetryAdjudicatesAll (review #2a): a submit landing
// between the snapshot and the accept lock (simulated by adjudicating with a stale rawPendingCount)
// aborts the accept with ErrStaleSnapshot and writes nothing; re-running with the true count
// adjudicates the full set with zero PENDING left behind.
func TestFinalizationStaleSnapshot_AbortsAndRetryAdjudicatesAll(t *testing.T) {
	pool, cleanup := finSetupTestDB(t)
	defer cleanup()
	st := finNewStack(t, pool)
	ctx := context.Background()

	user := finSeedUser(t, pool)
	lf := finSeedLeaf(t, pool, user, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)
	wu := finSeedWorkUnit(t, pool, lf, "COMPLETED", 3, 2, 9)
	agree := finChecksum('b')
	finSeedResult(t, pool, wu, finSeedVolunteer(t, pool), agree, "PENDING")
	finSeedResult(t, pool, wu, finSeedVolunteer(t, pool), agree, "PENDING")

	wuObj, err := st.wuRepo.GetByID(ctx, wu)
	if err != nil {
		t.Fatalf("load unit: %v", err)
	}
	lfObj, err := st.leafRepo.GetByID(ctx, lf)
	if err != nil {
		t.Fatalf("load leaf: %v", err)
	}
	policy := transition.ResolvePolicyWithTrust(lfObj, wuObj, transition.TrustPolicy{})

	// Snapshot of the pending set BEFORE the racing submit lands (2 results).
	pendingBefore := finLoadPending(t, ctx, st.resultRepo, wu)
	if len(pendingBefore) != 2 {
		t.Fatalf("pendingBefore = %d, want 2", len(pendingBefore))
	}
	majorityBefore, err := st.engine.Compare(ctx, wuObj, lfObj, st.engine.FilterPending(pendingBefore))
	if err != nil {
		t.Fatalf("compare (before): %v", err)
	}
	verdictBefore := transition.BuildComparisonVerdict(pendingBefore, majorityBefore, policy.TrustFloor)

	// The racing submit lands: a third PENDING result appears after the snapshot was taken.
	finSeedResult(t, pool, wu, finSeedVolunteer(t, pool), agree, "PENDING")

	// Accepting against the stale snapshot count (2) must abort on the in-tx recheck (real 3).
	err = st.engine.ApplyAccept(ctx, wuObj, lfObj, pendingBefore, majorityBefore, verdictBefore, policy, len(pendingBefore))
	if !errors.Is(err, transition.ErrStaleSnapshot) {
		t.Fatalf("ApplyAccept(stale) error = %v, want errors.Is(ErrStaleSnapshot)", err)
	}
	// Nothing written.
	if got := finUnitState(t, pool, wu); got != "COMPLETED" {
		t.Fatalf("after stale abort: state = %s, want COMPLETED", got)
	}
	if got := finStatusCount(t, pool, wu, "PENDING"); got != 3 {
		t.Fatalf("after stale abort: PENDING = %d, want 3 (nothing adjudicated)", got)
	}
	if got := finCreditRows(t, pool, wu); got != 0 {
		t.Fatalf("after stale abort: credit rows = %d, want 0", got)
	}

	// Re-drive with a fresh snapshot: the full set adjudicates, zero PENDING remain.
	if _, err := st.transitioner.Evaluate(ctx, wu); err != nil {
		t.Fatalf("Evaluate (fresh): %v", err)
	}
	if got := finUnitState(t, pool, wu); got != "VALIDATED" {
		t.Fatalf("after fresh drive: state = %s, want VALIDATED", got)
	}
	if got := finStatusCount(t, pool, wu, "PENDING"); got != 0 {
		t.Errorf("PENDING after fresh drive = %d, want 0 (no orphan)", got)
	}
	if got := finStatusCount(t, pool, wu, "AGREED"); got != 3 {
		t.Errorf("AGREED after fresh drive = %d, want 3 (full set adjudicated)", got)
	}
	if got := finCreditRows(t, pool, wu); got != 3 {
		t.Errorf("credit rows = %d, want 3", got)
	}
	finAssertE1S(t, pool)
}

// TestFinalizationNoDoubleCredit_ConcurrentEvaluateStorm (§9): N concurrent Evaluates on a
// quorum-ready unit, under NoopLocker (degraded advisory lock), produce exactly one VALIDATED flip
// and exactly one ledger row per AGREED result — the tx unit-row lock + uq_credit_ledger_result
// are the whole defense.
func TestFinalizationNoDoubleCredit_ConcurrentEvaluateStorm(t *testing.T) {
	pool, cleanup := finSetupTestDB(t)
	defer cleanup()
	st := finNewStack(t, pool)
	ctx := context.Background()

	user := finSeedUser(t, pool)
	lf := finSeedLeaf(t, pool, user, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)
	wu := finSeedWorkUnit(t, pool, lf, "COMPLETED", 2, 2, 8)
	agree := finChecksum('b')
	finSeedResult(t, pool, wu, finSeedVolunteer(t, pool), agree, "PENDING")
	finSeedResult(t, pool, wu, finSeedVolunteer(t, pool), agree, "PENDING")

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			// Errors (ErrStaleSnapshot on the losers, then a terminal no-op) are expected and
			// harmless; the DB state is the assertion.
			_, _ = st.transitioner.Evaluate(ctx, wu)
		}()
	}
	wg.Wait()

	if got := finUnitState(t, pool, wu); got != "VALIDATED" {
		t.Fatalf("after storm: state = %s, want exactly-once VALIDATED", got)
	}
	if got := finCreditRows(t, pool, wu); got != 2 {
		t.Fatalf("after storm: credit rows = %d, want exactly 2 (no double credit)", got)
	}
	if got := finStatusCount(t, pool, wu, "PENDING"); got != 0 {
		t.Errorf("after storm: PENDING = %d, want 0", got)
	}
	finAssertE1S(t, pool)
}

// TestRejectRequeueAtomic (BG-21): a non-agreeing full set with no live copies and no dispatch
// headroom rejects and requeues in one transaction — after Evaluate the unit is QUEUED (requeued)
// with all results DISAGREED and the reassignment count bumped.
func TestRejectRequeueAtomic(t *testing.T) {
	pool, cleanup := finSetupTestDB(t)
	defer cleanup()
	st := finNewStack(t, pool)
	ctx := context.Background()

	user := finSeedUser(t, pool)
	lf := finSeedLeaf(t, pool, user, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)
	// target == quorum == 2 with two conflicting results and no live copies: countable coverage is
	// met (no headroom), so Decide rejects the round rather than waiting.
	wu := finSeedWorkUnit(t, pool, lf, "COMPLETED", 2, 2, 8)
	finSeedResult(t, pool, wu, finSeedVolunteer(t, pool), finChecksum('b'), "PENDING")
	finSeedResult(t, pool, wu, finSeedVolunteer(t, pool), finChecksum('c'), "PENDING") // conflicting

	before := finReassignCount(t, pool, wu)
	if _, err := st.transitioner.Evaluate(ctx, wu); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if got := finUnitState(t, pool, wu); got != "QUEUED" {
		t.Fatalf("after reject: state = %s, want QUEUED (reject+requeue committed together)", got)
	}
	if got := finStatusCount(t, pool, wu, "DISAGREED"); got != 2 {
		t.Errorf("DISAGREED results = %d, want 2", got)
	}
	if got := finStatusCount(t, pool, wu, "PENDING"); got != 0 {
		t.Errorf("PENDING results = %d, want 0", got)
	}
	if after := finReassignCount(t, pool, wu); after != before+1 {
		t.Errorf("reassignment_count = %d, want %d", after, before+1)
	}
	finAssertE1S(t, pool)
}

// TestDeadLetterDisposesBelowQuorumPending (★BG-21i): a unit that dead-letters while holding
// a below-quorum PENDING result must not leave that row PENDING under the FAILED unit — that
// is a permanent E1-S orphan no recovery shape can ever see (every sweep predicate excludes
// terminal units and Evaluate no-ops on them). The disposal is SUPERSEDED (migration 00027),
// not DISAGREED: the row was never compared against anything, so it is not an error signal.
// Driven through the PRODUCTION path — the real transitioner's ActionDeadLetter arm over the
// real executor — with the whole-DB E1-S invariant asserted at the end. Pre-fix (55e99fb) the
// row survives PENDING under FAILED and finAssertE1S fails.
func TestDeadLetterDisposesBelowQuorumPending(t *testing.T) {
	pool, cleanup := finSetupTestDB(t)
	defer cleanup()
	st := finNewStack(t, pool)
	ctx := context.Background()

	user := finSeedUser(t, pool)
	lf := finSeedLeaf(t, pool, user, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)
	// quorum 2, max_total_copies 2: one volunteer submitted (1 PENDING, below quorum), the
	// other two copies EXPIRED — budget spent, nothing live, quorum unreachable.
	wu := finSeedWorkUnit(t, pool, lf, "QUEUED", 2, 2, 2)
	res := finSeedResult(t, pool, wu, finSeedVolunteer(t, pool), finChecksum('b'), "PENDING")
	finSeedHistory(t, pool, wu, finSeedVolunteer(t, pool), "EXPIRED")
	finSeedHistory(t, pool, wu, finSeedVolunteer(t, pool), "EXPIRED")

	outcome, err := st.transitioner.Evaluate(ctx, wu)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if outcome != transition.OutcomeDeadLettered {
		t.Fatalf("Evaluate outcome = %q, want FAILED (dead-letter)", outcome)
	}
	if got := finUnitState(t, pool, wu); got != "FAILED" {
		t.Fatalf("state = %s, want FAILED", got)
	}
	if got := finStatusCount(t, pool, wu, "PENDING"); got != 0 {
		t.Fatalf("PENDING under the FAILED unit = %d, want 0 (disposal must be atomic with the flip)", got)
	}
	if got := finStatusCount(t, pool, wu, "SUPERSEDED"); got != 1 {
		t.Fatalf("SUPERSEDED = %d, want 1 (the below-quorum row, disposed not orphaned)", got)
	}
	// SUPERSEDED is not an error signal: the wasted-work tally still counts only the two
	// EXPIRED copies, exactly as before the disposal.
	if got, err := st.wuRepo.CountErrorCopies(ctx, wu); err != nil {
		t.Fatalf("CountErrorCopies: %v", err)
	} else if got != 2 {
		t.Errorf("error copies = %d, want 2 (a SUPERSEDED result must not count as an error)", got)
	}
	if got := finCreditRows(t, pool, wu); got != 0 {
		t.Errorf("credit rows = %d, want 0 (nothing validated)", got)
	}
	_ = res
	finAssertE1S(t, pool)
}

// finLoadPending loads the PENDING results of a unit in insertion order.
func finLoadPending(t *testing.T, ctx context.Context, repo *result.PgxRepository, wuID types.ID) []*result.Result {
	t.Helper()
	all, err := repo.ListByWorkUnit(ctx, wuID)
	if err != nil {
		t.Fatalf("list results: %v", err)
	}
	var pending []*result.Result
	for _, r := range all {
		if r.ValidationStatus == result.ValidationPending {
			pending = append(pending, r)
		}
	}
	return pending
}
