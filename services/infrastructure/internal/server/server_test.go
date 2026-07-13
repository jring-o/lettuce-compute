package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// --- Health endpoint tests ---

func TestHealthHandler_DegradedWithNilPool(t *testing.T) {
	handler := HealthHandler(nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// BG-20: a degraded head must answer 503, not 200 — Docker healthchecks, load
	// balancers, and uptime monitors read the status CODE, so a 200 with a
	// "degraded" body reads as healthy to every machine consumer.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503 when degraded, got %d", rec.Code)
	}

	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// With nil pool, expect degraded + disconnected.
	if resp.Status != "degraded" {
		t.Errorf("expected status 'degraded' with nil pool, got '%s'", resp.Status)
	}
	if resp.Database != "disconnected" {
		t.Errorf("expected database 'disconnected' with nil pool, got '%s'", resp.Database)
	}
}

// TestBG20_HealthDetailedReturns503WhenDegraded locks the same status-code
// contract on the authed detailed endpoint (the second WriteHeader site).
func TestBG20_HealthDetailedReturns503WhenDegraded(t *testing.T) {
	handler := HealthDetailedHandler(nil, time.Now())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health/detailed", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503 when degraded, got %d", rec.Code)
	}
	var resp healthDetailedResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "degraded" {
		t.Errorf("expected status 'degraded', got '%s'", resp.Status)
	}
}

// TestBG20_HealthReturns503WhenDBUnreachable exercises the non-nil-pool degraded
// path: a real pgxpool aimed at a closed loopback port (pgxpool connects lazily,
// so construction succeeds and the health probe is what fails).
func TestBG20_HealthReturns503WhenDBUnreachable(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody:nothing@127.0.0.1:1/lettuce?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("failed to build lazy pool: %v", err)
	}
	defer pool.Close()

	handler := HealthHandler(pool)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503 with unreachable DB, got %d", rec.Code)
	}
	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "degraded" || resp.Database != "disconnected" {
		t.Errorf("expected degraded/disconnected body, got %q/%q", resp.Status, resp.Database)
	}
}

func TestHealthHandler_ResponseShape(t *testing.T) {
	handler := HealthHandler(nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got '%s'", ct)
	}

	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	// Public endpoint exposes "status" and "database" (the documented operator
	// contract in guides/head-setup.md); "database" is already implied by
	// "status" so it leaks nothing new. Uptime and other internal detail not
	// derivable from "status" stay behind auth on /health/detailed.
	for _, key := range []string{"status", "database"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing required field: %q", key)
		}
	}
	if _, ok := raw["uptime_seconds"]; ok {
		t.Error(`public health response must not expose "uptime_seconds"`)
	}
}

func TestHealthDetailedHandler_IncludesAllFields(t *testing.T) {
	startTime := time.Now().Add(-1 * time.Hour)
	handler := HealthDetailedHandler(nil, startTime)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health/detailed", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Nil pool means degraded, and a degraded head answers 503 (BG-20); the
	// field assertions below are what this test is actually about.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503 (degraded, nil pool), got %d", rec.Code)
	}

	var resp healthDetailedResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "degraded" {
		t.Errorf("expected status 'degraded' with nil pool, got '%s'", resp.Status)
	}
	if resp.Database != "disconnected" {
		t.Errorf("expected database 'disconnected' with nil pool, got '%s'", resp.Database)
	}
	if resp.UptimeSeconds < 3600 {
		t.Errorf("expected uptime >= 3600s, got %d", resp.UptimeSeconds)
	}
}

// --- Request ID middleware tests ---

func TestRequestIDMiddleware_GeneratesID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	deps := &Dependencies{
		Logger:    logger,
		Version:   "test",
		StartTime: time.Now(),
	}
	router, cleanup := NewRouter(deps)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	requestID := rec.Header().Get("X-Request-ID")
	if requestID == "" {
		t.Error("expected X-Request-ID header to be set")
	}
}

func TestRequestIDMiddleware_PropagatesExistingID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	deps := &Dependencies{
		Logger:    logger,
		Version:   "test",
		StartTime: time.Now(),
	}
	router, cleanup := NewRouter(deps)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("X-Request-ID", "test-request-id-123")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	requestID := rec.Header().Get("X-Request-ID")
	if requestID != "test-request-id-123" {
		t.Errorf("expected X-Request-ID 'test-request-id-123', got '%s'", requestID)
	}
}

