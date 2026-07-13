package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// poolAcquireTimeout bounds how long a request handler waits to ACQUIRE a pool
// connection when opening its transaction (BG-17 backstop). A healthy acquire
// completes in microseconds; a wait that reaches seconds means the pool is
// saturated — and in the worst case (a regression re-introducing a pool read
// inside an open transaction) every handler is waiting on a connection that
// only another waiting handler could free. Failing the acquire after a bound
// converts that permanent, connection-pinning hang into a retryable error.
// 5s sits far above healthy acquires and above dispatchDBTimeout (2s), so the
// shed-gated paths always fail on their own budget first.
const poolAcquireTimeout = 5 * time.Second

// beginTxBounded opens a transaction whose pool-acquire (and BEGIN round-trip)
// is bounded by poolAcquireTimeout. Only the acquire is bounded: the returned
// transaction's statements run on whatever context each call site passes, so a
// legitimately slow transaction is never cut short by this backstop.
func beginTxBounded(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, poolAcquireTimeout)
	defer cancel()
	return pool.Begin(acquireCtx)
}
