//go:build integration

package server_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// newFMTestKeyPair returns an Ed25519 keypair for fault-monitor fixtures. The key
// is only used to populate the volunteers.public_key column; the fault monitor
// runs no RPC, so the key is never used for signing.
func newFMTestKeyPair(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	return pub
}

// fmTestPool connects to the integration test database (skipping when unset).
func fmTestPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	cleanup := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM audit_repairs")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
		_, _ = pool.Exec(ctx, "DELETE FROM result_audits")
		_, _ = pool.Exec(ctx, "DELETE FROM trusted_runners")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}
	return pool, cleanup
}

func createFMTestUser(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	username := "fm-user-" + uuid.New().String()[:8]
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		id, username+"@test.example.com", username, "Test User "+username,
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("failed to create test user: %v", err)
	}
	return id
}

func createFMTestLeaf(t *testing.T, pool *pgxpool.Pool, creatorID *types.ID, state string) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	slug := "fm-leaf-" + uuid.New().String()[:8]
	_, err := pool.Exec(ctx, `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, $4, $5, 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false,"gpu_type":"","max_memory_mb":4096,"max_disk_mb":10240,"max_cpu_seconds":86400,"network_access":false,"min_vram_gb":0}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"deadline_multiplier":3.0,"max_reassignments":3,"checkpointing_enabled":false}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false,"min_bandwidth_mbps":0,"min_gpu_vram_mb":0}',
			false, 'PUBLIC', $6
		)`,
		id, "Test Leaf "+slug, slug, "A fault-monitor test leaf", state, creatorID,
	)
	if err != nil {
		t.Fatalf("failed to create test leaf: %v", err)
	}
	return id
}

// fmInsertVolunteer inserts an active NATIVE volunteer and returns its id.
func fmInsertVolunteer(t *testing.T, ctx context.Context, pool *pgxpool.Pool) types.ID {
	t.Helper()
	id := types.NewID()
	pubKey := []byte(newFMTestKeyPair(t))
	if _, err := pool.Exec(ctx, `
		INSERT INTO volunteers (id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at)
		VALUES ($1, $2, '{"cpu_cores":4,"cpu_model":"test","max_cpu_cores":4,"memory_total_mb":8192,"max_memory_mb":8192,"disk_available_mb":10240,"max_disk_mb":10240}',
			'{NATIVE}', 'ALWAYS', true, $3)`,
		id, pubKey, time.Now().UTC()); err != nil {
		t.Fatalf("create volunteer: %v", err)
	}
	return id
}

// fmNewMonitor builds a FaultMonitor over the real pgx repos. A real checkpoint repo
// (rooted at a temp dir) is wired so the dead-letter cleanup path never nil-derefs.
func fmNewMonitor(t *testing.T, pool *pgxpool.Pool) *server.FaultMonitor {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	checkpointRepo := checkpoint.NewPgxRepository(pool, t.TempDir())
	return server.NewFaultMonitor(wuRepo, assignRepo, checkpointRepo, nil, nil, nil, logger)
}

