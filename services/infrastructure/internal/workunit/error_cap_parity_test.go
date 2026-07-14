//go:build integration

package workunit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// These DB-backed tests close BG-27 (design §4.9): they pin the error-cap SQL fragments the
// dead-letter executor embeds to their Go source of truth (transition.ResolvePolicy /
// CountErrorCopies), assert the decider and the executor AGREE on every dead-letter snapshot
// (the agreement whose absence WAS BG-27 — only the Go resolution had ever been pinned, never
// the SQL execution), and regress the poison-unit scenario end to end.
//
// They live in the EXTERNAL test package (workunit_test) because they import internal/transition
// to call the real ResolvePolicy/Decide, and transition imports workunit (an in-package test
// importing transition would be an import cycle). The unexported SQL builders reach them through
// the export_test.go aliases. Per repo convention: build tag `integration`, SKIP without
// LETTUCE_TEST_DB_URL (via the shared setupTestDB), safe under -p 1.

// TestErrorCapResolveSQL_MatchesResolvePolicy is the golden parity test pinning effMaxErrorSQL —
// the error-ceiling SQL twin the dead-letter executor embeds — to its Go source,
// transition.ResolvePolicy(...).MaxErrorCopies. For a grid of per-unit override × leaf-config ×
// absent inputs it stamps the leaf + unit overrides, asks the database what effMaxErrorSQL
// yields, and asserts it equals what ResolvePolicy returns for the identical inputs. Note the
// deliberate ABSENCE of any target-derived default (unlike max_total): 0 stays 0 = unlimited.
func TestErrorCapResolveSQL_MatchesResolvePolicy(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := ecUser(t, pool)
	leafID := ecLeaf(t, pool, userID, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)

	// Pre-create one unit per per-unit override value (0 = no override).
	unitOverrides := []int{0, 3, 6}
	wuByOverride := make(map[int]types.ID)
	for _, ov := range unitOverrides {
		wuByOverride[ov] = ecWorkUnit(t, pool, leafID, func(wu *workunit.WorkUnit) { wu.MaxErrorCopies = ov })
	}

	expr := workunit.EffMaxErrorSQL("wu", "l")
	query := `SELECT ` + expr + ` FROM work_units wu JOIN leafs l ON l.id = wu.leaf_id WHERE wu.id = $1`

	// leafCfg -1 = the key is ABSENT from validation_config (COALESCE -> 0); >= 0 = stamped.
	leafCfgs := []int{-1, 0, 4, 9}
	for _, leafCfg := range leafCfgs {
		var vcJSON string
		goLeafCfg := 0
		if leafCfg < 0 {
			vcJSON = `{"redundancy_factor":2}`
		} else {
			vcJSON = fmt.Sprintf(`{"redundancy_factor":2,"max_error_copies":%d}`, leafCfg)
			goLeafCfg = leafCfg
		}
		if _, err := pool.Exec(ctx, `UPDATE leafs SET validation_config = $2 WHERE id = $1`, leafID, vcJSON); err != nil {
			t.Fatalf("stamp leaf validation_config: %v", err)
		}
		for _, ov := range unitOverrides {
			var sqlVal int
			if err := pool.QueryRow(ctx, query, wuByOverride[ov]).Scan(&sqlVal); err != nil {
				t.Fatalf("evaluate effMaxErrorSQL: %v", err)
			}
			lf := &leaf.Leaf{ValidationConfig: leaf.ValidationConfig{
				RedundancyFactor: 2, AgreementThreshold: 1.0, MaxErrorCopies: goLeafCfg,
			}}
			wu := &workunit.WorkUnit{MaxErrorCopies: ov}
			want := transition.ResolvePolicy(lf, wu).MaxErrorCopies
			if sqlVal != want {
				t.Fatalf("effMaxErrorSQL drift (unitOverride=%d leafCfg=%d): SQL=%d ResolvePolicy=%d\n"+
					"effMaxErrorSQL and transition.ResolvePolicy's MaxErrorCopies must stay identical — update both together.",
					ov, leafCfg, sqlVal, want)
			}
		}
	}
}

