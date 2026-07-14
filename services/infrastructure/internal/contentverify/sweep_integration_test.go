//go:build integration

// DB-backed tests for the promote path's terminal-unit door (★BG-21h). They live IN-PACKAGE so
// they can drive the REAL claim/apply SQL — the production write path — with the finalization
// interleaved between the claim (which snapshots the unit state) and the apply (which used to
// trust it). DB-gated: without LETTUCE_TEST_DB_URL they skip. Run with:
//
//	GOWORK=off go test -tags integration -p 1 ./internal/contentverify/
package contentverify

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

func cvSetupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
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
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}
	return pool, cleanup
}

func cvSeedUser(t *testing.T, pool *pgxpool.Pool) types.ID {
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

func cvSeedVolunteer(t *testing.T, pool *pgxpool.Pool) types.ID {
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

func cvSeedLeaf(t *testing.T, pool *pgxpool.Pool, creatorID types.ID) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, 'content-verify door test leaf', 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3,"allow_external_output":true,"external_output_hosts":["results.example.com"]}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576}',
			'{"credit_per_validated_work_unit":1.5}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false}',
			false, 'PUBLIC', $4
		)`,
		id, "Leaf "+id.String()[:8], "leaf-"+id.String()[:8], creatorID,
	); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}
	return id
}

func cvSeedWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID types.ID, state string) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO work_units (
			id, leaf_id, state, priority, input_data, code_artifact_ref, parameters,
			deadline_seconds, max_reassignments, target_copies, min_quorum, max_error_copies,
			max_total_copies, spot_check
		) VALUES ($1, $2, $3, 'NORMAL', $4, $5, $6, 3600, 3, 2, 2, 0, 8, false)`,
		id, leafID, state, json.RawMessage(`{"x":1}`), "ref://bin", json.RawMessage(`{"i":1}`),
	); err != nil {
		t.Fatalf("seed work unit: %v", err)
	}
	return id
}

// cvSeedAwaitingResult inserts a held (AWAITING_CONTENT_VERIFICATION) ref result whose fetch
// is due now.
func cvSeedAwaitingResult(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO results (
			id, work_unit_id, volunteer_id, output_data, output_data_ref, output_checksum,
			execution_metadata, validation_status, content_fetch_attempts,
			content_fetch_next_attempt_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 'AWAITING_CONTENT_VERIFICATION', 0, now() - interval '1 minute', now())`,
		id, wuID, volID, json.RawMessage(`{}`), "https://results.example.com/out.json",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		json.RawMessage(`{"wall_clock_seconds":10}`),
	); err != nil {
		t.Fatalf("seed awaiting result: %v", err)
	}
	return id
}

func cvResultStatus(t *testing.T, pool *pgxpool.Pool, id types.ID) (status string, lastError *string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT validation_status, content_fetch_last_error FROM results WHERE id = $1`, id).
		Scan(&status, &lastError); err != nil {
		t.Fatalf("read result status: %v", err)
	}
	return status, lastError
}

func cvWorker(pool *pgxpool.Pool) *Worker {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return NewWorker(pool, nil, true, 100<<20, nil, logger)
}

