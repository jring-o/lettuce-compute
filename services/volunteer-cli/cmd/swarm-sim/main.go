// Command swarm-sim is a volunteer-swarm load-test simulator for a Lettuce head.
//
// It spins up a configurable fleet of simulated volunteers, each speaking the
// real nonce-signed gRPC protocol via internal/client, against a running head.
// Two client profiles are supported:
//
//   - naive:    loops RequestWorkUnit (MaxAssignments=1) at a fixed interval,
//     ignoring the head's server-directed retry delay. Approximates a
//     pre-Layer-1 client and produces the maximum request noise.
//   - buffered: full Layer 1 — batches via MaxAssignments, maintains an
//     in-memory hours-sized client work buffer, obeys the server-directed retry
//     delay, makes zero RequestWorkUnit calls while the buffer is full, and
//     treats per-client rate limiting (ResourceExhausted) as a fixed local
//     backoff.
//   - overload: the Layer 2 saturation driver — a hot RequestWorkUnit loop with
//     NO inter-request delay that ignores the server-directed retry delay, so a
//     fleet larger than the head's dispatch ceiling pins the hot path. It is how
//     the simulator measures the single-head dispatch ceiling (peak dispatch/sec)
//     and proves the head sheds gracefully (ResourceExhausted) instead of
//     collapsing the DB pool (DeadlineExceeded / Unavailable).
//   - request-only: a supplementary HandOut-isolation probe — overload's hot
//     RequestWorkUnit loop with NO StartWork/SubmitResult, so it measures the pure
//     server-side HandOut path (the dispatch cache lock/scan ceiling and head-side
//     RequestWorkUnit p50/p99 vs concurrency) with zero write-path noise. It must
//     be run against a generously primed ready pool and a short duration, since it
//     reserves units without recycling them.
//
// It can seed a test leaf with work units over the head's HTTP admin API, runs
// the fleet for a fixed duration, and reports dispatch throughput (whole-run and
// per-second peak), per-RPC request rate, latency percentiles, the
// server-directed retry delay the head handed out, and the Layer 2 overload
// signals (ResourceExhausted shed-ratio and a pool-collapse flag).
//
// Liveness is deadline-based (no Heartbeat RPC); run-start is the explicit
// StartWork RPC, one call per unit executed (see metrics.go).
//
// The per-IP and per-pubkey gRPC rate limits are raised on the HEAD process for
// a ceiling/overload run (a single-host swarm shares one source IP, so the
// Layer 0 per-IP bucket would otherwise mask the Layer 2 admission cap). Set
// LETTUCE_GRPC_PER_IP_RATE_LIMIT / LETTUCE_GRPC_PER_PUBKEY_RATE_LIMIT very high
// (or 0 to leave defaults) when launching lettuce-server for the ceiling run.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type options struct {
	headGRPC string
	headHTTP string
	adminKey string

	volunteers int
	profile    string
	duration   time.Duration

	seedLeaf  string
	seedUnits int
	noSeed    bool
	creatorID string

	naiveInterval time.Duration
	bufferHours   float64
	maxAssign     int
	simFpops      float64
	maxCompute    time.Duration
	honorShed     bool

	report string
	quiet  bool
}

