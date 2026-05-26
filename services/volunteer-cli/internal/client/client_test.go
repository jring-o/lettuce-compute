package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// mockVolunteerService is a test helper implementing VolunteerServiceServer.
type mockVolunteerService struct {
	lettucev1.UnimplementedVolunteerServiceServer

	statusResp   *lettucev1.GetServerStatusResponse
	registerResp *lettucev1.RegisterVolunteerResponse
	registerReq  *lettucev1.RegisterVolunteerRequest // captures last request
	statusErr    error
	registerErr  error
}

func (m *mockVolunteerService) GetServerStatus(_ context.Context, _ *lettucev1.GetServerStatusRequest) (*lettucev1.GetServerStatusResponse, error) {
	if m.statusErr != nil {
		return nil, m.statusErr
	}
	if m.statusResp != nil {
		return m.statusResp, nil
	}
	return &lettucev1.GetServerStatusResponse{
		Status:         "ok",
		Version:        "test-0.1.0",
		UptimeSeconds:  42,
		DatabaseStatus: "healthy",
	}, nil
}

func (m *mockVolunteerService) RegisterVolunteer(_ context.Context, req *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	m.registerReq = req
	if m.registerErr != nil {
		return nil, m.registerErr
	}
	if m.registerResp != nil {
		return m.registerResp, nil
	}
	return &lettucev1.RegisterVolunteerResponse{
		VolunteerId: "test-volunteer-id",
		Registered:  true,
	}, nil
}

// startMockServer starts a gRPC server with the given service on a random port.
// Returns the address and a cleanup function.
func startMockServer(t *testing.T, svc lettucev1.VolunteerServiceServer) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()
	lettucev1.RegisterVolunteerServiceServer(s, svc)
	go s.Serve(lis)

	return lis.Addr().String(), func() {
		s.Stop()
		lis.Close()
	}
}

// newTestClient creates a Client connected to the given address with insecure creds.
func newTestClient(t *testing.T, addr string) *Client {
	t.Helper()
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return &Client{
		conn:           conn,
		svc:            lettucev1.NewVolunteerServiceClient(conn),
		logger:         slog.Default(),
		requestTimeout: 5 * time.Second,
	}
}

