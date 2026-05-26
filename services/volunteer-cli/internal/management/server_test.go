package management

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

func TestServerStartStop(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := NewServer(tmpDir, logger)
	d := newTestDaemon(tmpDir)
	bridge := NewDaemonBridge(d, filepath.Join(tmpDir, "config.yaml"))

	// Start the server.
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Verify port is assigned.
	if srv.Port() == 0 {
		t.Fatal("expected non-zero port")
	}

	// Verify token is 64 hex chars.
	if len(srv.Token()) != 64 {
		t.Fatalf("expected 64-char token, got %d chars", len(srv.Token()))
	}

	// Verify daemon.json was written.
	daemonJSONPath := filepath.Join(tmpDir, "daemon.json")
	data, err := os.ReadFile(daemonJSONPath)
	if err != nil {
		t.Fatalf("daemon.json not found: %v", err)
	}

	var info DaemonInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("parsing daemon.json: %v", err)
	}
	if info.Port != srv.Port() {
		t.Errorf("daemon.json port = %d, want %d", info.Port, srv.Port())
	}
	if info.Token != srv.Token() {
		t.Error("daemon.json token mismatch")
	}
	if info.PID == 0 {
		t.Error("expected non-zero PID in daemon.json")
	}
	if info.StartedAt == "" {
		t.Error("expected non-empty started_at in daemon.json")
	}

	// Shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() failed: %v", err)
	}

	// Verify daemon.json was removed.
	if _, err := os.Stat(daemonJSONPath); !os.IsNotExist(err) {
		t.Error("daemon.json should be removed after shutdown")
	}
}

func TestServerPortBeforeStart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := NewServer(t.TempDir(), logger)
	if srv.Port() != 0 {
		t.Errorf("Port() before Start should be 0, got %d", srv.Port())
	}
}

// TestHostCheckMiddleware verifies the Host-header allowlist that defends against
// DNS rebinding (M6). A foreign Host (e.g. an attacker hostname rebound to
// 127.0.0.1) must be rejected with 403 before auth; loopback hosts are allowed.
func TestHostCheckMiddleware(t *testing.T) {
	const port = 7780
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := hostCheckMiddleware(port, inner)

	tests := []struct {
		name     string
		host     string
		wantCode int
	}{
		{"loopback ip", fmt.Sprintf("127.0.0.1:%d", port), http.StatusOK},
		{"localhost", fmt.Sprintf("localhost:%d", port), http.StatusOK},
		{"foreign hostname", "evil.example.com", http.StatusForbidden},
		{"foreign hostname with port", fmt.Sprintf("evil.example.com:%d", port), http.StatusForbidden},
		{"loopback wrong port", "127.0.0.1:1234", http.StatusForbidden},
		{"empty host", "", http.StatusForbidden},
		{"rebound subdomain", fmt.Sprintf("attacker.test:%d", port), http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", "http://placeholder/api/v1/status", nil)
			if err != nil {
				t.Fatalf("creating request: %v", err)
			}
			// http.Request.Host is what the middleware inspects; set it directly so
			// we control the value independent of the URL.
			req.Host = tt.host
			w := newRecorder()
			handler.ServeHTTP(w, req)
			if w.code != tt.wantCode {
				t.Errorf("Host %q: got status %d, want %d", tt.host, w.code, tt.wantCode)
			}
		})
	}
}

// TestServerRejectsForeignHost verifies the end-to-end behavior on the running
// server: a request with a foreign Host header is rejected with 403 even with a
// valid token, while a request to the real bound Host with the valid token works.
func TestServerRejectsForeignHost(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := NewServer(tmpDir, logger)
	d := newTestDaemon(tmpDir)
	bridge := NewDaemonBridge(d, filepath.Join(tmpDir, "config.yaml"))
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", srv.Port())
	url := "http://" + addr + "/api/v1/status"

	// Foreign Host header with a valid token -> rejected with 403 before auth/handler.
	req, _ := http.NewRequest("GET", url, nil)
	req.Host = "evil.example.com"
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("foreign-host request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host: got %d, want 403", resp.StatusCode)
	}

	// Correct loopback Host (default from URL) with valid token -> allowed.
	req2, _ := http.NewRequest("GET", url, nil)
	req2.Header.Set("Authorization", "Bearer "+srv.Token())
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("loopback request failed: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("loopback Host with valid token: got %d, want 200", resp2.StatusCode)
	}
}

// TestNoCORSWildcard verifies the management API no longer emits a wildcard
// (or any) Access-Control-Allow-Origin header (M6). Local non-browser clients
// don't need CORS, and dropping it removes the DNS-rebinding read primitive.
func TestNoCORSWildcard(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := NewServer(tmpDir, logger)
	d := newTestDaemon(tmpDir)
	bridge := NewDaemonBridge(d, filepath.Join(tmpDir, "config.yaml"))
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/status", srv.Port())
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	// Simulate a cross-origin browser request.
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if acao := resp.Header.Get("Access-Control-Allow-Origin"); acao != "" {
		t.Errorf("Access-Control-Allow-Origin should be empty, got %q", acao)
	}
	if acao := resp.Header.Get("Access-Control-Allow-Origin"); acao == "*" {
		t.Error("Access-Control-Allow-Origin must never be wildcard")
	}
}

// minimalRecorder is a tiny ResponseWriter that records the status code, used by
// the unit-level Host-check test.
type minimalRecorder struct {
	header http.Header
	code   int
}

func newRecorder() *minimalRecorder {
	return &minimalRecorder{header: make(http.Header), code: http.StatusOK}
}

func (m *minimalRecorder) Header() http.Header     { return m.header }
func (m *minimalRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (m *minimalRecorder) WriteHeader(code int)    { m.code = code }

// newTestDaemon creates a minimal daemon for testing (no real gRPC connections).
func newTestDaemon(dataDir string) *daemon.Daemon {
	cfg := config.Defaults()
	cfg.DataDir = dataDir
	return daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
}