func parseFlags(args []string) (*options, error) {
	fs := flag.NewFlagSet("swarm-sim", flag.ContinueOnError)
	o := &options{}
	fs.StringVar(&o.headGRPC, "head-grpc", "127.0.0.1:9090", "head gRPC address(es) host:port — COMMA-SEPARATED for a multi-head scale-out run; the fleet is spread round-robin across the listed heads, so a load test can hit N replicas directly (no proxy) and the no-double-dispatch audit exercises every replica's refiller")
	fs.StringVar(&o.headHTTP, "head-http", "http://127.0.0.1:8080", "head HTTP base URL (for seeding; one head is enough since all replicas share one database)")
	fs.StringVar(&o.adminKey, "admin-key", os.Getenv("LETTUCE_ADMIN_API_KEY"), "admin API key for seeding (defaults to $LETTUCE_ADMIN_API_KEY)")

	fs.IntVar(&o.volunteers, "volunteers", 100, "number of simulated volunteers")
	fs.StringVar(&o.profile, "profile", "buffered", "client profile: naive | buffered | overload | request-only")
	fs.DurationVar(&o.duration, "duration", 60*time.Second, "how long to run the fleet")

	fs.StringVar(&o.seedLeaf, "seed-leaf", "swarm-test", "name/slug of the leaf to seed and target")
	fs.IntVar(&o.seedUnits, "seed-units", 10000, "number of work units to seed")
	fs.BoolVar(&o.noSeed, "no-seed", false, "skip seeding; the leaf must already exist (still resolved by name)")
	fs.StringVar(&o.creatorID, "creator-id", os.Getenv("SWARM_SIM_CREATOR_ID"), "users.id to own the seeded leaf (required when seeding a standalone head; defaults to $SWARM_SIM_CREATOR_ID)")

	fs.DurationVar(&o.naiveInterval, "naive-interval", 30*time.Second, "naive profile fixed request interval")
	fs.Float64Var(&o.bufferHours, "buffer-hours", 2.0, "buffered profile client work buffer size in hours")
	fs.IntVar(&o.maxAssign, "max-assignments", 8, "buffered profile batch size (server-capped)")
	fs.Float64Var(&o.simFpops, "sim-fpops", 1.0e9, "simulated benchmark FLOPS; pretend-compute = rsc_fpops_est/sim-fpops seconds")
	fs.DurationVar(&o.maxCompute, "max-compute", 2*time.Second, "cap on pretend-compute per unit so runs progress")
	fs.BoolVar(&o.honorShed, "honor-shed", false, "overload profile: back off on ResourceExhausted instead of immediately re-requesting (default false = maximal pressure for the ceiling run)")

	fs.StringVar(&o.report, "report", "text", "report format: text | json")
	fs.BoolVar(&o.quiet, "quiet", false, "suppress progress logging")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if o.profile != string(profileNaive) && o.profile != string(profileBuffered) &&
		o.profile != string(profileOverload) && o.profile != string(profileRequestOnly) {
		return nil, fmt.Errorf("invalid --profile %q: must be naive, buffered, overload, or request-only", o.profile)
	}
	if o.volunteers < 1 {
		return nil, fmt.Errorf("--volunteers must be >= 1")
	}
	if o.maxAssign < 1 {
		return nil, fmt.Errorf("--max-assignments must be >= 1")
	}
	if len(o.headGRPCTargets()) == 0 {
		return nil, fmt.Errorf("--head-grpc must list at least one host:port")
	}
	return o, nil
}

// headGRPCTargets splits the comma-separated --head-grpc value into one or more
// gRPC target addresses, trimming whitespace and dropping empty entries. A
// single address (the default) yields a one-element slice; multiple addresses
// drive a multi-head scale-out load test where the fleet is spread round-robin
// across the listed replicas (so every replica's dispatch refiller is exercised
// and the post-run no-double-dispatch DB audit covers all of them).
func (o *options) headGRPCTargets() []string {
	var targets []string
	for _, t := range strings.Split(o.headGRPC, ",") {
		if t = strings.TrimSpace(t); t != "" {
			targets = append(targets, t)
		}
	}
	return targets
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	level := slog.LevelInfo
	if o.quiet {
		level = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rep, err := runSimulation(ctx, o, logger)
	if err != nil {
		logger.Error("simulation failed", "error", err)
		os.Exit(1)
	}

	if err := writeReport(os.Stdout, rep, o.report); err != nil {
		logger.Error("failed to write report", "error", err)
		os.Exit(1)
	}
}

// runSimulation seeds (unless --no-seed), then runs the fleet of one profile
// for the configured duration and returns the report.
func runSimulation(ctx context.Context, o *options, logger *slog.Logger) (report, error) {
	// Resolve the target leaf (seed or look up by name).
	sd := newSeeder(o.headHTTP, o.adminKey, o.creatorID)
	var leafID string
	if o.noSeed {
		id, err := sd.leafByName(ctx, o.seedLeaf)
		if err != nil {
			return report{}, fmt.Errorf("resolve leaf %q: %w", o.seedLeaf, err)
		}
		if id == "" {
			return report{}, fmt.Errorf("leaf %q not found and --no-seed set", o.seedLeaf)
		}
		leafID = id
		logger.Info("using existing leaf", "leaf", o.seedLeaf, "leaf_id", leafID)
	} else {
		res, err := sd.Seed(ctx, o.seedLeaf, o.seedUnits)
		if err != nil {
			return report{}, fmt.Errorf("seeding: %w", err)
		}
		leafID = res.LeafID
		logger.Info("seeded leaf", "leaf", o.seedLeaf, "leaf_id", leafID, "units_created", res.UnitsCreated, "reused", res.Reused)
	}

	targets := o.headGRPCTargets()
	cfg := &simConfig{
		headGRPC:      targets[0],
		profile:       profileKind(o.profile),
		leafID:        leafID,
		naiveInterval: o.naiveInterval,
		bufferHours:   o.bufferHours,
		maxAssign:     int32(o.maxAssign),
		simFpops:      o.simFpops,
		maxCompute:    o.maxCompute,
		honorShed:     o.honorShed,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)), // quiet per-volunteer client
	}
	m := newMetrics()

	// Build and register the fleet.
	vols := make([]*simVolunteer, 0, o.volunteers)
	defer func() {
		for _, v := range vols {
			v.close()
		}
	}()

	if len(targets) > 1 {
		logger.Info("building fleet (multi-head scale-out)", "volunteers", o.volunteers, "profile", o.profile, "heads", targets)
	} else {
		logger.Info("building fleet", "volunteers", o.volunteers, "profile", o.profile)
	}
	regCtx, regCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer regCancel()
	var regWG sync.WaitGroup
	regErrs := make(chan error, o.volunteers)
	sem := make(chan struct{}, 64) // bound concurrent registrations
	for i := 0; i < o.volunteers; i++ {
		// Spread the fleet round-robin across the configured heads so a
		// multi-head run exercises every replica's dispatch refiller.
		target := targets[i%len(targets)]
		v, err := newSimVolunteer(cfg, m, int64(i)+1, target)
		if err != nil {
			return report{}, fmt.Errorf("creating volunteer %d: %w", i, err)
		}
		vols = append(vols, v)
		regWG.Add(1)
		go func(idx int, vol *simVolunteer) {
			defer regWG.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := vol.register(regCtx, fmt.Sprintf("swarm-sim-%d", idx)); err != nil {
				regErrs <- fmt.Errorf("register volunteer %d: %w", idx, err)
			}
		}(i, v)
	}
	regWG.Wait()
	close(regErrs)
	registered := 0
	for _, v := range vols {
		if v.id != "" {
			registered++
		}
	}
	if registered == 0 {
		// Surface the first error for diagnosis.
		for e := range regErrs {
			return report{}, fmt.Errorf("no volunteers registered: %w", e)
		}
		return report{}, fmt.Errorf("no volunteers registered: %w", errNotRegistered)
	}
	logger.Info("fleet registered", "registered", registered, "of", o.volunteers)

	// Run the fleet for the configured duration.
	runCtx, runCancel := context.WithTimeout(ctx, o.duration)
	defer runCancel()
	start := time.Now()

	// Sample the per-second dispatch rate so the report can show the peak
	// sustained dispatch/sec (the headline single-head ceiling number), which the
	// whole-run average understates as the fleet ramps and seeded units run out.
	var sampleWG sync.WaitGroup
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		m.samplePeak(runCtx)
	}()

	var runWG sync.WaitGroup
	for _, v := range vols {
		if v.id == "" {
			continue
		}
		runWG.Add(1)
		go func(vol *simVolunteer) {
			defer runWG.Done()
			vol.run(runCtx)
		}(v)
	}
	logger.Info("fleet running", "duration", o.duration)
	runWG.Wait()
	elapsed := time.Since(start)
	runCancel() // stop the peak sampler
	sampleWG.Wait()

	rep := m.buildReport(o.profile, registered, elapsed)
	return rep, nil
}

