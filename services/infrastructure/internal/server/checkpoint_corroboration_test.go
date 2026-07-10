//go:build integration

package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// These tests pin the checkpoint-sharing gate: GetCheckpoint may hand one volunteer
// another volunteer's saved in-progress checkpoint ONLY for a genuinely single-copy unit.
// The corroboration signal is the RESOLVED redundancy policy (target_copies / min_quorum /
// spot-check promotion), NOT the raw redundancy_factor — a leaf can require 2-of-3
// corroboration while redundancy_factor still reads 1, and sharing a trajectory between
// corroborators would let one volunteer's state poison (or simply produce) the other's
// "independent" result.

// createCPProjectWithValidation creates an ACTIVE checkpointing-enabled leaf with the
// given validation_config JSON (the sharing decision under test resolves redundancy
// from it).
func createCPProjectWithValidation(t *testing.T, pool *pgxpool.Pool, creatorID *types.ID, validationConfig string) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	slug := "cp-corr-" + uuid.New().String()[:8]

	_, err := pool.Exec(ctx, `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, $4, 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false,"gpu_type":"","max_memory_mb":4096,"max_disk_mb":10240,"max_cpu_seconds":86400,"network_access":false,"min_vram_gb":0}',
			$5,
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3,"checkpointing_enabled":true,"checkpoint_interval_seconds":60,"max_checkpoint_size_bytes":1048576}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false,"min_bandwidth_mbps":0,"min_gpu_vram_mb":0}',
			false, 'PUBLIC', $6
		)`,
		id, "Test Leaf "+slug, slug, "A checkpoint corroboration test leaf", validationConfig, creatorID,
	)
	if err != nil {
		t.Fatalf("failed to create test leaf: %v", err)
	}
	return id
}

// createCPWorkUnit inserts an ASSIGNED work unit held by volunteerID (optionally a
// spot-check unit), with the matching assignment-history row.
func createCPWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID, volunteerID types.ID, spotCheck bool) types.ID {
	t.Helper()
	ctx := context.Background()
	wuID := types.NewID()
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds,
			assigned_volunteer_id, assigned_at, last_heartbeat_at,
			reassignment_count, max_reassignments, flagged_for_review, spot_check
		) VALUES (
			$1, $2, 'ASSIGNED', 'NORMAL',
			'{"x": 42}', 'ref://test', '{"n": 100}',
			300, 3600,
			$3, $4, $4,
			0, 3, false, $5
		)`,
		wuID, leafID, volunteerID, now, spotCheck,
	)
	if err != nil {
		t.Fatalf("failed to create assigned work unit: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, wuID, volunteerID, now)
	if err != nil {
		t.Fatalf("failed to create assignment history: %v", err)
	}

	return wuID
}

// addOpenCopy gives volunteerID an open copy of the unit (the assignment-history row a
// redundant dispatch or a reassignment would create).
func addOpenCopy(t *testing.T, pool *pgxpool.Pool, wuID, volunteerID types.ID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, wuID, volunteerID, time.Now().UTC()); err != nil {
		t.Fatalf("failed to add open copy: %v", err)
	}
}

// saveCPCheckpoint saves a checkpoint as the given volunteer and fails the test on error.
func saveCPCheckpoint(t *testing.T, ctx context.Context, client lettucev1.VolunteerServiceClient, key cpKey, wuID, volunteerID types.ID, data []byte) {
	t.Helper()
	if _, err := client.SaveCheckpoint(signCP(ctx, key), &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         wuID.String(),
		VolunteerId:        volunteerID.String(),
		CheckpointData:     data,
		CheckpointSequence: 1,
	}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
}

// TestGetCheckpoint_CorroboratedUnitNotSharedAcrossVolunteers: a leaf whose corroboration
// requirement comes from target_copies/min_quorum (2-of-3) while redundancy_factor reads 1.
// Volunteer B, holding a concurrent copy of A's unit, must NOT receive A's checkpoint —
// a gate reading raw redundancy_factor would hand it over.
func TestGetCheckpoint_CorroboratedUnitNotSharedAcrossVolunteers(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPProjectWithValidation(t, pool, &userID,
		`{"redundancy_factor":1,"target_copies":3,"min_quorum":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)

	keyA, volA := registerCPVolunteer(t, ctx, client)
	keyB, volB := registerCPVolunteer(t, ctx, client)

	wuID := createCPWorkUnit(t, pool, leafID, volA, false)
	saveCPCheckpoint(t, ctx, client, keyA, wuID, volA, []byte("A-private-trajectory"))

	// B holds a second concurrent open copy of the same unit (a distinct corroborator).
	addOpenCopy(t, pool, wuID, volB)

	resp, err := client.GetCheckpoint(signCP(ctx, keyB), &lettucev1.GetCheckpointRequest{
		WorkUnitId: wuID.String(),
	})
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if resp.HasCheckpoint {
		t.Fatalf("corroborated unit (target_copies=3, min_quorum=2) shared volunteer A's checkpoint with corroborator B: created_by=%s seq=%d",
			resp.CreatedByVolunteerId, resp.CheckpointSequence)
	}
}