// --- CORS middleware tests ---

func TestCORSMiddleware_Headers(t *testing.T) {
	// With CORSOrigins unset, cross-origin sharing is DISABLED (fail-closed):
	// no Access-Control-Allow-Origin header should be emitted.
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	deps := &Dependencies{
		Logger:    logger,
		Version:   "test",
		StartTime: time.Now(),
	}
	router, cleanup := NewRouter(deps)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	origin := rec.Header().Get("Access-Control-Allow-Origin")
	if origin != "" {
		t.Errorf("expected no CORS Allow-Origin when origins unset, got '%s'", origin)
	}
	if cred := rec.Header().Get("Access-Control-Allow-Credentials"); cred != "" {
		t.Errorf("expected no Allow-Credentials when origins unset, got '%s'", cred)
	}

	methods := rec.Header().Get("Access-Control-Allow-Methods")
	if methods == "" {
		t.Error("expected CORS Allow-Methods header to be set")
	}
}

func TestCORSMiddleware_Preflight(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	deps := &Dependencies{
		Logger:    logger,
		Version:   "test",
		StartTime: time.Now(),
	}
	router, cleanup := NewRouter(deps)
	defer cleanup()

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/health", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204 for OPTIONS preflight, got %d", rec.Code)
	}
}

// --- Recovery middleware tests ---

func TestRecoveryMiddleware_CatchesPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	panickingHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("test panic")
	})

	handler := recoveryMiddleware(panickingHandler, logger)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}
}

// --- Router tests ---

func TestRouter_UnknownPath404(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	deps := &Dependencies{
		Logger:    logger,
		Version:   "test",
		StartTime: time.Now(),
	}
	router, cleanup := NewRouter(deps)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", rec.Code)
	}
}

// --- gRPC GetServerStatus tests ---

func TestGRPCGetServerStatus(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	startTime := time.Now()

	grpcServer, grpcCleanup := NewGRPCServer(nil, logger, nil)
	defer grpcCleanup()
	volunteerSvc := NewVolunteerService(nil, "0.1.0-test", startTime, nil, nil, nil, nil, nil, nil, nil, nil, logger, transition.TrustPolicy{})
	lettucev1.RegisterVolunteerServiceServer(grpcServer, volunteerSvc)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	go func() {
		if serveErr := grpcServer.Serve(lis); serveErr != nil {
			t.Logf("gRPC server stopped: %v", serveErr)
		}
	}()
	defer grpcServer.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
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

	// With nil pool, expect degraded
	if resp.Status != "degraded" {
		t.Errorf("expected status 'degraded', got '%s'", resp.Status)
	}
	if resp.DatabaseStatus != "disconnected" {
		t.Errorf("expected database_status 'disconnected', got '%s'", resp.DatabaseStatus)
	}

	if resp.UptimeSeconds < 0 {
		t.Errorf("expected non-negative uptime, got %d", resp.UptimeSeconds)
	}

	// Version must be the build version the service was constructed with — the
	// volunteer's head-version pairing depends on GetServerStatus returning it
	// (it was silently omitted before, so a populated check guards the regression).
	if resp.Version != "0.1.0-test" {
		t.Errorf("expected version '0.1.0-test', got '%s'", resp.Version)
	}
}

// --- TLS config tests ---

func TestLoadTLSConfig_EmptyReturnsNil(t *testing.T) {
	cfg := config.TLSConfig{}
	tlsCfg, err := LoadTLSConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tlsCfg != nil {
		t.Error("expected nil TLS config for empty cert")
	}
}

func TestLoadTLSConfig_InvalidPathReturnsError(t *testing.T) {
	cfg := config.TLSConfig{
		CertFile: "/nonexistent/cert.pem",
		KeyFile:  "/nonexistent/key.pem",
	}
	_, err := LoadTLSConfig(cfg)
	if err == nil {
		t.Error("expected error for invalid cert path")
	}
}

