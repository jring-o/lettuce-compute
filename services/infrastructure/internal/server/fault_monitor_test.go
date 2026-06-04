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

// TestFaultMonitorScanOnce_DeadlineExpiry asserts the deadline-based reassignment
// path: an ASSIGNED unit whose volunteer vanished after run-start is reclaimed once
// it is past its deadline (assigned_at + deadline_seconds < NOW()).
//
// With per-task heartbeats removed, this deadline sweep is the only liveness
// mechanism for run-started units. The NoDeadline synthetic-ceiling case and the
// lapsed-reservation reclaim case are owned by WP-HEAD-DEADLINE.
func TestFaultMonitorScanOnce_DeadlineExpiry(t *testing.T) {
	pool, cleanup := fmTestPool(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createFMTestUser(t, pool)
	leafID := createFMTestLeaf(t, pool, &userID, "ACTIVE")

	volunteerID := types.NewID()
	now := time.Now().UTC()
	pubKey := []byte(newFMTestKeyPair(t))
	_, err := pool.Exec(ctx, `
		INSERT INTO volunteers (id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at)
		VALUES ($1, $2, '{"cpu_cores":4,"cpu_model":"test","max_cpu_cores":4,"memory_total_mb":8192,"max_memory_mb":8192,"disk_available_mb":10240,"max_disk_mb":10240}',
			'{NATIVE}', 'ALWAYS', true, $3)`,
		volunteerID, pubKey, now)
	if err != nil {
		t.Fatalf("create volunteer: %v", err)
	}

	// Expired work unit: assigned 2 hours ago with a 1-second deadline.
	expiredWUID := types.NewID()
	pastTime := now.Add(-2 * time.Hour)
	_, err = pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds,
			assigned_volunteer_id, assigned_at,
			reassignment_count, max_reassignments, flagged_for_review
		) VALUES (
			$1, $2, 'ASSIGNED', 'NORMAL',
			'{"x": 1}', 'ref://test', '{"n": 1}',
			1, 1,
			$3, $4,
			0, 3, false
		)`, expiredWUID, leafID, volunteerID, pastTime)
	if err != nil {
		t.Fatalf("create expired work unit: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, expiredWUID, volunteerID, pastTime)
	if err != nil {
		t.Fatalf("create expired assignment history: %v", err)
	}

	// Run a single fault-monitor scan.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	monitor := server.NewFaultMonitor(wuRepo, assignRepo, nil, nil, logger)

	if err := monitor.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	// The expired unit should transition EXPIRED -> reassigned QUEUED.
	var expiredState string
	if err := pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", expiredWUID).Scan(&expiredState); err != nil {
		t.Fatalf("query expired state: %v", err)
	}
	if expiredState != "QUEUED" {
		t.Errorf("expired work unit state = %s, want QUEUED (reassigned)", expiredState)
	}

	// Its assignment history row should carry the EXPIRED outcome.
	var expiredOutcome *string
	if err := pool.QueryRow(ctx, "SELECT outcome FROM work_unit_assignment_history WHERE work_unit_id = $1", expiredWUID).Scan(&expiredOutcome); err != nil {
		t.Fatalf("query expired outcome: %v", err)
	}
	if expiredOutcome == nil || *expiredOutcome != "EXPIRED" {
		t.Errorf("expired assignment outcome = %v, want EXPIRED", expiredOutcome)
	}
}

// fmInsertReservedQueuedWU inserts a QUEUED unit that is reserved to volunteerID
// with the given reserved_until (pass a past time to simulate a lapsed lease). It
// mirrors the buffered-but-never-started state the dispatch cache produces.
func fmInsertReservedQueuedWU(t *testing.T, ctx context.Context, pool *pgxpool.Pool, leafID, volunteerID types.ID, reservedUntil time.Time) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds,
			reserved_until, reserved_volunteer_id,
			reassignment_count, max_reassignments, flagged_for_review
		) VALUES (
			$1, $2, 'QUEUED', 'NORMAL',
			'{"x": 1}', 'ref://test', '{"n": 1}',
			1, 10800,
			$3, $4,
			0, 3, false
		)`, id, leafID, reservedUntil, volunteerID)
	if err != nil {
		t.Fatalf("insert reserved QUEUED work unit: %v", err)
	}
	return id
}

