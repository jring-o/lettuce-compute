//go:build integration

package audit

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- fixtures ---------------------------------------------------------------------------

func setupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
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
		// FK-cleanup order (design doc §9.1 FK-cleanup lore) under the slice-3/4 RESTRICT
		// FKs: audit_repairs (→ result_audits + results) goes FIRST; credit_attestations
		// (→ credit_adjustments via adjustment_id, the slice-4 revocation FK) goes BEFORE
		// credit_adjustments; credit_adjustments (→ result_audits via audit_id, → credit_ledger
		// via ledger_entry_id) goes BEFORE result_audits and credit_ledger. Only then can
		// result_audits / trusted_runners be cleared. Errors are discarded, so a wrong order
		// would silently leak rows across tests rather than fail loudly.
		_, _ = pool.Exec(ctx, "DELETE FROM audit_repairs")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_attestations")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
		_, _ = pool.Exec(ctx, "DELETE FROM result_audits")
		_, _ = pool.Exec(ctx, "DELETE FROM trusted_runners")
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteer_rac")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM leaf_artifact_versions")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return pool, cleanup
}

func createTestUser(t *testing.T, pool *pgxpool.Pool, username string) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		id, username+"@test.example.com", username, "Test User "+username,
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("failed to create test user %s: %v", username, err)
	}
	return id
}

func createTestLeaf(t *testing.T, pool *pgxpool.Pool, creatorID types.ID) types.ID {
	t.Helper()
	id := types.NewID()
	slug := "audit-leaf-" + uuid.New().String()[:8]
	_, err := pool.Exec(context.Background(), `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, $4, 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"NATIVE","gpu_required":false}',
			'{"redundancy_factor":2,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576}',
			'{"credit_per_validated_work_unit":1.5}',
			'{"min_cpu_cores":1,"min_memory_mb":512,"min_disk_mb":1024,"gpu_required":false}',
			false, 'PUBLIC', $5
		)`,
		id, "Audit Leaf "+slug, slug, "A test leaf for audit tests", creatorID,
	)
	if err != nil {
		t.Fatalf("failed to create test leaf: %v", err)
	}
	return id
}

// createTestWorkUnit creates a COMPLETED work unit with the given deadline + optional HR pin.
func createTestWorkUnit(t *testing.T, pool *pgxpool.Pool, leafID types.ID, deadlineSeconds int, hrClass *string) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO work_units (
			id, leaf_id, state, priority, input_data, code_artifact_ref,
			parameters, deadline_seconds, max_reassignments, hr_class
		) VALUES (
			$1, $2, 'COMPLETED', 'NORMAL', $3, $4, $5, $6, 3, $7
		)`,
		id, leafID, json.RawMessage(`{"x":42}`), "ref://test-binary",
		json.RawMessage(`{"iterations":1000}`), deadlineSeconds, hrClass,
	)
	if err != nil {
		t.Fatalf("failed to create test work unit: %v", err)
	}
	return id
}

func randPubKey() []byte {
	pubKey := make([]byte, 32)
	copy(pubKey, uuid.New().NodeID())
	copy(pubKey[6:], uuid.New().NodeID())
	copy(pubKey[12:], uuid.New().NodeID())
	copy(pubKey[18:], uuid.New().NodeID())
	copy(pubKey[24:], uuid.New().NodeID())
	return pubKey
}

func createTestVolunteer(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO volunteers (
			id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at
		) VALUES ($1, $2, $3, $4, 'ALWAYS', true, now())`,
		id, randPubKey(),
		json.RawMessage(`{"cpu_cores":8,"max_cpu_cores":4,"memory_total_mb":32768,"max_memory_mb":16384,"disk_available_mb":102400,"max_disk_mb":10240}`),
		[]string{"NATIVE", "CONTAINER"},
	)
	if err != nil {
		t.Fatalf("failed to create test volunteer: %v", err)
	}
	return id
}

// createTestVolunteerWithDID creates a volunteer with a live (OK) DID binding, so its trust
// subject is the DID rather than the vol: sentinel.
func createTestVolunteerWithDID(t *testing.T, pool *pgxpool.Pool, did string) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO volunteers (
			id, public_key, hardware_capabilities, available_runtimes,
			scheduling_mode, is_active, last_seen_at, did, did_binding_status
		) VALUES ($1, $2, $3, $4, 'ALWAYS', true, now(), $5, 'OK')`,
		id, randPubKey(),
		json.RawMessage(`{"cpu_cores":8,"max_cpu_cores":4,"memory_total_mb":32768,"max_memory_mb":16384,"disk_available_mb":102400,"max_disk_mb":10240}`),
		[]string{"NATIVE", "CONTAINER"}, did,
	)
	if err != nil {
		t.Fatalf("failed to create test volunteer with DID: %v", err)
	}
	return id
}

func createTestResult(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID, checksum string) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO results (
			id, work_unit_id, volunteer_id, output_data, output_checksum,
			execution_metadata, validation_status
		) VALUES ($1, $2, $3, $4, $5, $6, 'AGREED')`,
		id, wuID, volID, json.RawMessage(`{"answer":42}`), checksum,
		json.RawMessage(`{"wall_clock_seconds":3600,"cpu_seconds_user":3200,"cpu_seconds_system":50,"cpu_cores_used":4,"peak_memory_mb":2048}`),
	)
	if err != nil {
		t.Fatalf("failed to create test result: %v", err)
	}
	return id
}

