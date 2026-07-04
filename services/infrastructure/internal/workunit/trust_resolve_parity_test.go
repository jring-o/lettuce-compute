//go:build integration

package workunit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// TestTrustResolveSQL_MatchesResolveTrust is the golden parity test pinning the two SQL twins
// of the trusted-corroborator resolution — workunit.EffTrustKSQL and EffTrustFloorSQL, which
// every dispatch reservation embeds — to their Go source of truth,
// transition.TrustPolicy.ResolveTrust. For a grid of head-gate / per-leaf-override / head-
// default / min-quorum inputs it seeds a leaf + work unit carrying those overrides, asks the
// database for the (K, floor) the shared SQL expressions yield, then asserts they equal what
// ResolveTrust returns for the identical inputs. A change to EITHER side that drifts from the
// other fails here, so the dispatch reservation can never silently resolve a different K or
// floor than validation does — the same structural guard subjectExprSQL has.
//
// It lives in an EXTERNAL test package (workunit_test) because it imports internal/transition
// to call the real ResolveTrust, and transition imports workunit (an in-package test importing
// transition would be an import cycle). The unexported SQL builders reach it through the
// export_test.go aliases. Per repo convention it is a DB-backed integration test: build tag
// `integration`, SKIP without LETTUCE_TEST_DB_URL (via the shared setupTestDB), safe under -p 1.
func TestTrustResolveSQL_MatchesResolveTrust(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := insertParityTrustUser(t, pool)
	leafID := insertParityTrustLeaf(t, pool, userID)

	// effQuorumSQL (the K clamp target) reads wu.min_quorum with spot_check false, so one work
	// unit per min-quorum value drives the clamp; the leaf's per-leaf overrides are re-stamped
	// per combo. Pre-creating the units keeps the grid to one query per combo.
	wuByQuorum := make(map[int]types.ID)
	for _, q := range []int{1, 2, 3} {
		wuByQuorum[q] = insertParityTrustWorkUnit(t, pool, leafID, q)
	}

	// The exact expressions the dispatch queries embed: $1 gate, $2 default K, $3 default floor.
	kExpr := workunit.EffTrustKSQL("wu", "l", "$1", "$2")
	floorExpr := workunit.EffTrustFloorSQL("l", "$3")
	query := `SELECT ` + kExpr + `, ` + floorExpr + `
		FROM work_units wu JOIN leafs l ON l.id = wu.leaf_id
		WHERE wu.id = $4`

	gates := []bool{false, true}
	leafKs := []int{0, 1, 2, 5}    // 0 = no per-leaf override
	leafFloors := []int{0, 10, 50} // 0 = no per-leaf override
	defaultKs := []int{1, 3}       // head default K
	defaultFloors := []int{25, 50} // head default floor
	quorums := []int{1, 2, 3}      // effQuorumSQL == wu.min_quorum here

	for _, gate := range gates {
		for _, leafK := range leafKs {
			for _, leafFloor := range leafFloors {
				// The per-leaf overrides depend only on (leafK, leafFloor); stamp them once here.
				vc := fmt.Sprintf(
					`{"redundancy_factor":2,"min_trusted_corroborators":%d,"trust_floor":%d}`,
					leafK, leafFloor)
				if _, err := pool.Exec(ctx,
					`UPDATE leafs SET validation_config = $2 WHERE id = $1`, leafID, vc); err != nil {
					t.Fatalf("stamp leaf overrides: %v", err)
				}
				for _, defK := range defaultKs {
					for _, defFloor := range defaultFloors {
						for _, q := range quorums {
							var sqlK, sqlFloor int
							if err := pool.QueryRow(ctx, query, gate, defK, defFloor, wuByQuorum[q]).
								Scan(&sqlK, &sqlFloor); err != nil {
								t.Fatalf("evaluate SQL twins: %v", err)
							}

							tp := transition.TrustPolicy{
								GateEnabled:             gate,
								DefaultMinCorroborators: defK,
								DefaultFloor:            defFloor,
							}
							wantK, wantFloor := tp.ResolveTrust(
								leaf.ValidationConfig{MinTrustedCorroborators: leafK, TrustFloor: leafFloor}, q)

							if sqlK != wantK || sqlFloor != wantFloor {
								t.Fatalf("trust-resolve twin drift "+
									"(gate=%v leafK=%d leafFloor=%d defaultK=%d defaultFloor=%d minQuorum=%d): "+
									"SQL=(K=%d,floor=%d) ResolveTrust=(K=%d,floor=%d)\n"+
									"The SQL builders and transition.TrustPolicy.ResolveTrust must stay identical "+
									"— update both together.",
									gate, leafK, leafFloor, defK, defFloor, q, sqlK, sqlFloor, wantK, wantFloor)
							}
						}
					}
				}
			}
		}
	}
}

// insertParityTrustUser inserts a minimal user to own the parity leaf and returns its id.
func insertParityTrustUser(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	id := types.NewID()
	name := "trustparity-" + uuid.New().String()[:8]
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		id, name+"@test.example.com", name, "Trust Parity "+name,
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	); err != nil {
		t.Fatalf("insert parity user: %v", err)
	}
	return id
}

// insertParityTrustLeaf inserts a leaf whose validation_config is re-stamped per combo; the
// other configs are the standard defaults (mirroring the in-package createTestLeaf, which the
// external test package cannot reach).
func insertParityTrustLeaf(t *testing.T, pool *pgxpool.Pool, creatorID types.ID) types.ID {
	t.Helper()
	id := types.NewID()
	slug := "trustparity-leaf-" + uuid.New().String()[:8]
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, $4, 'DRAFT', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false,"gpu_type":"","max_memory_mb":4096,"max_disk_mb":10240,"max_cpu_seconds":86400,"network_access":false,"min_vram_gb":0}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3,"checkpointing_enabled":false}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false,"min_bandwidth_mbps":0,"min_gpu_vram_mb":0}',
			false, 'PUBLIC', $5
		)`,
		id, "Trust Parity Leaf "+slug, slug, "trust-resolve parity leaf", creatorID,
	); err != nil {
		t.Fatalf("insert parity leaf: %v", err)
	}
	return id
}

// insertParityTrustWorkUnit creates a QUEUED-lineage work unit with the given min_quorum
// (spot_check defaults false), so effQuorumSQL — the K clamp target — evaluates to minQuorum.
func insertParityTrustWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID types.ID, minQuorum int) types.ID {
	t.Helper()
	repo := workunit.NewPgxWorkUnitRepository(pool)
	wu := &workunit.WorkUnit{
		LeafID:           leafID,
		State:            workunit.WorkUnitStateCreated,
		Priority:         workunit.WorkUnitPriorityNormal,
		InputData:        json.RawMessage(`{"x":1}`),
		CodeArtifactRef:  "ref://trustparity-" + uuid.New().String()[:8],
		Parameters:       json.RawMessage(`{}`),
		DeadlineSeconds:  3600,
		MaxReassignments: 3,
		MinQuorum:        minQuorum,
	}
	if err := repo.Create(context.Background(), wu); err != nil {
		t.Fatalf("create parity work unit (min_quorum=%d): %v", minQuorum, err)
	}
	return wu.ID
}
