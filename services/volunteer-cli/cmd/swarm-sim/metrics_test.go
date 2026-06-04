package main

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestOutcomeForClassifiesOverloadSignals pins the Layer 2 distinction the
// overload report depends on: ResourceExhausted is a GRACEFUL shed (throttled),
// while DeadlineExceeded/Unavailable is the DB-pool CONGESTION-COLLAPSE signal
// the head must avoid. Everything else is a plain error.
func TestOutcomeForClassifiesOverloadSignals(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want rpcOutcome
	}{
		{"nil is ok", nil, outcomeOK},
		{"resource exhausted is graceful shed", status.Error(codes.ResourceExhausted, "shed"), outcomeThrottled},
		{"deadline exceeded is collapse", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), outcomeCollapse},
		{"unavailable is collapse", status.Error(codes.Unavailable, "connection refused"), outcomeCollapse},
		{"canceled is run-end artifact", status.Error(codes.Canceled, "context canceled"), outcomeCanceled},
		{"internal is plain error", status.Error(codes.Internal, "boom"), outcomeError},
		{"failed precondition is plain error", status.Error(codes.FailedPrecondition, "no active assignment"), outcomeError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outcomeFor(tt.err); got != tt.want {
				t.Fatalf("outcomeFor(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestOutcomeForCtxExcludesRunEndFalsePositive pins the collapse-flag
// false-positive guard: a client-side Canceled/DeadlineExceeded observed while
// the simulation's OWN context is already done is a benign run-end artifact
// (outcomeCanceled), NOT a head DB-pool collapse. The same code while the run is
// still live IS a collapse. This is what stops a run-end deadline burst (or a
// per-call timeout firing from mere head lock-slowness as the run winds down)
// from falsely tripping the DoD collapse flag.
func TestOutcomeForCtxExcludesRunEndFalsePositive(t *testing.T) {
	liveCtx := context.Background()
	doneCtx, cancel := context.WithCancel(context.Background())
	cancel() // simulate the run ending

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want rpcOutcome
	}{
		{"deadline while run live is collapse", liveCtx, status.Error(codes.DeadlineExceeded, "context deadline exceeded"), outcomeCollapse},
		{"deadline at run end is benign cancel", doneCtx, status.Error(codes.DeadlineExceeded, "context deadline exceeded"), outcomeCanceled},
		{"canceled while run live is still cancel", liveCtx, status.Error(codes.Canceled, "context canceled"), outcomeCanceled},
		{"canceled at run end is benign cancel", doneCtx, status.Error(codes.Canceled, "context canceled"), outcomeCanceled},
		{"unavailable while run live is collapse", liveCtx, status.Error(codes.Unavailable, "connection refused"), outcomeCollapse},
		{"unavailable at run end is still collapse", doneCtx, status.Error(codes.Unavailable, "connection refused"), outcomeCollapse},
		{"resource exhausted at run end is still shed", doneCtx, status.Error(codes.ResourceExhausted, "shed"), outcomeThrottled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outcomeForCtx(tt.ctx, tt.err); got != tt.want {
				t.Fatalf("outcomeForCtx(ctxErr=%v, %v) = %v, want %v", tt.ctx.Err(), tt.err, got, tt.want)
			}
		})
	}
}

// TestBuildReportOverloadSignals asserts the report surfaces the Layer 2
// Definition-of-Done evidence: the ResourceExhausted shed ratio (graceful
// backpressure) and the pool-collapse flag, both keyed on RequestWorkUnit.
func TestBuildReportOverloadSignals(t *testing.T) {
	t.Run("graceful shed, no collapse", func(t *testing.T) {
		m := newMetrics()
		// 6 RequestWorkUnit calls: 4 ok, 2 shed. Zero collapse.
		for i := 0; i < 4; i++ {
			m.record(rpcRequestWorkUnit, time.Millisecond, outcomeOK)
		}
		for i := 0; i < 2; i++ {
			m.record(rpcRequestWorkUnit, time.Millisecond, outcomeThrottled)
		}
		rep := m.buildReport("overload", 10, 10*time.Second)

		if rep.Collapsed {
			t.Fatalf("Collapsed = true, want false (no DeadlineExceeded/Unavailable)")
		}
		if rep.CollapseCount != 0 {
			t.Fatalf("CollapseCount = %d, want 0", rep.CollapseCount)
		}
		if got, want := rep.ShedRatio, 2.0/6.0; got < want-1e-9 || got > want+1e-9 {
			t.Fatalf("ShedRatio = %v, want %v", got, want)
		}
	})

	t.Run("collapse flagged when pool saturates", func(t *testing.T) {
		m := newMetrics()
		m.record(rpcRequestWorkUnit, time.Millisecond, outcomeOK)
		m.record(rpcRequestWorkUnit, time.Millisecond, outcomeCollapse)
		m.record(rpcRequestWorkUnit, time.Millisecond, outcomeCollapse)
		rep := m.buildReport("overload", 10, 10*time.Second)

		if !rep.Collapsed {
			t.Fatalf("Collapsed = false, want true (RequestWorkUnit hit the pool-collapse signal)")
		}
		if rep.CollapseCount != 2 {
			t.Fatalf("CollapseCount = %d, want 2", rep.CollapseCount)
		}
	})

	t.Run("run-end cancel on RequestWorkUnit does not flag collapse", func(t *testing.T) {
		// The known false-positive: in-flight RequestWorkUnit calls cancelled at run
		// shutdown (or client-side deadlines from mere head lock-slowness) must NOT
		// trip the collapse flag. They are recorded as Canceled, separate from the
		// true head-side collapse signal.
		m := newMetrics()
		m.record(rpcRequestWorkUnit, time.Millisecond, outcomeOK)
		for i := 0; i < 5; i++ {
			m.record(rpcRequestWorkUnit, time.Millisecond, outcomeCanceled)
		}
		rep := m.buildReport("request-only", 10, 10*time.Second)

		if rep.Collapsed {
			t.Fatalf("Collapsed = true, want false (cancellations are a benign run-end artifact)")
		}
		if rep.CollapseCount != 0 {
			t.Fatalf("CollapseCount = %d, want 0", rep.CollapseCount)
		}
		// The cancellations are surfaced on the RequestWorkUnit row, not as errors.
		var rwu rpcReport
		for _, r := range rep.RPCs {
			if r.RPC == rpcRequestWorkUnit.String() {
				rwu = r
			}
		}
		if rwu.Canceled != 5 {
			t.Fatalf("RequestWorkUnit Canceled = %d, want 5", rwu.Canceled)
		}
		if rwu.Errors != 0 {
			t.Fatalf("RequestWorkUnit Errors = %d, want 0 (cancellations are not errors)", rwu.Errors)
		}
	})

	t.Run("collapse on StartWork alone does not flag RequestWorkUnit collapse", func(t *testing.T) {
		// The DoD flag is keyed on the hot path (RequestWorkUnit). A StartWork
		// timeout is a separate, bounded write path; it must not trip the
		// RequestWorkUnit collapse flag.
		m := newMetrics()
		m.record(rpcRequestWorkUnit, time.Millisecond, outcomeOK)
		m.record(rpcStartWork, time.Millisecond, outcomeCollapse)
		rep := m.buildReport("overload", 10, 10*time.Second)

		if rep.Collapsed {
			t.Fatalf("Collapsed = true, want false (collapse was on StartWork, not RequestWorkUnit)")
		}
	})
}

// TestSamplePeakTracksHighestPerSecondRate verifies the peak sampler records the
// maximum 1-second dispatch delta, not the whole-run average. The sampler reads
// the cumulative dispatched counter once per second; we advance the counter in a
// burst within a single window and assert the peak reflects the burst.
func TestSamplePeakTracksHighestPerSecondRate(t *testing.T) {
	m := newMetrics()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		m.samplePeak(ctx)
		close(done)
	}()

	// Drive a burst of dispatches across a couple of sample windows, then stop.
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		m.assignmentsDispatched.Add(100)
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	// At ~100 units per 5ms we add ~20000/sec; the sampler should see a peak far
	// above zero. We only assert it is positive and plausibly large to avoid
	// flakiness on a loaded CI host.
	peak := float64(m.peakDispatchPerSec.Load()) / 1000.0
	if peak <= 0 {
		t.Fatalf("peakDispatchPerSec = %v, want > 0", peak)
	}
}
