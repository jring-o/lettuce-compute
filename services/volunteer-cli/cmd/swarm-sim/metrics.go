package main

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// rpcKind labels the RPCs the simulator measures separately. With Layer 2 the
// per-task Heartbeat RPC is gone: liveness is deadline-based and run-start is the
// explicit StartWork RPC (one call per unit actually executed, NOT per request),
// so there is no per-unit lease-renewal traffic to fold in. The Layer 1
// Definition of Done still holds — a full-buffer volunteer makes ZERO
// RequestWorkUnit calls — and StartWork is the only new steady-state RPC.
type rpcKind int

const (
	rpcRequestWorkUnit rpcKind = iota
	rpcStartWork
	rpcSubmitResult
	rpcRegister
	numRPCKinds
)

func (k rpcKind) String() string {
	switch k {
	case rpcRequestWorkUnit:
		return "RequestWorkUnit"
	case rpcStartWork:
		return "StartWork"
	case rpcSubmitResult:
		return "SubmitResult"
	case rpcRegister:
		return "RegisterVolunteer"
	default:
		return "unknown"
	}
}

// rpcStat accumulates outcome counts and a bounded latency reservoir for a
// single RPC kind. Counters are atomic so volunteer goroutines record without
// contending on the mutex; the mutex only guards the latency reservoir.
type rpcStat struct {
	calls     atomic.Int64 // total attempts
	ok        atomic.Int64 // status OK
	errs      atomic.Int64 // non-OK (excludes ResourceExhausted, collapse, and run-end cancel)
	throttled atomic.Int64 // codes.ResourceExhausted (graceful shed: admission cap / rate limit)
	collapse  atomic.Int64 // TRUE DB-pool collapse: a head-side deadline/Unavailable while the run is live
	canceled  atomic.Int64 // run-end client-side cancel/deadline (benign; NOT a head collapse)

	mu        sync.Mutex
	latencies []time.Duration // reservoir of sampled latencies (capped)
	seen      int64           // total samples offered (for reservoir sampling)
	rng       uint64          // xorshift state for reservoir replacement
}

const latencyReservoirCap = 20000

func (s *rpcStat) record(d time.Duration, outcome rpcOutcome) {
	s.calls.Add(1)
	switch outcome {
	case outcomeOK:
		s.ok.Add(1)
	case outcomeThrottled:
		s.throttled.Add(1)
	case outcomeCollapse:
		s.collapse.Add(1)
	case outcomeCanceled:
		s.canceled.Add(1)
	default:
		s.errs.Add(1)
	}

	s.mu.Lock()
	s.seen++
	if len(s.latencies) < latencyReservoirCap {
		s.latencies = append(s.latencies, d)
	} else {
		// Reservoir sampling: replace a random slot with probability cap/seen.
		s.rng ^= s.rng << 13
		s.rng ^= s.rng >> 7
		s.rng ^= s.rng << 17
		idx := int(s.rng % uint64(s.seen))
		if idx < latencyReservoirCap {
			s.latencies[idx] = d
		}
	}
	s.mu.Unlock()
}

type rpcOutcome int

const (
	outcomeOK rpcOutcome = iota
	outcomeError
	// outcomeThrottled is codes.ResourceExhausted: the GRACEFUL shed signal
	// (Layer 2 admission cap or Layer 0 per-client rate limiting). The volunteer
	// treats it as a fixed local backoff; it is the DESIRED overload behavior.
	outcomeThrottled
	// outcomeCollapse is the TRUE DB-pool CONGESTION-COLLAPSE signal Layer 2 must
	// eliminate: a head-side deadline (the head's per-request DB touch timed out
	// with "context deadline exceeded") or codes.Unavailable, observed while the
	// simulation is still live. If the head shed gracefully under overload this
	// stays zero; a non-zero collapse count on RequestWorkUnit means the pool
	// saturated instead of the head returning ResourceExhausted.
	outcomeCollapse
	// outcomeCanceled is a BENIGN run-end artifact, NOT a head collapse: an
	// in-flight RPC that failed with codes.Canceled / codes.DeadlineExceeded
	// because the simulation's own (duration-bounded) context expired or was
	// cancelled out from under it at run shutdown. It is tracked separately so it
	// does NOT inflate the collapse flag. This is the known FALSE-POSITIVE the
	// collapse signal must exclude: a client-side gRPC deadline (run-end, or the
	// per-call timeout firing because HandOut was merely slow under lock
	// contention) is NOT evidence the head's DB pool collapsed.
	outcomeCanceled
)

