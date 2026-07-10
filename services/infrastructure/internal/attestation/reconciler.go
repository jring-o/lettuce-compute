package attestation

import (
	"context"
	"log/slog"
	"time"
)

// RevocationReconciler is a leader-gated singleton that periodically recovers revocation
// attestations whose in-handler emission was lost (see RevocationEmitter.Reconcile). It
// matches the DID-recheck / audit-reclaim worker shape: one sweep immediately on election,
// then on a fixed ticker until the context is cancelled.
type RevocationReconciler struct {
	emitter  *RevocationEmitter
	interval time.Duration
	logger   *slog.Logger
}

// NewRevocationReconciler builds the reconciler. The caller supplies the sweep interval (the
// head wires the operational default).
func NewRevocationReconciler(emitter *RevocationEmitter, interval time.Duration, logger *slog.Logger) *RevocationReconciler {
	return &RevocationReconciler{emitter: emitter, interval: interval, logger: logger}
}

// Run performs one reconciliation sweep immediately, then on the interval ticker until ctx is
// cancelled (leadership lost or head shutdown).
func (w *RevocationReconciler) Run(ctx context.Context) {
	w.logger.Info("revocation reconciler started", "interval", w.interval.String())
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("revocation reconciler stopping")
			return
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// sweep runs one Reconcile pass. A failure is logged and never aborts the loop; the next tick
// retries.
func (w *RevocationReconciler) sweep(ctx context.Context) {
	emitted, err := w.emitter.Reconcile(ctx)
	if err != nil {
		w.logger.Error("revocation reconcile sweep failed", "error", err)
		return
	}
	if emitted > 0 {
		w.logger.Info("revocation reconcile: emitted missing revocation attestations", "emitted", emitted)
	}
}