// TestErrorCopiesSQL_MatchesCountErrorCopies pins errorCopiesSQL — the wasted-work tally the
// dead-letter executor and RefundCopyBudget embed — to CountErrorCopies (which now embeds the
// same fragment), over seeded history + result rows. It asserts BOTH read the same number AND
// that number is the hand-computed EXPIRED/ABANDONED-history + DISAGREED-result total, so the
// single shared definition can never silently drift from either its consumers or its meaning.
func TestErrorCopiesSQL_MatchesCountErrorCopies(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := ecUser(t, pool)
	leafID := ecLeaf(t, pool, userID, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)
	repo := workunit.NewPgxWorkUnitRepository(pool)
	wuID := ecWorkUnit(t, pool, leafID, nil)

	// Error copies = EXPIRED + ABANDONED history + DISAGREED results. Live (NULL outcome) and
	// PENDING/AGREED results are NOT error copies and must not be counted.
	ecHistory(t, pool, wuID, ecVolunteer(t, pool), "EXPIRED")
	ecHistory(t, pool, wuID, ecVolunteer(t, pool), "ABANDONED")
	ecHistory(t, pool, wuID, ecVolunteer(t, pool), "") // live, not counted
	ecResult(t, pool, wuID, ecVolunteer(t, pool), "DISAGREED")
	ecResult(t, pool, wuID, ecVolunteer(t, pool), "PENDING") // not counted
	ecResult(t, pool, wuID, ecVolunteer(t, pool), "AGREED")  // not counted
	const wantCount = 3                                      // 2 error history + 1 DISAGREED

	var sqlVal int
	if err := pool.QueryRow(ctx, `SELECT `+workunit.ErrorCopiesSQL("$1"), wuID).Scan(&sqlVal); err != nil {
		t.Fatalf("evaluate errorCopiesSQL: %v", err)
	}
	repoVal, err := repo.CountErrorCopies(ctx, wuID)
	if err != nil {
		t.Fatalf("CountErrorCopies: %v", err)
	}
	if sqlVal != repoVal || sqlVal != wantCount {
		t.Fatalf("error-copy count drift: errorCopiesSQL=%d CountErrorCopies=%d want=%d\n"+
			"the shared errorCopiesSQL fragment must equal CountErrorCopies and the hand-computed tally.",
			sqlVal, repoVal, wantCount)
	}
}

