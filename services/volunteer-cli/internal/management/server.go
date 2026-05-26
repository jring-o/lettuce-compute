package management

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// DaemonInfo is written to daemon.json so local clients can discover the management API.
type DaemonInfo struct {
	Port      int    `json:"port"`
	Token     string `json:"token"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// ReadDaemonInfo reads the daemon.json file to discover the management API port and token.
func ReadDaemonInfo(dataDir string) (DaemonInfo, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "daemon.json"))
	if err != nil {
		return DaemonInfo{}, err
	}
	var info DaemonInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return DaemonInfo{}, err
	}
	return info, nil
}

// Server is the local management HTTP server for the volunteer daemon.
type Server struct {
	httpServer *http.Server
	listener   net.Listener
	token      string
	dataDir    string
	logger     *slog.Logger
}

// NewServer creates a new management server.
func NewServer(dataDir string, logger *slog.Logger) *Server {
	return &Server{
		dataDir: dataDir,
		logger:  logger,
	}
}

// Start binds to a random localhost port, writes daemon.json, and begins serving.
func (s *Server) Start(bridge *DaemonBridge) error {
	// Generate auth token.
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("generating auth token: %w", err)
	}
	s.token = hex.EncodeToString(tokenBytes)

	// Bind to random localhost port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("binding to localhost: %w", err)
	}
	s.listener = ln

	port := ln.Addr().(*net.TCPAddr).Port

	// Build router.
	mux := http.NewServeMux()
	registerHandlers(mux, bridge)

	// Wrap with host-allowlist, auth, body size limit, and logging middleware.
	//
	// Middleware order (outermost first): logging -> host check -> auth -> body limit -> mux.
	// The host check runs before auth so a DNS-rebinding request (attacker hostname
	// rebound to 127.0.0.1) is rejected with 403 before it can even attempt auth.
	//
	// CORS is intentionally NOT set: the local management API is consumed only by
	// non-browser Go HTTP clients (the CLI itself via internal/cli), which do not
	// enforce the same-origin policy. Emitting no Access-Control-Allow-Origin header
	// means a malicious web page cannot read cross-origin responses, removing the
	// DNS-rebinding read primitive that the previous wildcard "*" enabled.
	handler := s.loggingMiddleware(hostCheckMiddleware(port, authMiddleware(s.token, maxBodyMiddleware(1<<20, mux))))

	s.httpServer = &http.Server{
		Handler: handler,
	}

	// Write daemon.json.
	info := DaemonInfo{
		Port:      port,
		Token:     s.token,
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.writeDaemonJSON(info); err != nil {
		ln.Close()
		return fmt.Errorf("writing daemon.json: %w", err)
	}

	s.logger.Info("management API started", "port", port)

	// Serve in background.
	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("management API server error", "error", err)
		}
	}()

	return nil
}

// Shutdown gracefully stops the server and removes daemon.json.
func (s *Server) Shutdown(ctx context.Context) error {
	s.removeDaemonJSON()

	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutting down management server: %w", err)
		}
	}

	s.logger.Info("management API stopped")
	return nil
}

// Port returns the port the server is listening on, or 0 if not started.
func (s *Server) Port() int {
	if s.listener == nil {
		return 0
	}
	return s.listener.Addr().(*net.TCPAddr).Port
}

// Token returns the auth token.
func (s *Server) Token() string {
	return s.token
}

func (s *Server) daemonJSONPath() string {
	return filepath.Join(s.dataDir, "daemon.json")
}

func (s *Server) writeDaemonJSON(info DaemonInfo) error {
	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.daemonJSONPath(), data, 0600)
}

func (s *Server) removeDaemonJSON() {
	os.Remove(s.daemonJSONPath())
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.logger.Debug("management API request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// maxBodyMiddleware limits request body size for non-GET methods.
func maxBodyMiddleware(maxBytes int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodOptions {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// hostCheckMiddleware rejects any request whose Host header does not match the
// loopback addresses the server is actually bound to (127.0.0.1:<port> or
// localhost:<port>). This defeats DNS rebinding: a malicious web page cannot
// reach the API through an attacker-controlled hostname that has been rebound to
// 127.0.0.1, because the browser sends that attacker hostname in the Host header,
// which is not on the allowlist. The check runs before authentication so such
// requests never reach the auth/handler layers.
func hostCheckMiddleware(port int, next http.Handler) http.Handler {
	allowed := map[string]struct{}{
		fmt.Sprintf("127.0.0.1:%d", port): {},
		fmt.Sprintf("localhost:%d", port): {},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := allowed[r.Host]; !ok {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "Invalid Host header")
			return
		}
		next.ServeHTTP(w, r)
	})
}