func TestNewInsecure(t *testing.T) {
	c, err := New(ClientConfig{
		ServerURL: "127.0.0.1:9999",
		Insecure:  true,
	}, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if c.svc == nil {
		t.Error("svc is nil")
	}
}

func TestNewDefaultTLS(t *testing.T) {
	c, err := New(ClientConfig{
		ServerURL: "127.0.0.1:9999",
	}, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if c.svc == nil {
		t.Error("svc is nil")
	}
}

func TestNewTLSBadCert(t *testing.T) {
	_, err := New(ClientConfig{
		ServerURL:   "127.0.0.1:9999",
		TLSCertFile: "/nonexistent/cert.pem",
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error for nonexistent cert file")
	}
}

func TestNewTLSUnparseableCert(t *testing.T) {
	// Write a file with invalid PEM content.
	tmpFile := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(tmpFile, []byte("not-a-pem-cert"), 0644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}

	_, err := New(ClientConfig{
		ServerURL:   "127.0.0.1:9999",
		TLSCertFile: tmpFile,
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error for unparseable cert file")
	}
}

func TestNewDefaultRequestTimeout(t *testing.T) {
	c, err := New(ClientConfig{
		ServerURL: "127.0.0.1:9999",
		Insecure:  true,
	}, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if c.requestTimeout != 30*time.Second {
		t.Errorf("requestTimeout = %v, want 30s", c.requestTimeout)
	}
}

func TestNewCustomRequestTimeout(t *testing.T) {
	c, err := New(ClientConfig{
		ServerURL:      "127.0.0.1:9999",
		Insecure:       true,
		RequestTimeout: 5 * time.Second,
	}, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if c.requestTimeout != 5*time.Second {
		t.Errorf("requestTimeout = %v, want 5s", c.requestTimeout)
	}
}

func TestRpcCtxPreservesExistingDeadline(t *testing.T) {
	c, err := New(ClientConfig{
		ServerURL: "127.0.0.1:9999",
		Insecure:  true,
	}, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	deadline := time.Now().Add(1 * time.Hour)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	rpcCtx, rpcCancel := c.rpcCtx(ctx)
	defer rpcCancel()

	gotDeadline, ok := rpcCtx.Deadline()
	if !ok {
		t.Fatal("expected deadline on rpcCtx")
	}
	// The deadline should be the one we set, not the default request timeout.
	if !gotDeadline.Equal(deadline) {
		t.Errorf("deadline = %v, want %v", gotDeadline, deadline)
	}
}

func TestRpcCtxAppliesDefaultTimeout(t *testing.T) {
	c, err := New(ClientConfig{
		ServerURL: "127.0.0.1:9999",
		Insecure:  true,
	}, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	before := time.Now()
	rpcCtx, rpcCancel := c.rpcCtx(context.Background())
	defer rpcCancel()

	gotDeadline, ok := rpcCtx.Deadline()
	if !ok {
		t.Fatal("expected deadline on rpcCtx")
	}
	// Deadline should be roughly now + 30s (the default).
	expectedDeadline := before.Add(30 * time.Second)
	if gotDeadline.Before(expectedDeadline.Add(-1*time.Second)) || gotDeadline.After(expectedDeadline.Add(1*time.Second)) {
		t.Errorf("deadline = %v, expected ~%v", gotDeadline, expectedDeadline)
	}
}

func TestGetServerStatusError(t *testing.T) {
	mock := &mockVolunteerService{
		statusErr: status.Error(codes.Internal, "server error"),
	}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	_, err := client.GetServerStatus(context.Background())
	if err == nil {
		t.Fatal("expected error from GetServerStatus")
	}
}

func TestRegisterVolunteerError(t *testing.T) {
	mock := &mockVolunteerService{
		registerErr: status.Error(codes.AlreadyExists, "duplicate"),
	}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	req := &lettucev1.RegisterVolunteerRequest{
		PublicKey: make([]byte, 32),
	}
	_, err := client.RegisterVolunteer(context.Background(), req)
	if err == nil {
		t.Fatal("expected error from RegisterVolunteer")
	}
}

func TestGetServerStatus(t *testing.T) {
	mock := &mockVolunteerService{}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	resp, err := client.GetServerStatus(context.Background())
	if err != nil {
		t.Fatalf("GetServerStatus: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want %q", resp.Status, "ok")
	}
	if resp.Version != "test-0.1.0" {
		t.Errorf("version = %q, want %q", resp.Version, "test-0.1.0")
	}
	if resp.UptimeSeconds != 42 {
		t.Errorf("uptime = %d, want 42", resp.UptimeSeconds)
	}
	if resp.DatabaseStatus != "healthy" {
		t.Errorf("db status = %q, want %q", resp.DatabaseStatus, "healthy")
	}
}

// --- TLS configuration tests ---

// generateTestCA creates a self-signed CA cert and key, writes them to dir.
func generateTestCA(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "Test CA"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	certPath = filepath.Join(dir, "ca.crt")
	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certFile.Close()

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, "ca.key")
	keyFile, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	keyFile.Close()

	return certPath, keyPath
}

// generateTestClientCert creates a client cert signed by the given CA.
func generateTestClientCert(t *testing.T, dir, caCertPath, caKeyPath string) (certPath, keyPath string) {
	t.Helper()

	caCertPEM, _ := os.ReadFile(caCertPath)
	caBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caKeyPEM, _ := os.ReadFile(caKeyPath)
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	caKey, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "Test Client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	certPath = filepath.Join(dir, "client.crt")
	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certFile.Close()

	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, "client.key")
	keyFile, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	keyFile.Close()

	return certPath, keyPath
}

func TestNew_CustomCACert(t *testing.T) {
	dir := t.TempDir()
	caCertPath, _ := generateTestCA(t, dir)

	c, err := New(ClientConfig{
		ServerURL:   "localhost:9090",
		TLSCertFile: caCertPath,
	}, slog.Default())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer c.Close()
}

func TestNew_MTLS(t *testing.T) {
	dir := t.TempDir()
	caCertPath, caKeyPath := generateTestCA(t, dir)
	clientCertPath, clientKeyPath := generateTestClientCert(t, dir, caCertPath, caKeyPath)

	c, err := New(ClientConfig{
		ServerURL:     "localhost:9090",
		TLSCertFile:   caCertPath,
		TLSClientCert: clientCertPath,
		TLSClientKey:  clientKeyPath,
	}, slog.Default())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer c.Close()
}

func TestNew_MTLSMissingKey(t *testing.T) {
	dir := t.TempDir()
	caCertPath, caKeyPath := generateTestCA(t, dir)
	clientCertPath, _ := generateTestClientCert(t, dir, caCertPath, caKeyPath)

	_, err := New(ClientConfig{
		ServerURL:     "localhost:9090",
		TLSClientCert: clientCertPath,
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error when client cert provided without key")
	}
}

func TestNew_MTLSMissingCert(t *testing.T) {
	dir := t.TempDir()
	_, caKeyPath := generateTestCA(t, dir)

	_, err := New(ClientConfig{
		ServerURL:    "localhost:9090",
		TLSClientKey: caKeyPath,
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error when client key provided without cert")
	}
}

func TestNew_MTLSInvalidCertPair(t *testing.T) {
	dir := t.TempDir()
	badCert := filepath.Join(dir, "bad-client.crt")
	badKey := filepath.Join(dir, "bad-client.key")
	os.WriteFile(badCert, []byte("not a cert"), 0644)
	os.WriteFile(badKey, []byte("not a key"), 0644)

	_, err := New(ClientConfig{
		ServerURL:     "localhost:9090",
		TLSClientCert: badCert,
		TLSClientKey:  badKey,
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error for invalid client cert/key pair")
	}
}

func TestRegisterVolunteer(t *testing.T) {
	mock := &mockVolunteerService{
		registerResp: &lettucev1.RegisterVolunteerResponse{
			VolunteerId: "vol-abc-123",
			Registered:  true,
		},
	}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	req := &lettucev1.RegisterVolunteerRequest{
		PublicKey:         make([]byte, 32),
		DisplayName:       "test-host",
		AvailableRuntimes: []string{"NATIVE"},
		SchedulingMode:    "ALWAYS",
	}
	resp, err := client.RegisterVolunteer(context.Background(), req)
	if err != nil {
		t.Fatalf("RegisterVolunteer: %v", err)
	}
	if resp.VolunteerId != "vol-abc-123" {
		t.Errorf("volunteer_id = %q, want %q", resp.VolunteerId, "vol-abc-123")
	}
	if !resp.Registered {
		t.Error("registered = false, want true")
	}
	if mock.registerReq.DisplayName != "test-host" {
		t.Errorf("captured display_name = %q, want %q", mock.registerReq.DisplayName, "test-host")
	}
}
