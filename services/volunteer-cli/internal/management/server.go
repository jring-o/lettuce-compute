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
	"sort"
	"strings"
	"sync"
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

	// reads tallies the routine reads the request log summarises instead of
	// logging one by one (TB-91); now is its clock seam (nil = time.Now).
	reads readTally
	now   func() time.Time
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

// readSummaryInterval is how often the request log summarises the routine
// reads it no longer logs one by one (TB-91).
const readSummaryInterval = time.Minute

// loggingMiddleware logs each management API request at Debug — except the
// routine ones. The desktop app polls /status, /metrics, /heads and /notices
// about twice a second, which was 130,479 of one tester's 500,000 debug lines
// (TB-91), so a successful GET is counted instead and the counts are logged
// once a minute. Every write, and every request that failed, is still logged
// on its own.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		if r.Method == http.MethodGet && rw.statusCode < http.StatusBadRequest {
			if !s.logger.Enabled(r.Context(), slog.LevelDebug) {
				return
			}
			if n, span, paths, due := s.reads.add(r.URL.Path, s.clock()); due {
				s.logger.Debug("management API reads",
					"requests", n,
					"over", span.Round(time.Second).String(),
					"paths", paths,
				)
			}
			return
		}
		s.logger.Debug("management API request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// readTally counts routine reads per path over one summary window.
type readTally struct {
	mu     sync.Mutex
	since  time.Time
	counts map[string]int
}

// add counts one read of path at now. Once the window has run for
// readSummaryInterval it returns the window's total, its length and its
// per-path counts (busiest first, "path=count" separated by spaces), with
// due set, and starts a new window.
func (t *readTally) add(path string, now time.Time) (total int, span time.Duration, paths string, due bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil {
		t.counts = make(map[string]int)
		t.since = now
	}
	t.counts[path]++
	span = now.Sub(t.since)
	if span < readSummaryInterval {
		return 0, 0, "", false
	}
	keys := make([]string, 0, len(t.counts))
	for k, n := range t.counts {
		keys = append(keys, k)
		total += n
	}
	sort.Slice(keys, func(i, j int) bool {
		if t.counts[keys[i]] != t.counts[keys[j]] {
			return t.counts[keys[i]] > t.counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, t.counts[k])
	}
	t.counts = nil
	return total, span, strings.Join(parts, " "), true
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
