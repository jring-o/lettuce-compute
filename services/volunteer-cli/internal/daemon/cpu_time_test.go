package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Tests for the CPU time limit (resource_limits.max_cpu_time_pct): running
// work is paused and resumed in turn so it runs that share of the time.

// TestCPUTimeCycle: the running and paused parts of a period. At least 10 s,
// and long enough at the ends of the range that neither part is under a
// second (pausing a container through its engine takes about a quarter of a
// second each way).
func TestCPUTimeCycle(t *testing.T) {
	for _, tc := range []struct {
		pct        int
		run, pause time.Duration
	}{
		{50, 5 * time.Second, 5 * time.Second},
		{10, 1 * time.Second, 9 * time.Second},
		{90, 9 * time.Second, 1 * time.Second},
		{5, 1 * time.Second, 19 * time.Second},
		{95, 19 * time.Second, 1 * time.Second},
		{25, 2500 * time.Millisecond, 7500 * time.Millisecond},
		{100, 0, 0},
		{0, 0, 0},
	} {
		run, pause := cpuTimeCycle(tc.pct)
		if run != tc.run || pause != tc.pause {
			t.Errorf("%d %%: runs %v, pauses %v; want %v and %v", tc.pct, run, pause, tc.run, tc.pause)
		}
	}
}

// countingHandle is a process handle that counts suspends and resumes and
// says whether the task is frozen; safe for the slot manager's parallel
// suspend and resume.
type countingHandle struct {
	mu                sync.Mutex
	suspends, resumes int
	frozen            bool
	frozenFor         time.Duration
	frozenSince       time.Time
}

func (h *countingHandle) Suspend() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.suspends++
	h.frozen = true
	h.frozenSince = time.Now()
	return nil
}

func (h *countingHandle) Resume() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resumes++
	if h.frozen {
		h.frozenFor += time.Since(h.frozenSince)
	}
	h.frozen = false
	return nil
}

func (h *countingHandle) PID() int { return 0 }

func (h *countingHandle) counts() (suspends, resumes int, frozen bool, frozenFor time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.suspends, h.resumes, h.frozen, h.frozenFor
}

// fastCPUTimeCycle shortens the limit's cycle for a test: a 400 ms period,
// re-read every 10 ms.
func fastCPUTimeCycle(t *testing.T) {
	t.Helper()
	period, phase, poll := cpuTimeMinPeriod, cpuTimeMinPhase, cpuTimePoll
	cpuTimeMinPeriod, cpuTimeMinPhase, cpuTimePoll = 400*time.Millisecond, 40*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { cpuTimeMinPeriod, cpuTimeMinPhase, cpuTimePoll = period, phase, poll })
}

// TestCPUTimeLimit_RunLoopPausesAndResumesWork drives the daemon's run loop
// with one task running and a 50 % limit. The task is paused and resumed in
// turn, frozen for about half the time; the daemon is never reported paused;
// the fetcher is not stopped for the pauses (it starts once); and lifting the
// limit to 100 leaves the task running.
func TestCPUTimeLimit_RunLoopPausesAndResumesWork(t *testing.T) {
	fastCPUTimeCycle(t)
	handle := &countingHandle{}
	running := make(chan struct{})
	var once sync.Once
	served := false
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			if served {
				return nil, status.Error(codes.NotFound, "no work")
			}
			served = true
			return &lettucev1.RequestWorkUnitResponse{Assignments: []*lettucev1.WorkUnitAssignment{{
				WorkUnitId: "dc5ff9da-f084-4dd7-86b8-e829669814f8", LeafId: "proj-1", Runtime: "native",
				InputData: []byte("input"), ExecutionSpec: &lettucev1.ExecutionSpec{},
			}}}, nil
		},
	}
	var d *Daemon
	mr := &mockRuntime{canHandle: true, executeFn: func(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
		sm := d.slotManager
		for _, s := range sm.slots {
			s.mu.Lock()
			mine := s.active && s.wu != nil && s.wu.ID == wu.ID
			s.mu.Unlock()
			if mine {
				sm.attachProcessHandle(s, handle)
			}
		}
		once.Do(func() { close(running) })
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	d = newTestDaemon(mc, mr)
	d.cfg.ResourceLimits.MaxCPUTimePct = 50
	logs := &lockedWriter{buf: &bytes.Buffer{}}
	d.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("the task never started")
	}
	start := time.Now()
	sawPaused := false
	for time.Since(start) < 1600*time.Millisecond {
		if d.IsPaused() || d.PauseReason() != "" {
			sawPaused = true
		}
		time.Sleep(5 * time.Millisecond)
	}
	suspends, resumes, _, frozenFor := handle.counts()
	if suspends < 3 || resumes < 2 {
		t.Errorf("over four 400 ms periods at 50 %% the task was suspended %d and resumed %d times, want at least 3 and 2", suspends, resumes)
	}
	if share := frozenFor.Seconds() / time.Since(start).Seconds(); share < 0.3 || share > 0.7 {
		t.Errorf("the task was frozen %.0f %% of the time, want about half", share*100)
	}
	if sawPaused {
		t.Error("the daemon was reported paused by the CPU time limit; the limit is a standing setting, not a pause")
	}

	lifted := *d.cfg
	lifted.ResourceLimits.MaxCPUTimePct = 100
	d.ApplyConfig(&lifted)
	time.Sleep(300 * time.Millisecond)
	s1, _, frozen, _ := handle.counts()
	time.Sleep(500 * time.Millisecond)
	s2, _, _, _ := handle.counts()
	if frozen || s2 != s1 {
		t.Errorf("with the limit lifted the task is frozen=%v and was suspended %d more times; want it running and left alone", frozen, s2-s1)
	}

	if starts := strings.Count(logs.String(), "fetcher: started"); starts != 1 {
		t.Errorf("the fetcher started %d times; the CPU time limit's pauses must not stop it", starts)
	}
}

// TestCPUTimeLimit_IsNotReportedAsAPause: the limit's paused part leaves the
// daemon reported active with no pause reason; a real pause beside it is
// reported as usual.
func TestCPUTimeLimit_IsNotReportedAsAPause(t *testing.T) {
	d := &Daemon{logger: newTestLogger()}
	d.setAutoPause(pauseSourceCPUTime, true)
	if d.IsPaused() || d.PauseReason() != "" {
		t.Errorf("CPU time limit alone: paused=%v reason=%q, want neither", d.IsPaused(), d.PauseReason())
	}
	d.setAutoPause(pauseSourceThermal, true)
	if !d.IsPaused() || d.PauseReason() != "thermal" {
		t.Errorf("thermal beside the limit: paused=%v reason=%q, want paused/thermal", d.IsPaused(), d.PauseReason())
	}
	d.setAutoPause(pauseSourceThermal, false)
	if !d.onlyCPUTimePaused() || d.pauseStopsFetching() {
		t.Error("with only the limit holding, the fetcher must keep running")
	}
}
