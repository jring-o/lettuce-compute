//go:build integration

package server

// BG-17 regression suite: a handler that opens a pool transaction must never
// acquire a SECOND pool connection while the transaction holds its first. Under
// a submit storm, N handlers holding N tx connections all waiting for an
// (N+1)th connection that only another stuck handler could release deadlocks
// the whole pool (RequestWorkUnit hit exactly this and was fixed with
// leaf.GetByIDTx; the fix was never propagated to the three handlers below).
//
// The deadlock reproduces deterministically on a MaxConns=1 pool: pool.Begin
// takes the only connection, so the FIRST in-transaction pool read blocks
// forever. Each test drives one handler end-to-end on such a pool under a
// bounded context — pre-fix code times out (FAIL), fixed code completes
// because every read runs either before the transaction or on its connection.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// bg17Timeout bounds each handler call. A healthy submit on an idle test DB
// completes in milliseconds; only the pre-fix in-tx pool acquire (which would
// otherwise block forever) can consume this budget.
const bg17Timeout = 10 * time.Second

// newBG17SingleConnPool connects to the test database with MaxConns=1 — the
// deterministic deadlock harness — and registers cleanup of all rows the BG-17
// tests create.
func newBG17SingleConnPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse test DB URL: %v", err)
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	})
	return pool
}

// bg17Seed creates a user, an ACTIVE WASM leaf (redundancy 1), a QUEUED work
// unit, and a registered volunteer, returning the IDs and the volunteer's key.
func bg17Seed(t *testing.T, pool *pgxpool.Pool) (leafID, wuID types.ID, vol *volunteer.Volunteer, pub ed25519.PublicKey) {
	t.Helper()
	ctx := context.Background()

	userID := types.NewID()
	username := "bg17-user-" + uuid.New().String()[:8]
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		userID, username+"@test.example.com", username, "BG17 Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	leafID = types.NewID()
	slug := "bg17-leaf-" + uuid.New().String()[:8]
	if _, err := pool.Exec(ctx, `
		INSERT INTO leafs (
			id, name, slug, description, state, task_pattern,
			execution_config, validation_config, fault_tolerance_config,
			data_config, credit_config, resource_requirements,
			is_ongoing, visibility, creator_id
		) VALUES (
			$1, $2, $3, 'BG-17 deadlock regression leaf', 'ACTIVE', 'PARAMETER_SWEEP',
			'{"runtime":"WASM","gpu_required":false,"gpu_type":"","max_memory_mb":512,"max_disk_mb":1024,"max_cpu_seconds":86400,"network_access":false,"min_vram_gb":0}',
			'{"redundancy_factor":1,"agreement_threshold":1.0,"comparison_mode":"EXACT","max_retries":3}',
			'{"heartbeat_interval_seconds":300,"missed_heartbeats_threshold":3,"deadline_multiplier":3.0,"max_reassignments":3,"checkpointing_enabled":false}',
			'{"transfer_strategy":"INLINE","aggregation_format":"JSON","max_input_size_bytes":1048576,"max_output_size_bytes":104857600}',
			'{"credit_per_validated_work_unit":1.0}',
			'{"min_cpu_cores":1,"min_memory_mb":128,"min_disk_mb":128,"gpu_required":false,"min_bandwidth_mbps":0,"min_gpu_vram_mb":0}',
			false, 'PUBLIC', $4
		)`,
		leafID, "BG17 Leaf "+slug, slug, userID,
	); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}

	wuID = types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO work_units (
			id, leaf_id, state, priority,
			input_data, code_artifact_ref, parameters,
			estimated_duration_seconds, deadline_seconds,
			reassignment_count, max_reassignments, flagged_for_review
		) VALUES (
			$1, $2, 'QUEUED', 'NORMAL',
			'{"x": 42}', 'ref://bg17', '{"n": 1}',
			300, 3600,
			0, 3, false
		)`,
		wuID, leafID,
	); err != nil {
		t.Fatalf("seed work unit: %v", err)
	}

	pubKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	now := time.Now().UTC()
	v := &volunteer.Volunteer{
		PublicKey: pubKey,
		HardwareCapabilities: volunteer.HardwareCapabilities{
			CPUCores: 4, MaxCPUCores: 4,
			MemoryTotalMB: 8192, MaxMemoryMB: 8192,
			DiskAvailableMB: 10240, MaxDiskMB: 10240,
		},
		AvailableRuntimes: []string{"WASM"},
		SchedulingMode:    volunteer.ScheduleAlways,
		IsActive:          true,
		LastSeenAt:        &now,
	}
	if err := volunteer.NewPgxRepository(pool).Create(ctx, v); err != nil {
		t.Fatalf("seed volunteer: %v", err)
	}
	return leafID, wuID, v, pubKey
}

// bg17OpenCopy seeds an OPEN copy (assignment-history row, outcome NULL) so a
// submit for (wuID, vol) passes the active-assignment check.
func bg17OpenCopy(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at)
		VALUES ($1, $2, $3)`, wuID, volID, time.Now().UTC()); err != nil {
		t.Fatalf("seed open copy: %v", err)
	}
}