func TestLoadTLSConfig_ValidCert(t *testing.T) {
	certFile, keyFile := generateTestCert(t)

	cfg := config.TLSConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
	}

	tlsCfg, err := LoadTLSConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if tlsCfg == nil {
		t.Fatal("expected non-nil TLS config")
	}
	if tlsCfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("expected min TLS version 1.3, got %d", tlsCfg.MinVersion)
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("expected 1 certificate, got %d", len(tlsCfg.Certificates))
	}
}

func TestLoadTLSConfig_WithCA(t *testing.T) {
	certFile, keyFile := generateTestCert(t)

	cfg := config.TLSConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   certFile, // use same cert as CA for test
	}

	tlsCfg, err := LoadTLSConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if tlsCfg.ClientCAs == nil {
		t.Error("expected ClientCAs to be set")
	}
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("expected RequireAndVerifyClientCert, got %v", tlsCfg.ClientAuth)
	}
}

// --- HTTP server tests ---

func TestNewHTTPServer_Timeouts(t *testing.T) {
	srv := NewHTTPServer(":0", http.DefaultServeMux, nil)

	if srv.ReadTimeout != 10*time.Second {
		t.Errorf("expected read timeout 10s, got %s", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 30*time.Second {
		t.Errorf("expected write timeout 30s, got %s", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("expected idle timeout 120s, got %s", srv.IdleTimeout)
	}
}

// --- Response writer tests ---

func TestResponseWriter_CapturesStatusCode(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	rw.WriteHeader(http.StatusNotFound)
	if rw.statusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.statusCode)
	}
}

// --- Request logging middleware test ---

func TestRequestLoggingMiddleware(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := requestLoggingMiddleware(inner, logger, nil)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	// Should not panic, and should complete normally.
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

// --- Request logging middleware: verify log content ---

func TestRequestLoggingMiddleware_LogsFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	handler := requestLoggingMiddleware(inner, logger, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	logOutput := buf.String()
	for _, field := range []string{`"method"`, `"path"`, `"status"`, `"duration_ms"`} {
		if !strings.Contains(logOutput, field) {
			t.Errorf("expected log output to contain %s, got: %s", field, logOutput)
		}
	}
	if !strings.Contains(logOutput, `"POST"`) {
		t.Errorf("expected log to contain POST method")
	}
	if !strings.Contains(logOutput, `"/api/v1/health"`) {
		t.Errorf("expected log to contain path")
	}
}

// --- Recovery middleware: verify response body ---

func TestRecoveryMiddleware_ResponseBody(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	panickingHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("test panic")
	})

	handler := recoveryMiddleware(panickingHandler, logger)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "INTERNAL_ERROR") {
		t.Errorf("expected response body to contain INTERNAL_ERROR, got: %s", body)
	}
	if !strings.Contains(body, "internal server error") {
		t.Errorf("expected response body to contain 'internal server error', got: %s", body)
	}
}

// --- CORS with custom origin ---

func TestCORSMiddleware_CustomOrigin(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	deps := &Dependencies{
		Logger:      logger,
		Version:     "test",
		StartTime:   time.Now(),
		CORSOrigins: "https://example.com",
	}
	router, cleanup := NewRouter(deps)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	origin := rec.Header().Get("Access-Control-Allow-Origin")
	if origin != "https://example.com" {
		t.Errorf("expected CORS origin 'https://example.com', got '%s'", origin)
	}
}

// --- TLS config: invalid CA path ---

func TestLoadTLSConfig_InvalidCAPath(t *testing.T) {
	certFile, keyFile := generateTestCert(t)

	cfg := config.TLSConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   "/nonexistent/ca.pem",
	}

	_, err := LoadTLSConfig(cfg)
	if err == nil {
		t.Error("expected error for invalid CA path")
	}
}

func TestLoadTLSConfig_MalformedCA(t *testing.T) {
	certFile, keyFile := generateTestCert(t)

	// Create a file with non-PEM content
	dir := t.TempDir()
	badCA := filepath.Join(dir, "bad-ca.pem")
	if err := os.WriteFile(badCA, []byte("not a valid PEM certificate"), 0o600); err != nil {
		t.Fatalf("failed to write bad CA: %v", err)
	}

	cfg := config.TLSConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   badCA,
	}

	_, err := LoadTLSConfig(cfg)
	if err == nil {
		t.Error("expected error for malformed CA content")
	}
}

