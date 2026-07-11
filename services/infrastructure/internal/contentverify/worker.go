package contentverify

// worker.go is the verification worker (design doc §10.6): a leader-gated singleton on
// the reclaim-worker template, started UNCONDITIONALLY in main.go's leadership closure
// (the ReclaimWorker janitor rationale: held stragglers after a knob flip on→off must
// drain — via the expiry lane — without a config change). The KNOB gates fetching, not
// the worker: with the knob off no network I/O ever happens; only the expiry lane runs.
//
// Per tick, due rows are claimed with FOR UPDATE SKIP LOCKED (a two-leader failover
// window cannot double-process a row) and fetched CONCURRENTLY — up to batchSize
// goroutines, each with its own per-fetch deadline — so one slow allowlisted origin
// cannot head-of-line-block honest rows behind it. Every write is a guarded UPDATE
// keyed on validation_status = 'AWAITING_CONTENT_VERIFICATION': 0 rows affected means
// another actor resolved the row, and a crashed or failed-over worker re-runs a row
// idempotently on the next tick.

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Operational constants, fixed in v1 (no speculative knobs — house rule).
const (
	// workerInterval is the sweep tick; it must exceed the 15s leadership
	// failover poll.
	workerInterval = 30 * time.Second
	// batchSize bounds one tick's claim (and its concurrent fetch goroutines).
	batchSize = 10
	// maxAttempts is the TRANSIENT-failure budget; a successful fetch always
	// promotes and never consumes it (§10.6).
	maxAttempts = 3
	// retryDelay is the backoff after a transient fetch failure.
	retryDelay = 5 * time.Minute
	// holdingLifetime is how long a row may sit held before the expiry lane
	// terminates it (HOLDING_EXPIRED, or FETCH_DISABLED when the knob is off).
	holdingLifetime = 24 * time.Hour
)

// EvaluateFunc re-evaluates a work unit after a promotion — a closure over the real
// transitioner wired in main.go, so this package never imports the transition
// machinery. The transitioner owns the COMPLETED mark and the quorum decision
// (the browser-submit precedent). Evaluate failure is WARN-and-continue: the unit is
// re-adjudicated on its next natural event, the same best-effort posture as submit.
type EvaluateFunc func(ctx context.Context, workUnitID types.ID) error

// Worker is the leader-gated content-verification sweep.
type Worker struct {
	pool           *pgxpool.Pool
	client         *http.Client
	fetchEnabled   bool
	globalMaxBytes int64
	evaluate       EvaluateFunc
	logger         *slog.Logger
	interval       time.Duration
}

// NewWorker builds the worker. client is the guarded fetch client (NewHTTPClient in
// production; tests inject an httptest-TLS client). fetchEnabled and globalMaxBytes
// are the two §10.9 knobs, resolved by the caller (cfg.Head.ContentFetchEnabled,
// cfg.Head.EffectiveContentFetchMaxBytes()).
func NewWorker(pool *pgxpool.Pool, client *http.Client, fetchEnabled bool, globalMaxBytes int64, evaluate EvaluateFunc, logger *slog.Logger) *Worker {
	return &Worker{
		pool:           pool,
		client:         client,
		fetchEnabled:   fetchEnabled,
		globalMaxBytes: globalMaxBytes,
		evaluate:       evaluate,
		logger:         logger,
		interval:       workerInterval,
	}
}

// Start runs one sweep immediately on election, then on the interval ticker until ctx
// is cancelled (leadership lost or head shutdown). Reclaim-worker template.
func (w *Worker) Start(ctx context.Context) {
	w.logger.Info("content verification worker started",
		"interval", w.interval.String(), "fetch_enabled", w.fetchEnabled)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("content verification worker stopping")
			return
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}
