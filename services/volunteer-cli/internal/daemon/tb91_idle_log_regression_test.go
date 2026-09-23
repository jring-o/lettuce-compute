package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
)

// TB-91 regression tests: while nothing can change — a closed schedule
// window, a full buffer, every slot busy — the fetcher and the slot filler
// re-check once a second, and they used to write a Debug line on every
// re-check: 7,190 "shouldFetch returned false" lines in one two-hour schedule
// pause, 76,252 "work buffer full" and 153,718 "filling slots" / "no available
// slots" lines in 21 hours. The re-checks stay; each wait is now logged when
// it starts and when it ends.

// debugCapture returns a Debug-level JSON logger and the buffer it writes to.
// slog's handlers serialise their writes, so goroutines may share it; read the
// buffer only after they have stopped.
func debugCapture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// countMsg counts the log records in buf whose message is exactly msg and
// whose attributes include every key/value pair in attrs.
func countMsg(t *testing.T, buf *bytes.Buffer, msg string, attrs ...string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unparseable log line %q: %v", line, err)
		}
		if rec["msg"] != msg {
			continue
		}
		match := true
		for i := 0; i+1 < len(attrs); i += 2 {
			if rec[attrs[i]] != attrs[i+1] {
				match = false
			}
		}
		if match {
			n++
		}
	}
	return n
}