// latencyPercentiles returns p50/p90/p99/max in milliseconds (sorted copy).
type latencySummary struct {
	P50 float64 `json:"p50_ms"`
	P90 float64 `json:"p90_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
}

func (s *rpcStat) summary() latencySummary {
	s.mu.Lock()
	cp := make([]time.Duration, len(s.latencies))
	copy(cp, s.latencies)
	s.mu.Unlock()
	if len(cp) == 0 {
		return latencySummary{}
	}
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
	pct := func(p float64) time.Duration {
		idx := int(p * float64(len(cp)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(cp) {
			idx = len(cp) - 1
		}
		return cp[idx]
	}
	return latencySummary{
		P50: ms(pct(0.50)),
		P90: ms(pct(0.90)),
		P99: ms(pct(0.99)),
		Max: ms(cp[len(cp)-1]),
	}
}

// metrics is the shared, fleet-wide aggregator. One instance is shared by every
// volunteer goroutine.
type metrics struct {
	stats [numRPCKinds]rpcStat

	assignmentsDispatched atomic.Int64 // units actually returned to the fleet
	resultsSubmitted      atomic.Int64 // units the fleet reported done

	// retryAfterSum / retryAfterCount track the server-directed retry delay the
	// head handed out so the report can show the observed average (and max).
	retryAfterSum   atomic.Int64
	retryAfterCount atomic.Int64
	retryAfterMax   atomic.Int64

	// peakDispatchPerSec is the highest 1-second dispatch rate observed by the
	// sampler goroutine (see samplePeak). The whole-run DispatchPerSec average
	// understates the true single-head ceiling because the fleet ramps and units
	// run out; the per-second peak is the headline "how fast can one head hand
	// out work" number the Layer 2 Definition of Done compares against ~240/s.
	peakDispatchPerSec atomic.Int64 // stored as units/sec * 1000 (millis) for precision
}

func newMetrics() *metrics { return &metrics{} }

// samplePeak runs until ctx is cancelled, sampling the cumulative dispatched
// counter every second and tracking the highest per-second delta as the peak
// sustained dispatch rate. It is the headline single-head ceiling metric: a
// whole-run average is diluted by fleet ramp-up and by the seeded units running
// out, whereas the per-second peak captures the saturated dispatch rate.
func (m *metrics) samplePeak(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := m.assignmentsDispatched.Load()
	lastT := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cur := m.assignmentsDispatched.Load()
			elapsed := now.Sub(lastT).Seconds()
			if elapsed > 0 {
				rate := float64(cur-last) / elapsed
				millis := int64(rate * 1000.0)
				for {
					prev := m.peakDispatchPerSec.Load()
					if millis <= prev {
						break
					}
					if m.peakDispatchPerSec.CompareAndSwap(prev, millis) {
						break
					}
				}
			}
			last = cur
			lastT = now
		}
	}
}

func (m *metrics) record(kind rpcKind, d time.Duration, outcome rpcOutcome) {
	m.stats[kind].record(d, outcome)
}

func (m *metrics) recordRetryAfter(seconds int32) {
	m.retryAfterSum.Add(int64(seconds))
	m.retryAfterCount.Add(1)
	for {
		cur := m.retryAfterMax.Load()
		if int64(seconds) <= cur {
			break
		}
		if m.retryAfterMax.CompareAndSwap(cur, int64(seconds)) {
			break
		}
	}
}

// rpcReport is the per-RPC slice of the final report.
type rpcReport struct {
	RPC         string         `json:"rpc"`
	Calls       int64          `json:"calls"`
	OK          int64          `json:"ok"`
	Errors      int64          `json:"errors"`
	Throttled   int64          `json:"throttled"`
	Collapse    int64          `json:"collapse"`
	Canceled    int64          `json:"canceled"` // benign run-end client cancel/deadline (NOT a collapse)
	CallsPerSec float64        `json:"calls_per_sec"`
	Latency     latencySummary `json:"latency"`
}

// report is the full machine-readable result of one profile run.
type report struct {
	Profile               string  `json:"profile"`
	Volunteers            int     `json:"volunteers"`
	DurationSeconds       float64 `json:"duration_seconds"`
	AssignmentsDispatched int64   `json:"assignments_dispatched"`
	DispatchPerSec        float64 `json:"dispatch_per_sec"`
	PeakDispatchPerSec    float64 `json:"peak_dispatch_per_sec"`
	ResultsSubmitted      int64   `json:"results_submitted"`
	RequestRatePerSec     float64 `json:"request_work_unit_per_sec"`
	RetryAfterAvgSeconds  float64 `json:"retry_after_avg_seconds"`
	RetryAfterMaxSeconds  int64   `json:"retry_after_max_seconds"`

	// --- Layer 2 overload signals (DoD: shed gracefully, never collapse) ---

	// ShedRatio is the fraction of RequestWorkUnit calls the head shed with
	// codes.ResourceExhausted (graceful backpressure). Non-zero under overload is
	// the DESIRED behavior.
	ShedRatio float64 `json:"shed_ratio"`
	// CollapseCount is the number of RequestWorkUnit calls that failed with the
	// TRUE DB-pool congestion-collapse signal (a head-side deadline / Unavailable
	// observed while the run was live). It EXCLUDES benign run-end client
	// cancellations and client-side deadlines from mere lock-slowness (those are
	// counted as Canceled on the per-RPC rows). Layer 2 must keep this at ZERO
	// under overload.
	CollapseCount int64 `json:"collapse_count"`
	// Collapsed is the DoD pass/fail flag: true iff any RequestWorkUnit call hit
	// the TRUE pool-collapse signal (run-end cancellations and client-side
	// deadlines from lock-slowness do not trip it). The Layer 2 overload run
	// asserts this is false.
	Collapsed bool `json:"collapsed"`

	RPCs []rpcReport `json:"rpcs"`
}

func (m *metrics) buildReport(profile string, volunteers int, elapsed time.Duration) report {
	secs := elapsed.Seconds()
	if secs <= 0 {
		secs = 1
	}
	rpcs := make([]rpcReport, 0, numRPCKinds)
	for k := rpcKind(0); k < numRPCKinds; k++ {
		st := &m.stats[k]
		calls := st.calls.Load()
		rpcs = append(rpcs, rpcReport{
			RPC:         k.String(),
			Calls:       calls,
			OK:          st.ok.Load(),
			Errors:      st.errs.Load(),
			Throttled:   st.throttled.Load(),
			Collapse:    st.collapse.Load(),
			Canceled:    st.canceled.Load(),
			CallsPerSec: float64(calls) / secs,
			Latency:     st.summary(),
		})
	}

	var retryAvg float64
	if c := m.retryAfterCount.Load(); c > 0 {
		retryAvg = float64(m.retryAfterSum.Load()) / float64(c)
	}

	// Overload signals are keyed on RequestWorkUnit: that is the hot path Layer 2
	// moves off Postgres, so its shed/collapse counts are the DoD evidence.
	rwu := &m.stats[rpcRequestWorkUnit]
	rwuCalls := rwu.calls.Load()
	rwuShed := rwu.throttled.Load()
	rwuCollapse := rwu.collapse.Load()
	var shedRatio float64
	if rwuCalls > 0 {
		shedRatio = float64(rwuShed) / float64(rwuCalls)
	}

	dispatched := m.assignmentsDispatched.Load()
	return report{
		Profile:               profile,
		Volunteers:            volunteers,
		DurationSeconds:       secs,
		AssignmentsDispatched: dispatched,
		DispatchPerSec:        float64(dispatched) / secs,
		PeakDispatchPerSec:    float64(m.peakDispatchPerSec.Load()) / 1000.0,
		ResultsSubmitted:      m.resultsSubmitted.Load(),
		RequestRatePerSec:     float64(rwuCalls) / secs,
		RetryAfterAvgSeconds:  retryAvg,
		RetryAfterMaxSeconds:  m.retryAfterMax.Load(),
		ShedRatio:             shedRatio,
		CollapseCount:         rwuCollapse,
		Collapsed:             rwuCollapse > 0,
		RPCs:                  rpcs,
	}
}