// TestFaultMonitorScanOnce_LapsedReservationReclaim asserts the #22 lapsed-lease
// reclaim: a still-QUEUED unit whose reservation lapsed (its buffered holder
// vanished before StartWork) has its reservation cleared by the monitor, leaving the
// unit plain QUEUED and immediately re-stageable. A unit with a LIVE reservation is
// left untouched. This is the load-bearing dead-holder reclaim now that per-task
// heartbeats and lease-renewal are gone.
func TestFaultMonitorScanOnce_LapsedReservationReclaim(t *testing.T) {
	pool, cleanup := fmTestPool(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createFMTestUser(t, pool)
	leafID := createFMTestLeaf(t, pool, &userID, "ACTIVE")

	volunteerID := types.NewID()
	pubKey := []byte(newFMTestKeyPair(t))
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO volunteers (id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at)
		VALUES ($1, $2, '{"cpu_cores":4,"max_cpu_cores":4,"memory_total_mb":8192,"max_memory_mb":8192,"disk_available_mb":10240,"max_disk_mb":10240}',
			'{NATIVE}', 'ALWAYS', true, $3)`,
		volunteerID, pubKey, now); err != nil {
		t.Fatalf("create volunteer: %v", err)
	}

	// Lapsed reservation: reserved_until two minutes in the past.
	lapsedWUID := fmInsertReservedQueuedWU(t, ctx, pool, leafID, volunteerID, now.Add(-2*time.Minute))
	// Live reservation: reserved_until an hour in the future — must be left alone.
	liveWUID := fmInsertReservedQueuedWU(t, ctx, pool, leafID, volunteerID, now.Add(time.Hour))

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	monitor := server.NewFaultMonitor(wuRepo, assignRepo, nil, nil, logger)

	if err := monitor.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	// The lapsed unit stays QUEUED but its reservation is cleared.
	var state string
	var reservedUntil *time.Time
	var reservedVol *types.ID
	if err := pool.QueryRow(ctx,
		"SELECT state, reserved_until, reserved_volunteer_id FROM work_units WHERE id = $1", lapsedWUID).
		Scan(&state, &reservedUntil, &reservedVol); err != nil {
		t.Fatalf("query lapsed unit: %v", err)
	}
	if state != "QUEUED" {
		t.Errorf("lapsed unit state = %s, want QUEUED", state)
	}
	if reservedUntil != nil || reservedVol != nil {
		t.Errorf("lapsed unit reservation not cleared: until=%v vol=%v", reservedUntil, reservedVol)
	}

	// The live reservation is untouched.
	var liveVol *types.ID
	if err := pool.QueryRow(ctx,
		"SELECT reserved_volunteer_id FROM work_units WHERE id = $1", liveWUID).Scan(&liveVol); err != nil {
		t.Fatalf("query live unit: %v", err)
	}
	if liveVol == nil || *liveVol != volunteerID {
		t.Errorf("live reservation should be untouched, got reserved_volunteer_id=%v", liveVol)
	}
}

// TestFaultMonitorScanOnce_NoDeadlineCeilingReclaim asserts that a unit carrying the
// synthetic NoDeadline ceiling (deadline_seconds > 0, what ResolveDeadlineSeconds now
// stamps for a NoDeadline leaf) IS reclaimed once it is past that ceiling, while a
// unit with deadline_seconds = 0 (the OLD NoDeadline behavior) is NEVER reclaimed.
// This is the guarantee that, with heartbeats gone, a NoDeadline unit on a vanished
// volunteer is no longer permanently stranded.
func TestFaultMonitorScanOnce_NoDeadlineCeilingReclaim(t *testing.T) {
	pool, cleanup := fmTestPool(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createFMTestUser(t, pool)
	leafID := createFMTestLeaf(t, pool, &userID, "ACTIVE")

	volunteerID := types.NewID()
	pubKey := []byte(newFMTestKeyPair(t))
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO volunteers (id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at)
		VALUES ($1, $2, '{"cpu_cores":4,"max_cpu_cores":4,"memory_total_mb":8192,"max_memory_mb":8192,"disk_available_mb":10240,"max_disk_mb":10240}',
			'{NATIVE}', 'ALWAYS', true, $3)`,
		volunteerID, pubKey, now); err != nil {
		t.Fatalf("create volunteer: %v", err)
	}

	// ceilingWU: stamped with a small synthetic "ceiling" (1s here for test speed),
	// assigned 2h ago — past the ceiling, so it must be reclaimed. This models the
	// NoDeadline-ceiling unit (deadline_seconds > 0).
	insertAssigned := func(deadline int) types.ID {
		id := types.NewID()
		past := now.Add(-2 * time.Hour)
		if _, err := pool.Exec(ctx, `
			INSERT INTO work_units (
				id, leaf_id, state, priority,
				input_data, code_artifact_ref, parameters,
				estimated_duration_seconds, deadline_seconds,
				assigned_volunteer_id, assigned_at,
				reassignment_count, max_reassignments, flagged_for_review
			) VALUES (
				$1, $2, 'ASSIGNED', 'NORMAL',
				'{"x":1}', 'ref://test', '{"n":1}',
				1, $5,
				$3, $4,
				0, 3, false
			)`, id, leafID, volunteerID, past, deadline); err != nil {
			t.Fatalf("insert assigned work unit (deadline=%d): %v", deadline, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
			VALUES ($1, $2, $3)`, id, volunteerID, past); err != nil {
			t.Fatalf("insert assignment history (deadline=%d): %v", deadline, err)
		}
		return id
	}
	ceilingWUID := insertAssigned(1)  // synthetic ceiling -> reclaimable
	zeroWUID := insertAssigned(0)     // deadline_seconds=0 -> never reclaimed

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	monitor := server.NewFaultMonitor(wuRepo, assignRepo, nil, nil, logger)

	if err := monitor.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	var ceilingState string
	if err := pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", ceilingWUID).Scan(&ceilingState); err != nil {
		t.Fatalf("query ceiling unit: %v", err)
	}
	if ceilingState != "QUEUED" {
		t.Errorf("ceiling (NoDeadline-synthetic) unit state = %s, want QUEUED (reassigned)", ceilingState)
	}

	var zeroState string
	if err := pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", zeroWUID).Scan(&zeroState); err != nil {
		t.Fatalf("query zero-deadline unit: %v", err)
	}
	if zeroState != "ASSIGNED" {
		t.Errorf("deadline_seconds=0 unit state = %s, want ASSIGNED (never reclaimed); the synthetic ceiling is why NoDeadline units no longer strand", zeroState)
	}
}