// --- gRPC recovery interceptor ---

func TestGRPCRecoveryInterceptor(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	grpcServer, grpcCleanup := NewGRPCServer(nil, logger, nil)
	defer grpcCleanup()

	// Register a service that panics
	lettucev1.RegisterVolunteerServiceServer(grpcServer, &panickingVolunteerService{})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	go func() {
		_ = grpcServer.Serve(lis)
	}()
	defer grpcServer.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	client := lettucev1.NewVolunteerServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = client.GetServerStatus(ctx, &lettucev1.GetServerStatusRequest{})
	if err == nil {
		t.Fatal("expected error from panicking handler")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("expected codes.Internal, got %s", st.Code())
	}
}

// --- RegisterVolunteer gRPC handler tests ---

// mockVolunteerRepo is an in-memory mock of volunteer.Repository for unit tests.
type mockVolunteerRepo struct {
	volunteers map[string]*volunteer.Volunteer // keyed by base64(public_key)
}

func newMockVolunteerRepo() *mockVolunteerRepo {
	return &mockVolunteerRepo{volunteers: make(map[string]*volunteer.Volunteer)}
}

func (m *mockVolunteerRepo) Create(_ context.Context, v *volunteer.Volunteer) error {
	key := string(v.PublicKey)
	if _, exists := m.volunteers[key]; exists {
		return apierror.Conflict("volunteer with this public key already exists", nil)
	}
	v.ID = types.NewID()
	now := time.Now().UTC()
	v.RegisteredAt = now
	v.CreatedAt = now
	v.UpdatedAt = now
	m.volunteers[key] = v
	return nil
}

// CreateAdmitted delegates to Create: the in-map fake has no transaction/counter, and
// the nil-gate contract is "exactly Create" anyway.
func (m *mockVolunteerRepo) CreateAdmitted(ctx context.Context, v *volunteer.Volunteer, _ *admission.CreateGate) error {
	return m.Create(ctx, v)
}

func (m *mockVolunteerRepo) GetByID(_ context.Context, id types.ID) (*volunteer.Volunteer, error) {
	for _, v := range m.volunteers {
		if v.ID == id {
			return v, nil
		}
	}
	return nil, apierror.NotFound("volunteer", id.String())
}

func (m *mockVolunteerRepo) GetByPublicKey(_ context.Context, publicKey []byte) (*volunteer.Volunteer, error) {
	if v, ok := m.volunteers[string(publicKey)]; ok {
		return v, nil
	}
	return nil, apierror.NotFound("volunteer", "public_key")
}

func (m *mockVolunteerRepo) Update(_ context.Context, v *volunteer.Volunteer) error {
	for key, existing := range m.volunteers {
		if existing.ID == v.ID {
			v.UpdatedAt = time.Now().UTC()
			m.volunteers[key] = v
			return nil
		}
	}
	return apierror.NotFound("volunteer", v.ID.String())
}

func (m *mockVolunteerRepo) UpdateLastSeen(_ context.Context, id types.ID) error {
	for _, v := range m.volunteers {
		if v.ID == id {
			now := time.Now().UTC()
			v.LastSeenAt = &now
			return nil
		}
	}
	return apierror.NotFound("volunteer", id.String())
}

func (m *mockVolunteerRepo) SetActive(_ context.Context, id types.ID, active bool) error {
	for _, v := range m.volunteers {
		if v.ID == id {
			v.IsActive = active
			return nil
		}
	}
	return apierror.NotFound("volunteer", id.String())
}

func (m *mockVolunteerRepo) IncrementWorkUnitsCompleted(_ context.Context, _ types.ID) error {
	return nil
}

func (m *mockVolunteerRepo) IncrementWorkUnitsRejected(_ context.Context, _ types.ID) error {
	return nil
}

func (m *mockVolunteerRepo) List(_ context.Context, _ volunteer.VolunteerListFilters, _ types.PaginationRequest) ([]*volunteer.Volunteer, types.PaginationResponse, error) {
	var result []*volunteer.Volunteer
	for _, v := range m.volunteers {
		result = append(result, v)
	}
	return result, types.PaginationResponse{}, nil
}

