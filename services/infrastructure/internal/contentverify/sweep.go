package contentverify

import (
	"context"
)

// sweep runs one tick of the §10.6 per-row pipeline: claim due rows (FOR UPDATE SKIP
// LOCKED), run the expiry lane, re-check the leaf contract against the CURRENT config,
// fetch concurrently with per-row deadlines, and dispose — promote to PENDING on the
// served hash, retry a transient failure, or terminate CONTENT_VERIFICATION_FAILED.
func (w *Worker) sweep(ctx context.Context) {
	// Keel skeleton: the pipeline lands with the worker-implementation change set of
	// this slice. A tick with no due rows is a no-op either way.
}