// fmInsertQueuedWU inserts a QUEUED work unit. Per-copy dispatch (migration 00006)
// keeps the unit QUEUED while its copies run; deadlineSeconds is the unit's deadline
// and maxTotalCopies is the dead-letter ceiling (0 = derive from redundancy).
func fmInsertQueuedWU(t *testing.T, ctx context.Context, pool *pgxpool.Pool, leafID types.ID, deadlineSeconds, maxTotalCopies int) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds, max_total_copies,
			reassignment_count, max_reassignments, flagged_for_review
		) VALUES (
			$1, $2, 'QUEUED', 'NORMAL',
			'{"x":1}', 'ref://test', '{"n":1}',
			1, $3, $4,
			0, 3, false
		)`, id, leafID, deadlineSeconds, maxTotalCopies); err != nil {
		t.Fatalf("insert QUEUED work unit: %v", err)
	}
	return id
}

// fmInsertCopy inserts one dispatched COPY (a work_unit_assignment_history row) of a
// unit with outcome NULL (a LIVE copy). A RUNNING copy passes a non-nil startedAt; a
// buffered RESERVED copy passes reservedUntil with a nil startedAt. deadlineSeconds is
// the per-copy snapshot the running-deadline sweep reads. Returns the copy id.
func fmInsertCopy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wuID, volID types.ID, assignedAt time.Time, startedAt, reservedUntil *time.Time, deadlineSeconds int) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO work_unit_assignment_history (
			id, work_unit_id, volunteer_id, assigned_at,
			reserved_until, started_at, deadline_seconds
		) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, wuID, volID, assignedAt, reservedUntil, startedAt, deadlineSeconds); err != nil {
		t.Fatalf("insert copy: %v", err)
	}
	return id
}

// fmCopyOutcome returns a copy's outcome (nil = still live).
func fmCopyOutcome(t *testing.T, ctx context.Context, pool *pgxpool.Pool, copyID types.ID) *string {
	t.Helper()
	var outcome *string
	if err := pool.QueryRow(ctx, "SELECT outcome FROM work_unit_assignment_history WHERE id = $1", copyID).Scan(&outcome); err != nil {
		t.Fatalf("query copy outcome: %v", err)
	}
	return outcome
}

// fmUnitState returns a work unit's state.
func fmUnitState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wuID types.ID) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", wuID).Scan(&state); err != nil {
		t.Fatalf("query unit state: %v", err)
	}
	return state
}

// TestFaultMonitorScanOnce_RunningCopyDeadlineExpiry asserts the per-copy deadline
// sweep: a RUNNING copy (started_at set) past started_at + deadline_seconds is closed
// with the EXPIRED outcome (FindExpiredCopies -> CloseCopy), while its work UNIT stays
// QUEUED so it immediately redispatches a fresh copy to a distinct volunteer
// (property 6). The unit is NOT dead-lettered: its total copies (1) is far below the
// retry ceiling (redundancy 2 + margin).
func TestFaultMonitorScanOnce_RunningCopyDeadlineExpiry(t *testing.T) {
	pool, cleanup := fmTestPool(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createFMTestUser(t, pool)
	leafID := createFMTestLeaf(t, pool, &userID, "ACTIVE")
	volunteerID := fmInsertVolunteer(t, ctx, pool)

	past := time.Now().UTC().Add(-2 * time.Hour)
	// QUEUED unit with one RUNNING copy started 2h ago with a 1s deadline -> expired.
	wuID := fmInsertQueuedWU(t, ctx, pool, leafID, 1, 0)
	copyID := fmInsertCopy(t, ctx, pool, wuID, volunteerID, past, &past, nil, 1)

	if err := fmNewMonitor(t, pool).ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	// The copy is closed EXPIRED (a run-started copy that missed its deadline).
	if oc := fmCopyOutcome(t, ctx, pool, copyID); oc == nil || *oc != "EXPIRED" {
		t.Errorf("running copy outcome = %v, want EXPIRED", oc)
	}
	// The unit stays QUEUED (it redispatches a fresh copy; not dead-lettered).
	if st := fmUnitState(t, ctx, pool, wuID); st != "QUEUED" {
		t.Errorf("unit state = %s, want QUEUED (redispatch, not dead-lettered)", st)
	}
}

// TestFaultMonitorScanOnce_BufferedCopyAbandon asserts the buffered-lapse reclaim: a
// RESERVED copy (started_at NULL) whose holder vanished before run-start is found once
// reserved_until < NOW() and closed with the ABANDONED outcome, leaving the unit plain
// QUEUED and immediately re-dispatchable. A RESERVED copy still within its lease is left
// untouched (outcome stays NULL). This is the load-bearing dead-holder reclaim now that
// per-task heartbeats are gone.
func TestFaultMonitorScanOnce_BufferedCopyAbandon(t *testing.T) {
	pool, cleanup := fmTestPool(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createFMTestUser(t, pool)
	leafID := createFMTestLeaf(t, pool, &userID, "ACTIVE")
	volunteerID := fmInsertVolunteer(t, ctx, pool)

	now := time.Now().UTC()
	// Lapsed buffered copy: reserved_until two minutes in the past, never run-started.
	lapsedWU := fmInsertQueuedWU(t, ctx, pool, leafID, 10800, 0)
	lapsedReserved := now.Add(-2 * time.Minute)
	lapsedCopy := fmInsertCopy(t, ctx, pool, lapsedWU, volunteerID, lapsedReserved, nil, &lapsedReserved, 10800)
	// Live buffered copy: reserved_until an hour in the future — must be left alone.
	liveWU := fmInsertQueuedWU(t, ctx, pool, leafID, 10800, 0)
	liveReserved := now.Add(time.Hour)
	liveCopy := fmInsertCopy(t, ctx, pool, liveWU, volunteerID, now, nil, &liveReserved, 10800)

	if err := fmNewMonitor(t, pool).ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	// The lapsed copy is closed ABANDONED (a buffered copy whose holder vanished).
	if oc := fmCopyOutcome(t, ctx, pool, lapsedCopy); oc == nil || *oc != "ABANDONED" {
		t.Errorf("lapsed buffered copy outcome = %v, want ABANDONED", oc)
	}
	if st := fmUnitState(t, ctx, pool, lapsedWU); st != "QUEUED" {
		t.Errorf("lapsed unit state = %s, want QUEUED", st)
	}
	// The live buffered copy is untouched (still RESERVED, outcome NULL).
	if oc := fmCopyOutcome(t, ctx, pool, liveCopy); oc != nil {
		t.Errorf("live buffered copy should be untouched, got outcome=%v", *oc)
	}
}

// TestFaultMonitorScanOnce_RunningDeadlineVsZero asserts the running-deadline sweep
// only fires for copies with a positive deadline: a RUNNING copy with deadline_seconds
// > 0, past its deadline, is closed EXPIRED, while a RUNNING copy with deadline_seconds
// = 0 (a NoDeadline leaf) is NEVER swept and stays live. The per-copy snapshot keeps
// the sweep index-driven with no join.
func TestFaultMonitorScanOnce_RunningDeadlineVsZero(t *testing.T) {
	pool, cleanup := fmTestPool(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createFMTestUser(t, pool)
	leafID := createFMTestLeaf(t, pool, &userID, "ACTIVE")
	volunteerID := fmInsertVolunteer(t, ctx, pool)

	past := time.Now().UTC().Add(-2 * time.Hour)
	// Positive-deadline RUNNING copy past its deadline -> reclaimable.
	ceilingWU := fmInsertQueuedWU(t, ctx, pool, leafID, 1, 0)
	ceilingCopy := fmInsertCopy(t, ctx, pool, ceilingWU, volunteerID, past, &past, nil, 1)
	// deadline_seconds=0 RUNNING copy -> never reclaimed.
	zeroWU := fmInsertQueuedWU(t, ctx, pool, leafID, 0, 0)
	zeroCopy := fmInsertCopy(t, ctx, pool, zeroWU, volunteerID, past, &past, nil, 0)

	if err := fmNewMonitor(t, pool).ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	if oc := fmCopyOutcome(t, ctx, pool, ceilingCopy); oc == nil || *oc != "EXPIRED" {
		t.Errorf("positive-deadline copy outcome = %v, want EXPIRED", oc)
	}
	if oc := fmCopyOutcome(t, ctx, pool, zeroCopy); oc != nil {
		t.Errorf("deadline_seconds=0 copy must never be swept, got outcome=%v", *oc)
	}
}

// TestFaultMonitorScanOnce_DeadLetterExhausted asserts the property-6 dead-letter
// ceiling: when a unit's last live copy times out AND it has reached its
// max_total_copies ceiling with redundancy still unmet and no live copy left,
// DeadLetterIfExhausted parks it FAILED + flagged-for-review (the ONLY cap on requeue).
func TestFaultMonitorScanOnce_DeadLetterExhausted(t *testing.T) {
	pool, cleanup := fmTestPool(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createFMTestUser(t, pool)
	leafID := createFMTestLeaf(t, pool, &userID, "ACTIVE")
	volunteerID := fmInsertVolunteer(t, ctx, pool)

	past := time.Now().UTC().Add(-2 * time.Hour)
	// max_total_copies = 1: a single timed-out copy exhausts the ceiling.
	wuID := fmInsertQueuedWU(t, ctx, pool, leafID, 1, 1)
	copyID := fmInsertCopy(t, ctx, pool, wuID, volunteerID, past, &past, nil, 1)

	if err := fmNewMonitor(t, pool).ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	// The copy is closed EXPIRED, and the now-exhausted unit is dead-lettered.
	if oc := fmCopyOutcome(t, ctx, pool, copyID); oc == nil || *oc != "EXPIRED" {
		t.Errorf("copy outcome = %v, want EXPIRED", oc)
	}
	var state string
	var flagged bool
	if err := pool.QueryRow(ctx,
		"SELECT state, flagged_for_review FROM work_units WHERE id = $1", wuID).
		Scan(&state, &flagged); err != nil {
		t.Fatalf("query dead-lettered unit: %v", err)
	}
	if state != "FAILED" {
		t.Errorf("exhausted unit state = %s, want FAILED (dead-lettered)", state)
	}
	if !flagged {
		t.Errorf("dead-lettered unit should be flagged_for_review")
	}
}