// TestDecideExecutorAgreement_DeadLetter is the test whose ABSENCE was BG-27: for a grid of unit
// snapshots seeded into Postgres it asserts transition.Decide(snapshot).Action ==
// ActionDeadLetter ⟺ DeadLetterIfExhausted(unit) affects one row. The Go snapshot is built from
// the SAME seeded counts the SQL reads (CountLiveCopies/CountTotalCopies/CountErrorCopies +
// raw PENDING count), resolved via ResolvePolicy — so the decider and the executor are two views
// of one truth. The grid spans state (QUEUED/COMPLETED), live copies, pending vs quorum, the
// total ceiling, and the error ceiling (cap 0 and > 0; error below/at/above the cap) — including
// the error-ceiling case the executor could not enforce before BG-27 was wired.
func TestDecideExecutorAgreement_DeadLetter(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := ecUser(t, pool)
	// target == quorum == 2 (redundancy_factor). Per-unit maxTotal/maxError overrides drive the grid.
	leafID := ecLeaf(t, pool, userID, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)
	leafRepo := leaf.NewPgxRepository(pool)
	repo := workunit.NewPgxWorkUnitRepository(pool)
	lf, err := leafRepo.GetByID(ctx, leafID)
	if err != nil {
		t.Fatalf("load leaf: %v", err)
	}

	type scenario struct {
		name       string
		state      workunit.WorkUnitState
		live       int // live history rows (outcome NULL)
		expired    int // EXPIRED history rows (count toward total AND error)
		disagreed  int // DISAGREED results (count toward error only)
		pending    int // PENDING results
		maxTotal   int
		maxError   int
		wantFailed bool // documents the expected shared outcome; the assertion is Decide<->executor agreement
	}
	q := workunit.WorkUnitStateQueued
	c := workunit.WorkUnitStateCompleted
	scenarios := []scenario{
		{"queued under both ceilings -> not dead-letter", q, 0, 1, 0, 0, 5, 0, false},
		{"queued total ceiling hit -> dead-letter", q, 0, 5, 0, 0, 5, 0, true},
		{"queued ERROR ceiling hit (BG-27) -> dead-letter", q, 0, 2, 0, 0, 10, 2, true},
		{"queued error above cap -> dead-letter", q, 0, 2, 1, 0, 10, 2, true},
		{"queued error cap hit but a copy is live -> not dead-letter", q, 1, 2, 0, 0, 10, 2, false},
		{"queued error cap 0 = unlimited, total not hit -> not dead-letter", q, 0, 3, 0, 0, 10, 0, false},
		{"completed error ceiling hit -> dead-letter", c, 0, 2, 0, 0, 10, 2, true},
		{"queued at quorum with budget spent -> not dead-letter (reject, not DL)", q, 0, 5, 0, 2, 5, 0, false},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			wuID := ecWorkUnit(t, pool, leafID, func(wu *workunit.WorkUnit) {
				wu.MaxTotalCopies = s.maxTotal
				wu.MaxErrorCopies = s.maxError
			})
			// $3 carries the backdate condition separately: $2 feeds the enum state column, and
			// reusing it in a text comparison would make Postgres deduce two conflicting
			// parameter types (SQLSTATE 42P08).
			if _, err := pool.Exec(ctx, `
				UPDATE work_units SET state = $2,
					completed_at = CASE WHEN $3 THEN NOW() ELSE completed_at END
				WHERE id = $1`, wuID, s.state, s.state == "COMPLETED"); err != nil {
				t.Fatalf("set unit state: %v", err)
			}
			for i := 0; i < s.live; i++ {
				ecHistory(t, pool, wuID, ecVolunteer(t, pool), "")
			}
			for i := 0; i < s.expired; i++ {
				ecHistory(t, pool, wuID, ecVolunteer(t, pool), "EXPIRED")
			}
			for i := 0; i < s.disagreed; i++ {
				ecResult(t, pool, wuID, ecVolunteer(t, pool), "DISAGREED")
			}
			for i := 0; i < s.pending; i++ {
				ecResult(t, pool, wuID, ecVolunteer(t, pool), "PENDING")
			}

			// Build the Go snapshot from the SAME seeded counts the executor SQL reads.
			wu, err := repo.GetByID(ctx, wuID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			live, err := repo.CountLiveCopies(ctx, wuID)
			if err != nil {
				t.Fatalf("CountLiveCopies: %v", err)
			}
			total, err := repo.CountTotalCopies(ctx, wuID)
			if err != nil {
				t.Fatalf("CountTotalCopies: %v", err)
			}
			errC, err := repo.CountErrorCopies(ctx, wuID)
			if err != nil {
				t.Fatalf("CountErrorCopies: %v", err)
			}
			snap := transition.UnitSnapshot{
				State:        wu.State,
				Policy:       transition.ResolvePolicy(lf, wu),
				LiveCopies:   live,
				TotalCopies:  total,
				ErrorCopies:  errC,
				PendingCount: ecPendingCount(t, pool, wuID),
			}
			wantDL := transition.Decide(snap).Action == transition.ActionDeadLetter

			gotDL, err := repo.DeadLetterIfExhausted(ctx, wuID)
			if err != nil {
				t.Fatalf("DeadLetterIfExhausted: %v", err)
			}

			if wantDL != gotDL {
				t.Fatalf("decide<->executor DISAGREE: Decide=DeadLetter(%v) executor.affected(%v)\n"+
					"snapshot: state=%s live=%d total=%d error=%d pending=%d maxTotal=%d maxError=%d",
					wantDL, gotDL, snap.State, snap.LiveCopies, snap.TotalCopies, snap.ErrorCopies,
					snap.PendingCount, s.maxTotal, s.maxError)
			}
			if gotDL != s.wantFailed {
				t.Fatalf("unexpected outcome: executor.affected=%v want=%v (snapshot state=%s error=%d maxError=%d total=%d maxTotal=%d)",
					gotDL, s.wantFailed, snap.State, snap.ErrorCopies, s.maxError, snap.TotalCopies, s.maxTotal)
			}
		})
	}
}

