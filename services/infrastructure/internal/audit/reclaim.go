package audit

// reclaim.go implements the audit-lifecycle reclaim worker.
//
// An audit job is leased to a runner at claim time (lease = max(unit deadline, LeaseFloor)).
// A runner can vanish mid-job — crash, disconnect, or simply never submit — leaving the row
// CLAIMED with a lease that will lapse. A separate failure mode is a job no eligible runner
// ever claims (e.g. a required hardware class no registered runner presents), which would sit
// QUEUED forever. This worker closes both gaps on a leader-only ticker: it requeues (or, once
// the attempt budget is spent, EXPIRES) rows whose lease has lapsed, and EXPIRES rows that
// have waited past QueuedLifetime. It matches the DID-recheck / artifact-GC worker shape.

import (
	"context"
	"log/slog"
	"time"
)

// reclaimInterval is how often the sweep runs. Constant in v1 (the lease itself self-scales
// with each unit's deadline); promoted to a knob only when a real deployment needs it.
const reclaimInterval = 60 * time.Second

// reclaimStore is the narrow slice of AuditsRepository the worker drives.
type reclaimStore interface {
	SweepLapsedLeases(ctx context.Context) (requeued int, expired int, err error)
	SweepStaleQueued(ctx context.Context) (expired int, err error)
}

// ReclaimWorker is a leader-gated singleton that reclaims lapsed audit leases and expires
// stale queued jobs on a fixed ticker.
type ReclaimWorker struct {
	repo     reclaimStore
	logger   *slog.Logger
	interval time.Duration
}

// NewReclaimWorker builds the reclaim worker.
func NewReclaimWorker(repo reclaimStore, logger *slog.Logger) *ReclaimWorker {
	return &ReclaimWorker{
		repo:     repo,
		logger:   logger,
		interval: reclaimInterval,
	}
}

// Start runs one sweep immediately on election, then on the interval ticker until ctx is
// cancelled (leadership lost or head shutdown). It matches the DID-recheck worker pattern.
func (w *ReclaimWorker) Start(ctx context.Context) {
	w.logger.Info("audit reclaim worker started", "interval", w.interval.String())
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("audit reclaim worker stopping")
			return
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// sweep runs both reclaim actions. A failure in either is logged and never aborts the loop:
// the next tick retries, and a transient DB blip must not silently kill the audit net.
func (w *ReclaimWorker) sweep(ctx context.Context) {
	requeued, expired, err := w.repo.SweepLapsedLeases(ctx)
	if err != nil {
		w.logger.Error("audit reclaim: sweep lapsed leases failed", "error", err)
	} else if requeued > 0 || expired > 0 {
		w.logger.Info("audit reclaim: lapsed leases swept", "requeued", requeued, "expired", expired)
	}

	staleExpired, err := w.repo.SweepStaleQueued(ctx)
	if err != nil {
		w.logger.Error("audit reclaim: sweep stale queued failed", "error", err)
	} else if staleExpired > 0 {
		w.logger.Info("audit reclaim: stale queued jobs expired", "expired", staleExpired)
	}
}
