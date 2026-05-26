package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/server"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestF01_FullServerLifecycle exercises the full F01 user journey:
// start both HTTP and gRPC servers, call health/status endpoints,
// then gracefully shut down.
func TestF01_FullServerLifecycle(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	startTime := time.Now()
	version := "0.1.0-test"

	// Wire up HTTP router (no DB pool — will report degraded).
	deps := &server.Dependencies{
		Pool:      nil,
		Logger:    logger,
		Version:   version,
		StartTime: startTime,
	}
	router, rateLimitCleanup := server.NewRouter(deps)
	defer rateLimitCleanup()
	httpServer := server.NewHTTPServer("127.0.0.1:0", router, nil)

	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for HTTP: %v", err)
	}
	httpAddr := httpLis.Addr().String()

	go func() {
		_ = httpServer.Serve(httpLis)
	}()

	// Wire up gRPC server.
	grpcServer, grpcCleanup := server.NewGRPCServer(nil, logger)
	defer grpcCleanup()
	volunteerSvc := server.NewVolunteerService(nil, version, startTime, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	lettucev1.RegisterVolunteerServiceServer(grpcServer, volunteerSvc)

	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for gRPC: %v", err)
	}
	grpcAddr := grpcLis.Addr().String()

	go func() {
		_ = grpcServer.Serve(grpcLis)
	}()

	// --- Test REST health endpoint ---
	t.Run("REST_Health", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://%s/api/v1/health", httpAddr))
		if err != nil {
			t.Fatalf("HTTP request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		requestID := resp.Header.Get("X-Request-ID")
		if requestID == "" {
			t.Error("missing X-Request-ID header")
		}

		// CORSOrigins is unset for this test, so cross-origin sharing is
		// DISABLED (fail-closed): no Access-Control-Allow-Origin is emitted.
		if corsOrigin := resp.Header.Get("Access-Control-Allow-Origin"); corsOrigin != "" {
			t.Errorf("expected no CORS Allow-Origin when origins unset, got %q", corsOrigin)
		}

		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if body["status"] != "degraded" {
			t.Errorf("expected status 'degraded' (no DB), got %q", body["status"])
		}
		// Public health endpoint must not expose database or uptime fields.
		if _, ok := body["database"]; ok {
			t.Error("public health response must not expose 'database' field")
		}
		if _, ok := body["uptime_seconds"]; ok {
			t.Error("public health response must not expose 'uptime_seconds' field")
		}
	})

	// --- Test gRPC GetServerStatus ---
	t.Run("gRPC_GetServerStatus", func(t *testing.T) {
		conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("failed to connect: %v", err)
		}
		defer conn.Close()

		client := lettucev1.NewVolunteerServiceClient(conn)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resp, err := client.GetServerStatus(ctx, &lettucev1.GetServerStatusRequest{})
		if err != nil {
			t.Fatalf("GetServerStatus failed: %v", err)
		}

		if resp.Status != "degraded" {
			t.Errorf("expected status 'degraded', got %q", resp.Status)
		}
		if resp.DatabaseStatus != "disconnected" {
			t.Errorf("expected database_status 'disconnected', got %q", resp.DatabaseStatus)
		}
		if resp.UptimeSeconds < 0 {
			t.Errorf("expected non-negative uptime, got %d", resp.UptimeSeconds)
		}
	})

	// --- Test REST/gRPC consistency ---
	t.Run("REST_gRPC_Consistency", func(t *testing.T) {
		// Call both endpoints and verify they return consistent data.
		httpResp, err := http.Get(fmt.Sprintf("http://%s/api/v1/health", httpAddr))
		if err != nil {
			t.Fatalf("HTTP request failed: %v", err)
		}
		defer httpResp.Body.Close()

		var httpBody map[string]any
		if err := json.NewDecoder(httpResp.Body).Decode(&httpBody); err != nil {
			t.Fatalf("failed to decode HTTP response: %v", err)
		}

		conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("failed to connect: %v", err)
		}
		defer conn.Close()

		client := lettucev1.NewVolunteerServiceClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		grpcResp, err := client.GetServerStatus(ctx, &lettucev1.GetServerStatusRequest{})
		if err != nil {
			t.Fatalf("GetServerStatus failed: %v", err)
		}

		// Both should report the same status.
		if httpBody["status"] != grpcResp.Status {
			t.Errorf("status mismatch: REST=%q gRPC=%q", httpBody["status"], grpcResp.Status)
		}
	})

	// --- Test 404 for unknown paths ---
	t.Run("REST_404", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://%s/nonexistent", httpAddr))
		if err != nil {
			t.Fatalf("HTTP request failed: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404, got %d", resp.StatusCode)
		}
	})

	// --- Graceful shutdown ---
	t.Run("GracefulShutdown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			server.GracefulShutdown(ctx, httpServer, grpcServer, nil, 5*time.Second)
			close(done)
		}()

		cancel()

		select {
		case <-done:
			// Success.
		case <-time.After(10 * time.Second):
			t.Fatal("graceful shutdown did not complete within timeout")
		}

		// Verify HTTP server is no longer accepting connections.
		_, err := http.Get(fmt.Sprintf("http://%s/api/v1/health", httpAddr))
		if err == nil {
			t.Error("expected HTTP connection to fail after shutdown")
		}
	})
}