// TestDecideExecutorAgreement_VersionHeterogeneousPending (★BG-21g) extends the agreement
// grid to a snapshot the original grid could not express: raw PENDING ≠ version-homogeneous
// PENDING. Decide reads the FILTERED count (the transitioner applies validation.FilterPending
// before building the snapshot — replicated here through the real engine), so for a unit with
// two different-version PENDING results (filtered 1 < quorum 2), zero live copies, and a spent
// budget, Decide says ActionDeadLetter. Pre-fix the executor's quorum probe counted RAW rows
// (2 >= 2 -> refuse) and disagreed — the decider-says/executor-noops pathology through the
// count instead of the cap, which also gated BOTH ceiling disjuncts (hardening note (f)). A
// budget-open control pins the other direction (both sides refuse: WAIT with real headroom).
func TestDecideExecutorAgreement_VersionHeterogeneousPending(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := ecUser(t, pool)
	leafID := ecLeaf(t, pool, userID, `{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}`)
	leafRepo := leaf.NewPgxRepository(pool)
	repo := workunit.NewPgxWorkUnitRepository(pool)
	resultRepo := result.NewPgxRepository(pool)
	lf, err := leafRepo.GetByID(ctx, leafID)
	if err != nil {
		t.Fatalf("load leaf: %v", err)
	}
	v1 := ecArtifactVersion(t, pool, leafID, "v1")
	v2 := ecArtifactVersion(t, pool, leafID, "v2")
	// The real production filter (the exact function the transitioner runs before Decide).
	filter := validation.NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, transition.TrustPolicy{})

	cases := []struct {
		name       string
		maxTotal   int  // per-unit ceiling; expired copies seeded to match when spent
		spent      bool // whether to burn the budget (2 EXPIRED copies)
		wantFailed bool
	}{
		{"het pending at raw quorum, budget spent -> dead-letter", 2, true, true},
		{"het pending at raw quorum, budget open -> wait (not dead-letter)", 8, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wuID := ecWorkUnit(t, pool, leafID, func(wu *workunit.WorkUnit) {
				wu.MaxTotalCopies = c.maxTotal
			})
			if _, err := pool.Exec(ctx, `UPDATE work_units SET state = 'QUEUED' WHERE id = $1`, wuID); err != nil {
				t.Fatalf("queue unit: %v", err)
			}
			ecResultVersioned(t, pool, wuID, ecVolunteer(t, pool), "PENDING", v1)
			ecResultVersioned(t, pool, wuID, ecVolunteer(t, pool), "PENDING", v2)
			if c.spent {
				ecHistory(t, pool, wuID, ecVolunteer(t, pool), "EXPIRED")
				ecHistory(t, pool, wuID, ecVolunteer(t, pool), "EXPIRED")
			}

			// Build the snapshot the way the TRANSITIONER does: raw rows loaded, then the
			// version-homogeneity filter applied before the count Decide sees.
			all, err := resultRepo.ListByWorkUnit(ctx, wuID)
			if err != nil {
				t.Fatalf("list results: %v", err)
			}
			var pending []*result.Result
			for _, r := range all {
				if r.ValidationStatus == result.ValidationPending {
					pending = append(pending, r)
				}
			}
			if len(pending) != 2 {
				t.Fatalf("raw pending = %d, want 2", len(pending))
			}
			filtered := filter.FilterPending(pending)
			if len(filtered) != 1 {
				t.Fatalf("version-homogeneous pending = %d, want 1 (two versions split 1/1)", len(filtered))
			}

			wu, err := repo.GetByID(ctx, wuID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			live, err := repo.CountLiveCopies(ctx, wuID)
			if err != nil {
				t.Fatalf("CountLiveCopies: %v", err)
			}
			total, err := repo.CountTotalCopies(ctx, wuID)
			if err != nil {
				t.Fatalf("CountTotalCopies: %v", err)
			}
			errC, err := repo.CountErrorCopies(ctx, wuID)
			if err != nil {
				t.Fatalf("CountErrorCopies: %v", err)
			}
			snap := transition.UnitSnapshot{
				State:        wu.State,
				Policy:       transition.ResolvePolicy(lf, wu),
				LiveCopies:   live,
				TotalCopies:  total,
				ErrorCopies:  errC,
				PendingCount: len(filtered),
			}
			wantDL := transition.Decide(snap).Action == transition.ActionDeadLetter

			gotDL, err := repo.DeadLetterIfExhausted(ctx, wuID)
			if err != nil {
				t.Fatalf("DeadLetterIfExhausted: %v", err)
			}
			if wantDL != gotDL {
				t.Fatalf("decide<->executor DISAGREE on a version-heterogeneous set: Decide=DeadLetter(%v) executor.affected(%v)\n"+
					"snapshot: state=%s live=%d total=%d error=%d rawPending=2 filteredPending=%d maxTotal=%d",
					wantDL, gotDL, snap.State, live, total, errC, len(filtered), c.maxTotal)
			}
			if gotDL != c.wantFailed {
				t.Fatalf("unexpected outcome: executor.affected=%v want=%v", gotDL, c.wantFailed)
			}

			// The exported fragment must read the same number the production filter yields —
			// the SQL<->Go pin for the shared count itself.
			var sqlFiltered int
			if err := pool.QueryRow(ctx, `SELECT `+workunit.VersionHomogeneousPendingSQL("$1"), wuID).Scan(&sqlFiltered); err != nil {
				t.Fatalf("evaluate versionHomogeneousPendingSQL: %v", err)
			}
			if sqlFiltered != len(filtered) {
				t.Fatalf("versionHomogeneousPendingSQL=%d, FilterPending=%d — the SQL twin drifted from the Go filter",
					sqlFiltered, len(filtered))
			}
		})
	}
}

