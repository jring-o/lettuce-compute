package management

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TB-91 regression test: the request log wrote one Debug line per management
// API request, and the desktop app polls /status, /metrics, /heads and
// /notices about twice a second — 130,479 of one tester's 500,000 debug lines.
// Routine reads are now counted and summarised once a minute; writes and
// failed requests are still logged one by one.

// requestLogServer returns a Server whose request log writes to the returned
// buffer at Debug level, and the middleware-wrapped handler under test, which
// answers 401 on /api/v1/denied and 200 everywhere else.
func requestLogServer() (*Server, http.Handler, *bytes.Buffer) {
	var buf bytes.Buffer
	s := &Server{logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	h := s.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/denied" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	return s, h, &buf
}

func serve(h http.Handler, method, path string) {
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
}

func TestTB91_RoutineReadsNotLoggedOneByOne(t *testing.T) {
	_, h, buf := requestLogServer()

	// Half a minute of the app's polling, at the observed rate.
	for i := 0; i < 25; i++ {
		serve(h, http.MethodGet, "/api/v1/status")
		serve(h, http.MethodGet, "/api/v1/metrics")
	}
	if got := strings.Count(buf.String(), `"msg":"management API`); got > 1 {
		t.Fatalf("request-log lines = %d for 50 routine reads, want at most 1 (a summary)", got)
	}

	// A write is still logged on its own.
	serve(h, http.MethodPost, "/api/v1/pause")
	if !strings.Contains(buf.String(), `"method":"POST","path":"/api/v1/pause"`) {
		t.Errorf("POST not logged individually; log:\n%s", buf.String())
	}
	// So is a failed read — a recurring 401 is how an unauthenticated
	// client was once diagnosed.
	serve(h, http.MethodGet, "/api/v1/denied")
	if !strings.Contains(buf.String(), `"path":"/api/v1/denied","status":401`) {
		t.Errorf("failed GET not logged individually; log:\n%s", buf.String())
	}
}