// bg17BrowserDeps builds the browser-handler dependency set over the given pool
// with real pgx-backed repositories (the production wiring, minus the optional
// engine/transitioner, which are irrelevant to the transaction under test).
func bg17BrowserDeps(pool *pgxpool.Pool) *browserVolunteerDeps {
	return &browserVolunteerDeps{
		pool:                    pool,
		volunteerRepo:           volunteer.NewPgxRepository(pool),
		wuRepo:                  workunit.NewPgxWorkUnitRepository(pool),
		leafRepo:                leaf.NewPgxRepository(pool),
		assignRepo:              assignment.NewPgxRepository(pool),
		resultRepo:              result.NewPgxRepository(pool),
		batchRepo:               workunit.NewPgxBatchRepository(pool),
		trustRepo:               trust.NewPgxRepository(pool),
		logger:                  slog.New(slog.NewTextHandler(os.Stderr, nil)),
		maxInflightPerVolunteer: 10,
	}
}

// TestBG17_SubmitResult_NoSelfDeadlockOnSingleConnPool re-runs the BG-17 attack
// against the gRPC SubmitResult handler: on a one-connection pool, any pool
// read between pool.Begin and Commit (pre-fix: the trust-snapshot volunteer
// load, the artifact-version resolve, and the completion-leaf read) blocks on a
// connection the transaction itself is holding.
func TestBG17_SubmitResult_NoSelfDeadlockOnSingleConnPool(t *testing.T) {
	pool := newBG17SingleConnPool(t)
	_, wuID, vol, pub := bg17Seed(t, pool)
	bg17OpenCopy(t, pool, wuID, vol.ID)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	svc := NewVolunteerService(pool, "bg17-test", time.Now(),
		volunteer.NewPgxRepository(pool),
		workunit.NewPgxWorkUnitRepository(pool),
		leaf.NewPgxRepository(pool),
		assignment.NewPgxRepository(pool),
		result.NewPgxRepository(pool),
		workunit.NewPgxBatchRepository(pool),
		nil, // checkpointRepo: unused by this path
		nil, // validationEngine: transition evaluation is post-commit and out of scope
		logger, transition.TrustPolicy{})

	output := []byte(`{"answer": 42}`)
	sum := sha256.Sum256(output)
	req := &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuID.String(),
		VolunteerId:          vol.ID.String(),
		PublicKey:            pub,
		OutputData:           output,
		OutputChecksumSha256: hex.EncodeToString(sum[:]),
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 1},
	}

	ctx, cancel := context.WithTimeout(contextWithGRPCAuthPublicKey(context.Background(), pub), bg17Timeout)
	defer cancel()

	resp, err := svc.SubmitResult(ctx, req)
	if err != nil {
		t.Fatalf("BG-17 ATTACK LIVE: SubmitResult did not complete on a single-connection pool "+
			"(a pool read inside its own transaction deadlocks until the deadline): %v", err)
	}
	if !resp.Accepted {
		t.Fatalf("expected result to be accepted, got: %+v", resp)
	}
}

// TestBG17_BrowserRequestWork_NoSelfDeadlockOnSingleConnPool re-runs the attack
// against the browser/WASM request-work handler (pre-fix: the leaf read for the
// spot-check decision ran on the pool inside the open assignment transaction).
func TestBG17_BrowserRequestWork_NoSelfDeadlockOnSingleConnPool(t *testing.T) {
	pool := newBG17SingleConnPool(t)
	_, _, _, pub := bg17Seed(t, pool)

	handler := handleBrowserRequestWork(bg17BrowserDeps(pool))

	body := `{"max_memory_mb": 8192, "max_disk_mb": 10240}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/request-work", strings.NewReader(body))
	ctx, cancel := context.WithTimeout(ContextWithEd25519PubKey(req.Context(), pub), bg17Timeout)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("BG-17 ATTACK LIVE: browser request-work did not complete on a single-connection pool "+
			"(status %d): %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["work_unit_id"] == "" {
		t.Fatalf("expected an assigned work unit, got: %v", resp)
	}
}

// TestBG17_BrowserSubmitResult_NoSelfDeadlockOnSingleConnPool re-runs the attack
// against the browser/WASM submit handler (pre-fix: the trust-score read and
// the completion-leaf read both ran on the pool inside the open transaction).
func TestBG17_BrowserSubmitResult_NoSelfDeadlockOnSingleConnPool(t *testing.T) {
	pool := newBG17SingleConnPool(t)
	_, wuID, vol, pub := bg17Seed(t, pool)
	bg17OpenCopy(t, pool, wuID, vol.ID)

	handler := handleBrowserSubmitResult(bg17BrowserDeps(pool))

	output := []byte(`{"answer": 42}`)
	sum := sha256.Sum256(output)
	body := `{"work_unit_id":"` + wuID.String() + `","output_data":"` +
		base64.StdEncoding.EncodeToString(output) + `","output_checksum":"` +
		hex.EncodeToString(sum[:]) + `","metrics":{"wall_clock_seconds":1}}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	ctx, cancel := context.WithTimeout(ContextWithEd25519PubKey(req.Context(), pub), bg17Timeout)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("BG-17 ATTACK LIVE: browser submit-result did not complete on a single-connection pool "+
			"(status %d): %s", rec.Code, rec.Body.String())
	}
	var resp browserSubmitResultResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Accepted {
		t.Fatalf("expected result to be accepted, got: %+v", resp)
	}
}