// ecArtifactVersion inserts an artifact-version row for the leaf (★BG-21g seeding).
func ecArtifactVersion(t *testing.T, pool *pgxpool.Pool, leafID types.ID, label string) types.ID {
	t.Helper()
	var id types.ID
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO leaf_artifact_versions (leaf_id, version_label, runtime_type, execution_config)
		VALUES ($1, $2, 'NATIVE', '{}') RETURNING id`,
		leafID, label).Scan(&id); err != nil {
		t.Fatalf("insert artifact version %q: %v", label, err)
	}
	return id
}

// ecResultVersioned is ecResult plus an artifact_version_id stamp (★BG-21g seeding).
func ecResultVersioned(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, status string, versionID types.ID) {
	t.Helper()
	ecChecksumSeq++
	checksum := fmt.Sprintf("%064d", ecChecksumSeq)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO results (work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata, validation_status, artifact_version_id)
		 VALUES ($1, $2, '{"x":1}'::jsonb, $3, '{}'::jsonb, $4, $5)`,
		wuID, volID, checksum, status, versionID); err != nil {
		t.Fatalf("insert versioned %s result: %v", status, err)
	}
}

// TestErrorCapDeadLettersPoisonUnit is the Phase E1-C exit-criterion regression: a leaf whose
// error cap equals its target (max_error_copies = target_copies = 2) with a large total ceiling
// (max_total_copies = 12). A unit that burns its error budget (2 EXPIRED copies) with no live
// copy and no pending results is parked FAILED + flagged and is no longer dispatchable — even
// though its 2 total copies are far below the 12 total ceiling. Pre-fix (BG-27) the executor
// embedded only the total ceiling, so the unit stayed QUEUED until it burned 12 copies. A
// control unit whose leaf sets NO error cap (0 = unlimited) with the same 2 EXPIRED copies is
// NOT dead-lettered, proving it is the error cap — not the total ceiling — doing the work.
func TestErrorCapDeadLettersPoisonUnit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	userID := ecUser(t, pool)
	repo := workunit.NewPgxWorkUnitRepository(pool)

	// Poison leaf: max_error_copies = target_copies = 2, max_total_copies = 12.
	poisonLeaf := ecLeaf(t, pool, userID,
		`{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3,"max_error_copies":2,"max_total_copies":12}`)
	poison := ecWorkUnit(t, pool, poisonLeaf, nil)
	if _, err := pool.Exec(ctx, `UPDATE work_units SET state = 'QUEUED' WHERE id = $1`, poison); err != nil {
		t.Fatalf("queue poison unit: %v", err)
	}
	// Burn the error budget: 2 EXPIRED copies, no live copy, no pending results.
	ecHistory(t, pool, poison, ecVolunteer(t, pool), "EXPIRED")
	ecHistory(t, pool, poison, ecVolunteer(t, pool), "EXPIRED")

	failed, err := repo.DeadLetterIfExhausted(ctx, poison)
	if err != nil {
		t.Fatalf("DeadLetterIfExhausted(poison): %v", err)
	}
	if !failed {
		t.Fatal("poison unit with error budget spent (2/2) must dead-letter even below the total ceiling (12)")
	}
	got, err := repo.GetByID(ctx, poison)
	if err != nil {
		t.Fatalf("GetByID(poison): %v", err)
	}
	if got.State != workunit.WorkUnitStateFailed {
		t.Errorf("poison unit State = %s, want FAILED (a FAILED unit is not dispatchable)", got.State)
	}
	if !got.FlaggedForReview {
		t.Error("dead-lettered poison unit must be flagged_for_review (operator-visible tripwire)")
	}

	// Control: same 2 EXPIRED copies, but the leaf sets NO error cap (unlimited). Total 2 << 12,
	// so nothing dead-letters — isolating the error cap as the cause above.
	ctrlLeaf := ecLeaf(t, pool, userID,
		`{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3,"max_total_copies":12}`)
	ctrl := ecWorkUnit(t, pool, ctrlLeaf, nil)
	if _, err := pool.Exec(ctx, `UPDATE work_units SET state = 'QUEUED' WHERE id = $1`, ctrl); err != nil {
		t.Fatalf("queue control unit: %v", err)
	}
	ecHistory(t, pool, ctrl, ecVolunteer(t, pool), "EXPIRED")
	ecHistory(t, pool, ctrl, ecVolunteer(t, pool), "EXPIRED")
	if ctrlFailed, err := repo.DeadLetterIfExhausted(ctx, ctrl); err != nil {
		t.Fatalf("DeadLetterIfExhausted(control): %v", err)
	} else if ctrlFailed {
		t.Fatal("control unit with no error cap and total 2 << ceiling 12 must NOT dead-letter")
	}
}

