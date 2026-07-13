package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/database"
)

// checkDBHealth returns the server status and database status strings
// based on pool connectivity. Shared by HTTP and gRPC health checks.
func checkDBHealth(ctx context.Context, pool *pgxpool.Pool) (status, dbStatus string) {
	if pool == nil {
		return "degraded", "disconnected"
	}
	if err := database.HealthCheck(ctx, pool); err != nil {
		return "degraded", "disconnected"
	}
	return "healthy", "connected"
}

// healthStatusCode maps the computed health status to the HTTP status code.
// Docker healthchecks, load balancers, and uptime monitors read the CODE, not
// the JSON body, so a degraded head must answer non-2xx (503) or every machine
// consumer keeps routing traffic to a head whose database is unreachable. The
// descriptive JSON body is kept either way.
func healthStatusCode(status string) int {
	if status == "healthy" {
		return http.StatusOK
	}
	return http.StatusServiceUnavailable
}

type healthResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

type healthDetailedResponse struct {
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Database      string `json:"database"`
}

// HealthHandler handles GET /api/v1/health (public, no auth).
// Returns {"status":...,"database":...}. This is the operator-facing liveness
// contract documented in guides/head-setup.md. Exposing "database" discloses
// nothing beyond "status", which already encodes pool connectivity ("degraded"
// is returned iff the database is unreachable). Internal detail that is NOT
// derivable from "status" — e.g. uptime — stays behind auth on /health/detailed.
func HealthHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, dbStatus := checkDBHealth(r.Context(), pool)

		resp := healthResponse{Status: status, Database: dbStatus}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(healthStatusCode(status))
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// HealthDetailedHandler handles GET /api/v1/health/detailed (auth required).
// Returns status, uptime, and database connectivity.
func HealthDetailedHandler(pool *pgxpool.Pool, startTime time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, dbStatus := checkDBHealth(r.Context(), pool)

		resp := healthDetailedResponse{
			Status:        status,
			UptimeSeconds: int64(time.Since(startTime).Seconds()),
			Database:      dbStatus,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(healthStatusCode(status))
		_ = json.NewEncoder(w).Encode(resp)
	}
}
