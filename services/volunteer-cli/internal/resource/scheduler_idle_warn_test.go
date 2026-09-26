package resource

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// lockedBuffer is a bytes.Buffer safe to log into from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func debugLogger() (*slog.Logger, *lockedBuffer) {
	var buf lockedBuffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// A failed idle reading is asked about on every status poll, fetch check and
// monitor tick. It must be logged at WARN when it starts and then only as an
// hourly reminder, not once per reading, and its end must be logged once.
func TestScheduler_FailedIdleReadingWarnsOnceThenHourly(t *testing.T) {
	logger, logBuf := debugLogger()
	s := NewScheduler(&config.Scheduling{Mode: "WHEN_IDLE", IdleThresholdMins: 1}, logger)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return now }
	failing := true
	s.idleFunc = func() (int, error) {
		if failing {
			return 0, errors.New("no idle source answered")
		}
		return 600, nil
	}

	for i := 0; i < 50; i++ {
		if s.ShouldRun() {
			t.Fatal("ShouldRun() = true while idle time cannot be read")
		}
		now = now.Add(10 * time.Second)
	}
	if got := strings.Count(logBuf.String(), "level=WARN"); got != 1 {
		t.Fatalf("WARN lines after 50 failed readings over about 8 minutes = %d, want 1:\n%s", got, logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "run when idle") {
		t.Errorf("the WARN does not say what the failure costs:\n%s", logBuf.String())
	}

	now = now.Add(time.Hour)
	s.ShouldRun()
	if got := strings.Count(logBuf.String(), "level=WARN"); got != 2 {
		t.Fatalf("WARN lines after an hour more of failing = %d, want 2 (one hourly reminder)", got)
	}

	failing = false
	if !s.ShouldRun() {
		t.Fatal("ShouldRun() = false with 10 minutes idle and a 1 minute threshold")
	}
	s.ShouldRun()
	if got := strings.Count(logBuf.String(), "idle detection works again"); got != 1 {
		t.Errorf("recovery lines = %d, want 1:\n%s", got, logBuf.String())
	}
}