// TestGetCheckpoint_SpotCheckUnitNotSharedAcrossVolunteers: a spot-check unit on an
// otherwise single-copy leaf is promoted to a 2-of-2 corroboration at runtime, so its
// checkpoint must not cross volunteers either — redundancy_factor never learns about the
// promotion.
func TestGetCheckpoint_SpotCheckUnitNotSharedAcrossVolunteers(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPProjectWithValidation(t, pool, &userID,
		`{"redundancy_factor":1,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)

	keyA, volA := registerCPVolunteer(t, ctx, client)
	keyB, volB := registerCPVolunteer(t, ctx, client)

	wuID := createCPWorkUnit(t, pool, leafID, volA, true) // spot-check unit
	saveCPCheckpoint(t, ctx, client, keyA, wuID, volA, []byte("A-spot-check-trajectory"))

	addOpenCopy(t, pool, wuID, volB)

	resp, err := client.GetCheckpoint(signCP(ctx, keyB), &lettucev1.GetCheckpointRequest{
		WorkUnitId: wuID.String(),
	})
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if resp.HasCheckpoint {
		t.Fatalf("spot-check unit shared volunteer A's checkpoint with its corroborator B: created_by=%s seq=%d",
			resp.CreatedByVolunteerId, resp.CheckpointSequence)
	}
}

// TestGetCheckpoint_SingleCopyUnitStillSharesAcrossVolunteers (control): a genuinely
// single-copy unit reassigned to a fresh volunteer still resumes from the previous
// volunteer's checkpoint — the fail-closed gate must not break the designed resume path.
func TestGetCheckpoint_SingleCopyUnitStillSharesAcrossVolunteers(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	leafID := createCPProjectWithValidation(t, pool, &userID,
		`{"redundancy_factor":1,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)

	keyA, volA := registerCPVolunteer(t, ctx, client)
	keyB, volB := registerCPVolunteer(t, ctx, client)

	wuID := createCPWorkUnit(t, pool, leafID, volA, false)
	saveCPCheckpoint(t, ctx, client, keyA, wuID, volA, []byte("A-resumable-state"))

	// The unit is reassigned to B (single copy — A's assignment lapsed).
	addOpenCopy(t, pool, wuID, volB)

	resp, err := client.GetCheckpoint(signCP(ctx, keyB), &lettucev1.GetCheckpointRequest{
		WorkUnitId: wuID.String(),
	})
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if !resp.HasCheckpoint {
		t.Fatal("single-copy unit must still share the previous volunteer's checkpoint on reassignment")
	}
	if string(resp.CheckpointData) != "A-resumable-state" {
		t.Errorf("checkpoint data = %q, want %q", resp.CheckpointData, "A-resumable-state")
	}
	if resp.CreatedByVolunteerId != volA.String() {
		t.Errorf("created_by = %q, want volunteer A %q", resp.CreatedByVolunteerId, volA.String())
	}
}
