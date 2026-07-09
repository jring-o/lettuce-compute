//go:build integration

package server_test

// Wire-level integration tests for BG-25 server-issued host identity. They drive the full
// gRPC RegisterVolunteer / RequestWorkUnit path against a live Postgres and assert the
// issuance three-way (mint-on-empty, echo-known, empty-on-unknown), the per-account cap
// refusal, and the work-path host validation (valid id meters + bumps last-seen; unknown id
// refused with the pinned prefix; empty id works as the account fallback).
//
// The in-process dispatch cache MUST run for these: the work-path host-owner oracle lives in
// the cache, and without it RequestWorkUnit folds instead of validating/refusing. The harness
// therefore calls StartDispatchCache with a cancelable context and also applies a host-cap
// policy. Like the rest of the suite these skip unless LETTUCE_TEST_DB_URL is set and must run
// with -p 1 (shared database, DELETE-clean between runs).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/credit"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type hostKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newHostKeyPair(t *testing.T) hostKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	return hostKey{pub: pub, priv: priv}
}

// signHost wraps ctx so the client interceptor signs the outgoing RPC with k.
func signHost(ctx context.Context, k hostKey) context.Context {
	return server.ContextWithTestSigner(ctx, k.pub, k.priv)
}

// setupHostIssuanceServer stands up the full gRPC volunteer service against the test DB with a
// per-account host cap of capPerAccount and the dispatch cache running (so the work-path
// oracle validates host ids). It returns the pool, a signing client, and a cleanup func.
func setupHostIssuanceServer(t *testing.T, capPerAccount int) (*pgxpool.Pool, lettucev1.VolunteerServiceClient, func()) {
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

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	startTime := time.Now()

	grpcServer, grpcCleanup := server.NewGRPCServer(nil, logger, nil)
	defer grpcCleanup()
	volunteerRepo := volunteer.NewPgxRepository(pool)
	wuRepo := workunit.NewPgxWorkUnitRepository(pool)
	leafRepo := leaf.NewPgxRepository(pool)
	assignRepo := assignment.NewPgxRepository(pool)
	resultRepo := result.NewPgxRepository(pool)
	batchRepo := workunit.NewPgxBatchRepository(pool)
	creditRepo := credit.NewPgxRepository(pool)
	validationEngine := validation.NewEngine(resultRepo, wuRepo, leafRepo, creditRepo, nil, volunteerRepo, assignRepo, nil, nil, nil, logger, nil, transition.TrustPolicy{})
	svc := server.NewVolunteerService(pool, "0.9.0-test", startTime, volunteerRepo, wuRepo, leafRepo, assignRepo, resultRepo, batchRepo, nil, validationEngine, logger, transition.TrustPolicy{})
	server.SetHostCapPolicy(svc, server.HostCapPolicy{PerAccount: capPerAccount, ActiveWindow: 30 * 24 * time.Hour})

	// The work-path host-owner oracle lives in the dispatch cache; start it (cancelable) so
	// RequestWorkUnit validates/refuses host ids instead of folding.
	cacheCtx, cacheCancel := context.WithCancel(context.Background())
	server.StartDispatchCache(svc, cacheCtx)

	lettucev1.RegisterVolunteerServiceServer(grpcServer, svc)

	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for gRPC: %v", err)
	}
	go func() { _ = grpcServer.Serve(grpcLis) }()

	conn, err := grpc.NewClient(grpcLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(server.TestSigningInterceptor()),
	)
	if err != nil {
		t.Fatalf("failed to connect gRPC: %v", err)
	}
	client := lettucev1.NewVolunteerServiceClient(conn)

	cleanup := func() {
		cacheCancel()
		conn.Close()
		grpcServer.Stop()
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM hosts")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return pool, client, cleanup
}

// registerHost registers (or re-registers) k, echoing hostID (empty = mint request), and
// returns the response.
func registerHost(t *testing.T, ctx context.Context, client lettucev1.VolunteerServiceClient, k hostKey, hostID string) *lettucev1.RegisterVolunteerResponse {
	t.Helper()
	resp, err := client.RegisterVolunteer(signHost(ctx, k), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         k.pub,
		AvailableRuntimes: []string{"NATIVE"},
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores: 4, MaxCpuCores: 4,
			MemoryTotalMb: 8192, MaxMemoryMb: 8192,
		},
		HostId: hostID,
	})
	if err != nil {
		t.Fatalf("RegisterVolunteer(hostID=%q): %v", hostID, err)
	}
	return resp
}

