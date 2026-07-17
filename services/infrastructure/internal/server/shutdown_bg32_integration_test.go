//go:build integration

package server

// Regression tests for the graceful-shutdown ordering defects (BG-32 / BG-32b):
// the pre-fix shutdown closed the database pool BEFORE cancelling the background
// jobs, so (BG-32b) a leader replica deadlocked forever inside pool.Close() on
// the leadership manager's dedicated advisory-lock connection — every deploy of
// a leader ended in Docker's stop_grace_period SIGKILL — and (BG-32) the
// dispatch cache's final best-effort reservation flush ran against a closed
// pool and was lost. The fixed tail (StopBackgroundAndClosePool) cancels, joins
// bounded, THEN closes.
//
// Differential verification: on pre-fix ORDER (pool.Close() before cancelJobs
// inside StopBackgroundAndClosePool) the leader subtest times out and the flush
// subtest finds no landed copy; on the fixed order both pass. Like the rest of
// the suite these skip unless LETTUCE_TEST_DB_URL is set and must run with -p 1
// (shared database, migrations applied first).

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

func bg32TestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	return pool
}

func bg32TestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// bg32Fixtures inserts the minimal row set a reservation flush needs to land: a
// user, an ACTIVE redundancy-1 leaf, a volunteer, and a QUEUED work unit.
// Mirrors the internal/workunit dispatch-repo test fixtures.
func bg32Fixtures(t *testing.T, pool *pgxpool.Pool) (unitID, volunteerID types.ID) {
	t.Helper()
	ctx := context.Background()

	userID := types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		userID,
		"bg32-shutdown-"+uuid.New().String()[:8]+"@test.example.com",
		"bg32-shutdown-"+uuid.New().String()[:8],
		"BG32 Shutdown Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	); err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	leafID := types.NewID()
	slug := "bg32-leaf-" + uuid.New().String()[:8]
	if _, err := pool.Exec(ctx, `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, $4, 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false}',
			'{"redundancy_factor":1,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false,"min_gpu_vram_mb":0}',
			false, 'PUBLIC', $5
		)`,
		leafID, "BG32 Leaf "+slug, slug, "Shutdown-flush regression leaf", userID,
	); err != nil {
		t.Fatalf("insert test leaf: %v", err)
	}

	volunteerID = types.NewID()
	id1, id2 := uuid.New(), uuid.New()
	pubKey := make([]byte, 32)
	copy(pubKey, id1[:])
	copy(pubKey[16:], id2[:])
	if _, err := pool.Exec(ctx, `
		INSERT INTO volunteers (
			id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at
		) VALUES ($1, $2, $3, $4, 'ALWAYS', true, $5)`,
		volunteerID, pubKey,
		`{"cpu_cores":8,"max_cpu_cores":4,"memory_total_mb":32768,"max_memory_mb":16384,"disk_available_mb":102400,"max_disk_mb":10240}`,
		[]string{"NATIVE"},
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("insert test volunteer: %v", err)
	}

	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	wu := &workunit.WorkUnit{
		LeafID:           leafID,
		State:            workunit.WorkUnitStateQueued,
		Priority:         workunit.WorkUnitPriorityNormal,
		InputData:        json.RawMessage(`{"x": 42}`),
		CodeArtifactRef:  "ref://bg32-" + uuid.New().String()[:8],
		Parameters:       json.RawMessage(`{"iterations": 1}`),
		DeadlineSeconds:  3600,
		MaxReassignments: 3,
	}
	if err := wuRepo.Create(ctx, wu); err != nil {
		t.Fatalf("create queued work unit: %v", err)
	}

	t.Cleanup(func() {
		// The pool under test is closed by the shutdown tail; clean on a fresh one.
		cpool, err := pgxpool.New(context.Background(), os.Getenv("LETTUCE_TEST_DB_URL"))
		if err != nil {
			return
		}
		defer cpool.Close()
		cctx := context.Background()
		_, _ = cpool.Exec(cctx, "DELETE FROM work_unit_assignment_history WHERE work_unit_id = $1", wu.ID)
		_, _ = cpool.Exec(cctx, "DELETE FROM work_units WHERE id = $1", wu.ID)
		_, _ = cpool.Exec(cctx, "DELETE FROM leafs WHERE id = $1", leafID)
		_, _ = cpool.Exec(cctx, "DELETE FROM volunteers WHERE id = $1", volunteerID)
		_, _ = cpool.Exec(cctx, "DELETE FROM users WHERE id = $1", userID)
	})

	return wu.ID, volunteerID
}

