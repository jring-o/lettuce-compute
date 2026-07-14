package server

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/validation"
)

// These tests pin the ★BG-21e boot assertion in newTransitioner: a pool-backed service (the
// production shape) must refuse a validation engine that has no FinalizationTxRunner, because
// such an engine finalizes through the non-transactional passthrough — the exact condition
// that once shipped unnoticed while every atomicity test hand-wired the runner. No database
// is needed: pgxpool.New only parses the config (connections are lazy), so a non-nil pool
// stands in for "production-shaped" without connecting anywhere.

func guardTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://guard:guard@127.0.0.1:1/guard")
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func guardTestEngine() *validation.Engine {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return validation.NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, logger, nil, transition.TrustPolicy{})
}

// TestNewTransitioner_PanicsOnPoolBackedEngineWithoutTxRunner: pool + engine + no runner is
// the ★BG-21e production bug; construction must fail loudly instead of running open.
func TestNewTransitioner_PanicsOnPoolBackedEngineWithoutTxRunner(t *testing.T) {
	pool := guardTestPool(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	defer func() {
		if recover() == nil {
			t.Fatal("newTransitioner(pool, engine-without-runner) did not panic: a pool-backed head " +
				"would finalize non-atomically through the passthrough (★BG-21e)")
		}
	}()
	newTransitioner(pool, nil, nil, nil, guardTestEngine(), transition.TrustPolicy{}, logger)
}

// TestNewTransitioner_AcceptsProductionWiring: the exact main.go shape — pool-backed engine
// with the production runner — constructs cleanly.
func TestNewTransitioner_AcceptsProductionWiring(t *testing.T) {
	pool := guardTestPool(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	engine := guardTestEngine().WithTxRunner(validation.NewPgxFinalizationTxRunner(pool))
	if tr := newTransitioner(pool, nil, nil, nil, engine, transition.TrustPolicy{}, logger); tr == nil {
		t.Fatal("newTransitioner returned nil for a fully wired production-shaped engine")
	}
}

// TestNewTransitioner_MockRegimesUnaffected: the two legitimate non-production shapes — no
// engine at all (plumbing tests), and an engine with no pool (mock passthrough tests) — keep
// working without a runner.
func TestNewTransitioner_MockRegimesUnaffected(t *testing.T) {
	pool := guardTestPool(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if tr := newTransitioner(pool, nil, nil, nil, nil, transition.TrustPolicy{}, logger); tr != nil {
		t.Fatal("nil engine must yield a nil transitioner (plumbing-test regime)")
	}
	if tr := newTransitioner(nil, nil, nil, nil, guardTestEngine(), transition.TrustPolicy{}, logger); tr == nil {
		t.Fatal("nil pool + runnerless engine must still construct (mock passthrough regime)")
	}
}