// writeReport renders the report as text or JSON.
func writeReport(w io.Writer, rep report, format string) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	fmt.Fprintf(w, "\n=== swarm-sim report (profile=%s) ===\n", rep.Profile)
	fmt.Fprintf(w, "volunteers:            %d\n", rep.Volunteers)
	fmt.Fprintf(w, "duration:              %.1fs\n", rep.DurationSeconds)
	fmt.Fprintf(w, "assignments dispatched: %d (%.1f/s avg, %.1f/s peak)\n", rep.AssignmentsDispatched, rep.DispatchPerSec, rep.PeakDispatchPerSec)
	fmt.Fprintf(w, "results submitted:     %d\n", rep.ResultsSubmitted)
	fmt.Fprintf(w, "RequestWorkUnit rate:  %.2f calls/s  (the request-noise metric)\n", rep.RequestRatePerSec)
	fmt.Fprintf(w, "retry_after handed out: avg=%.1fs max=%ds\n", rep.RetryAfterAvgSeconds, rep.RetryAfterMaxSeconds)
	fmt.Fprintf(w, "shed (ResourceExhausted): %.1f%% of RequestWorkUnit  (graceful backpressure)\n", rep.ShedRatio*100.0)
	collapse := "no"
	if rep.Collapsed {
		collapse = fmt.Sprintf("YES (%d RequestWorkUnit calls hit DeadlineExceeded/Unavailable)", rep.CollapseCount)
	}
	fmt.Fprintf(w, "DB-pool collapse:      %s\n", collapse)
	fmt.Fprintf(w, "\n%-18s %8s %8s %8s %8s %8s %8s %9s %9s %9s %9s\n", "rpc", "calls", "ok", "errors", "throttl", "collaps", "cancel", "p50ms", "p90ms", "p99ms", "maxms")
	for _, r := range rep.RPCs {
		fmt.Fprintf(w, "%-18s %8d %8d %8d %8d %8d %8d %9.2f %9.2f %9.2f %9.2f\n",
			r.RPC, r.Calls, r.OK, r.Errors, r.Throttled, r.Collapse, r.Canceled,
			r.Latency.P50, r.Latency.P90, r.Latency.P99, r.Latency.Max)
	}
	fmt.Fprintln(w)
	return nil
}