// TestPromote_TerminalUnitDoor_RefusesMidFetchFinalization (★BG-21h): the unit finalizes
// BETWEEN the claim (which snapshots wu.state) and the apply pass (whose fetch window is up to
// the per-row deadline). The pure state machine promotes on the stale claim-time state — that
// lane is pinned by dispose_test — so the door must be in the WRITE: apply re-checks the unit
// state under the work_units row lock and terminates the row UNIT_FINALIZED instead of landing
// a PENDING result under a terminal unit, which no sweep shape or Evaluate could ever
// adjudicate. Pre-fix (b4210e9) this promotes: the row goes PENDING under VALIDATED and the
// E1-S assertion at the bottom fails.
func TestPromote_TerminalUnitDoor_RefusesMidFetchFinalization(t *testing.T) {
	pool, cleanup := cvSetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	w := cvWorker(pool)

	user := cvSeedUser(t, pool)
	lf := cvSeedLeaf(t, pool, user)
	wu := cvSeedWorkUnit(t, pool, lf, "QUEUED")
	res := cvSeedAwaitingResult(t, pool, wu, cvSeedVolunteer(t, pool))

	// The tick begins: claim the row (unit state snapshots as QUEUED).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin claim tx: %v", err)
	}
	defer tx.Rollback(ctx)
	rows, err := w.claim(ctx, tx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(rows) != 1 || rows[0].resultID != res {
		t.Fatalf("claim returned %d rows, want the seeded row", len(rows))
	}
	snap := rows[0]
	if snap.unitState != "QUEUED" {
		t.Fatalf("claim-time unit state = %s, want QUEUED", snap.unitState)
	}

	// Mid-fetch, the unit finalizes on another connection (a quorum of inline submits).
	if _, err := pool.Exec(ctx, `UPDATE work_units SET state = 'VALIDATED', validated_at = now() WHERE id = $1`, wu); err != nil {
		t.Fatalf("finalize unit mid-fetch: %v", err)
	}

	// The fetch succeeded, so the disposition is a promotion decided on the STALE claim-time
	// state (exactly what decide() yields for a QUEUED snapshot and a successful fetch).
	d := disposition{action: actionPromote, servedHash: snap.claimedChecksum}

	promoted, ok := w.apply(ctx, tx, snap, d)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit claim tx: %v", err)
	}

	if ok {
		t.Errorf("apply reported a landed promotion (%v) for a finalized unit; the door must refuse", promoted)
	}
	status, lastError := cvResultStatus(t, pool, res)
	if status != "CONTENT_VERIFICATION_FAILED" {
		t.Fatalf("result status = %s, want CONTENT_VERIFICATION_FAILED (door downgrade)", status)
	}
	if lastError == nil || !strings.HasPrefix(*lastError, CodeUnitFinalized) {
		t.Errorf("content_fetch_last_error = %v, want prefix %s", lastError, CodeUnitFinalized)
	}

	// E1-S: no PENDING result may sit under a terminal unit — the exact orphan class the
	// door exists to prevent (★E1-6 via the content-verify writer).
	var orphans int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM results r
		JOIN work_units wuq ON wuq.id = r.work_unit_id
		WHERE wuq.state IN ('VALIDATED','FAILED') AND r.validation_status = 'PENDING'`).Scan(&orphans); err != nil {
		t.Fatalf("E1-S query: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("E1-S violated: %d PENDING results under terminal units (content-verify orphan)", orphans)
	}
}

// TestPromote_NonTerminalUnitStillPromotes: the door must not over-refuse — a unit that is
// still QUEUED at apply time promotes exactly as before (PENDING on the served hash).
func TestPromote_NonTerminalUnitStillPromotes(t *testing.T) {
	pool, cleanup := cvSetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	w := cvWorker(pool)

	user := cvSeedUser(t, pool)
	lf := cvSeedLeaf(t, pool, user)
	wu := cvSeedWorkUnit(t, pool, lf, "QUEUED")
	res := cvSeedAwaitingResult(t, pool, wu, cvSeedVolunteer(t, pool))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin claim tx: %v", err)
	}
	defer tx.Rollback(ctx)
	rows, err := w.claim(ctx, tx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("claim returned %d rows, want 1", len(rows))
	}

	d := disposition{action: actionPromote, servedHash: rows[0].claimedChecksum}
	promoted, ok := w.apply(ctx, tx, rows[0], d)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit claim tx: %v", err)
	}

	if !ok || promoted.resultID != res || promoted.workUnitID != wu {
		t.Fatalf("promotion did not land (ok=%v, promoted=%+v)", ok, promoted)
	}
	status, _ := cvResultStatus(t, pool, res)
	if status != "PENDING" {
		t.Fatalf("result status = %s, want PENDING (door must not over-refuse)", status)
	}
}
