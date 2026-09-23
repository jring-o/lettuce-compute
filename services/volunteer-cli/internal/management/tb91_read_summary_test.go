package management

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The routine reads the request log no longer lists one by one are still
// accounted for: once a minute, one line with the count per path (TB-91).
func TestTB91_RoutineReadsSummarisedEachMinute(t *testing.T) {
	s, h, buf := requestLogServer()
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	for i := 0; i < 10; i++ {
		serve(h, http.MethodGet, "/api/v1/status")
		if i < 5 {
			serve(h, http.MethodGet, "/api/v1/metrics")
		}
		now = now.Add(5 * time.Second)
	}
	if buf.Len() != 0 {
		t.Fatalf("logged before the minute was up:\n%s", buf.String())
	}

	now = now.Add(15 * time.Second) // 65 s after the first read
	serve(h, http.MethodGet, "/api/v1/status")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("summary lines = %d, want 1; log:\n%s", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "management API reads" || rec["requests"] != float64(16) || rec["over"] != "1m5s" ||
		rec["paths"] != "/api/v1/status=11 /api/v1/metrics=5" {
		t.Errorf("summary = %v, want 16 requests over 1m5s, /api/v1/status=11 /api/v1/metrics=5", rec)
	}

	// A new window starts: the next read is counted, not logged.
	now = now.Add(time.Second)
	serve(h, http.MethodGet, "/api/v1/status")
	if got := strings.Count(buf.String(), "\n"); got != 1 {
		t.Errorf("lines after the summary = %d, want still 1", got)
	}
}
