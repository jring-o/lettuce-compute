//go:build integration

package server

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestRequesterFallback_Postgres_ServesAHeavyContributorPastAPoolOfItsOwnResults drives the whole
// shape against Postgres: a real refill fills the ready pool with the leaf's oldest units, every
// one carrying the requester's result; the requester is handed the untouched units behind them
// through the real per-requester query and landing write; the pool is left as it was; and the
// reply says ALREADY_CONTRIBUTED only once the database has nothing more for the requester.
func TestRequesterFallback_Postgres_ServesAHeavyContributorPastAPoolOfItsOwnResults(t *testing.T) {
	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"work_unit_assignment_history", "results", "work_units", "leafs", "volunteers", "users"} {
			_, _ = pool.Exec(ctx, "DELETE FROM "+table)
		}
		pool.Close()
	})

	userID := types.NewID()
	username := "fallback-" + uuid.New().String()[:8]
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, 'Fallback Test User', 'x')`,
		userID, username+"@test.example.com", username); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	leafID := types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $2, 'per-requester fallback leaf', 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false,"max_memory_mb":512}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"max_reassignments":3,"checkpointing_enabled":false}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":128,"min_disk_mb":1,"gpu_required":false}',
			false, 'PUBLIC', $3
		)`, leafID, "fallback-leaf-"+uuid.New().String()[:8], userID); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}
	now := time.Now().UTC()
	vol := &volunteer.Volunteer{
		PublicKey: []byte(strings.Repeat("k", 32)),
		HardwareCapabilities: volunteer.HardwareCapabilities{
			CPUCores: 8, MaxCPUCores: 8, MemoryTotalMB: 8192, MaxMemoryMB: 8192,
			DiskAvailableMB: 10240, MaxDiskMB: 10240,
		},
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    volunteer.ScheduleAlways,
		IsActive:          true,
		LastSeenAt:        &now,
	}
	if err := volunteer.NewPgxRepository(pool).Create(ctx, vol); err != nil {
		t.Fatalf("seed volunteer: %v", err)
	}
	addUnit := func(ageSecs int) types.ID {
		t.Helper()
		id := types.NewID()
		if _, err := pool.Exec(ctx, `
			INSERT INTO work_units (id, leaf_id, state, priority, input_data, code_artifact_ref, parameters,
				estimated_duration_seconds, deadline_seconds, reassignment_count, max_reassignments,
				flagged_for_review, created_at)
			VALUES ($1, $2, 'QUEUED', 'NORMAL', '{"x":1}', 'ref://fallback', '{"n":1}', 300, 3600, 0, 3, false,
				NOW() - make_interval(secs => $3))`, id, leafID, ageSecs); err != nil {
			t.Fatalf("seed unit: %v", err)
		}
		return id
	}
	var own, untouched []types.ID
	for i := 0; i < 6; i++ {
		id := addUnit(1000 - i)
		if _, err := pool.Exec(ctx, `
			INSERT INTO results (work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata, validation_status)
			VALUES ($1, $2, '{"x":1}'::jsonb, $3, '{}'::jsonb, 'PENDING')`, id, vol.ID, strings.Repeat("a", 64)); err != nil {
			t.Fatalf("seed result: %v", err)
		}
		own = append(own, id)
	}
	for i := 0; i < 3; i++ {
		untouched = append(untouched, addUnit(500-i))
	}

	c := newDispatchCache(dispatchCacheConfig{
		readyPoolSize:   len(own),
		lowWatermark:    len(own),
		refillBatchSize: 50,
		admissionCap:    4,
		flushInterval:   time.Hour,
		leaseSeconds:    900,
	}, dispatchDeps{
		wuRepo:     workunit.NewPgxWorkUnitRepository(pool),
		leafRepo:   leaf.NewPgxRepository(pool),
		assignRepo: assignment.NewPgxRepository(pool),
	}, testLogger())
	c.refillOnce(ctx)
	staged := stagedIDs(c)
	if len(staged) != len(own) {
		t.Fatalf("refill staged %d units, want the pool's %d", len(staged), len(own))
	}
	for i := range own {
		if staged[i] != own[i] {
			t.Fatalf("refill staged %v, want the requester's own oldest units %v", staged, own)
		}
	}

	opts := capableOpts(vol.ID, 0)
	opts.LeafIDs = []types.ID{leafID}
	res, _, noWork := c.HandOutWithReason(vol.ID, opts, 64)
	var got []types.ID
	for _, r := range res {
		got = append(got, r.unit.ID)
	}
	if len(got) != len(untouched) {
		t.Fatalf("handed %v (reason %v), want the untouched units %v", got, noWork.reason, untouched)
	}
	for i := range untouched {
		if got[i] != untouched[i] {
			t.Fatalf("handed %v, want %v in dispatch order", got, untouched)
		}
	}
	var copies int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM work_unit_assignment_history
		WHERE volunteer_id = $1 AND outcome IS NULL AND work_unit_id = ANY($2::uuid[])`,
		vol.ID, untouched).Scan(&copies); err != nil {
		t.Fatalf("count copies: %v", err)
	}
	if copies != len(untouched) {
		t.Fatalf("%d live copies landed for the requester, want %d", copies, len(untouched))
	}
	after := stagedIDs(c)
	if len(after) != len(staged) {
		t.Fatalf("pool changed: %d staged, want %d", len(after), len(staged))
	}

	// Moments later the database's answer is not known yet, so "every task" is not claimed.
	if res, _, noWork := c.HandOutWithReason(vol.ID, opts, 64); len(res) != 0 ||
		noWork.reason == lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED {
		t.Fatalf("second ask: handed %d, reason %v", len(res), noWork.reason)
	}
	// Once the fallback may run again, the database has nothing more: now it is true.
	later := time.Now().Add(20 * time.Second)
	c.now = func() time.Time { return later }
	if res, _, noWork := c.HandOutWithReason(vol.ID, opts, 64); len(res) != 0 ||
		noWork.reason != lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED {
		t.Fatalf("third ask: handed %d, reason %v; want nothing and ALREADY_CONTRIBUTED", len(res), noWork.reason)
	}
}