func createArtifactVersion(t *testing.T, pool *pgxpool.Pool, leafID, publishedBy types.ID, label string, publishedAt time.Time) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO leaf_artifact_versions (
			id, leaf_id, version_label, runtime_type, execution_config, published_by, published_at
		) VALUES ($1, $2, $3, 'NATIVE', '{"runtime":"NATIVE"}', $4, $5)`,
		id, leafID, label, publishedBy, publishedAt,
	)
	if err != nil {
		t.Fatalf("failed to create artifact version: %v", err)
	}
	return id
}

func randChecksum() string {
	return uuid.New().String()[:32] + uuid.New().String()[:32]
}

// auditFixture stands up the full prerequisite chain (user/leaf/volunteer/work unit/result)
// and returns the ids needed to enqueue an audit.
type auditFixture struct {
	userID types.ID
	leafID types.ID
	volID  types.ID
	wuID   types.ID
	result types.ID
}

func newAuditFixture(t *testing.T, pool *pgxpool.Pool, deadlineSeconds int, hrClass *string) auditFixture {
	t.Helper()
	userID := createTestUser(t, pool, "audit-"+uuid.New().String()[:8])
	leafID := createTestLeaf(t, pool, userID)
	volID := createTestVolunteer(t, pool)
	wuID := createTestWorkUnit(t, pool, leafID, deadlineSeconds, hrClass)
	resultID := createTestResult(t, pool, wuID, volID, randChecksum())
	return auditFixture{userID, leafID, volID, wuID, resultID}
}

// enqueueTestAudit enqueues one QUEUED audit against the fixture, returning the populated row.
func enqueueTestAudit(t *testing.T, repo *PgxAuditsRepository, f auditFixture, hrClass *string, versionID *types.ID) *Audit {
	t.Helper()
	key := randChecksum()
	a := &Audit{
		WorkUnitID:            f.wuID,
		LeafID:                f.leafID,
		AcceptedResultID:      f.result,
		AcceptedComparisonKey: &key,
		ComparisonSnapshot:    ComparisonSnapshot{ComparisonMode: "EXACT"},
		RequiredHRClass:       hrClass,
		ArtifactVersionID:     versionID,
		ExecutionSnapshot:     leaf.ExecutionConfig{Runtime: "NATIVE"},
	}
	if err := repo.Enqueue(context.Background(), a); err != nil {
		t.Fatalf("enqueue audit: %v", err)
	}
	return a
}

// registerTestRunner registers a fresh volunteer as an active trusted runner and returns its
// runner id.
func registerTestRunner(t *testing.T, pool *pgxpool.Pool, label string) types.ID {
	t.Helper()
	volID := createTestVolunteer(t, pool)
	rn, err := NewPgxRunnersRepository(pool).Register(context.Background(), volID, label, "")
	if err != nil {
		t.Fatalf("register runner: %v", err)
	}
	return rn.ID
}

func strptr(s string) *string { return &s }

// --- Claim ------------------------------------------------------------------------------

func TestAuditClaimHappyPath(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)

	f := newAuditFixture(t, pool, 1200, nil)
	enqueueTestAudit(t, repo, f, nil, nil)
	runnerID := registerTestRunner(t, pool, "runner-1")

	claimed, err := repo.Claim(ctx, runnerID, "GenuineIntel/linux/amd64")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("claim returned nil, want a job")
	}
	if claimed.Status != StatusClaimed {
		t.Errorf("status = %s, want CLAIMED", claimed.Status)
	}
	if claimed.ClaimedBy == nil || *claimed.ClaimedBy != runnerID {
		t.Errorf("claimed_by = %v, want %v", claimed.ClaimedBy, runnerID)
	}
	if claimed.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (incremented by the claim)", claimed.Attempts)
	}
	if claimed.ClaimedAt == nil || claimed.LeaseExpiresAt == nil {
		t.Fatal("claimed_at / lease_expires_at must be set on a claimed row")
	}

	// A second claim finds nothing (the only job is now CLAIMED).
	again, err := repo.Claim(ctx, runnerID, "GenuineIntel/linux/amd64")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if again != nil {
		t.Errorf("second claim = %v, want nil (queue drained)", again.ID)
	}
}

func TestAuditClaimLease(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)
	runnerID := registerTestRunner(t, pool, "runner-lease")

	// deadline 1200s > floor: lease = 1200s.
	fBig := newAuditFixture(t, pool, 1200, nil)
	enqueueTestAudit(t, repo, fBig, nil, nil)
	big, err := repo.Claim(ctx, runnerID, "any")
	if err != nil || big == nil {
		t.Fatalf("claim big: %v (nil=%v)", err, big == nil)
	}
	if d := big.LeaseExpiresAt.Sub(*big.ClaimedAt); absDur(d-1200*time.Second) > 2*time.Second {
		t.Errorf("lease span = %v, want ~1200s (unit deadline)", d)
	}

	// deadline 60s < floor: lease = LeaseFloor (600s).
	fSmall := newAuditFixture(t, pool, 60, nil)
	enqueueTestAudit(t, repo, fSmall, nil, nil)
	small, err := repo.Claim(ctx, runnerID, "any")
	if err != nil || small == nil {
		t.Fatalf("claim small: %v (nil=%v)", err, small == nil)
	}
	if d := small.LeaseExpiresAt.Sub(*small.ClaimedAt); absDur(d-LeaseFloor) > 2*time.Second {
		t.Errorf("lease span = %v, want ~%v (floor)", d, LeaseFloor)
	}
}

func TestAuditClaimHRClassFilter(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)

	const pinned = "GenuineIntel/linux/amd64"
	f := newAuditFixture(t, pool, 600, strptr(pinned))
	enqueueTestAudit(t, repo, f, strptr(pinned), nil)
	runnerID := registerTestRunner(t, pool, "runner-hr")

	// A runner of the wrong class cannot claim the pinned job.
	wrong, err := repo.Claim(ctx, runnerID, "AuthenticAMD/linux/amd64")
	if err != nil {
		t.Fatalf("claim wrong class: %v", err)
	}
	if wrong != nil {
		t.Fatalf("wrong-class runner claimed a pinned job (%v); want nil", wrong.ID)
	}

	// The matching class claims it.
	right, err := repo.Claim(ctx, runnerID, pinned)
	if err != nil {
		t.Fatalf("claim matching class: %v", err)
	}
	if right == nil {
		t.Fatal("matching-class runner got nil, want the pinned job")
	}

	// A NULL-pin job is claimable by any class.
	f2 := newAuditFixture(t, pool, 600, nil)
	enqueueTestAudit(t, repo, f2, nil, nil)
	any, err := repo.Claim(ctx, runnerID, "Whatever/plan9/sparc")
	if err != nil || any == nil {
		t.Fatalf("claim null-pin job: %v (nil=%v)", err, any == nil)
	}
}

func TestAuditClaimPerRunnerCap(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)
	runnerID := registerTestRunner(t, pool, "greedy")

	// Enqueue one more than the cap.
	for i := 0; i < MaxConcurrentClaims+1; i++ {
		f := newAuditFixture(t, pool, 600, nil)
		enqueueTestAudit(t, repo, f, nil, nil)
	}

	for i := 0; i < MaxConcurrentClaims; i++ {
		got, err := repo.Claim(ctx, runnerID, "any")
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if got == nil {
			t.Fatalf("claim %d returned nil below the cap", i)
		}
	}
	// The (cap+1)th concurrent claim is refused even though a QUEUED job remains.
	over, err := repo.Claim(ctx, runnerID, "any")
	if err != nil {
		t.Fatalf("over-cap claim: %v", err)
	}
	if over != nil {
		t.Fatalf("runner claimed job %v past the %d-lease cap", over.ID, MaxConcurrentClaims)
	}
}

func TestAuditClaimConcurrentDistinct(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)

	// Two queued jobs, two runners claiming concurrently â€” SKIP LOCKED must hand out two
	// DISTINCT jobs, never the same one twice.
	enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	r1 := registerTestRunner(t, pool, "r1")
	r2 := registerTestRunner(t, pool, "r2")

	var wg sync.WaitGroup
	got := make([]*Audit, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	runners := []types.ID{r1, r2}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got[i], errs[i] = repo.Claim(ctx, runners[i], "any")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d claim: %v", i, err)
		}
		if got[i] == nil {
			t.Fatalf("goroutine %d got nil; both jobs should be claimable", i)
		}
	}
	if got[0].ID == got[1].ID {
		t.Fatalf("both runners claimed the SAME job %v (SKIP LOCKED failed)", got[0].ID)
	}
}

// --- Complete / Release guards ----------------------------------------------------------

func TestAuditCompleteVerdictGuards(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)

	a := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	runnerID := registerTestRunner(t, pool, "claimant")
	other := registerTestRunner(t, pool, "impostor")

	claimed, err := repo.Claim(ctx, runnerID, "any")
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v (nil=%v)", err, claimed == nil)
	}

	// The wrong runner cannot complete another runner's claim.
	if err := repo.CompleteVerdict(ctx, a.ID, other, VerdictMatch, "", []byte("out"), "cs", false); err != ErrNotClaimant {
		t.Fatalf("wrong-runner complete err = %v, want ErrNotClaimant", err)
	}

	// The claimant completes it.
	out := []byte(`{"answer":42}`)
	if err := repo.CompleteVerdict(ctx, a.ID, runnerID, VerdictMatch, "", out, "deadbeef", false); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, err := repo.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusCompleted || got.Verdict == nil || *got.Verdict != VerdictMatch {
		t.Errorf("after complete: status=%s verdict=%v, want COMPLETED/MATCH", got.Status, got.Verdict)
	}
	if got.RunnerOutputChecksum == nil || *got.RunnerOutputChecksum != "deadbeef" {
		t.Errorf("checksum = %v, want deadbeef", got.RunnerOutputChecksum)
	}

	// A double-complete is refused (no longer CLAIMED).
	if err := repo.CompleteVerdict(ctx, a.ID, runnerID, VerdictMatch, "", out, "deadbeef", false); err != ErrNotClaimant {
		t.Fatalf("double complete err = %v, want ErrNotClaimant", err)
	}
}

func TestAuditReleaseFailureRequeueThenExpire(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)

	a := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	runnerID := registerTestRunner(t, pool, "flaky")

	// Consume the attempt budget: each claim increments attempts; a failure below the budget
	// requeues, and the last failure (attempts == MaxAttempts) expires.
	for i := 1; i <= MaxAttempts; i++ {
		claimed, err := repo.Claim(ctx, runnerID, "any")
		if err != nil || claimed == nil {
			t.Fatalf("claim %d: %v (nil=%v)", i, err, claimed == nil)
		}
		if claimed.Attempts != i {
			t.Fatalf("attempts on claim %d = %d, want %d", i, claimed.Attempts, i)
		}
		if err := repo.ReleaseFailure(ctx, a.ID, runnerID, "boom"); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
		got, err := repo.GetByID(ctx, a.ID)
		if err != nil {
			t.Fatalf("get after release %d: %v", i, err)
		}
		if i < MaxAttempts {
			if got.Status != StatusQueued {
				t.Fatalf("release %d: status = %s, want QUEUED (requeued)", i, got.Status)
			}
			if got.ClaimedBy != nil {
				t.Errorf("release %d: claimed_by should be cleared on requeue", i)
			}
		} else {
			if got.Status != StatusExpired {
				t.Fatalf("final release: status = %s, want EXPIRED (attempts exhausted)", got.Status)
			}
			if got.Verdict != nil {
				t.Errorf("EXPIRED row must carry no verdict, got %v", got.Verdict)
			}
			if got.VerdictDetail == nil || *got.VerdictDetail != "boom" {
				t.Errorf("verdict_detail = %v, want the failure message preserved", got.VerdictDetail)
			}
		}
	}

	// A wrong-runner release is refused.
	other := registerTestRunner(t, pool, "impostor2")
	if err := repo.ReleaseFailure(ctx, a.ID, other, "x"); err != ErrNotClaimant {
		t.Fatalf("release on non-CLAIMED/wrong-runner err = %v, want ErrNotClaimant", err)
	}
}

// --- Sweeps -----------------------------------------------------------------------------

func TestAuditSweepLapsedLeases(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)
	runnerID := registerTestRunner(t, pool, "sweeper")

	requeueTarget := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	expireTarget := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	if _, err := repo.Claim(ctx, runnerID, "any"); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if _, err := repo.Claim(ctx, runnerID, "any"); err != nil {
		t.Fatalf("claim 2: %v", err)
	}

	// Lapse both leases; force the second past the attempt budget so it expires.
	if _, err := pool.Exec(ctx,
		"UPDATE result_audits SET lease_expires_at = now() - interval '5 minutes'"); err != nil {
		t.Fatalf("age leases: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE result_audits SET attempts = $1 WHERE id = $2", MaxAttempts, expireTarget.ID); err != nil {
		t.Fatalf("exhaust attempts: %v", err)
	}

	requeued, expired, err := repo.SweepLapsedLeases(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if requeued != 1 || expired != 1 {
		t.Fatalf("sweep counts = (requeued %d, expired %d), want (1, 1)", requeued, expired)
	}

	rq, _ := repo.GetByID(ctx, requeueTarget.ID)
	if rq.Status != StatusQueued || rq.ClaimedBy != nil {
		t.Errorf("requeue target: status=%s claimed_by=%v, want QUEUED/nil", rq.Status, rq.ClaimedBy)
	}
	ex, _ := repo.GetByID(ctx, expireTarget.ID)
	if ex.Status != StatusExpired {
		t.Errorf("expire target: status=%s, want EXPIRED", ex.Status)
	}
}

func TestAuditSweepStaleQueued(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)

	stale := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	fresh := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)

	// Age the first past the queue lifetime.
	if _, err := pool.Exec(ctx,
		"UPDATE result_audits SET created_at = now() - make_interval(secs => $1) WHERE id = $2",
		int(QueuedLifetime.Seconds())+3600, stale.ID); err != nil {
		t.Fatalf("age created_at: %v", err)
	}

	expired, err := repo.SweepStaleQueued(ctx)
	if err != nil {
		t.Fatalf("sweep stale: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want 1", expired)
	}
	s, _ := repo.GetByID(ctx, stale.ID)
	if s.Status != StatusExpired {
		t.Errorf("stale: status = %s, want EXPIRED", s.Status)
	}
	fr, _ := repo.GetByID(ctx, fresh.ID)
	if fr.Status != StatusQueued {
		t.Errorf("fresh: status = %s, want QUEUED (untouched)", fr.Status)
	}
}

// --- Registry ---------------------------------------------------------------------------

func TestActiveRunnerSubjects(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	runners := NewPgxRunnersRepository(pool)

	// A keypair-only runner â†’ vol: sentinel subject.
	volPlain := createTestVolunteer(t, pool)
	if _, err := runners.Register(ctx, volPlain, "plain", ""); err != nil {
		t.Fatalf("register plain: %v", err)
	}
	// A DID-bound runner â†’ the DID is its subject.
	const did = "did:plc:auditsubjecttest"
	volDID := createTestVolunteerWithDID(t, pool, did)
	if _, err := runners.Register(ctx, volDID, "did", ""); err != nil {
		t.Fatalf("register did: %v", err)
	}
	// A deactivated runner is excluded.
	volOff := createTestVolunteer(t, pool)
	if _, err := runners.Register(ctx, volOff, "off", ""); err != nil {
		t.Fatalf("register off: %v", err)
	}
	if err := runners.Deactivate(ctx, volOff); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	subjects, err := runners.ActiveRunnerSubjects(ctx)
	if err != nil {
		t.Fatalf("ActiveRunnerSubjects: %v", err)
	}
	set := map[string]bool{}
	for _, s := range subjects {
		set[s] = true
	}
	if len(subjects) != 2 {
		t.Fatalf("subjects = %v, want exactly 2 (deactivated excluded)", subjects)
	}
	if !set[trust.SubjectForVolunteerID(volPlain)] {
		t.Errorf("missing keypair sentinel subject for %v", volPlain)
	}
	if !set[did] {
		t.Errorf("missing DID subject %q (DID binding must resolve to the DID, not the sentinel)", did)
	}
}

func TestRunnerRegisterUpsertAndUnknown(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	runners := NewPgxRunnersRepository(pool)

	// Unknown volunteer â†’ ErrUnknownVolunteer (FK violation surfaced).
	if _, err := runners.Register(ctx, types.NewID(), "ghost", ""); err != ErrUnknownVolunteer {
		t.Fatalf("register unknown volunteer err = %v, want ErrUnknownVolunteer", err)
	}

	volID := createTestVolunteer(t, pool)
	first, err := runners.Register(ctx, volID, "label-one", "note-one")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Deactivate then re-register: the upsert reactivates + relabels the SAME row.
	if err := runners.Deactivate(ctx, volID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	second, err := runners.Register(ctx, volID, "label-two", "note-two")
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("upsert minted a new row (%v vs %v); want the same row reactivated", second.ID, first.ID)
	}
	if !second.Active || second.Label != "label-two" {
		t.Errorf("re-registered runner = active %v / label %q, want active/label-two", second.Active, second.Label)
	}

	// GetActiveByVolunteerID resolves it; Deactivate then Get â†’ ErrNotRegistered.
	if _, err := runners.GetActiveByVolunteerID(ctx, volID); err != nil {
		t.Fatalf("get active: %v", err)
	}
	if err := runners.Deactivate(ctx, volID); err != nil {
		t.Fatalf("deactivate 2: %v", err)
	}
	if _, err := runners.GetActiveByVolunteerID(ctx, volID); err != ErrNotRegistered {
		t.Fatalf("get inactive err = %v, want ErrNotRegistered", err)
	}
	// Deactivating an unknown volunteer â†’ ErrNotRegistered.
	if err := runners.Deactivate(ctx, types.NewID()); err != ErrNotRegistered {
		t.Fatalf("deactivate unknown err = %v, want ErrNotRegistered", err)
	}
}

func TestEnqueueDuplicateOpenAudit(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxAuditsRepository(pool)

	f := newAuditFixture(t, pool, 600, nil)
	enqueueTestAudit(t, repo, f, nil, nil)

	// A second OPEN audit for the same unit violates the partial unique index.
	dupKey := randChecksum()
	dup := &Audit{
		WorkUnitID:            f.wuID,
		LeafID:                f.leafID,
		AcceptedResultID:      f.result,
		AcceptedComparisonKey: &dupKey,
		ComparisonSnapshot:    ComparisonSnapshot{ComparisonMode: "EXACT"},
		ExecutionSnapshot:     leaf.ExecutionConfig{Runtime: "NATIVE"},
	}
	if err := repo.Enqueue(context.Background(), dup); err != ErrDuplicateOpenAudit {
		t.Fatalf("duplicate enqueue err = %v, want ErrDuplicateOpenAudit", err)
	}
}

// --- Artifact-GC pin (spec Â§7.5, F-M7) --------------------------------------------------

func TestAuditGCPinPruneAndDelete(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	auditRepo := NewPgxAuditsRepository(pool)
	leafRepo := leaf.NewPgxRepository(pool)

	// --- PruneAllVersions spares an open-audit version, prunes it once completed ---
	f := newAuditFixture(t, pool, 600, nil)
	now := time.Now().UTC()
	vOld := createArtifactVersion(t, pool, f.leafID, f.userID, "v-old", now.Add(-time.Hour))
	_ = createArtifactVersion(t, pool, f.leafID, f.userID, "v-new", now)

	a := enqueueTestAudit(t, auditRepo, f, nil, &vOld)

	// keep=1 would prune v-old (rn=2), but the OPEN audit pins it.
	pruned, err := leafRepo.PruneAllVersions(ctx, 1)
	if err != nil {
		t.Fatalf("prune with open audit: %v", err)
	}
	if _, err := leafRepo.GetVersionByID(ctx, vOld); err != nil {
		t.Fatalf("open-audit version was pruned (pruned=%d): %v", pruned, err)
	}

	// Complete the audit â†’ no longer OPEN â†’ v-old becomes prunable.
	runnerID := registerTestRunner(t, pool, "gc-runner")
	if _, err := auditRepo.Claim(ctx, runnerID, "any"); err != nil {
		t.Fatalf("claim for GC: %v", err)
	}
	if err := auditRepo.CompleteVerdict(ctx, a.ID, runnerID, VerdictMatch, "", []byte("o"), "cs", false); err != nil {
		t.Fatalf("complete for GC: %v", err)
	}
	if _, err := leafRepo.PruneAllVersions(ctx, 1); err != nil {
		t.Fatalf("prune after complete: %v", err)
	}
	if _, err := leafRepo.GetVersionByID(ctx, vOld); err == nil {
		t.Fatal("v-old should be pruned once the audit is no longer open")
	}

	// --- DeleteVersion is NOT blocked by an open audit (SET NULL degrade) ---
	f2 := newAuditFixture(t, pool, 600, nil)
	vDel := createArtifactVersion(t, pool, f2.leafID, f2.userID, "v-del", now)
	a2 := enqueueTestAudit(t, auditRepo, f2, nil, &vDel)

	if err := leafRepo.DeleteVersion(ctx, f2.leafID, vDel); err != nil {
		t.Fatalf("DeleteVersion must NOT be blocked by an open audit: %v", err)
	}
	if _, err := leafRepo.GetVersionByID(ctx, vDel); err == nil {
		t.Fatal("v-del should be gone after DeleteVersion")
	}
	// The audit survives with a nulled artifact_version_id (degrades to INCONCLUSIVE later).
	got, err := auditRepo.GetByID(ctx, a2.ID)
	if err != nil {
		t.Fatalf("get audit after version delete: %v", err)
	}
	if got.ArtifactVersionID != nil {
		t.Errorf("artifact_version_id = %v, want NULL after the referenced version was deleted", got.ArtifactVersionID)
	}
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// --- slice-3 enforcement surface --------------------------------------------------------

// completeAudit claims the (single QUEUED) audit `a` as the given runner+class and completes
// it with the verdict + enforcement-eligibility stamp, returning the reloaded row. Callers
// must ensure `a` is the only claimable QUEUED job when this runs (assert on the claimed id).
func completeAudit(t *testing.T, repo *PgxAuditsRepository, a *Audit, runnerID types.ID, runnerClass string, verdict Verdict, eligible bool) *Audit {
	t.Helper()
	ctx := context.Background()
	claimed, err := repo.Claim(ctx, runnerID, runnerClass)
	if err != nil || claimed == nil {
		t.Fatalf("claim for completion: %v (nil=%v)", err, claimed == nil)
	}
	if claimed.ID != a.ID {
		t.Fatalf("claim grabbed a different audit (%v) than the target (%v)", claimed.ID, a.ID)
	}
	out := []byte("groundtruth-" + a.ID.String())
	if err := repo.CompleteVerdict(ctx, a.ID, runnerID, verdict, "", out, "cs", eligible); err != nil {
		t.Fatalf("complete verdict: %v", err)
	}
	got, err := repo.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reload after complete: %v", err)
	}
	return got
}

func TestCompleteVerdictAwaitingConfirmationFold(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewPgxAuditsRepository(pool)
	runner := registerTestRunner(t, pool, "fold-runner")

	// Eligible MISMATCH original → folded to AWAITING_CONFIRMATION inside the verdict UPDATE.
	a1 := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	got := completeAudit(t, repo, a1, runner, "classX", VerdictMismatch, true)
	if !got.EnforcementEligible || got.EnforcementState != EnforcementAwaitingConfirmation {
		t.Errorf("eligible MISMATCH: eligible=%v state=%q, want true/AWAITING_CONFIRMATION",
			got.EnforcementEligible, got.EnforcementState)
	}

	// Eligible MATCH stays NONE (the fold is MISMATCH-only) but still records the era.
	a2 := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	got = completeAudit(t, repo, a2, runner, "classX", VerdictMatch, true)
	if !got.EnforcementEligible || got.EnforcementState != EnforcementNone {
		t.Errorf("eligible MATCH: eligible=%v state=%q, want true/NONE",
			got.EnforcementEligible, got.EnforcementState)
	}

	// Ineligible (observe-era) MISMATCH stays NONE and ineligible — never actionable (F-M10).
	a3 := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	got = completeAudit(t, repo, a3, runner, "classX", VerdictMismatch, false)
	if got.EnforcementEligible || got.EnforcementState != EnforcementNone {
		t.Errorf("ineligible MISMATCH: eligible=%v state=%q, want false/NONE",
			got.EnforcementEligible, got.EnforcementState)
	}
}

func TestClaimStampsHRClass(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)

	enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	runner := registerTestRunner(t, pool, "class-runner")

	const class = "GenuineIntel/linux/amd64"
	claimed, err := repo.Claim(ctx, runner, class)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v (nil=%v)", err, claimed == nil)
	}
	if claimed.ClaimedHRClass == nil || *claimed.ClaimedHRClass != class {
		t.Errorf("claimed_hr_class = %v, want %q (stamped by the claim)", claimed.ClaimedHRClass, class)
	}
	// And it persists on the row.
	got, _ := repo.GetByID(ctx, claimed.ID)
	if got.ClaimedHRClass == nil || *got.ClaimedHRClass != class {
		t.Errorf("persisted claimed_hr_class = %v, want %q", got.ClaimedHRClass, class)
	}
}

func TestEnqueueConfirmationCopiesAndConflict(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)
	runner := registerTestRunner(t, pool, "conf-runner")

	root := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	rootDone := completeAudit(t, repo, root, runner, "classX", VerdictMismatch, true)

	conf, err := repo.EnqueueConfirmation(ctx, rootDone.ID)
	if err != nil {
		t.Fatalf("enqueue confirmation: %v", err)
	}
	if conf.ConfirmsAuditID == nil || *conf.ConfirmsAuditID != rootDone.ID {
		t.Errorf("confirms_audit_id = %v, want %v", conf.ConfirmsAuditID, rootDone.ID)
	}
	if conf.Status != StatusQueued {
		t.Errorf("confirmation status = %s, want QUEUED", conf.Status)
	}
	if conf.WorkUnitID != rootDone.WorkUnitID || conf.LeafID != rootDone.LeafID ||
		conf.AcceptedResultID != rootDone.AcceptedResultID {
		t.Errorf("confirmation did not copy the root's unit/leaf/accepted ids")
	}
	if !eqStrPtr(conf.AcceptedComparisonKey, rootDone.AcceptedComparisonKey) {
		t.Errorf("accepted_comparison_key = %v, want copied %v", conf.AcceptedComparisonKey, rootDone.AcceptedComparisonKey)
	}
	if conf.EnforcementEligible || conf.EnforcementState != EnforcementNone {
		t.Errorf("fresh confirmation eligible=%v state=%q, want false/NONE", conf.EnforcementEligible, conf.EnforcementState)
	}

	// A second confirmation while the first is still OPEN (QUEUED) violates uq_result_audits_open_unit.
	if _, err := repo.EnqueueConfirmation(ctx, rootDone.ID); err != ErrDuplicateOpenAudit {
		t.Fatalf("duplicate confirmation err = %v, want ErrDuplicateOpenAudit", err)
	}
}

// A confirmation on an UNPINNED unit refuses the root's own runner (a) and a same-class
// runner (c); it accepts a class-diverse, non-parent runner. The M1 regression.
func TestConfirmationClaimExclusionsUnpinned(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)

	root := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	runnerRoot := registerTestRunner(t, pool, "root-runner")
	rootDone := completeAudit(t, repo, root, runnerRoot, "classX", VerdictMismatch, true)
	if _, err := repo.EnqueueConfirmation(ctx, rootDone.ID); err != nil {
		t.Fatalf("enqueue confirmation: %v", err)
	}
	runnerOther := registerTestRunner(t, pool, "other-runner")

	// (a) The root's own runner cannot claim its confirmation (class differs, isolating (a)).
	if got, err := repo.Claim(ctx, runnerRoot, "classY"); err != nil || got != nil {
		t.Fatalf("root runner claimed its own confirmation (%v, err=%v); want refused", got, err)
	}
	// (c) A same-class runner cannot claim an unpinned unit's confirmation.
	if got, err := repo.Claim(ctx, runnerOther, "classX"); err != nil || got != nil {
		t.Fatalf("same-class runner claimed unpinned confirmation (%v, err=%v); want refused", got, err)
	}
	// A class-diverse, non-parent runner CAN claim it.
	got, err := repo.Claim(ctx, runnerOther, "classY")
	if err != nil || got == nil {
		t.Fatalf("class-diverse runner claim: %v (nil=%v); want the confirmation", err, got == nil)
	}
	if got.ConfirmsAuditID == nil || *got.ConfirmsAuditID != rootDone.ID {
		t.Errorf("claimed row is not the confirmation of the root")
	}
}

// A confirmation on a PINNED unit has the hardware channel closed by the pin, so the
// class-diversity rule does not apply: a different runner of the SAME (pinned) class claims it.
func TestConfirmationClaimPinnedAllowsSameClass(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)

	const pin = "classP"
	root := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, strptr(pin)), strptr(pin), nil)
	runnerRoot := registerTestRunner(t, pool, "pinned-root")
	rootDone := completeAudit(t, repo, root, runnerRoot, pin, VerdictMismatch, true)
	if _, err := repo.EnqueueConfirmation(ctx, rootDone.ID); err != nil {
		t.Fatalf("enqueue confirmation: %v", err)
	}
	runnerOther := registerTestRunner(t, pool, "pinned-other")

	// (a) still refuses the parent runner even at the matching class.
	if got, err := repo.Claim(ctx, runnerRoot, pin); err != nil || got != nil {
		t.Fatalf("parent runner claimed pinned confirmation (%v, err=%v); want refused", got, err)
	}
	// A different runner of the same pinned class claims it ((c) does not apply to pinned units).
	got, err := repo.Claim(ctx, runnerOther, pin)
	if err != nil || got == nil {
		t.Fatalf("same-class different runner on a pinned unit: %v (nil=%v); want the confirmation", err, got == nil)
	}
}

// Each re-enqueued confirmation must reach a FRESH runner: a runner that INCONCLUSIVE'd a
// prior confirmation of the same root cannot claim the re-enqueue. The M2 regression.
func TestConfirmationClaimExcludesPriorInconclusiveToucher(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)

	root := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	runnerRoot := registerTestRunner(t, pool, "m2-root")
	rootDone := completeAudit(t, repo, root, runnerRoot, "classX", VerdictMismatch, true)

	// conf1: claimed by runnerA (class-diverse), completed INCONCLUSIVE.
	conf1, err := repo.EnqueueConfirmation(ctx, rootDone.ID)
	if err != nil {
		t.Fatalf("enqueue conf1: %v", err)
	}
	runnerA := registerTestRunner(t, pool, "m2-a")
	got, err := repo.Claim(ctx, runnerA, "classY")
	if err != nil || got == nil || got.ID != conf1.ID {
		t.Fatalf("runnerA claim conf1: %v (nil=%v)", err, got == nil)
	}
	if err := repo.CompleteInconclusive(ctx, conf1.ID, runnerA, "ARTIFACT_UNAVAILABLE"); err != nil {
		t.Fatalf("complete conf1 inconclusive: %v", err)
	}

	// conf2: the re-enqueue. runnerA (prior INCONCLUSIVE toucher) is refused; a fresh
	// runnerB is accepted.
	conf2, err := repo.EnqueueConfirmation(ctx, rootDone.ID)
	if err != nil {
		t.Fatalf("enqueue conf2: %v", err)
	}
	if refused, err := repo.Claim(ctx, runnerA, "classY"); err != nil || refused != nil {
		t.Fatalf("prior-INCONCLUSIVE runnerA claimed the re-enqueue (%v, err=%v); want refused", refused, err)
	}
	runnerB := registerTestRunner(t, pool, "m2-b")
	fresh, err := repo.Claim(ctx, runnerB, "classY")
	if err != nil || fresh == nil || fresh.ID != conf2.ID {
		t.Fatalf("fresh runnerB claim conf2: %v (nil=%v)", err, fresh == nil)
	}
}

func TestSweepStaleQueuedPerRowLifetime(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)
	runner := registerTestRunner(t, pool, "stale-runner")

	// A confirmation on unitB, aged 30h: past the 24h confirmation lifetime → expires.
	rootB := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	rootBDone := completeAudit(t, repo, rootB, runner, "classX", VerdictMismatch, true)
	confB, err := repo.EnqueueConfirmation(ctx, rootBDone.ID)
	if err != nil {
		t.Fatalf("enqueue confirmation: %v", err)
	}

	// An ORIGINAL sampled audit on unitA, aged 30h: still WELL under the 72h original lifetime.
	origA := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)

	if _, err := pool.Exec(ctx,
		"UPDATE result_audits SET created_at = now() - interval '30 hours' WHERE id = ANY($1)",
		[]types.ID{confB.ID, origA.ID}); err != nil {
		t.Fatalf("age created_at: %v", err)
	}

	expired, err := repo.SweepStaleQueued(ctx)
	if err != nil {
		t.Fatalf("sweep stale: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want 1 (only the >24h confirmation)", expired)
	}
	if c, _ := repo.GetByID(ctx, confB.ID); c.Status != StatusExpired {
		t.Errorf("confirmation status = %s, want EXPIRED (past 24h)", c.Status)
	}
	if o, _ := repo.GetByID(ctx, origA.ID); o.Status != StatusQueued {
		t.Errorf("original status = %s, want QUEUED (under 72h)", o.Status)
	}
}

func TestListActionableRootsPredicate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)
	runnerRoot := registerTestRunner(t, pool, "actionable-root")
	runnerConf := registerTestRunner(t, pool, "actionable-conf")

	// Actionable: eligible MISMATCH original (→ AWAITING_CONFIRMATION).
	aMismatch := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	doneMismatch := completeAudit(t, repo, aMismatch, runnerRoot, "classX", VerdictMismatch, true)

	// Not actionable: ineligible (observe-era) MISMATCH — the F-M10 structural exclusion.
	aObserve := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	completeAudit(t, repo, aObserve, runnerRoot, "classX", VerdictMismatch, false)

	// Not actionable: eligible MATCH.
	aMatch := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	completeAudit(t, repo, aMatch, runnerRoot, "classX", VerdictMatch, true)

	// Not actionable: a confirmation row, even when eligible MISMATCH (confirms_audit_id set).
	conf, err := repo.EnqueueConfirmation(ctx, doneMismatch.ID)
	if err != nil {
		t.Fatalf("enqueue confirmation: %v", err)
	}
	claimedConf, err := repo.Claim(ctx, runnerConf, "classY")
	if err != nil || claimedConf == nil || claimedConf.ID != conf.ID {
		t.Fatalf("claim confirmation: %v (nil=%v)", err, claimedConf == nil)
	}
	if err := repo.CompleteVerdict(ctx, conf.ID, runnerConf, VerdictMismatch, "", []byte("gt"), "cs", true); err != nil {
		t.Fatalf("complete confirmation: %v", err)
	}

	roots, err := repo.ListActionableRoots(ctx, 100)
	if err != nil {
		t.Fatalf("list actionable roots: %v", err)
	}
	if len(roots) != 1 || roots[0].ID != doneMismatch.ID {
		ids := make([]types.ID, len(roots))
		for i, r := range roots {
			ids[i] = r.ID
		}
		t.Fatalf("actionable roots = %v, want exactly [%v]", ids, doneMismatch.ID)
	}
}

func TestSetEnforcementStateGuards(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)
	runner := registerTestRunner(t, pool, "guard-runner")

	a := enqueueTestAudit(t, repo, newAuditFixture(t, pool, 600, nil), nil, nil)
	done := completeAudit(t, repo, a, runner, "classX", VerdictMismatch, true) // AWAITING_CONFIRMATION

	// AWAITING_CONFIRMATION → ENFORCED: guard hits, enforced_at stamped.
	ok, err := repo.SetEnforcementState(ctx, done.ID, EnforcementEnforced)
	if err != nil || !ok {
		t.Fatalf("set ENFORCED: ok=%v err=%v, want true/nil", ok, err)
	}
	got, _ := repo.GetByID(ctx, done.ID)
	if got.EnforcementState != EnforcementEnforced || got.EnforcedAt == nil {
		t.Errorf("after ENFORCED: state=%q enforced_at=%v, want ENFORCED + a timestamp", got.EnforcementState, got.EnforcedAt)
	}

	// A terminal row is guarded: a further transition is a no-op (ok=false), state unchanged.
	ok, err = repo.SetEnforcementState(ctx, done.ID, EnforcementStalled)
	if err != nil {
		t.Fatalf("set STALLED on terminal: %v", err)
	}
	if ok {
		t.Errorf("guard let a terminal ENFORCED row transition to STALLED")
	}
	got, _ = repo.GetByID(ctx, done.ID)
	if got.EnforcementState != EnforcementEnforced {
		t.Errorf("terminal state mutated to %q, want ENFORCED", got.EnforcementState)
	}
}

func TestClaimRepairOnce(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := NewPgxAuditsRepository(pool)
	runner := registerTestRunner(t, pool, "repair-runner")

	f := newAuditFixture(t, pool, 600, nil)
	a := enqueueTestAudit(t, repo, f, nil, nil)
	done := completeAudit(t, repo, a, runner, "classX", VerdictMismatch, true)

	// First claim of the result wins.
	claimed, err := repo.ClaimRepair(ctx, done.ID, f.result)
	if err != nil || !claimed {
		t.Fatalf("first ClaimRepair: claimed=%v err=%v, want true/nil", claimed, err)
	}
	// A second claim of the SAME result loses (UNIQUE(result_id)) — the idempotency guard.
	claimed, err = repo.ClaimRepair(ctx, done.ID, f.result)
	if err != nil {
		t.Fatalf("second ClaimRepair: %v", err)
	}
	if claimed {
		t.Errorf("second ClaimRepair of the same result claimed again; want false")
	}

	// A fresh result (distinct volunteer — uq_results_work_unit_volunteer forbids a second
	// result from the same volunteer on the unit) claims true.
	other := createTestResult(t, pool, f.wuID, createTestVolunteer(t, pool), randChecksum())
	claimed, err = repo.ClaimRepair(ctx, done.ID, other)
	if err != nil || !claimed {
		t.Fatalf("ClaimRepair of a fresh result: claimed=%v err=%v, want true/nil", claimed, err)
	}
}

func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
