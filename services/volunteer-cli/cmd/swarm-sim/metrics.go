package main

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// rpcKind labels the RPCs the simulator measures separately. Lease renewal for
// buffered units rides on the Heartbeat RPC (per-unit PREPARING heartbeats), so
// the Heartbeat line includes both running-task liveness and lease-renewal
// traffic. The Layer 1 Definition of Done asserts a full-buffer volunteer makes
// ZERO RequestWorkUnit calls, so renewal traffic is never folded into the
// RequestWorkUnit count.
type rpcKind int

const (
	rpcRequestWorkUnit rpcKind = iota
	rpcHeartbeat
	rpcSubmitResult
	rpcRegister
	numRPCKinds
)

func (k rpcKind) String() string {
	switch k {
	case rpcRequestWorkUnit:
		return "RequestWorkUnit"
	case rpcHeartbeat:
		return "Heartbeat"
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
	errs      atomic.Int64 // non-OK (excludes ResourceExhausted)
	throttled atomic.Int64 // codes.ResourceExhausted (per-client rate limiting shed)

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
	outcomeThrottled
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
}

func newMetrics() *metrics { return &metrics{} }

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
	CallsPerSec float64        `json:"calls_per_sec"`
	Latency     latencySummary `json:"latency"`
}

// report is the full machine-readable result of one profile run.
type report struct {
	Profile               string      `json:"profile"`
	Volunteers            int         `json:"volunteers"`
	DurationSeconds       float64     `json:"duration_seconds"`
	AssignmentsDispatched int64       `json:"assignments_dispatched"`
	DispatchPerSec        float64     `json:"dispatch_per_sec"`
	ResultsSubmitted      int64       `json:"results_submitted"`
	RequestRatePerSec     float64     `json:"request_work_unit_per_sec"`
	RetryAfterAvgSeconds  float64     `json:"retry_after_avg_seconds"`
	RetryAfterMaxSeconds  int64       `json:"retry_after_max_seconds"`
	RPCs                  []rpcReport `json:"rpcs"`
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
			CallsPerSec: float64(calls) / secs,
			Latency:     st.summary(),
		})
	}

	var retryAvg float64
	if c := m.retryAfterCount.Load(); c > 0 {
		retryAvg = float64(m.retryAfterSum.Load()) / float64(c)
	}

	dispatched := m.assignmentsDispatched.Load()
	return report{
		Profile:               profile,
		Volunteers:            volunteers,
		DurationSeconds:       secs,
		AssignmentsDispatched: dispatched,
		DispatchPerSec:        float64(dispatched) / secs,
		ResultsSubmitted:      m.resultsSubmitted.Load(),
		RequestRatePerSec:     float64(m.stats[rpcRequestWorkUnit].calls.Load()) / secs,
		RetryAfterAvgSeconds:  retryAvg,
		RetryAfterMaxSeconds:  m.retryAfterMax.Load(),
		RPCs:                  rpcs,
	}
}
