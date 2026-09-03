package runtime

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingSink captures notices the thermal monitor emits, and — in order
// with them — the resolutions it requests.
type recordingSink struct {
	mu      sync.Mutex
	entries []struct{ level, code, message string }
	// events is every call in order: "notify:<level>" or "resolve:<code>".
	events []string
}

func (s *recordingSink) Notify(level, code, message, head, leaf string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, struct{ level, code, message string }{level, code, message})
	s.events = append(s.events, "notify:"+level)
}

func (s *recordingSink) Resolve(code, head, leaf string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "resolve:"+code)
	return 1
}

func (s *recordingSink) snapshot() []struct{ level, code, message string } {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]struct{ level, code, message string }(nil), s.entries...)
}

func (s *recordingSink) eventLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// The throttle's activation WARN and release Info are the two thermal
// escalations a volunteer should see without reading the log: activation as
// a warn notice naming the temperatures, release as an info notice.
func TestThermalMonitor_EmitsThrottleNotices(t *testing.T) {
	var polls int
	var mu sync.Mutex
	withMockCPUTempFunc(t, func() int {
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls <= 2 {
			return 90 // hot: above the 85C pause threshold
		}
		return 60 // cool: below the 75C resume threshold
	})
	withMockExecutor(t, notFoundForAll) // no GPU

	sink := &recordingSink{}
	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	monitor := NewThermalMonitor(defaultThermalConfig(), pauseCh, logger)
	monitor.SetNoticeSink(sink)
	monitor.SetPollIntervalForTest(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	monitor.Start(ctx)

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(sink.snapshot()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	monitor.Stop()

	got := sink.snapshot()
	if len(got) < 2 {
		t.Fatalf("got %d thermal notices, want an activation and a release: %+v", len(got), got)
	}
	activated, released := got[0], got[1]
	if activated.code != "thermal_throttle" || activated.level != "warn" {
		t.Errorf("activation notice = %+v; want code thermal_throttle at level warn", activated)
	}
	if !strings.Contains(activated.message, "90°C") || !strings.Contains(activated.message, "85°C") {
		t.Errorf("activation message must name the temperature and threshold: %q", activated.message)
	}
	if released.code != "thermal_throttle" || released.level != "info" {
		t.Errorf("release notice = %+v; want code thermal_throttle at level info", released)
	}
	if !strings.Contains(released.message, "resumed") {
		t.Errorf("release message must say computing resumed: %q", released.message)
	}
}

// TB-50: the activation notice is a condition ("computing paused"), so the
// release must resolve it — before the release notice is emitted, so that
// notice starts its own entry instead of refreshing the warning in place.
func TestThermalMonitor_ReleaseResolvesTheActivationNotice(t *testing.T) {
	var polls int
	var mu sync.Mutex
	withMockCPUTempFunc(t, func() int {
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls <= 2 {
			return 90
		}
		return 60
	})
	withMockExecutor(t, notFoundForAll)

	sink := &recordingSink{}
	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	monitor := NewThermalMonitor(defaultThermalConfig(), pauseCh, logger)
	monitor.SetNoticeSink(sink)
	monitor.SetPollIntervalForTest(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	monitor.Start(ctx)

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(sink.snapshot()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	monitor.Stop()

	got := sink.eventLog()
	want := []string{"notify:warn", "resolve:thermal_throttle", "notify:info"}
	if len(got) < len(want) {
		t.Fatalf("sink saw %v, want at least %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sink saw %v, want %v (activation, then its resolution, then the release notice)", got, want)
		}
	}
}

// A monitor with no sink must keep working exactly as before.
func TestThermalMonitor_NoSinkIsHarmless(t *testing.T) {
	withMockCPUTemp(t, 90)
	withMockExecutor(t, notFoundForAll)

	pauseCh := make(chan bool, 10)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	monitor := NewThermalMonitor(defaultThermalConfig(), pauseCh, logger)
	monitor.SetPollIntervalForTest(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	monitor.Start(ctx)
	<-ctx.Done()
	monitor.Stop()

	select {
	case paused := <-pauseCh:
		if !paused {
			t.Error("expected pause=true")
		}
	default:
		t.Error("expected a pause signal with no sink configured")
	}
}
