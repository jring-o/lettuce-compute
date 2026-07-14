//go:build integration

package generate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/montecarlo"
	"github.com/lettuce-compute/infrastructure/internal/paramsweep"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// These tests exercise Phase E1-B generation correctness against a real Postgres: ordinal
// disjointness (§4.5), lazy exhaustion (§4.6), windowed pacing (§4.7), and atomic batch+cursor
// with the guarded advance (§4.8, BG-22c). Each RED on pre-fix code. They SKIP without
// LETTUCE_TEST_DB_URL; the orchestrator applies the migrations before running the suite.

// trialParams mirrors the Monte Carlo work unit parameters JSON (montecarlo's struct is
// unexported).
type trialParams struct {
	Seed       int64 `json:"seed"`
	TrialIndex int   `json:"trial_index"`
}

func setupGenTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	cleanup := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}
	return pool, cleanup
}

// seedCreator inserts a user to satisfy chk_leafs_creator (a leaf needs a creator_id or a
// creator_public_key).
func seedCreator(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	id := types.NewID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, 'Gen Test User', '$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash')`,
		id, id.String()+"@test.example.com", "gen"+id.String()[:8],
	); err != nil {
		t.Fatalf("seed creator user: %v", err)
	}
	return id
}

// insertLazyLeaf inserts an ACTIVE lazy leaf with the given task pattern and data_config JSON,
// returning its id. data_config is passed as a single jsonb-cast parameter (each parameter is
// used in exactly one type context — no SQLSTATE 42P08).
func insertLazyLeaf(t *testing.T, pool *pgxpool.Pool, taskPattern, dataConfig string, ongoing bool) types.ID {
	t.Helper()
	ctx := context.Background()
	id := types.NewID()
	slug := "lazy-" + uuid.New().String()[:8]
	creatorID := seedCreator(t, pool)
	_, err := pool.Exec(ctx, `
		INSERT INTO leafs (id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements, is_ongoing, visibility, creator_id)
		VALUES ($1, $2, $3, $4, 'ACTIVE', $5,
			'{"runtime":"NATIVE","binaries":{"linux-amd64":"sha256:testbinary"},"gpu_required":false,"gpu_type":"","max_memory_mb":4096,"max_disk_mb":10240,"max_cpu_seconds":86400,"network_access":false,"min_vram_gb":0}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":2.0,"max_reassignments":5,"checkpointing_enabled":false}',
			$6::jsonb,
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_disk_mb":1024,"gpu_required":false,"min_bandwidth_mbps":0,"min_gpu_vram_mb":0}',
			$7, 'PUBLIC', $8)`,
		id, "Lazy "+slug, slug, "lazy generation test", taskPattern, dataConfig, ongoing, creatorID)
	if err != nil {
		t.Fatalf("insert lazy leaf: %v", err)
	}
	return id
}

func lazyMCDataConfig(seedStrategy string, numTrials, lazyBatch, lazyThreshold int) string {
	return fmt.Sprintf(`{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600,"generation_mode":"lazy","lazy_threshold":%d,"lazy_batch_size":%d,"splitting_config":{"num_trials":%d,"seed_strategy":"%s"}}`,
		lazyThreshold, lazyBatch, numTrials, seedStrategy)
}

func newGenManager(pool *pgxpool.Pool) (*generate.LazyManager, *generate.PgxBatchSink) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	router := generate.NewRouter(paramsweep.Generate, nil, montecarlo.Generate, nil, logger)
	sink := generate.NewPgxBatchSink(pool, logger)
	mgr := generate.NewLazyManager(router, workunit.NewPgxWorkUnitRepository(pool), sink, leaf.NewPgxRepository(pool), logger)
	return mgr, sink
}

func countWorkUnits(t *testing.T, pool *pgxpool.Pool, leafID types.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM work_units WHERE leaf_id = $1", leafID).Scan(&n); err != nil {
		t.Fatalf("count work units: %v", err)
	}
	return n
}

func countBatches(t *testing.T, pool *pgxpool.Pool, leafID types.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM batches WHERE leaf_id = $1", leafID).Scan(&n); err != nil {
		t.Fatalf("count batches: %v", err)
	}
	return n
}

// readTrials returns every work unit's (seed, trial_index) for a leaf.
func readTrials(t *testing.T, pool *pgxpool.Pool, leafID types.ID) []trialParams {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT parameters FROM work_units WHERE leaf_id = $1", leafID)
	if err != nil {
		t.Fatalf("query trials: %v", err)
	}
	defer rows.Close()
	var out []trialParams
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan trial: %v", err)
		}
		var p trialParams
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("unmarshal trial: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func leafCursor(t *testing.T, pool *pgxpool.Pool, leafID types.ID) *generate.GenerationCursor {
	t.Helper()
	p, err := leaf.GetByIDTx(context.Background(), pool, leafID)
	if err != nil {
		t.Fatalf("get leaf: %v", err)
	}
	return generate.LoadCursorForTest(p.GenerationCursor)
}

// TestLazyHashMC_ConsecutiveTicksDisjoint: two lazy hash ticks emit disjoint seed AND
// trial_index sets (RED pre-fix: identical batches every tick).
func TestLazyHashMC_ConsecutiveTicksDisjoint(t *testing.T) {
	pool, cleanup := setupGenTestDB(t)
	defer cleanup()
	ctx := context.Background()

	leafID := insertLazyLeaf(t, pool, "MONTE_CARLO", lazyMCDataConfig("hash", 1000, 10, 5), false)
	mgr, _ := newGenManager(pool)

	if _, err := mgr.CheckAndGenerate(ctx, leafID); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if _, err := mgr.CheckAndGenerate(ctx, leafID); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	trials := readTrials(t, pool, leafID)
	if len(trials) != 20 {
		t.Fatalf("expected 20 units after two ticks, got %d", len(trials))
	}
	seeds := map[int64]int{}
	idxs := map[int]int{}
	for _, tr := range trials {
		seeds[tr.Seed]++
		idxs[tr.TrialIndex]++
	}
	if len(seeds) != 20 {
		t.Errorf("expected 20 distinct hash seeds, got %d (duplication across ticks)", len(seeds))
	}
	if len(idxs) != 20 {
		t.Errorf("expected 20 distinct trial_index values, got %d", len(idxs))
	}
	// trial_index are the global ordinals 0..19.
	for i := 0; i < 20; i++ {
		if idxs[i] != 1 {
			t.Errorf("expected exactly one unit with trial_index=%d, got %d", i, idxs[i])
		}
	}
}

// TestLazyMCExhaustion: a finite lazy MC leaf exhausts after covering N, emitting exactly N
// units. Covers both N%batch != 0 (250/100) and N%batch == 0 (200/100). RED pre-fix (num_trials
// overwritten with the batch size ⇒ never exhausts).
func TestLazyMCExhaustion(t *testing.T) {
	cases := []struct {
		name  string
		n     int
		batch int
	}{
		{"250 over batch 100", 250, 100},
		{"200 exact multiple of 100", 200, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, cleanup := setupGenTestDB(t)
			defer cleanup()
			ctx := context.Background()

			leafID := insertLazyLeaf(t, pool, "MONTE_CARLO", lazyMCDataConfig("sequential", tc.n, tc.batch, 1000), false)
			mgr, _ := newGenManager(pool)

			for i := 0; i < 10; i++ {
				if _, err := mgr.CheckAndGenerate(ctx, leafID); err != nil {
					t.Fatalf("tick %d: %v", i, err)
				}
				if leafCursor(t, pool, leafID).GenerationExhausted {
					break
				}
			}

			if got := countWorkUnits(t, pool, leafID); got != tc.n {
				t.Errorf("expected exactly %d units, got %d", tc.n, got)
			}
			if !leafCursor(t, pool, leafID).GenerationExhausted {
				t.Error("expected leaf to be exhausted")
			}
			// One more tick after exhaustion generates nothing.
			gen, err := mgr.CheckAndGenerate(ctx, leafID)
			if err != nil {
				t.Fatalf("post-exhaustion tick: %v", err)
			}
			if gen != 0 {
				t.Errorf("expected 0 generated after exhaustion, got %d", gen)
			}
			if got := countWorkUnits(t, pool, leafID); got != tc.n {
				t.Errorf("unit count changed after exhaustion: %d != %d", got, tc.n)
			}
		})
	}
}

// TestGenerationBatchAtomicity: a failure inside the batch tx (after BulkCreate) rolls the WHOLE
// batch back — no batch row, zero units, cursor unadvanced. After healing, the full space is
// generated with the emitted ordinals being exactly 0..N-1 with no duplicates.
func TestGenerationBatchAtomicity(t *testing.T) {
	pool, cleanup := setupGenTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const n, batch = 250, 100
	leafID := insertLazyLeaf(t, pool, "MONTE_CARLO", lazyMCDataConfig("sequential", n, batch, 1000), false)
	mgr, sink := newGenManager(pool)

	// Inject a failure after BulkCreate (the CREATED->QUEUED transition fails) inside the tx.
	generate.SetBatchSinkDecorateWU(sink, func(r workunit.WorkUnitRepository) workunit.WorkUnitRepository {
		return &failTransitionRepo{WorkUnitRepository: r}
	})
	if _, err := mgr.CheckAndGenerate(ctx, leafID); err == nil {
		t.Fatal("expected the injected in-tx failure to surface")
	}
	if got := countBatches(t, pool, leafID); got != 0 {
		t.Errorf("expected 0 batch rows after rollback, got %d", got)
	}
	if got := countWorkUnits(t, pool, leafID); got != 0 {
		t.Errorf("expected 0 work units after rollback, got %d", got)
	}
	if c := leafCursor(t, pool, leafID); c.TotalGenerated != 0 || c.LastSeedOffset != 0 {
		t.Errorf("expected cursor unadvanced after rollback, got %+v", c)
	}

	// Heal and run to completion.
	generate.SetBatchSinkDecorateWU(sink, nil)
	for i := 0; i < 10; i++ {
		if _, err := mgr.CheckAndGenerate(ctx, leafID); err != nil {
			t.Fatalf("heal tick %d: %v", i, err)
		}
		if leafCursor(t, pool, leafID).GenerationExhausted {
			break
		}
	}

	trials := readTrials(t, pool, leafID)
	if len(trials) != n {
		t.Fatalf("expected %d units after healing, got %d", n, len(trials))
	}
	seen := make([]bool, n)
	for _, tr := range trials {
		if tr.TrialIndex < 0 || tr.TrialIndex >= n {
			t.Fatalf("ordinal %d out of range [0,%d)", tr.TrialIndex, n)
		}
		if seen[tr.TrialIndex] {
			t.Errorf("duplicate ordinal %d", tr.TrialIndex)
		}
		seen[tr.TrialIndex] = true
	}
	for i, ok := range seen {
		if !ok {
			t.Errorf("missing ordinal %d (hole in the space)", i)
		}
	}
}

// TestCursorImmuneToOwnerUpdate (BG-22c): after the cursor has advanced, an owner leaf Update
// carrying a stale (empty-cursor) snapshot must NOT roll the generation_cursor back.
func TestCursorImmuneToOwnerUpdate(t *testing.T) {
	pool, cleanup := setupGenTestDB(t)
	defer cleanup()
	ctx := context.Background()

	leafID := insertLazyLeaf(t, pool, "MONTE_CARLO", lazyMCDataConfig("sequential", 1000, 100, 1000), false)
	mgr, _ := newGenManager(pool)
	if _, err := mgr.CheckAndGenerate(ctx, leafID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	before := leafCursor(t, pool, leafID)
	if before.TotalGenerated != 100 {
		t.Fatalf("expected cursor total_generated=100 after tick, got %d", before.TotalGenerated)
	}

	// Owner edit: load, change a field, and Update. A dashboard PATCH's leaf snapshot never
	// carries the generation cursor, so simulate that by clearing it before Update.
	leafRepo := leaf.NewPgxRepository(pool)
	p, err := leafRepo.GetByID(ctx, leafID)
	if err != nil {
		t.Fatalf("get leaf: %v", err)
	}
	p.Description = "owner edited the description"
	p.GenerationCursor = nil
	if err := leafRepo.Update(ctx, p); err != nil {
		t.Fatalf("owner update: %v", err)
	}

	after := leafCursor(t, pool, leafID)
	if after.TotalGenerated != before.TotalGenerated || after.LastSeedOffset != before.LastSeedOffset {
		t.Errorf("owner update rolled the cursor back: before=%+v after=%+v", before, after)
	}
}

// TestCursorGuardAbortsConcurrentTick: a PersistBatch whose cursor advance carries the wrong
// expected-prev (a concurrent writer already advanced) returns ErrCursorConflict and persists
// NOTHING.
func TestCursorGuardAbortsConcurrentTick(t *testing.T) {
	pool, cleanup := setupGenTestDB(t)
	defer cleanup()
	ctx := context.Background()

	leafID := insertLazyLeaf(t, pool, "MONTE_CARLO", lazyMCDataConfig("sequential", 1000, 100, 1000), false)
	mgr, sink := newGenManager(pool)
	if _, err := mgr.CheckAndGenerate(ctx, leafID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	unitsBefore := countWorkUnits(t, pool, leafID)
	batchesBefore := countBatches(t, pool, leafID)

	// Attempt a direct batch whose guard expects a total_generated that does not match (the real
	// value is 100).
	batch := &workunit.Batch{LeafID: leafID, SequenceNumber: 999, TotalWorkUnits: 1}
	wus := []*workunit.WorkUnit{{
		LeafID:           leafID,
		State:            workunit.WorkUnitStateCreated,
		Priority:         workunit.WorkUnitPriorityNormal,
		CodeArtifactRef:  "ref://bin",
		InputData:        json.RawMessage(`{"x":1}`),
		Parameters:       json.RawMessage(`{"i":1}`),
		DeadlineSeconds:  3600,
		MaxReassignments: 3,
	}}
	staleCursor, _ := json.Marshal(generate.GenerationCursor{TotalGenerated: 1000, LastSeedOffset: 1000})
	err := sink.PersistBatch(ctx, batch, wus, &workunit.GenerationCursorAdvance{
		LeafID:                     leafID,
		Cursor:                     staleCursor,
		ExpectedPrevTotalGenerated: 999, // wrong; the real value is 100
	})
	if err != generate.ErrCursorConflict {
		t.Fatalf("expected ErrCursorConflict, got %v", err)
	}
	if got := countWorkUnits(t, pool, leafID); got != unitsBefore {
		t.Errorf("guarded-abort tx persisted units: %d != %d", got, unitsBefore)
	}
	if got := countBatches(t, pool, leafID); got != batchesBefore {
		t.Errorf("guarded-abort tx persisted a batch: %d != %d", got, batchesBefore)
	}
	if c := leafCursor(t, pool, leafID); c.TotalGenerated != 100 {
		t.Errorf("cursor changed under a guarded-abort: %+v", c)
	}
}

// TestLazyParamSweepWindowedTick: a lazy parameter-sweep tick over a large space emits exactly
// LazyBatchSize units in one batch (windowed pacing, no full materialization).
func TestLazyParamSweepWindowedTick(t *testing.T) {
	pool, cleanup := setupGenTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// 100 x 100 = 10000 combinations; lazy_batch_size 100.
	xs := make([]int, 100)
	ys := make([]int, 100)
	for i := range xs {
		xs[i] = i
		ys[i] = i
	}
	xj, _ := json.Marshal(xs)
	yj, _ := json.Marshal(ys)
	dataConfig := fmt.Sprintf(`{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600,"generation_mode":"lazy","lazy_threshold":50,"lazy_batch_size":100,"splitting_config":{"x":%s,"y":%s}}`, xj, yj)
	leafID := insertLazyLeaf(t, pool, "PARAMETER_SWEEP", dataConfig, false)

	mgr, _ := newGenManager(pool)
	gen, err := mgr.CheckAndGenerate(ctx, leafID)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if gen != 100 {
		t.Errorf("expected exactly 100 units (one window), got %d", gen)
	}
	if got := countWorkUnits(t, pool, leafID); got != 100 {
		t.Errorf("expected 100 persisted units, got %d", got)
	}
	if got := countBatches(t, pool, leafID); got != 1 {
		t.Errorf("expected exactly one batch, got %d", got)
	}
	if c := leafCursor(t, pool, leafID); c.LastGeneratedOffset != 100 {
		t.Errorf("expected last_generated_offset=100, got %d", c.LastGeneratedOffset)
	}
}

// failTransitionRepo wraps a WorkUnitRepository and fails BulkTransitionByBatch (the step after
// BulkCreate), used to inject an in-transaction fault for the atomicity test.
type failTransitionRepo struct {
	workunit.WorkUnitRepository
}

func (r *failTransitionRepo) BulkTransitionByBatch(ctx context.Context, batchID types.ID, from, to workunit.WorkUnitState) (int64, error) {
	return 0, fmt.Errorf("injected in-tx failure after BulkCreate")
}
