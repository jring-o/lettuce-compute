package server

import (
	"net/http"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// This file is the head's whole Prometheus surface (BG-29). Design constraints:
//
//   - ONE process-wide registry, populated once at package init. Nothing is
//     registered at wiring time (NewRouter / NewGRPCServer / StartDispatchCache
//     are all called repeatedly by tests; per-call registration would panic on
//     the second duplicate register). Instance-bound sources (the pgx pool, the
//     dispatch cache) are therefore read through atomic pointers that the
//     wiring points merely STORE into — storing is idempotent, so tests and
//     production share the same registration path.
//   - Instrumentation lives INSIDE the existing observation points (the gRPC
//     loggingInterceptor, the HTTP requestLoggingMiddleware, the existing
//     dispatch shed sites) — no new middleware layers.
//   - The scrape endpoint is admin-gated and registered OUTSIDE /api/v1/ so the
//     shipped Caddy topology never proxies it; see the route registration in
//     router.go for the topology note.
var metricsRegistry = prometheus.NewRegistry()

var (
	// grpcRequestsTotal counts every unary gRPC request the head finishes, by
	// full method and final status code. code="ResourceExhausted" here is a
	// superset of load shedding (rate-limit refusals and the SaveCheckpoint
	// oversized-payload refusal use the same code); the accurate shed family is
	// lettuce_dispatch_shed_total below, which counts only the shed sites.
	grpcRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lettuce_grpc_requests_total",
		Help: "Unary gRPC requests handled, by full method and status code.",
	}, []string{"method", "code"})

	grpcRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "lettuce_grpc_request_duration_seconds",
		Help:    "Unary gRPC request duration in seconds, by full method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})

	// httpRequestsTotal deliberately carries NO path label: the REST routes
	// embed leaf/work-unit/volunteer UUIDs in the path, an unbounded label
	// cardinality that would grow the scrape without bound. Method + status is
	// enough for rate/error alerting; per-route latency questions go to the
	// request log, which has the full path.
	httpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lettuce_http_requests_total",
		Help: "HTTP requests handled, by method and response status code.",
	}, []string{"method", "status"})

	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "lettuce_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, by method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})

	// dispatchShedTotal counts load-shed refusals (ResourceExhausted) on the
	// volunteer request paths, by shed site. Sites:
	//
	//   request_work_ready_pool  RequestWorkUnit hard backstop — ready pool
	//                            empty AND the DB-admission semaphore saturated.
	//   request_work_identity    RequestWorkUnit identity resolve shed (cold
	//                            snapshot miss under a saturated admission gate).
	//   write_path_identity      resolveAuthedVolunteer shed — the bounded
	//                            identity read shared by the write RPCs.
	//   submit_result_db_slot    SubmitResult DB-admission slot unavailable.
	//   start_work_db_slot       StartWork DB-admission slot unavailable.
	//   abandon_db_slot          AbandonWorkUnit DB-admission slot unavailable.
	dispatchShedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lettuce_dispatch_shed_total",
		Help: "Load-shed refusals (ResourceExhausted) on the volunteer request paths, by shed site.",
	}, []string{"site"})
)

// httpMethodLabel bounds the http metric method label. r.Method is an
// attacker-controlled token (Go's HTTP server accepts any method string), so
// labeling it raw would let a client mint unbounded label pairs; anything
// outside the standard set folds to "OTHER". The gRPC method label needs no
// such fold: grpc-go answers unknown methods at the transport layer without
// running the unary interceptor chain, so only registered RPC names reach it.
func httpMethodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	}
	return "OTHER"
}

// metricsPool / metricsDispatchCache are the live instances the GaugeFuncs
// below read. Stored (not registered) by the wiring points: main.go stores the
// pool right after it connects; StartDispatchCache stores the cache it builds.
// A nil load — a process that never wired one, e.g. a router-only test — reads
// as 0, which keeps the gauge FAMILIES present on every scrape so dashboards
// and the regression test never see a missing family.
var (
	metricsPool          atomic.Pointer[pgxpool.Pool]
	metricsDispatchCache atomic.Pointer[dispatchCache]
)

// SetMetricsPool points the lettuce_db_pool_* gauges at the serving pool.
// Called once by main after the pool connects; safe to call again (tests) —
// the gauges simply follow the latest pool.
func SetMetricsPool(pool *pgxpool.Pool) {
	if pool != nil {
		metricsPool.Store(pool)
	}
}

// setMetricsDispatchCache points the lettuce_dispatch_* gauges at the running
// dispatch cache. Called by StartDispatchCache; latest cache wins (a process
// runs one cache; sequential test caches harmlessly supersede each other).
func setMetricsDispatchCache(c *dispatchCache) {
	if c != nil {
		metricsDispatchCache.Store(c)
	}
}

// poolGauge builds a GaugeFunc over the current metrics pool's Stat(). Each
// Stat() call takes a pool-internal snapshot; at scrape cadence that cost is
// negligible.
func poolGauge(name, help string, read func(*pgxpool.Stat) float64) prometheus.GaugeFunc {
	return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, func() float64 {
		pool := metricsPool.Load()
		if pool == nil {
			return 0
		}
		return read(pool.Stat())
	})
}

// dispatchGauge builds a GaugeFunc over the current dispatch cache. The read
// funcs below all take the cache mutex internally (readyLen and friends), so a
// scrape observes consistent lengths without holding any lock across families.
func dispatchGauge(name, help string, read func(*dispatchCache) float64) prometheus.GaugeFunc {
	return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, func() float64 {
		c := metricsDispatchCache.Load()
		if c == nil {
			return 0
		}
		return read(c)
	})
}

func init() {
	metricsRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),

		grpcRequestsTotal,
		grpcRequestDuration,
		httpRequestsTotal,
		httpRequestDuration,
		dispatchShedTotal,

		dispatchGauge("lettuce_dispatch_ready_pool_size",
			"Work-unit reservations staged in the in-memory dispatch ready pool.",
			func(c *dispatchCache) float64 { return float64(c.readyLen()) }),
		dispatchGauge("lettuce_dispatch_pending_reservation_writes",
			"Handed-out reservations queued for the async flush to Postgres.",
			func(c *dispatchCache) float64 { return float64(c.pendingWriteCount()) }),
		dispatchGauge("lettuce_dispatch_pending_spot_check_writes",
			"Spot-check markings queued for the async flush to Postgres.",
			func(c *dispatchCache) float64 { return float64(c.pendingSpotCheckCount()) }),

		poolGauge("lettuce_db_pool_acquired_conns",
			"Database pool connections currently acquired (in use).",
			func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) }),
		poolGauge("lettuce_db_pool_idle_conns",
			"Database pool connections currently idle.",
			func(s *pgxpool.Stat) float64 { return float64(s.IdleConns()) }),
		poolGauge("lettuce_db_pool_max_conns",
			"Database pool maximum connection count.",
			func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) }),
	)
}

// MetricsHandler returns the Prometheus scrape handler over the process-wide
// registry. Registered admin-gated at GET /metrics (outside /api/v1/ — see the
// topology note at the route registration in router.go).
func MetricsHandler() http.Handler {
	return promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{})
}