func hostRowExists(t *testing.T, pool *pgxpool.Pool, id types.ID) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM hosts WHERE id = $1)", id).Scan(&exists); err != nil {
		t.Fatalf("host exists: %v", err)
	}
	return exists
}

func hostCountForVolunteer(t *testing.T, pool *pgxpool.Pool, volunteerID types.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM hosts WHERE volunteer_id = $1", volunteerID).Scan(&n); err != nil {
		t.Fatalf("host count for volunteer: %v", err)
	}
	return n
}

func hostLastSeen(t *testing.T, pool *pgxpool.Pool, id types.ID) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(context.Background(),
		"SELECT last_seen_at FROM hosts WHERE id = $1", id).Scan(&ts); err != nil {
		t.Fatalf("host last_seen: %v", err)
	}
	return ts
}

// TestHostIssuance_RegisterEmptyMintsAndPersists: registering with an empty host id mints a
// server-generated UUID, returns it, and persists a hosts row.
func TestHostIssuance_RegisterEmptyMintsAndPersists(t *testing.T) {
	pool, client, cleanup := setupHostIssuanceServer(t, 10)
	defer cleanup()
	ctx := context.Background()

	key := newHostKeyPair(t)
	resp := registerHost(t, ctx, client, key, "")
	if resp.HostId == "" {
		t.Fatal("register with an empty host id should mint and return a host id")
	}
	id, err := types.ParseID(resp.HostId)
	if err != nil {
		t.Fatalf("issued host id %q is not a valid uuid: %v", resp.HostId, err)
	}
	if !hostRowExists(t, pool, id) {
		t.Error("the issued host id should have a persisted hosts row")
	}
}

// TestHostIssuance_ReRegisterEchoKeepsSameID: re-registering while echoing the issued id
// returns the SAME id and lands on the same row (no second row).
func TestHostIssuance_ReRegisterEchoKeepsSameID(t *testing.T) {
	pool, client, cleanup := setupHostIssuanceServer(t, 10)
	defer cleanup()
	ctx := context.Background()

	key := newHostKeyPair(t)
	first := registerHost(t, ctx, client, key, "")
	if first.HostId == "" {
		t.Fatal("first register should mint a host id")
	}
	second := registerHost(t, ctx, client, key, first.HostId)
	if second.HostId != first.HostId {
		t.Errorf("echo re-register = %q, want the same id %q", second.HostId, first.HostId)
	}
	volID, _ := types.ParseID(first.VolunteerId)
	if n := hostCountForVolunteer(t, pool, volID); n != 1 {
		t.Errorf("echo should refresh the SAME row, host count = %d, want 1", n)
	}
}

// TestHostIssuance_RegisterUnknownIDReturnsEmpty: echoing a foreign/unknown id yields an empty
// response host id and creates NOTHING (the client re-registers empty to mint).
func TestHostIssuance_RegisterUnknownIDReturnsEmpty(t *testing.T) {
	pool, client, cleanup := setupHostIssuanceServer(t, 10)
	defer cleanup()
	ctx := context.Background()

	key := newHostKeyPair(t)
	seed := registerHost(t, ctx, client, key, "") // mint so the account exists
	if seed.HostId == "" {
		t.Fatal("seed mint failed")
	}
	volID, _ := types.ParseID(seed.VolunteerId)
	before := hostCountForVolunteer(t, pool, volID)

	resp := registerHost(t, ctx, client, key, types.NewID().String()) // unknown id
	if resp.HostId != "" {
		t.Errorf("unknown-id echo = %q, want empty", resp.HostId)
	}
	if after := hostCountForVolunteer(t, pool, volID); after != before {
		t.Errorf("unknown-id echo must not create a row: count %d -> %d", before, after)
	}
}