// --- seeding helpers (external test package; distinct from the in-package createTest* set) ---

func ecUser(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	id := types.NewID()
	name := "errcap-" + uuid.New().String()[:8]
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		id, name+"@test.example.com", name, "Err Cap "+name,
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// ecLeaf inserts an ACTIVE leaf with the given validation_config JSON and standard other configs.
func ecLeaf(t *testing.T, pool *pgxpool.Pool, creatorID types.ID, valConfig string) types.ID {
	t.Helper()
	id := types.NewID()
	slug := "errcap-leaf-" + uuid.New().String()[:8]
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, $4, 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false,"gpu_type":"","max_memory_mb":4096,"max_disk_mb":10240,"max_cpu_seconds":86400,"network_access":false,"min_vram_gb":0}',
			$6::jsonb,
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3,"checkpointing_enabled":false}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false,"min_bandwidth_mbps":0,"min_gpu_vram_mb":0}',
			false, 'PUBLIC', $5
		)`,
		id, "Err Cap Leaf "+slug, slug, "error-cap parity leaf", creatorID, valConfig,
	); err != nil {
		t.Fatalf("insert leaf: %v", err)
	}
	return id
}

func ecVolunteer(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	id := types.NewID()
	pubKey := make([]byte, 32)
	a, b := uuid.New(), uuid.New()
	copy(pubKey, a[:])
	copy(pubKey[16:], b[:])
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO volunteers (id, public_key, hardware_capabilities, available_runtimes, scheduling_mode, is_active, last_seen_at)
		VALUES ($1, $2, '{"cpu_cores":4}', $3, 'ALWAYS', true, NOW())`,
		id, pubKey, []string{"NATIVE", "CONTAINER"},
	); err != nil {
		t.Fatalf("insert volunteer: %v", err)
	}
	return id
}