func (m *mockVolunteerRepo) GetByUserID(_ context.Context, _ types.ID) (*volunteer.Volunteer, error) {
	return nil, nil
}

func (m *mockVolunteerRepo) MarkInactiveOlderThan(_ context.Context, _ time.Duration) (int, error) {
	return 0, nil
}
func (m *mockVolunteerRepo) SetDIDBinding(_ context.Context, _ types.ID, _, _, _ string, _ time.Time) error {
	return nil
}
func (m *mockVolunteerRepo) ListDIDBindingsForRecheck(_ context.Context, _ time.Time, _ int) ([]*volunteer.Volunteer, error) {
	return nil, nil
}
func (m *mockVolunteerRepo) MarkDIDBindingChecked(_ context.Context, _ types.ID, _ string, _ time.Time) error {
	return nil
}
func (m *mockVolunteerRepo) MarkDIDBindingCheckFailed(_ context.Context, _ types.ID, _ time.Time, _ int) error {
	return nil
}
func (m *mockVolunteerRepo) RevokeDIDBinding(_ context.Context, _ types.ID, _ time.Time) error {
	return nil
}
func (m *mockVolunteerRepo) SetDIDFrozenUntil(_ context.Context, _ types.ID, _ time.Time) error {
	return nil
}

// testRegisterPubKey is the Ed25519 public key paired with the signing key the
// register-test client uses (see setupRegisterTestServer). RegisterVolunteer now
// binds the authenticated key to req.PublicKey, so the request key must match.
var testRegisterPubKey ed25519.PublicKey

func validRegisterRequest() *lettucev1.RegisterVolunteerRequest {
	pk := make([]byte, 32)
	copy(pk, testRegisterPubKey)
	return &lettucev1.RegisterVolunteerRequest{
		PublicKey:   pk,
		DisplayName: "Test Volunteer",
		Hardware: &lettucev1.HardwareCapabilities{
			CpuCores:      8,
			CpuModel:      "AMD Ryzen 7 5800X",
			MaxCpuCores:   4,
			MemoryTotalMb: 32768,
			MaxMemoryMb:   16384,
		},
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	}
}

// testSigningClientInterceptor signs every outgoing (non-public) RPC with the given
// Ed25519 key, mirroring the volunteer-cli's client interceptor and the server's
// canonical message format. It lets in-process gRPC tests pass the auth interceptor.
func testSigningClientInterceptor(priv ed25519.PrivateKey, pub ed25519.PublicKey) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if grpcPublicMethods[method] {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		msg, ok := req.(proto.Message)
		if !ok {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
		if err != nil {
			return err
		}
		unixTs := timeNow().Unix()
		// The nonce is REQUIRED on every signed RPC (the legacy no-nonce form was
		// removed). Emit a fresh 128-bit nonce per call, sign the with-nonce
		// canonical form, and attach the same hex string as x-lettuce-nonce metadata,
		// exactly as the real client does.
		var nonceBytes [16]byte
		if _, err := rand.Read(nonceBytes[:]); err != nil {
			return err
		}
		nonce := hex.EncodeToString(nonceBytes[:])
		signed := canonicalGRPCAuthMessage(unixTs, method, requestBytes, nonce)
		sig := ed25519.Sign(priv, []byte(signed))
		ctx = metadata.AppendToOutgoingContext(ctx,
			grpcAuthPubKeyMeta, string(pub),
			grpcAuthTimestampMeta, strconv.FormatInt(unixTs, 10),
			grpcAuthSignatureMeta, string(sig),
			grpcAuthNonceMeta, nonce,
		)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func setupRegisterTestServer(t *testing.T, repo volunteer.Repository) (lettucev1.VolunteerServiceClient, func()) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate keypair: %v", err)
	}
	testRegisterPubKey = pub

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	grpcServer, grpcCleanup := NewGRPCServer(nil, logger, nil)
	defer grpcCleanup()
	svc := NewVolunteerService(nil, "0.1.0-test", time.Now(), repo, nil, nil, nil, nil, nil, nil, nil, logger, transition.TrustPolicy{})
	lettucev1.RegisterVolunteerServiceServer(grpcServer, svc)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(testSigningClientInterceptor(priv, pub)),
	)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	cleanup := func() {
		conn.Close()
		grpcServer.Stop()
	}
	return lettucev1.NewVolunteerServiceClient(conn), cleanup
}

