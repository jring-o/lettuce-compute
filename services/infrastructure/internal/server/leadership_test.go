//go:build integration

package server_test

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/server"
)

// leadershipTestPool connects to the integration test database with a small pool
// that still has room for two dedicated leadership connections (skipping when the
// DB URL is unset).
func leadershipTestPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	// Two managers each hold one dedicated connection for the advisory lock.
	cfg.MaxConns = 5
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	return pool, pool.Close
}

// newFastLeadershipManager builds a manager and shrinks its poll interval so the
// failover path completes inside the test timeout. The poll interval is
// unexported, so we use the exported tunable seam via a short-lived helper.
func newFastLeadershipManager(t *testing.T, pool *pgxpool.Pool) *server.LeadershipManager {
	t.Helper()
	m := server.NewLeadershipManager(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	server.SetLeadershipPollIntervalForTest(m, 200*time.Millisecond)
	return m
}

// TestLeadershipSingleLeaderAndFailover verifies the core leadership contract:
//   - the first replica to call Run acquires leadership and runs its leader jobs,
//   - a second replica running concurrently does NOT run its leader jobs while the
//     first holds the lock,
//   - when the leader relinquishes (ctx cancelled), the follower takes over and
//     runs its leader jobs.
func TestLeadershipSingleLeaderAndFailover(t *testing.T) {
	pool, cleanup := leadershipTestPool(t)
	defer cleanup()

	var aRuns, bRuns atomic.Int32

	mgrA := newFastLeadershipManager(t, pool)
	mgrB := newFastLeadershipManager(t, pool)

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		mgrA.Run(ctxA, "replica-a", func(leaderCtx context.Context) {
			aRuns.Add(1)
		})
	}()

	// Let A win leadership.
	waitUntil(t, 3*time.Second, func() bool { return aRuns.Load() == 1 })

	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		mgrB.Run(ctxB, "replica-b", func(leaderCtx context.Context) {
			bRuns.Add(1)
		})
	}()

	// B must NOT run leader jobs while A holds the lock.
	time.Sleep(1 * time.Second)
	if bRuns.Load() != 0 {
		t.Fatalf("follower ran leader jobs while leader held the lock: bRuns=%d", bRuns.Load())
	}
	if aRuns.Load() != 1 {
		t.Fatalf("leader ran its jobs more than once: aRuns=%d", aRuns.Load())
	}

	// A relinquishes leadership; B should take over.
	cancelA()
	<-doneA
	waitUntil(t, 5*time.Second, func() bool { return bRuns.Load() == 1 })

	cancelB()
	<-doneB
}

// waitUntil polls cond until it returns true or the deadline elapses.
func waitUntil(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}