// ecWorkUnit creates a CREATED work unit under leafID; the optional customize hook stamps
// per-unit overrides (max caps etc.) before insert. Returns the new unit's id.
func ecWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID types.ID, customize func(*workunit.WorkUnit)) types.ID {
	t.Helper()
	repo := workunit.NewPgxWorkUnitRepository(pool)
	wu := &workunit.WorkUnit{
		LeafID:           leafID,
		State:            workunit.WorkUnitStateCreated,
		Priority:         workunit.WorkUnitPriorityNormal,
		InputData:        json.RawMessage(`{"x":1}`),
		CodeArtifactRef:  "ref://errcap-" + uuid.New().String()[:8],
		Parameters:       json.RawMessage(`{}`),
		DeadlineSeconds:  3600,
		MaxReassignments: 3,
	}
	if customize != nil {
		customize(wu)
	}
	if err := repo.Create(context.Background(), wu); err != nil {
		t.Fatalf("create work unit: %v", err)
	}
	return wu.ID
}

// ecHistory inserts one work_unit_assignment_history row. outcome "" is a LIVE copy (NULL
// outcome); otherwise it is a closed copy with the given assignment_outcome.
func ecHistory(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, outcome string) {
	t.Helper()
	if outcome == "" {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at) VALUES ($1, $2, NOW())`,
			wuID, volID); err != nil {
			t.Fatalf("insert live copy: %v", err)
		}
		return
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at, outcome, outcome_at)
		 VALUES ($1, $2, NOW(), $3::assignment_outcome, NOW())`,
		wuID, volID, outcome); err != nil {
		t.Fatalf("insert %s copy: %v", outcome, err)
	}
}

var ecChecksumSeq int

// ecResult inserts one result row with the given validation_status and a unique checksum.
func ecResult(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, status string) {
	t.Helper()
	ecChecksumSeq++
	checksum := fmt.Sprintf("%064d", ecChecksumSeq)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO results (work_unit_id, volunteer_id, output_data, output_checksum, execution_metadata, validation_status)
		 VALUES ($1, $2, '{"x":1}'::jsonb, $3, '{}'::jsonb, $4)`,
		wuID, volID, checksum, status); err != nil {
		t.Fatalf("insert %s result: %v", status, err)
	}
}

func ecPendingCount(t *testing.T, pool *pgxpool.Pool, wuID types.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM results WHERE work_unit_id = $1 AND validation_status = 'PENDING'`,
		wuID).Scan(&n); err != nil {
		t.Fatalf("count pending results: %v", err)
	}
	return n
}