func TestRegisterVolunteer_NewRegistration(t *testing.T) {
	repo := newMockVolunteerRepo()
	client, cleanup := setupRegisterTestServer(t, repo)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.RegisterVolunteer(ctx, validRegisterRequest())
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	if !resp.Registered {
		t.Error("expected registered = true for new volunteer")
	}
	if resp.VolunteerId == "" {
		t.Error("expected non-empty volunteer_id")
	}
}

func TestRegisterVolunteer_UpdateExisting(t *testing.T) {
	repo := newMockVolunteerRepo()
	client, cleanup := setupRegisterTestServer(t, repo)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := validRegisterRequest()

	// First call: register.
	resp1, err := client.RegisterVolunteer(ctx, req)
	if err != nil {
		t.Fatalf("first RegisterVolunteer: %v", err)
	}
	if !resp1.Registered {
		t.Error("first call: expected registered = true")
	}

	// Second call with same key: update.
	req.DisplayName = "Updated Name"
	req.Hardware.MaxCpuCores = 8
	resp2, err := client.RegisterVolunteer(ctx, req)
	if err != nil {
		t.Fatalf("second RegisterVolunteer: %v", err)
	}
	if resp2.Registered {
		t.Error("second call: expected registered = false")
	}
	if resp2.VolunteerId != resp1.VolunteerId {
		t.Errorf("volunteer_id changed: %q → %q", resp1.VolunteerId, resp2.VolunteerId)
	}
}

func TestRegisterVolunteer_InvalidPublicKeyLength(t *testing.T) {
	repo := newMockVolunteerRepo()
	client, cleanup := setupRegisterTestServer(t, repo)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := validRegisterRequest()
	req.PublicKey = make([]byte, 16) // wrong length

	_, err := client.RegisterVolunteer(ctx, req)
	if err == nil {
		t.Fatal("expected error for invalid public key length")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", st.Code())
	}
}

func TestRegisterVolunteer_MissingHardware(t *testing.T) {
	repo := newMockVolunteerRepo()
	client, cleanup := setupRegisterTestServer(t, repo)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := validRegisterRequest()
	req.Hardware = nil

	_, err := client.RegisterVolunteer(ctx, req)
	if err == nil {
		t.Fatal("expected error for missing hardware")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", st.Code())
	}
}

func TestRegisterVolunteer_InvalidHardwareValues(t *testing.T) {
	tests := []struct {
		name   string
		modify func(req *lettucev1.RegisterVolunteerRequest)
	}{
		{"cpu_cores=0", func(req *lettucev1.RegisterVolunteerRequest) { req.Hardware.CpuCores = 0 }},
		{"max_cpu_cores=0", func(req *lettucev1.RegisterVolunteerRequest) { req.Hardware.MaxCpuCores = 0 }},
		{"max_cpu_cores>cpu_cores", func(req *lettucev1.RegisterVolunteerRequest) { req.Hardware.MaxCpuCores = 16 }},
		{"memory_total_mb=0", func(req *lettucev1.RegisterVolunteerRequest) { req.Hardware.MemoryTotalMb = 0 }},
		{"max_memory_mb=0", func(req *lettucev1.RegisterVolunteerRequest) { req.Hardware.MaxMemoryMb = 0 }},
		{"max_memory_mb>memory_total_mb", func(req *lettucev1.RegisterVolunteerRequest) { req.Hardware.MaxMemoryMb = 65536 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newMockVolunteerRepo()
			client, cleanup := setupRegisterTestServer(t, repo)
			defer cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			req := validRegisterRequest()
			tt.modify(req)

			_, err := client.RegisterVolunteer(ctx, req)
			if err == nil {
				t.Fatal("expected error")
			}
			st, _ := status.FromError(err)
			if st.Code() != codes.InvalidArgument {
				t.Errorf("expected InvalidArgument, got %s", st.Code())
			}
		})
	}
}

