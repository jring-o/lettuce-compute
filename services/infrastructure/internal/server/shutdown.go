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

// GracefulShutdown listens for OS signals (SIGTERM, SIGINT) and coordinates
// graceful shutdown of both HTTP and gRPC servers.
func GracefulShutdown(ctx context.Context, httpServer *http.Server, grpcServer *grpc.Server, pool *pgxpool.Pool, shutdownTimeout time.Duration) {
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

	// Close database pool.
	if pool != nil {
		pool.Close()
	}

	slog.Info("shutdown complete")
}