// TestStopBackgroundAndClosePool_LeaderShutdown_BG32b: a leader replica's
// graceful shutdown must complete. The leadership manager holds a dedicated
// pool connection for its session advisory lock; pgxpool.Close blocks until
// every acquired connection is returned, so the pre-fix order (pool first)
// deadlocked here forever.
func TestStopBackgroundAndClosePool_LeaderShutdown_BG32b(t *testing.T) {
	pool := bg32TestPool(t)
	t.Cleanup(pool.Close) // idempotent; normally the shutdown tail closes it

	mgr := NewLeadershipManager(pool, bg32TestLogger())
	mgr.pollInterval = 200 * time.Millisecond

	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	becameLeader := make(chan struct{})
	go mgr.Run(monitorCtx, "bg32b-test-replica", func(leaderCtx context.Context) {
		close(becameLeader)
	})
	select {
	case <-becameLeader:
	case <-time.After(5 * time.Second):
		monitorCancel()
		t.Fatal("leadership was not acquired within 5s")
	}

	done := make(chan struct{})
	go func() {
		StopBackgroundAndClosePool(monitorCancel, map[string]<-chan struct{}{
			"leadership-manager": mgr.Done(),
		}, 10*time.Second, pool)
		close(done)
	}()

	select {
	case <-done:
		// Graceful: cancel released the advisory-lock connection, the join saw
		// Run return, and pool.Close() completed.
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown tail never completed: pool.Close() is deadlocked on the " +
			"leadership manager's held advisory-lock connection (BG-32b)")
	}
}

// TestStopBackgroundAndClosePool_FinalFlushLands_BG32: the dispatch cache's
// final best-effort flush on shutdown must land its queued reservations in the
// database BEFORE the pool closes. The flush interval is set to an hour so the
// shutdown flush is the only flush that can possibly land the write.
func TestStopBackgroundAndClosePool_FinalFlushLands_BG32(t *testing.T) {
	pool := bg32TestPool(t)
	t.Cleanup(pool.Close) // idempotent; normally the shutdown tail closes it

	unitID, volunteerID := bg32Fixtures(t, pool)

	cache := newDispatchCache(dispatchCacheConfig{
		flushInterval: time.Hour,
	}, dispatchDeps{
		wuRepo: workunit.NewPgxWorkUnitRepository(pool),
	}, bg32TestLogger())

	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	go cache.runFlusher(monitorCtx)

	// Queue one unflushed reservation, exactly as a hand-out would.
	cache.mu.Lock()
	cache.pendingWrites = append(cache.pendingWrites, workunit.FlushReservation{
		WorkUnitID:      unitID,
		VolunteerID:     volunteerID,
		ReservedUntil:   time.Now().UTC().Add(15 * time.Minute),
		DeadlineSeconds: 3600,
	})
	cache.mu.Unlock()

	done := make(chan struct{})
	go func() {
		StopBackgroundAndClosePool(monitorCancel, map[string]<-chan struct{}{
			"dispatch-cache-flusher": cache.Drained(),
		}, 10*time.Second, pool)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown tail never completed")
	}

	// The pool under test is now closed; verify on a fresh connection that the
	// final flush landed the copy row while the pool was still alive.
	vpool := bg32TestPool(t)
	defer vpool.Close()
	var live int
	if err := vpool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM work_unit_assignment_history
		WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL`,
		unitID, volunteerID).Scan(&live); err != nil {
		t.Fatalf("verify query: %v", err)
	}
	if live != 1 {
		t.Fatalf("shutdown's final flush did not land the reservation (live copies = %d, want 1): "+
			"the flush ran against a closed pool (BG-32)", live)
	}
}
