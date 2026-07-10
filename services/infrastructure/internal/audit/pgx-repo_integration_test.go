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
		// Children first: result_audits / trusted_runners are FK-bearing children of
		// work_units/leafs/results/volunteers (audit spec Â§7.1 FK-cleanup lore).
		_, _ = pool.Exec(ctx, "DELETE FROM result_audits")
		_, _ = pool.Exec(ctx, "DELETE FROM trusted_runners")
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_attestations")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
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

	if err := leafRepo.DeleteVersion(ctx, vDel); err != nil {
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