// TestTB91_FetcherLogsEachWaitOnce drives the fetcher loop through a closed
// fetch gate (three 1-second re-checks — the schedule-window case) and then a
// full buffer (thirty re-checks): one line entering each wait, one line when
// the gate opens, nothing per re-check.
func TestTB91_FetcherLogsEachWaitOnce(t *testing.T) {
	q := NewPreFetchQueue(8, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	var gateChecks, bufferChecks atomic.Int32
	f := quietFetcher(q, func() bool { return gateChecks.Add(1) > 3 }, time.Now)
	logger, buf := debugCapture()
	f.logger = logger
	f.workBufferFullFn = func() bool { bufferChecks.Add(1); return true }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	for bufferChecks.Load() < 30 {
		select {
		case <-ctx.Done():
			t.Fatalf("fetcher reached only %d gate and %d buffer checks", gateChecks.Load(), bufferChecks.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if got := countMsg(t, buf, "fetcher: shouldFetch returned false, not requesting"); got != 1 {
		t.Errorf("closed-gate lines = %d over 3 re-checks, want 1 (logged on entering the wait only); gate lines under the old wording: %d",
			got, countMsg(t, buf, "fetcher: shouldFetch returned false, waiting 1s"))
	}
	if got := countMsg(t, buf, "fetcher: wait over", "reason", "fetch_gate"); got != 1 {
		t.Errorf("gate-opened lines = %d, want 1 (the wait's end, with how long it lasted)", got)
	}
	if got := countMsg(t, buf, "fetcher: work buffer full, not requesting"); got != 1 {
		t.Errorf("buffer-full lines = %d over %d re-checks, want 1", got, bufferChecks.Load())
	}
}

// TestTB91_ScheduleClosedLoggedOnTransitions: shouldFetch runs once a second
// for the whole of a closed window; it says so when the window closes and when
// it reopens, not on every call.
func TestTB91_ScheduleClosedLoggedOnTransitions(t *testing.T) {
	logger, buf := debugCapture()
	idleSecs := 0
	sched := resource.NewScheduler(&config.Scheduling{Mode: "WHEN_IDLE", IdleThresholdMins: 5}, logger)
	sched.SetIdleFunc(func() (int, error) { return idleSecs, nil })
	d := &Daemon{scheduler: sched, logger: logger}

	for i := 0; i < 30; i++ {
		if d.shouldFetch() {
			t.Fatal("shouldFetch = true with the machine in use under WHEN_IDLE")
		}
	}
	if got := countMsg(t, buf, "shouldFetch: scheduler says don't run"); got != 1 {
		t.Fatalf("closed-schedule lines = %d over 30 calls, want 1", got)
	}

	idleSecs = 3600
	for i := 0; i < 30; i++ {
		if !d.shouldFetch() {
			t.Fatal("shouldFetch = false with the machine idle past the threshold")
		}
	}
	if got := countMsg(t, buf, "shouldFetch: scheduler allows running again"); got != 1 {
		t.Errorf("reopened lines = %d, want 1", got)
	}

	idleSecs = 0
	d.shouldFetch()
	d.shouldFetch()
	if got := countMsg(t, buf, "shouldFetch: scheduler says don't run"); got != 2 {
		t.Errorf("closed-schedule lines after a second closing = %d, want 2", got)
	}
}

// TestTB91_FillSlotsDeadEndsLoggedOnce: the slot filler's two dead ends — no
// free slot, and a free slot with nothing runnable — are each logged once
// while they persist, and again only after something started.
func TestTB91_FillSlotsDeadEndsLoggedOnce(t *testing.T) {
	logger, buf := debugCapture()
	d := &Daemon{logger: logger, slotManager: NewSlotManager(1, logger), prefetchQueue: NewPreFetchQueue(8, logger)}
	ctx := context.Background()

	taken := d.slotManager.AvailableSlotID() // every slot busy
	for i := 0; i < 30; i++ {
		d.fillSlots(ctx)
	}
	if got := countMsg(t, buf, "fillSlots: no available slots"); got != 1 {
		t.Errorf("no-available-slot lines = %d over 30 ticks, want 1", got)
	}

	d.slotManager.ReturnSlotID(taken) // a free slot, an empty buffer
	for i := 0; i < 30; i++ {
		d.fillSlots(ctx)
	}
	if got := countMsg(t, buf, "fillSlots: no runnable buffered unit"); got != 1 {
		t.Errorf("no-runnable-unit lines = %d over 30 ticks, want 1", got)
	}
}

// TestTB91_BlockedUnitReasonLoggedOnChange: a buffered unit waiting for memory
// gets its one Info line (TB-23) and then no per-tick Debug repeat of the
// same reason.
func TestTB91_BlockedUnitReasonLoggedOnChange(t *testing.T) {
	blockCh := make(chan struct{})
	defer close(blockCh)
	d := newBackfillTestDaemon(t, blockCh)
	logger, buf := debugCapture()
	d.logger = logger

	d.prefetchQueue.Push(mkBufferedItem("grep-buffered", 6000, blockCh))
	for i := 0; i < 30; i++ {
		d.fillSlots(context.Background())
	}

	if got := countMsg(t, buf, "buffered work unit waiting for capacity"); got != 1 {
		t.Errorf("Info capacity-wait lines = %d, want 1 (TB-23)", got)
	}
	if got := countMsg(t, buf, "buffered work unit still waiting for capacity"); got != 0 {
		t.Errorf("Debug repeats of an unchanged reason = %d over 30 ticks, want 0", got)
	}
	if got := countMsg(t, buf, "fillSlots: no runnable buffered unit"); got != 1 {
		t.Errorf("no-runnable-unit lines = %d over 30 ticks, want 1", got)
	}
}

// TestTB91_FreeRAMFigureIsNotANewReason: a unit that fits the budget but not
// the free RAM is refused with the current free figure in the reason, which
// moves on nearly every tick; that is the same wait, not a new one.
func TestTB91_FreeRAMFigureIsNotANewReason(t *testing.T) {
	blockCh := make(chan struct{})
	defer close(blockCh)
	d := newBackfillTestDaemon(t, blockCh)
	logger, buf := debugCapture()
	d.logger = logger

	free := 300
	prev := freeSystemMemoryMB
	freeSystemMemoryMB = func() (int, bool) { free++; return free, true }
	t.Cleanup(func() { freeSystemMemoryMB = prev })

	d.prefetchQueue.Push(mkBufferedItem("beyblade-buffered", 768, blockCh))
	for i := 0; i < 30; i++ {
		d.fillSlots(context.Background())
	}

	if got := countMsg(t, buf, "buffered work unit waiting for capacity"); got != 1 {
		t.Fatalf("Info capacity-wait lines = %d, want 1; log:\n%s", got, buf.String())
	}
	if got := countMsg(t, buf, "buffered work unit still waiting for capacity"); got != 0 {
		t.Errorf("Debug lines for a moving free-RAM figure = %d over 30 ticks, want 0", got)
	}
}

// TestTB91_MainLoopTickQuietWhenNothingChanged runs the real main loop on an
// idle machine (no work served, nothing buffered) across three 1-second ticks:
// the "filling slots" line is logged once, not once per tick.
func TestTB91_MainLoopTickQuietWhenNothingChanged(t *testing.T) {
	d := newTestDaemon(&mockClient{}, &mockRuntime{canHandle: true})
	logger, buf := debugCapture()
	d.logger = logger

	ctx, cancel := context.WithTimeout(context.Background(), 3200*time.Millisecond)
	defer cancel()
	runErr := d.Run(ctx)

	if got := countMsg(t, buf, "daemon: filling slots"); got != 1 {
		t.Errorf("filling-slots lines = %d over ~3 idle ticks, want 1 (Run returned %v)", got, runErr)
	}
}