// TestHostIssuance_MintRefusedAtCapReturnsEmpty: at cap 1 with the first host freshly active,
// a second mint request is refused (empty response id) and the account stays at one row.
func TestHostIssuance_MintRefusedAtCapReturnsEmpty(t *testing.T) {
	pool, client, cleanup := setupHostIssuanceServer(t, 1)
	defer cleanup()
	ctx := context.Background()

	key := newHostKeyPair(t)
	first := registerHost(t, ctx, client, key, "")
	if first.HostId == "" {
		t.Fatal("first mint (within cap 1) should succeed")
	}
	second := registerHost(t, ctx, client, key, "")
	if second.HostId != "" {
		t.Errorf("mint refused at cap should return an empty host id, got %q", second.HostId)
	}
	volID, _ := types.ParseID(first.VolunteerId)
	if n := hostCountForVolunteer(t, pool, volID); n != 1 {
		t.Errorf("cap 1 should hold at one host row, got %d", n)
	}
}

// TestHostIssuance_WorkPathValidIDMetersAndBumps: a work request carrying the issued id
// succeeds and bumps the host's last_seen_at (the eviction clock that keeps a working machine
// out of the stale-eviction window).
func TestHostIssuance_WorkPathValidIDMetersAndBumps(t *testing.T) {
	pool, client, cleanup := setupHostIssuanceServer(t, 10)
	defer cleanup()
	ctx := context.Background()

	key := newHostKeyPair(t)
	reg := registerHost(t, ctx, client, key, "")
	if reg.HostId == "" {
		t.Fatal("mint failed")
	}
	id, _ := types.ParseID(reg.HostId)
	before := hostLastSeen(t, pool, id)

	_, err := client.RequestWorkUnit(signHost(ctx, key), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    reg.VolunteerId,
		PublicKey:      key.pub,
		HostId:         reg.HostId,
		MaxAssignments: 1,
	})
	if err != nil {
		t.Fatalf("work request with a valid issued host id should succeed: %v", err)
	}
	if after := hostLastSeen(t, pool, id); !after.After(before) {
		t.Errorf("a valid host work request should bump last_seen (%v -> %v)", before, after)
	}
}

// TestHostIssuance_WorkPathUnknownIDRefused: a work request carrying an id the head never
// issued is refused with FailedPrecondition whose message starts with the pinned prefix.
func TestHostIssuance_WorkPathUnknownIDRefused(t *testing.T) {
	_, client, cleanup := setupHostIssuanceServer(t, 10)
	defer cleanup()
	ctx := context.Background()

	key := newHostKeyPair(t)
	reg := registerHost(t, ctx, client, key, "")
	if reg.VolunteerId == "" {
		t.Fatal("register failed")
	}
	_, err := client.RequestWorkUnit(signHost(ctx, key), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    reg.VolunteerId,
		PublicKey:      key.pub,
		HostId:         types.NewID().String(),
		MaxAssignments: 1,
	})
	if err == nil {
		t.Fatal("an unknown host id on the work path should be refused")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		t.Fatalf("unknown host refusal = %v, want FailedPrecondition", err)
	}
	if !strings.HasPrefix(st.Message(), server.HostUnknownMessagePrefix) {
		t.Errorf("refusal message %q must start with %q", st.Message(), server.HostUnknownMessagePrefix)
	}
}

// TestHostIssuance_WorkPathEmptyIDAccountFallback: an empty host id on the work path is always
// accepted (the per-account fallback bucket).
func TestHostIssuance_WorkPathEmptyIDAccountFallback(t *testing.T) {
	_, client, cleanup := setupHostIssuanceServer(t, 10)
	defer cleanup()
	ctx := context.Background()

	key := newHostKeyPair(t)
	reg := registerHost(t, ctx, client, key, "")
	if reg.VolunteerId == "" {
		t.Fatal("register failed")
	}
	_, err := client.RequestWorkUnit(signHost(ctx, key), &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    reg.VolunteerId,
		PublicKey:      key.pub,
		HostId:         "",
		MaxAssignments: 1,
	})
	if err != nil {
		t.Fatalf("an empty host id (account fallback) should be accepted: %v", err)
	}
}
