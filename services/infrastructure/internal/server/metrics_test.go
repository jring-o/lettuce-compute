package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Regression tests for BG-29 (no metrics endpoint / no instrumentation).
// Pre-fix, GET /metrics does not exist (404 through the router) and none of the
// lettuce_* metric families are exported; these tests pin the fixed surface:
// the admin-gated scrape route, the request counters fed by the EXISTING gRPC
// loggingInterceptor and HTTP requestLoggingMiddleware, and the dispatch-cache
// and DB-pool gauge families.

// newMetricsTestRouter builds the production router with the admin API key set
// and a stub DB-key repo, so Bearer <adminKey> authenticates as ADMIN and any
// other key path falls through to the mock.
func newMetricsTestRouter(t *testing.T) (http.Handler, string) {
	t.Helper()
	deps := &Dependencies{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:     "test",
		StartTime:   time.Now(),
		ApiKeyRepo:  &mockApiKeyRepo{},
		AdminAPIKey: "metrics-test-admin-key",
	}
	router, cleanup := NewRouter(deps)
	t.Cleanup(cleanup)
	return router, "metrics-test-admin-key"
}

// TestMetricsEndpoint_RefusesUnauthenticated_BG29 pins the gate: the scrape
// and pprof routes exist but refuse anonymous callers (401) — runtime
// internals must not be readable without the admin credential even though the
// shipped Caddy topology never proxies these paths.
func TestMetricsEndpoint_RefusesUnauthenticated_BG29(t *testing.T) {
	router, _ := newMetricsTestRouter(t)

	// /debug/pprof without the trailing slash is included: an exact-match route
	// exists precisely so the mux's automatic 301 → /debug/pprof/ never answers
	// an anonymous probe ahead of the admin gate.
	for _, path := range []string{"/metrics", "/debug/pprof/", "/debug/pprof"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated: want 401, got %d", path, rec.Code)
		}
	}
}

// TestMetricsEndpoint_AdminScrape_BG29 is the plan's smallest proving test:
// fire one gRPC unary call (through the real loggingInterceptor) and one HTTP
// request (through the real router middleware), scrape /metrics as admin, and
// assert the request counters and the dispatch-cache / DB-pool gauge families
// are exported.
func TestMetricsEndpoint_AdminScrape_BG29(t *testing.T) {
	router, adminKey := newMetricsTestRouter(t)

	// One unary gRPC call through the production interceptor. The method name
	// is unique to this test so the exact-count assertion below cannot collide
	// with other tests feeding the same process-wide counter.
	const method = "/lettuce.test.MetricsBG29/Ping"
	ic := loggingInterceptor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := ic(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: method},
		func(ctx context.Context, req any) (any, error) { return "pong", nil },
	); err != nil {
		t.Fatalf("interceptor call failed: %v", err)
	}

	// One HTTP request through the full middleware chain.
	healthReq := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	router.ServeHTTP(httptest.NewRecorder(), healthReq)

	// Scrape as admin.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics as admin: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The gRPC request counter recorded the unary call. Presence of the labeled
	// sample IS the proof: a CounterVec child only materializes on first use,
	// so this line exists only if the interceptor incremented it. (No exact
	// count — under -count=N the process-wide counter accumulates across runs.)
	// Exposition label order is alphabetical.
	grpcSample := `lettuce_grpc_requests_total{code="OK",method="` + method + `"} `
	if !strings.Contains(body, grpcSample) {
		t.Errorf("scrape missing gRPC request-counter sample %q", grpcSample)
	}

	// The HTTP request counter recorded the health request (at least — other
	// package tests share the process-wide counter, so assert presence).
	if !regexp.MustCompile(`(?m)^lettuce_http_requests_total\{method="GET",status="\d+"\} `).MatchString(body) {
		t.Error(`scrape missing lettuce_http_requests_total{method="GET",...} sample`)
	}

	// Dispatch-cache gauge families are present on every scrape (0 with no
	// cache running in this process; the family itself is what dashboards and
	// this regression pin).
	for _, family := range []string{
		"lettuce_dispatch_ready_pool_size",
		"lettuce_dispatch_pending_reservation_writes",
		"lettuce_dispatch_pending_spot_check_writes",
		"lettuce_db_pool_acquired_conns",
		"lettuce_db_pool_idle_conns",
		"lettuce_db_pool_max_conns",
	} {
		if !regexp.MustCompile(`(?m)^` + family + ` `).MatchString(body) {
			t.Errorf("scrape missing gauge family %q", family)
		}
	}
}

// TestMetricsEndpoint_NonAdminForbidden_BG29 pins the role check: a valid
// non-admin (USER role) API key is 403, not 200 — the gate is ADMIN, not
// merely "authenticated".
func TestMetricsEndpoint_NonAdminForbidden_BG29(t *testing.T) {
	deps := &Dependencies{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:   "test",
		StartTime: time.Now(),
		ApiKeyRepo: &mockApiKeyRepo{
			getByHashFn: func(ctx context.Context, keyHash []byte) (*apikey.ApiKey, error) {
				return &apikey.ApiKey{ID: types.NewID(), UserID: types.NewID()}, nil
			},
		},
		AdminAPIKey: "metrics-test-admin-key",
	}
	router, cleanup := NewRouter(deps)
	t.Cleanup(cleanup)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer some-user-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /metrics as non-admin: want 403, got %d", rec.Code)
	}
}