func TestRegisterVolunteer_EmptyRuntimes(t *testing.T) {
	repo := newMockVolunteerRepo()
	client, cleanup := setupRegisterTestServer(t, repo)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := validRegisterRequest()
	req.AvailableRuntimes = nil

	_, err := client.RegisterVolunteer(ctx, req)
	if err == nil {
		t.Fatal("expected error for empty runtimes")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", st.Code())
	}
}

func TestRegisterVolunteer_InvalidRuntime(t *testing.T) {
	repo := newMockVolunteerRepo()
	client, cleanup := setupRegisterTestServer(t, repo)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := validRegisterRequest()
	req.AvailableRuntimes = []string{"NATIVE", "SCRIPT"}

	_, err := client.RegisterVolunteer(ctx, req)
	if err == nil {
		t.Fatal("expected error for invalid runtime")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", st.Code())
	}
}

func TestRegisterVolunteer_WASMRuntime(t *testing.T) {
	repo := newMockVolunteerRepo()
	client, cleanup := setupRegisterTestServer(t, repo)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := validRegisterRequest()
	req.AvailableRuntimes = []string{"NATIVE", "WASM"}

	resp, err := client.RegisterVolunteer(ctx, req)
	if err != nil {
		t.Fatalf("RegisterVolunteer with WASM runtime: %v", err)
	}
	if !resp.Registered {
		t.Error("expected registered = true")
	}
}

func TestRegisterVolunteer_InvalidSchedulingMode(t *testing.T) {
	repo := newMockVolunteerRepo()
	client, cleanup := setupRegisterTestServer(t, repo)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := validRegisterRequest()
	req.SchedulingMode = "NEVER"

	_, err := client.RegisterVolunteer(ctx, req)
	if err == nil {
		t.Fatal("expected error for invalid scheduling mode")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", st.Code())
	}
}

func TestRegisterVolunteer_DefaultSchedulingMode(t *testing.T) {
	repo := newMockVolunteerRepo()
	client, cleanup := setupRegisterTestServer(t, repo)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := validRegisterRequest()
	req.SchedulingMode = "" // should default to ALWAYS

	resp, err := client.RegisterVolunteer(ctx, req)
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	if !resp.Registered {
		t.Error("expected registered = true")
	}
}

type panickingVolunteerService struct {
	lettucev1.UnimplementedVolunteerServiceServer
}

func (s *panickingVolunteerService) GetServerStatus(context.Context, *lettucev1.GetServerStatusRequest) (*lettucev1.GetServerStatusResponse, error) {
	panic("deliberate test panic")
}

// --- Graceful shutdown test ---

func TestGracefulShutdown_ContextCancel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// Create real HTTP server on a random port.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	httpServer := NewHTTPServer("127.0.0.1:0", mux, nil)

	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for HTTP: %v", err)
	}

	go func() {
		_ = httpServer.Serve(httpLis)
	}()

	// Create real gRPC server.
	grpcSrv, grpcCleanup := NewGRPCServer(nil, logger, nil)
	defer grpcCleanup()
	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for gRPC: %v", err)
	}

	go func() {
		_ = grpcSrv.Serve(grpcLis)
	}()

	// Verify HTTP is reachable before shutdown.
	resp, err := http.Get("http://" + httpLis.Addr().String() + "/")
	if err != nil {
		t.Fatalf("HTTP server not reachable: %v", err)
	}
	resp.Body.Close()

	// Trigger shutdown via context cancellation.
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		GracefulShutdown(ctx, httpServer, grpcSrv, nil, 5*time.Second)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Shutdown completed successfully.
	case <-time.After(10 * time.Second):
		t.Fatal("graceful shutdown did not complete within timeout")
	}
}

// --- HTTP server with TLS config ---

func TestNewHTTPServer_WithTLS(t *testing.T) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13}
	srv := NewHTTPServer(":0", http.DefaultServeMux, tlsCfg)

	if srv.TLSConfig == nil {
		t.Error("expected TLS config to be set")
	}
	if srv.TLSConfig.MinVersion != tls.VersionTLS13 {
		t.Errorf("expected TLS 1.3 min version")
	}
}

// --- Helpers ---

// generateTestCert creates a self-signed certificate for testing.
func generateTestCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("failed to create cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	certOut.Close()

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("failed to marshal key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("failed to create key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}
	keyOut.Close()

	return certPath, keyPath
}
