package server

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
)

// GracefulShutdown listens for OS signals (SIGTERM, SIGINT) and drains both the
// HTTP and gRPC servers. It deliberately does NOT stop background jobs or close
// the database pool — the caller owns that tail (see StopBackgroundAndClosePool),
// because the background jobs must be cancelled and joined BEFORE the pool
// closes (BG-32/BG-32b).
func GracefulShutdown(ctx context.Context, httpServer *http.Server, grpcServer *grpc.Server, shutdownTimeout time.Duration) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		slog.Info("received shutdown signal", "signal", sig.String())
	case <-ctx.Done():
		slog.Info("context canceled, shutting down")
	}

	slog.Info("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Stop accepting new gRPC connections.
	done := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
	case <-shutdownCtx.Done():
		slog.Warn("gRPC graceful stop timed out, forcing stop")
		grpcServer.Stop()
	}

	// Stop accepting new HTTP connections.
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}

	slog.Info("servers drained")
}

// StopBackgroundAndClosePool finishes shutdown after the servers have drained:
// it cancels the background-job context, waits — bounded by joinTimeout — for
// each named job's done channel, and only then closes the pool.
//
// The order is load-bearing (BG-32/BG-32b). pgxpool.Pool.Close blocks until
// every acquired connection is returned, and the leadership manager holds a
// dedicated advisory-lock connection that is released only on cancellation —
// so closing the pool before cancelling deadlocks a leader replica until the
// container runtime SIGKILLs it at stop_grace_period (BG-32b). Cancelling
// first also lets the dispatch cache run its final reservation flush against
// a live pool instead of a closed one (BG-32).
//
// The join is bounded, never load-bearing for correctness: a job that misses
// the window is logged and abandoned, and the crash-consistency design covers
// its lost final write (unflushed reservations expire and re-dispatch).
func StopBackgroundAndClosePool(cancelJobs context.CancelFunc, jobs map[string]<-chan struct{}, joinTimeout time.Duration, pool *pgxpool.Pool) {
	cancelJobs()

	joinCtx, cancel := context.WithTimeout(context.Background(), joinTimeout)
	defer cancel()
	for name, done := range jobs {
		select {
		case <-done:
		case <-joinCtx.Done():
			slog.Warn("background job missed the shutdown join window; closing pool anyway",
				"job", name, "join_timeout", joinTimeout)
		}
	}

	if pool != nil {
		pool.Close()
	}

	slog.Info("shutdown complete")
}
