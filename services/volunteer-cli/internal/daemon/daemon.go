package daemon

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// WorkClient defines the gRPC operations the daemon needs from the server.
type WorkClient interface {
	RequestWorkUnit(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error)
	SubmitResult(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error)
	// StartWork run-starts a buffered reserved unit (QUEUED -> ASSIGNED) when a slot
	// begins executing it. It replaces the removed per-task Heartbeat RPC; liveness
	// is deadline-based.
	StartWork(ctx context.Context, req *lettucev1.StartWorkRequest) (*lettucev1.StartWorkResponse, error)
	SaveCheckpoint(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error)
	GetCheckpoint(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error)
	GetHeadInfo(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error)
	AbandonWorkUnit(ctx context.Context, req *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error)
	// GetMyContribution returns the authenticated account's own credit (across all
	// leaves and machines) from this head. Used by the management bridge to surface
	// authoritative head credit instead of the local history.jsonl proxy.
	GetMyContribution(ctx context.Context, req *lettucev1.GetMyContributionRequest) (*lettucev1.GetMyContributionResponse, error)
	Close() error
}

// registerClient is the subset of a head client the work-path self-heal needs. The
// production client (*client.Client) implements it; keeping it a separate narrow
// interface (rather than widening WorkClient) means the many WorkClient mocks need not
// grow a RegisterVolunteer method just to compile.
type registerClient interface {
	RegisterVolunteer(ctx context.Context, req *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error)
}

// reRegisterHost re-registers this machine against one head after a host-unknown
// work-path refusal (BG-25 self-heal). It registers with an EMPTY host id so the head
// mints a fresh one (discarding the refused id), persists exactly what the head returns
// (empty => run host-less), and returns the new id. Re-registration of the existing
// account key is an UPDATE branch, so it never trips registration proof-of-work — no
// solver needed here. It is wired into the Fetcher as reRegisterFn.
func (d *Daemon) reRegisterHost(ctx context.Context, head *ServerConnection) (string, error) {
	rc, ok := head.Client.(registerClient)
	if !ok {
		return "", fmt.Errorf("head client does not support re-registration")
	}
	hostname, _ := os.Hostname()
	resp, err := rc.RegisterVolunteer(ctx, &lettucev1.RegisterVolunteerRequest{
		PublicKey:         d.pubKey,
		DisplayName:       hostname,
		Hardware:          d.cachedHW,
		AvailableRuntimes: d.advertisedRuntimes(),
		SchedulingMode:    d.cfg.Scheduling.Mode,
		HostId:            "", // discard the refused id: empty => the head mints a fresh one
	})
	if err != nil {
		return "", err
	}
	if d.hostIDStore != nil {
		key := head.Config.GRPCAddress
		var perr error
		if resp.HostId == "" {
			perr = d.hostIDStore.Delete(key)
		} else {
			perr = d.hostIDStore.Set(key, resp.HostId)
		}
		if perr != nil {
			d.logger.Warn("daemon: failed to persist re-issued host id", "server", head.Name, "error", perr)
		}
	}
	return resp.HostId, nil
}

// advertisedRuntimes returns the UPPERCASE runtime enum names this daemon can actually
// run, derived from the live registry (registry Name()s are lowercase). It mirrors the
// list start.go advertises at initial registration so a self-heal re-register presents
// the same capabilities.
func (d *Daemon) advertisedRuntimes() []string {
	if d.runtimeRegistry == nil {
		return nil
	}
	names := d.runtimeRegistry.AvailableRuntimes()
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, strings.ToUpper(n))
	}
	sort.Strings(out)
	return out
}

// Daemon manages the volunteer compute loop using concurrent execution slots
// and a pre-fetch queue.
type Daemon struct {
	cfg     *config.Config
	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey
	// hostIDStore persists head-issued per-machine host ids keyed by head gRPC address
	// (BG-25). The work-path self-heal (reRegisterHost) updates it after a host-unknown
	// refusal. Per-head ids themselves live on each ServerConnection.HostID; this store
	// is the durable backing. May be nil in tests that never exercise self-heal.
	hostIDStore     *identity.HostIDStore
	multiClient     *MultiServerClient
	runtimeRegistry *RuntimeRegistry
	logger          *slog.Logger

	// Resource management
	limiter   resource.Limiter
	scheduler *resource.Scheduler

	// Thermal monitoring
	thermalMonitor *runtime.ThermalMonitor
	thermalPauseCh chan bool

	// Backoff configuration (overridable for tests)
	initialBackoff time.Duration
	maxBackoff     time.Duration

	// Cached hardware capabilities (detected once at startup)
	cachedHW *lettucev1.HardwareCapabilities

	// Podman machine lifecycle (Windows/macOS). Whether this process started the
	// machine (and so may stop it at shutdown, PB-27) is tracked by the manager
	// itself — see runtime.PodmanMachineManager.StartedByThisProcess.
	machineManager *runtime.PodmanMachineManager

	// Leaf discovery and weighted scheduling.
	leafCache        *LeafCache
	weightedSelector *WeightedSelector

	// Per-leaf execution-failure breaker (TB-10): counts consecutive local
	// failures per leaf so the fetcher stops re-requesting a leaf that keeps
	// failing on THIS machine, and so `status`/`leafs list` can say it is
	// happening instead of leaving the volunteer to infer they are receiving no
	// work. See leaf_failures.go.
	leafFailures *leafFailureTracker

	// Concurrent execution (replaces serial currentWU/pipelining).
	slotManager   *SlotManager
	prefetchQueue *PreFetchQueue
	fetcher       *Fetcher

	// State
	mu       sync.Mutex
	stopping bool
	running  bool
	paused   bool

	// User-initiated pause (separate from resource/thermal auto-pause).
	userPaused  bool
	pauseReason string // "user", "thermal", "scheduled", ""
	userPauseCh chan bool

	// Daemon start time for uptime calculation.
	startedAt time.Time

	// Process group: ensures child processes are killed when daemon exits.
	// Windows: Job Object with KILL_ON_JOB_CLOSE. Unix: setpgid + tracked pgids.
	processGroup ProcessGroup
	runCancel    context.CancelFunc // cancels all slot contexts on Stop()

	// CPU benchmark score for runtime estimation.
	benchmarkFPOPS float64
	dcfTracker     *DCFTracker

	// Per-leaf per-unit seconds observed on the most recent ARRIVED batch (TB-34):
	// the mean estSecondsForUnit over the units a batch actually delivered. The
	// batch-size estimate (leafEstSeconds) takes the max of this and the leaf-level
	// figure, so one 60× over-ask corrects itself on the very next round instead of
	// waiting on the DCF — which learns only from COMPLETIONS and so never hears
	// about units that keep being returned un-run (the self-sustaining loop).
	arrivalEstMu  sync.Mutex
	arrivalEstSec map[string]float64

	// Fetch-gate hysteresis (TB-34): once the buffer fills to the hours target,
	// fetching stays closed until the REMAINING buffered work drains below the
	// low-water mark (workBufferLowWaterFrac), instead of reopening one unit under
	// the target line. Without the latch, fetch-gate and arrival-acceptance shared
	// one threshold, so a buffer hovering at the line requested-and-refused
	// indefinitely (the observed 170–283 give-backs per host per day).
	bufferFilledMu sync.Mutex
	bufferFilled   bool

	// Image-presence gate: caches, per enabled leaf image, whether that image is
	// already pulled, so the disk gate requires only workspace headroom for a
	// cached-image rerun — for THAT leaf, not for every leaf (TB-24; the old
	// single-bool cache let any cached image relax the gate for images that still
	// needed a full pull).
	imgCacheMu      sync.Mutex
	imgCacheChecked time.Time
	imgCached       map[string]bool

	// Lettuce's own measured disk usage (TB-24): the data-dir tree plus the
	// cached container images of the wanted repositories. Feeds the
	// usage + need <= allowance budget check and the management metrics; cached
	// because the data-dir walk is not free.
	diskUsageMu      sync.Mutex
	diskUsageChecked time.Time
	diskUsageMB      int64
	diskUsageOK      bool

	// Image-store path cache (TODO #31): the container backend's image/layer
	// store filesystem (Docker DockerRootDir / Podman graphroot), which the disk
	// gate checks in addition to the data dir because the image does not land
	// under the data dir. Cached for imageCacheCheckTTL to avoid an /info probe on
	// every fetch-loop iteration.
	imgStoreMu      sync.Mutex
	imgStoreChecked time.Time
	imgStorePaths   []string
	imgStoreKnown   bool

	// Disk-gate warning state: surfaces the otherwise-silent "no free disk, so
	// not fetching" stall as a one-time WARN, reset once the gate clears, so a
	// volunteer that's idle on disk space says so instead of only at Debug.
	diskGateMu     sync.Mutex
	diskGateWarned bool
	// unstattableStores records image-store paths whose free space cannot be
	// determined from this host (see noteUnstattableImageStore), so the
	// informational log fires once per path per daemon run. Guarded by diskGateMu.
	unstattableStores map[string]bool

	// Slot-starvation visibility (TB-32): an idle slot beside a buffer of
	// inadmissible units lived entirely at INFO/DEBUG — testers measured
	// slot-hours of idleness from log archaeology. trackSlotStarvation stamps
	// when the state began and throttles the WARN.
	slotStarveMu       sync.Mutex
	slotStarvedSince   time.Time
	slotStarveWarnedAt time.Time

	// Volunteer-facing notices (see notices.go): the log's WARN/escalation
	// sites mirrored into a ring the management API serves, so a desktop
	// client can show what the log would otherwise say only to a reader of
	// the log. Shared with the thermal monitor and the fetcher.
	notices *NoticeLog

	// Per-head version and update-required state (see head_status.go),
	// keyed by gRPC address. Seeded at start-up from registration, then kept
	// current by the fetcher on every work request to the head.
	headStatus *HeadStatusTracker

	// clientVersion is this build's version string, reported on the
	// management API's status so a client can compare it with each head's.
	clientVersion string
}

// DaemonConfig holds all dependencies for creating a Daemon.
type DaemonConfig struct {
	Config  *config.Config
	PubKey  ed25519.PublicKey
	PrivKey ed25519.PrivateKey
	// HostIDStore persists head-issued per-machine host ids keyed by head gRPC address
	// (BG-25). Threaded in so the work-path self-heal can persist a re-minted id. May
	// be nil (self-heal then re-registers but persists nothing).
	HostIDStore *identity.HostIDStore

	// Multi-server: preferred way to configure servers.
	Servers []*ServerConnection

	// Hardware is the machine's already-detected capabilities (client.DetectHardware),
	// advertised to heads and consulted for GPU budgets. Start-up detects once and
	// passes the result here; when nil the daemon detects for itself (tests, and
	// any caller without a prior detection).
	Hardware *lettucev1.HardwareCapabilities
	// ClientVersion is this build's version string (the value `--version`
	// prints), surfaced on GET /api/v1/status as client_version.
	ClientVersion string
	// Notices and HeadStatus are created by the daemon when nil. Start-up
	// passes its own so notices and head state observed BEFORE the daemon
	// exists — a too-old rejection at registration, a head's reported version
	// — are carried into the running daemon rather than lost.
	Notices    *NoticeLog
	HeadStatus *HeadStatusTracker

	// Legacy single-server fields (used if Servers is empty).
	Client      WorkClient
	VolunteerID string

	Runtime         runtime.Runtime               // Legacy: wraps in single-entry registry if RuntimeRegistry is nil
	RuntimeRegistry *RuntimeRegistry              // Preferred: explicit registry with multiple runtimes
	MachineManager  *runtime.PodmanMachineManager // optional: Podman machine lifecycle
	Logger          *slog.Logger
	Limiter         resource.Limiter    // optional, auto-detected if nil
	Scheduler       *resource.Scheduler // optional, created from config if nil
}

// NewDaemon creates a new daemon with the provided configuration.
func NewDaemon(cfg DaemonConfig) *Daemon {
	limiter := cfg.Limiter
	if limiter == nil {
		limiter = resource.NewLimiter(cfg.Logger)
	}

	scheduler := cfg.Scheduler
	if scheduler == nil {
		scheduler = resource.NewScheduler(&cfg.Config.Scheduling, cfg.Logger)
	}

	// Build runtime registry.
	registry := cfg.RuntimeRegistry
	if registry == nil {
		registry = NewRuntimeRegistry()
		if cfg.Runtime != nil {
			registry.Register(cfg.Runtime)
		}
	}

	// Create process group for child process lifecycle management.
	// On Windows: Job Object with KILL_ON_JOB_CLOSE ensures children die with daemon.
	// On Unix: setpgid + tracked pgids for explicit cleanup.
	pg, pgErr := NewProcessGroup(cfg.Logger)
	if pgErr != nil {
		cfg.Logger.Warn("failed to create process group, child processes may outlive daemon", "error", pgErr)
	}

	// Wire resource limiter and process group hooks into any NativeRuntime. The
	// limiter is enforced against a PER-UNIT copy of the configured limits whose
	// memory ceiling is BookedMemMB(declared, configured) — the same clamped number
	// admission books — so native enforcement matches admission instead of always
	// capping at the whole configured budget (BG-16).
	limits := &cfg.Config.ResourceLimits
	perUnitLimits := func(declaredMemMB int) *config.ResourceLimits {
		l := *limits
		l.MaxMemoryMB = runtime.BookedMemMB(declaredMemMB, limits.MaxMemoryMB)
		return &l
	}
	for _, rt := range registry.runtimes {
		if nr, ok := rt.(*runtime.NativeRuntime); ok {
			nr.SetCommandModifier(func(cmd *exec.Cmd, declaredMemMB int) error {
				if pg != nil {
					pg.ConfigureCommand(cmd)
				}
				return limiter.Apply(cmd, perUnitLimits(declaredMemMB))
			})
			nr.SetProcessNotifier(func(pid int, declaredMemMB int) (func(), error) {
				if pg != nil {
					if err := pg.Add(pid); err != nil {
						cfg.Logger.Warn("failed to add process to group", "pid", pid, "error", err)
					}
				}
				return limiter.Enforce(pid, perUnitLimits(declaredMemMB))
			})
		}
	}

	// Create thermal monitor.
	thermalPauseCh := make(chan bool, 1)
	thermalCfg := runtime.ThermalConfig{
		Enabled:             cfg.Config.Thermal.Enabled,
		CPUPauseThresholdC:  cfg.Config.Thermal.CPUPauseThresholdC,
		CPUResumeThresholdC: cfg.Config.Thermal.CPUResumeThresholdC,
		GPUPauseThresholdC:  cfg.Config.Thermal.GPUPauseThresholdC,
		GPUResumeThresholdC: cfg.Config.Thermal.GPUResumeThresholdC,
		PollIntervalSeconds: cfg.Config.Thermal.PollIntervalSeconds,
		MaxThrottleMinutes:  cfg.Config.Thermal.MaxThrottleMinutes,
	}
	thermalMonitor := runtime.NewThermalMonitor(thermalCfg, thermalPauseCh, cfg.Logger)

	// Notices and per-head state: adopt start-up's instances when given (they
	// may already hold a registration-time rejection), else start empty.
	notices := cfg.Notices
	if notices == nil {
		notices = NewNoticeLog()
	}
	headStatus := cfg.HeadStatus
	if headStatus == nil {
		headStatus = NewHeadStatusTracker()
	}
	// The thermal monitor emits its own throttle notices (it alone knows the
	// temperatures and the cause); it only needs somewhere to put them.
	thermalMonitor.SetNoticeSink(notices)

	// Build multi-server client. Support both new Servers field and legacy
	// Client/VolunteerID for backward compatibility with existing tests.
	servers := cfg.Servers
	if len(servers) == 0 && cfg.Client != nil {
		// Legacy single-Client constructor. This branch is TEST-ONLY: production always
		// passes Servers (built from config, each carrying the volunteer's per-head runtime
		// trust). There is no per-head config to consult here, so the synthesized head trusts
		// all runtime kinds — otherwise the fetcher's per-head trust gate would abandon every
		// unit and no execution-flow test could run.
		servers = []*ServerConnection{{
			Client:      cfg.Client,
			VolunteerID: cfg.VolunteerID,
			Name:        "default",
			Available:   true,
			Config:      config.ServerConfig{TrustedRuntimes: []string{"CONTAINER", "NATIVE"}},
		}}
	}
	multiClient := NewMultiServerClient(servers, cfg.Logger)

	// Use the hardware start-up already detected; detect here only when the
	// caller did not. Detection launches vendor tools and reads platform
	// registries, and a second probe per start is exactly what once raised a
	// second UAC prompt on Windows.
	hw := cfg.Hardware
	if hw == nil {
		hw = client.DetectHardware(cfg.Config)
	}

	// Run or load CPU benchmark for runtime estimation.
	var benchFPOPS float64
	cpuModel := ""
	if hw != nil {
		cpuModel = hw.CpuModel
	}
	benchFPOPS, benchErr := EnsureBenchmark(cfg.Config.DataDir, cpuModel)
	if benchErr != nil {
		cfg.Logger.Warn("failed to save benchmark result", "error", benchErr)
	}
	if benchFPOPS > 0 && hw != nil {
		hw.BenchmarkFpops = benchFPOPS
	}

	// Load duration correction factors.
	dcfTracker := LoadDCFTracker(cfg.Config.DataDir)

	// Create leaf cache (5 min refresh) and weighted selector.
	leafCache := NewLeafCache(5*time.Minute, cfg.Logger)
	ws := NewWeightedSelector()

	// Initialize head weights from config.
	headWeights := make(map[string]int, len(cfg.Config.Servers))
	for _, srv := range cfg.Config.Servers {
		w := srv.Weight
		if w <= 0 {
			w = 100
		}
		headWeights[srv.DisplayName()] = w
	}
	ws.SetHeadWeights(headWeights)

	return &Daemon{
		cfg:              cfg.Config,
		pubKey:           cfg.PubKey,
		privKey:          cfg.PrivKey,
		hostIDStore:      cfg.HostIDStore,
		multiClient:      multiClient,
		runtimeRegistry:  registry,
		machineManager:   cfg.MachineManager,
		logger:           cfg.Logger,
		limiter:          limiter,
		scheduler:        scheduler,
		thermalMonitor:   thermalMonitor,
		thermalPauseCh:   thermalPauseCh,
		initialBackoff:   1 * time.Second,
		maxBackoff:       30 * time.Second,
		cachedHW:         hw,
		leafCache:        leafCache,
		weightedSelector: ws,
		leafFailures:     newLeafFailureTracker(time.Now),
		userPauseCh:      make(chan bool, 1),
		processGroup:     pg,
		benchmarkFPOPS:   benchFPOPS,
		dcfTracker:       dcfTracker,
		arrivalEstSec:    make(map[string]float64),
		notices:          notices,
		headStatus:       headStatus,
		clientVersion:    cfg.ClientVersion,
	}
}

// Notices returns the daemon's volunteer-facing notice ring.
func (d *Daemon) Notices() *NoticeLog {
	return d.notices
}

// HeadStatus returns the per-head version and update-required tracker.
func (d *Daemon) HeadStatus() *HeadStatusTracker {
	return d.headStatus
}

// ClientVersion returns this build's version string as configured at start-up.
func (d *Daemon) ClientVersion() string {
	return d.clientVersion
}

// Run starts the coordinator loop. It blocks until ctx is cancelled or Stop() is called.
// On context cancellation, it waits for all active slots to finish before returning.
func (d *Daemon) Run(ctx context.Context) error {
	// Wrap context so Stop() can cancel all slot execution.
	ctx, runCancel := context.WithCancel(ctx)
	d.mu.Lock()
	d.running = true
	d.startedAt = time.Now()
	d.runCancel = runCancel
	d.mu.Unlock()
	defer func() {
		runCancel()
		d.mu.Lock()
		d.running = false
		d.runCancel = nil
		d.mu.Unlock()
	}()

	maxSlots := d.cfg.MaxConcurrentTasks
	if maxSlots <= 0 {
		maxSlots = 1
	}

	serverNames := make([]string, len(d.multiClient.Servers()))
	for i, s := range d.multiClient.Servers() {
		serverNames[i] = s.Name
	}
	d.logger.Info("daemon started",
		"servers", serverNames,
		"server_count", len(serverNames),
		"max_concurrent_tasks", maxSlots,
		"runtimes", d.runtimeRegistry.AvailableRuntimes(),
		"scheduling_mode", d.cfg.Scheduling.Mode,
	)
	for _, warning := range d.cfg.LeafConfigWarnings() {
		d.logger.Warn("leaf-filter config", "warning", warning)
	}
	for _, warning := range d.cfg.DeprecatedKeyWarnings() {
		d.logger.Warn("config", "warning", warning)
	}

	// Initialize leaf cache from all servers.
	d.leafCache.RefreshAll(ctx, d.multiClient.Servers())
	d.initializeWeights()

	// Give the container runtime the keep-set for its stale-image reaper: every
	// image an enabled leaf wants cached, so a re-pushed mutable tag's superseded
	// copies are reclaimed without ever removing an image another active leaf
	// still needs (#60).
	if cr, ok := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime); ok && cr != nil {
		cr.SetWantedImages(d.allEnabledImageRefs)
		// The startup image reap runs later (after the resumers), once stranded
		// containers have been cleared so superseded images are actually reclaimable.
	}

	// Readiness banner: now that runtimes are registered and the leaf list is
	// fetched, report what this volunteer can actually run and warn loudly about
	// the silent "connected but will never get work" cases (no matching runtime,
	// disk already below the allowance).
	d.logReadiness()

	// Write PID file.
	if err := WritePID(d.cfg.DataDir); err != nil {
		d.logger.Warn("failed to write PID file", "error", err)
	}
	defer RemovePID(d.cfg.DataDir)

	// Initialize slot manager and the client work buffer. The buffer's "fullness"
	// is governed by work_buffer_hours (see workBufferFull); the queue's hard
	// maxDepth is only a safety ceiling on descriptor count, so it is set well
	// above the hours target to avoid being the binding constraint.
	d.slotManager = NewSlotManager(maxSlots, d.logger)
	d.prefetchQueue = NewPreFetchQueue(workBufferQueueDepth, d.logger)

	// Start resource monitor goroutine.
	pauseCh := make(chan bool, 1)
	monitorCtx, monitorCancel := context.WithCancel(ctx)
	defer monitorCancel()
	monitor := resource.NewMonitor(d.limiter, d.scheduler, &d.cfg.ResourceLimits, d.cfg.DataDir, d.logger)
	go monitor.Run(monitorCtx, pauseCh)

	// Start thermal monitor.
	if d.thermalMonitor != nil {
		d.thermalMonitor.Start(monitorCtx)
		defer d.thermalMonitor.Stop()
	}

	// Resume any tasks preserved from the previous daemon session: first the running
	// tasks (back into slots), then the buffered prefetch units (back into the queue),
	// so the volunteer reports its full held set on its first request and the head
	// keeps the matching reservations instead of stranding them.
	d.resumePersistedTasks(ctx)
	d.resumePrefetchBuffer(ctx)

	// Reap orphaned per-unit work dirs left by a previous unclean exit (#58). MUST be
	// after both resumers so a dir about to be re-attached is never deleted; the owned set
	// is exactly the active slots' + restored buffer's work dirs at this point.
	d.gcOrphanedWorkDirs()

	// Reclaim container-image disk left by a previous session, in two ordered steps.
	// MUST run after the resumers (so a just-resumed unit is in the owned set and its
	// freshly-created container is spared) and off the startup path. First remove this
	// volunteer's own stranded work-unit containers (crash/dirty-shutdown leftovers) —
	// they pin the leaf image, so the non-force image reaper cannot reclaim it — THEN
	// sweep superseded cached images now that they are unpinned. Without this, a stale
	// image left while the wanted image is already cached lingers indefinitely, since
	// the per-pull reaper only fires on a fresh pull (confirmed in the field on
	// v0.8.11/v0.8.12). Best-effort; never blocks startup.
	if cr, ok := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime); ok && cr != nil {
		owned := d.ownedWorkUnitIDs()
		go func() {
			cr.ReapStrandedContainers(ctx, owned)
			cr.ReapStaleImages(ctx)
		}()
	}

	// Start the pending-result retry worker. It sweeps once now (recovering any
	// results stranded by a previous run's submission failure) then periodically.
	go d.runPendingResultRetry(ctx)

	// Start fetcher goroutine. We track the cancel func in a variable
	// so it can be replaced when the fetcher is restarted after pause.
	var fetcherCancel context.CancelFunc
	startFetcher := func() {
		var fetcherCtx context.Context
		fetcherCtx, fetcherCancel = context.WithCancel(ctx)
		d.fetcher = NewFetcher(d, d.prefetchQueue, d.weightedSelector, d.leafCache)
		go d.fetcher.Run(fetcherCtx)
	}
	startFetcher()

	// Buffer maintenance is tied to the RUN context, not the fetcher's (TB-19).
	//
	// A thermal or resource pause cancels the fetcher outright and the coordinator
	// loop below blocks in waitForResume, so neither one sweeps the buffer while
	// paused — and a pause is precisely when a buffered unit sits long enough to
	// outlive its head-side reservation. Observed: 69 min and 2 h 28 min thermal
	// freezes on two tester hosts, over which nothing would have aged the buffer
	// out. This goroutine keeps running across pauses and outlives every fetcher
	// restart.
	go d.runBufferMaintenance(ctx)

	// Coordinator cleanup on exit.
	defer func() {
		if fetcherCancel != nil {
			fetcherCancel()
		}

		// Signal shutdown so slots preserve work directories instead of cleaning up.
		d.slotManager.SetShuttingDown()

		// Wait for all active slots to finish.
		d.slotManager.StopAll()

		// Collect preserved tasks (work dirs kept for resumption).
		preserved := d.slotManager.GetPreservedTasks()

		// Drain any remaining results and submit them.
		// Use Background context since the original ctx may be cancelled.
		submitCtx := context.Background()
		for {
			result, ok := d.slotManager.TryGetResult()
			if !ok {
				break
			}
			d.handleSlotResult(submitCtx, result)
		}

		// Return remaining buffered (un-run, reserved) units to the head so they
		// aren't held until their reservation window lapses, then clean up.
		// abandonItem uses a detached context since the run context is already
		// cancelled. See item 4.
		for _, item := range d.prefetchQueue.Clear() {
			d.abandonItem(item, "volunteer shutdown")
			if item.Runtime != nil && item.Prep != nil {
				item.Runtime.Cleanup(item.Prep)
			}
		}
		// Buffered units were just returned to the head and their work dirs cleaned, so
		// the persisted buffer is stale — clear it to make the next startup's resume a
		// no-op (these units must NOT be re-enqueued; they are no longer ours).
		ClearBufferState(d.cfg.DataDir)

		// Save preserved tasks to disk for next startup.
		if len(preserved) > 0 {
			if err := SaveActiveState(d.cfg.DataDir, preserved); err != nil {
				d.logger.Error("failed to save active tasks for resume", "error", err)
			} else {
				d.logger.Info("saved active tasks for resume", "count", len(preserved))
			}
		} else {
			ClearActiveState(d.cfg.DataDir)
		}

		// Kill any remaining child processes via the process group.
		if d.processGroup != nil {
			d.processGroup.Terminate()
		}

		d.slotManager = nil
		d.prefetchQueue = nil
		d.fetcher = nil
	}()

	// Coordinator tick for queue maintenance.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		// Check if we should stop.
		d.mu.Lock()
		stopping := d.stopping
		d.mu.Unlock()
		if stopping {
			d.logger.Info("daemon stopping (stop requested)")
			return nil
		}

		select {
		case <-ctx.Done():
			d.logger.Info("daemon stopping (context cancelled)")
			return nil
		default:
		}

		// Check for pause/resume signals from resource, thermal, and user.
		d.checkPauseSignals(pauseCh)

		// If paused, stop execution but keep fetcher running for user pause.
		d.mu.Lock()
		systemPaused := d.paused
		userPaused := d.userPaused
		d.mu.Unlock()
		if systemPaused || userPaused {
			if systemPaused {
				// Thermal/resource pause: stop everything including fetcher.
				fetcherCancel()
			}

			// Suspend all running processes (freeze in place).
			d.slotManager.SuspendAll()
			d.logger.Info("suspended all active processes",
				"reason", d.pauseReason,
				"active_slots", d.slotManager.ActiveCount(),
			)

			// Wait for resume.
			if !d.waitForResume(ctx, pauseCh) {
				// Shutting down — resume processes so they can be cleaned up. Mark the
				// shutdown BEFORE resuming: the executors' own cancel paths are already
				// racing to unpause-and-stop their containers, so a resume that finds
				// its container gone (or no longer paused) is cleanup succeeding, not
				// failing, and ResumeAll must not WARN about it (TB-29).
				d.slotManager.SetShuttingDown()
				d.slotManager.ResumeAll()
				return nil // context cancelled
			}

			// Resume all suspended processes.
			d.slotManager.ResumeAll()
			d.logger.Info("resumed all active processes")

			// Restart fetcher if it was stopped (system pause).
			if systemPaused {
				startFetcher()
			}
			continue
		}

		// Check scheduler before filling slots. When inactive this also suspends any
		// already-running tasks (e.g. ones resumed from a previous session) for the
		// duration of the off-schedule window instead of letting them run through it.
		if !d.waitForScheduleActive(ctx) {
			return nil
		}

		// Process completed slots.
		for {
			result, ok := d.slotManager.TryGetResult()
			if !ok {
				break
			}
			d.logger.Info("daemon: slot completed", "work_unit_id", result.WU.ID, "leaf_id", result.WU.LeafID, "server", result.Conn.Name, "volunteer_id", result.Conn.VolunteerID, "success", result.Err == nil)
			d.handleSlotResult(ctx, result)
			d.persistActiveTasks()
		}

		// Fill available slots from the pre-fetch queue.
		d.logger.Debug("daemon: filling slots", "active_slots", d.slotManager.ActiveCount(), "queue_len", d.prefetchQueue.Len())
		d.fillSlots(ctx)

		// Surface a slot that stays idle with only inadmissible work buffered
		// (TB-32) — throttled WARN, otherwise the condition lives at DEBUG.
		d.trackSlotStarvation()

		// Keep the persisted prefetch buffer current so a non-graceful exit can resume
		// it. Runs every iteration — the loop wakes on a queue push (Notify) and on slot
		// completion, so additions and pops are captured promptly.
		d.persistPrefetchBuffer()

		// Wait for next event: slot completion, queue item, pause signal, or tick.
		select {
		case <-ctx.Done():
			d.logger.Info("daemon stopping (context cancelled)")
			return nil
		case result := <-d.slotManager.results:
			d.logger.Info("daemon: slot result received", "work_unit_id", result.WU.ID)
			d.handleSlotResult(ctx, result)
			d.persistActiveTasks()
			// Immediately try to fill the freed slot.
			d.fillSlots(ctx)
		case <-d.prefetchQueue.Notify():
			d.logger.Debug("daemon: prefetch queue notification, filling slots")
			// New item in queue — try to fill slots.
			d.fillSlots(ctx)
		case shouldPause := <-pauseCh:
			d.mu.Lock()
			d.paused = shouldPause
			if shouldPause {
				d.pauseReason = "scheduled"
			}
			d.mu.Unlock()
		case shouldPause := <-d.thermalPauseCh:
			d.mu.Lock()
			d.paused = shouldPause
			if shouldPause {
				d.pauseReason = "thermal"
			}
			d.mu.Unlock()
		case shouldPause := <-d.userPauseCh:
			d.mu.Lock()
			d.userPaused = shouldPause
			d.mu.Unlock()
		case <-ticker.C:
			// Periodic maintenance — drop expiring items, refresh weights.
		}
	}
}

// handleSlotResult submits the result from a completed slot to the server.
func (d *Daemon) handleSlotResult(ctx context.Context, result SlotResult) {
	wu := result.WU
	conn := result.Conn

	if result.Err != nil {
		if errors.Is(result.Err, context.Canceled) {
			// Execution was cancelled because the daemon is shutting down or a stop
			// was requested (Stop() cancels the run context). The slot has already
			// preserved the work dir + checkpoint for resume — this is the normal
			// graceful-stop path, not a compute failure, so it must not surface at
			// ERROR (it would trip alarm-on-ERROR monitoring on every clean shutdown).
			d.logger.Info("slot execution cancelled (shutdown/stop); work preserved for resume",
				"work_unit_id", wu.ID,
				"slot", result.SlotID,
			)
			return
		}
		if errors.Is(result.Err, errStartWorkDropped) {
			// The slot never executed: run-start reported the unit is no longer ours,
			// so the head already re-staged it via its lapsed-reservation / deadline
			// sweep. Abandoning would earn a FailedPrecondition (no live copy for this
			// volunteer) — nothing to submit and nothing to abandon, just drop it.
			d.logger.Info("slot dropped at run-start; unit no longer reserved for this volunteer, head re-staged it",
				"work_unit_id", wu.ID,
				"slot", result.SlotID,
			)
			return
		}
		d.logger.Error("slot execution failed",
			"work_unit_id", wu.ID,
			"slot", result.SlotID,
			"error", result.Err,
			"log_tail", logTailOrNone(result.FailureLogTail),
		)
		d.noteLeafFailure(wu, result.Err.Error())
		d.abandonUnit(wu, conn, withLogTail("execution failed: "+result.Err.Error(), result.FailureLogTail))
		return
	}

	if result.Result == nil {
		d.logger.Error("slot returned nil result", "work_unit_id", wu.ID, "slot", result.SlotID)
		return
	}

	if result.Result.ExitCode != 0 {
		reason := fmt.Sprintf("non-zero exit code %d", result.Result.ExitCode)
		d.logger.Error("slot execution non-zero exit",
			"work_unit_id", wu.ID,
			"slot", result.SlotID,
			"exit_code", result.Result.ExitCode,
			"log_tail", logTailOrNone(result.FailureLogTail),
		)
		d.noteLeafFailure(wu, reason)
		// Send the log tail to the head too. Without it the abandon reason was
		// just the exit code, so an operator whose artifact is broken for a whole
		// class of volunteer machines had nothing to diagnose from and the
		// volunteer's own log was the only copy (TB-10).
		d.abandonUnit(wu, conn, withLogTail(reason, result.FailureLogTail))
		return
	}

	// The unit ran to a clean exit: this leaf works on this machine, so clear any
	// failure streak the breaker was accumulating for it. Success is judged on the
	// exit code, not on the submission below — whether the head is reachable says
	// nothing about whether the artifact runs here.
	d.noteLeafSuccess(wu)

	// Persist result JSON for replay if the leaf has a viz bundle. The bundle
	// the runtime extracted into the work directory is already gone (the slot
	// removed the work directory on completion), so SaveResult re-extracts a
	// persistent copy from the cached tarball, identified by the spec's URL
	// and checksum.
	if result.VizBundlePath != "" && len(result.Result.OutputData) > 0 {
		leafName, leafSlug := d.resolveLeafInfo(wu.LeafID)
		maxBytes := int64(d.cfg.ResultCacheMaxMB) * 1024 * 1024
		viz := VizBundleSource{
			URL:      wu.ExecutionSpec.Binaries["viz"],
			Checksum: strings.ToLower(wu.ExecutionSpec.BinaryChecksums["viz"]),
		}
		if err := SaveResult(d.cfg.DataDir, wu.ID, leafName, leafSlug, conn.Name, result.Result.OutputData, viz, maxBytes); err != nil {
			d.logger.Warn("failed to persist result for replay",
				"work_unit_id", wu.ID,
				"error", err,
			)
		}
	}

	// Submit result to the server.
	submitReq := d.buildSubmitRequest(wu, result.Result, conn)
	submitResp, err := conn.Client.SubmitResult(ctx, submitReq)
	if err != nil {
		// Don't drop a finished result on a network blip — persist it and let the
		// retry worker resubmit (now and on future daemon starts). See item 6.
		d.logger.Error("submit result failed; persisting for retry",
			"work_unit_id", wu.ID,
			"slot", result.SlotID,
			"error", err,
		)
		d.persistPendingResult(wu, result, conn, submitReq)
		return
	}

	d.logger.Info("result submitted",
		"work_unit_id", wu.ID,
		"leaf_id", wu.LeafID,
		"result_id", submitResp.ResultId,
		"accepted", submitResp.Accepted,
		"server", conn.Name,
		"volunteer_id", conn.VolunteerID,
		"slot", result.SlotID,
	)

	// wallClock is elapsed time INCLUDING any period the unit spent suspended —
	// frozen by a thermal throttle, the resource monitor, or a schedule window
	// closing. activeSeconds subtracts that: the time it was actually computing.
	wallClock := result.Result.Metrics.WallClockSeconds
	active := activeSeconds(wallClock, result.TotalPausedDur)

	// Update duration correction factor from actual vs estimated time.
	//
	// This must use the ACTIVE duration, not the raw wall clock (TB-18). The
	// factor scales every future estimate for this leaf, ramps up aggressively
	// (80/20) while decaying at 10% per unit, and is persisted to dcf.json — so a
	// single unit that happened to be suspended mid-run poisoned the estimate for
	// days across restarts, and the client throttled its own work intake in
	// response. Observed: a unit reporting 10212 s wall clock for ~1400 s of
	// computation after a 2 h 28 min thermal freeze, against a normal range of
	// 146–2821 s for that leaf on that host.
	//
	// Elapsed rather than CPU time is deliberate and stays: competing load from
	// the volunteer's own other work genuinely does make a unit take longer here,
	// and the estimate should reflect that. Suspension is the opposite case —
	// time the unit was not running at all.
	if d.dcfTracker != nil && wu.RscFpopsEst > 0 && d.benchmarkFPOPS > 0 {
		estimatedSec := wu.RscFpopsEst / d.benchmarkFPOPS
		if active > 0 {
			d.dcfTracker.Update(wu.LeafID, estimatedSec, float64(active))
		}
	}

	d.recordHistory(wu, wallClock, active, submitResp.Accepted, conn.Name)
}

// activeSeconds is the time a work unit spent actually computing: its elapsed
// wall clock less any period it was suspended.
//
// The two differ whenever a unit is frozen mid-run — a thermal throttle, the
// resource monitor, or a schedule window closing — and conflating them is TB-18.
// Suspension is not slowness: the unit was not running at all. Competing load
// from the volunteer's own other work IS slowness and stays in the figure, which
// is why this subtracts pause time rather than switching to CPU time.
//
// Clamped at zero: pause accounting is sampled independently of the runtime's
// wall clock, and a rounding disagreement must never reach the estimator as a
// negative duration.
func activeSeconds(wallClockSec int64, paused time.Duration) int64 {
	active := wallClockSec - int64(paused.Seconds())
	if active < 0 {
		return 0
	}
	return active
}

// persistActiveTasks writes the current active tasks to disk so they survive
// a crash or force-kill. Called after every task start and completion — the file
// is always up to date, no graceful shutdown needed.
func (d *Daemon) persistActiveTasks() {
	if d.slotManager == nil {
		return
	}
	tasks := d.slotManager.GetActivePersistableTasks()
	if len(tasks) > 0 {
		if err := SaveActiveState(d.cfg.DataDir, tasks); err != nil {
			d.logger.Warn("failed to persist active tasks", "error", err)
		}
	} else {
		ClearActiveState(d.cfg.DataDir)
	}
}

// heldWorkUnitIDs returns the ids of every work unit this volunteer currently holds:
// its prefetch buffer (buffered, not yet started) plus its active slots (in-transit
// and running). Reported on each RequestWorkUnit so the head can release any
// reservation the volunteer no longer holds.
//
// Running units MUST be included. The head reconciles RUNNING copies against this set
// too, and releases any it does not find (TB-13) — without that, a claim held by a
// crashed client stood until the leaf's whole deadline and locked the volunteer out of
// all work. Under-reporting therefore costs real work: a running unit left out of the
// set is reaped and handed to someone else while this machine is still computing it.
func (d *Daemon) heldWorkUnitIDs() []string {
	held := d.heldWorkUnits()
	ids := make([]string, 0, len(held))
	for _, wu := range held {
		ids = append(ids, wu.ID)
	}
	return ids
}

// heldWorkUnits returns every work unit this volunteer currently holds,
// exactly once each: queued in the prefetch buffer, mid queue→slot handoff
// (popped by fillSlots, slot not yet active — TB-33), or running in a slot.
// The handoff overlaps the active set for an instant at its end, so the union
// deduplicates by work-unit ID. All buffer arithmetic (fullness, held-IDs
// reported to heads) is built on this so a unit in transition never reads as
// dropped or doubled.
func (d *Daemon) heldWorkUnits() []*runtime.WorkUnit {
	seen := make(map[string]struct{})
	var held []*runtime.WorkUnit
	add := func(wu *runtime.WorkUnit) {
		if wu == nil {
			return
		}
		if _, dup := seen[wu.ID]; dup {
			return
		}
		seen[wu.ID] = struct{}{}
		held = append(held, wu)
	}
	if d.prefetchQueue != nil {
		queued, starting := d.prefetchQueue.HeldSnapshot()
		for _, item := range queued {
			add(item.WU)
		}
		for _, item := range starting {
			add(item.WU)
		}
	}
	if d.slotManager != nil {
		for _, wu := range d.slotManager.ActiveWorkUnits() {
			add(wu)
		}
	}
	return held
}

// persistPrefetchBuffer writes the current prefetch-buffer contents (buffered,
// not-yet-started units) to disk so a non-graceful exit (crash/force-kill) does not
// strand them: on the next startup the volunteer re-enqueues them and reports them as
// held, so the head keeps their reservations instead of leaving them stranded until
// their deadline. Called every coordinator iteration so the file stays current without
// a graceful shutdown; a graceful shutdown returns buffered units to the head and
// clears the file, making the next resume a no-op.
func (d *Daemon) persistPrefetchBuffer() {
	if d.prefetchQueue == nil {
		return
	}
	items := d.prefetchQueue.Items()
	tasks := make([]PersistedTask, 0, len(items))
	for _, it := range items {
		if it.WU == nil || it.Prep == nil || it.Conn == nil {
			continue
		}
		tasks = append(tasks, PersistedTask{
			WorkUnitID:             it.WU.ID,
			LeafID:                 it.WU.LeafID,
			ServerGRPCAddress:      it.Conn.Config.GRPCAddress,
			ServerName:             it.Conn.Name,
			VolunteerID:            it.Conn.VolunteerID,
			RuntimeName:            it.WU.Runtime,
			WorkDir:                it.Prep.WorkDir,
			BinaryPath:             it.Prep.BinaryPath,
			InputPath:              it.Prep.InputPath,
			CodeArtifactURL:        it.WU.CodeArtifactURL,
			ParametersJSON:         it.WU.ParametersJSON,
			DeadlineSeconds:        it.WU.DeadlineSeconds,
			EnvVars:                it.WU.EnvVars,
			ExecutionSpec:          it.WU.ExecutionSpec,
			RscFpopsEst:            it.WU.RscFpopsEst,
			VizBundlePath:          it.Prep.VizBundlePath,
			CheckpointIntervalSecs: int32(it.WU.CheckpointIntervalSeconds),
			ReservedUntilUnix:      it.WU.ReservedUntilUnix,
			FetchedAt:              it.FetchedAt,
		})
	}
	if len(tasks) == 0 {
		ClearBufferState(d.cfg.DataDir)
		return
	}
	if err := SaveBufferState(d.cfg.DataDir, tasks); err != nil {
		d.logger.Warn("failed to persist prefetch buffer", "error", err)
	}
}

// fillSlots fills available execution slots from the pre-fetch queue. The
// picker is first-fit, not pop-head-or-give-up: a buffered unit the machine
// cannot currently fit stays in place, in order, while fitting units behind it
// may start — see PopFit for the backfill and starvation rules (TB-22).
func (d *Daemon) fillSlots(ctx context.Context) {
	for {
		slotID := d.slotManager.AvailableSlotID()
		if slotID < 0 {
			d.logger.Debug("fillSlots: no available slots", "active", d.slotManager.ActiveCount())
			return // no available slots
		}

		item := d.prefetchQueue.PopFit(func(it *PreFetchItem) bool {
			ok, reason := d.canAccommodateWU(it.WU)
			if !ok {
				// Once per unit at Info, then Debug: this check runs on a
				// 1-second tick, and per-check Info was ~30k identical
				// lines/day on a machine waiting for capacity (TB-23).
				if it.BlockedSince.IsZero() {
					it.BlockedSince = time.Now()
					d.logger.Info("buffered work unit waiting for capacity",
						"work_unit_id", it.WU.ID, "leaf_id", it.WU.LeafID, "reason", reason)
				} else {
					d.logger.Debug("buffered work unit still waiting for capacity",
						"work_unit_id", it.WU.ID, "reason", reason)
				}
			}
			return ok
		}, d.itemMayDelay)
		if item == nil {
			// Queue empty, nothing currently fits, or backfill is held for a
			// starved unit — return the slot and wait for capacity to change.
			d.logger.Debug("fillSlots: no runnable buffered unit", "queue_len", d.prefetchQueue.Len())
			d.slotManager.ReturnSlotID(slotID)
			return
		}

		startAttrs := []any{
			"work_unit_id", item.WU.ID,
			"leaf_id", item.WU.LeafID,
			"slot", slotID,
			"server", item.Conn.Name,
		}
		if !item.BlockedSince.IsZero() {
			startAttrs = append(startAttrs, "waited_for_capacity", time.Since(item.BlockedSince).Round(time.Second).String())
		}
		d.logger.Info("starting work unit in slot", startAttrs...)

		if err := d.slotManager.StartSlot(ctx, slotID, item, d); err != nil {
			d.logger.Error("failed to start slot", "slot", slotID, "error", err)
			d.slotManager.ReturnSlotID(slotID)
			// Return the un-run unit to the head instead of holding its reservation.
			d.abandonItem(item, "slot start failed")
			if item.Runtime != nil && item.Prep != nil {
				item.Runtime.Cleanup(item.Prep)
			}
		} else {
			d.persistActiveTasks()
		}
		// End the handoff only now: on success the active slot carries the unit
		// (set before StartSlot returned), on failure it was abandoned to the
		// head — either way the accounting never saw it uncounted (TB-33).
		d.prefetchQueue.FinishStart(item.WU.ID)
	}
}

// canAccommodateWU checks whether there are enough resources to run the WU
// before admitting it to a slot. It applies four guards (only run
// what the machine can actually fit):
//
//  1. Configured budget — the sum of declared per-WU memory across active slots
//     plus this WU must stay within the volunteer's max_memory_mb. Uses declared
//     maxes, so it is robust to container memory ramping up over time.
//  2. Real free system RAM — the machine must currently have enough available
//     memory for this WU (plus a small headroom), regardless of the configured
//     budget. Skipped on platforms where free memory can't be read.
//  3. GPU exclusivity — at most one GPU work unit per physical GPU, so concurrent
//     units never oversubscribe VRAM (the per-WU VRAM requirement isn't
//     transmitted, so admission gates on device count).
//  4. Disk workspace — free space on the data-dir volume must cover the unit's
//     enforced /work ceiling (BookedDiskMB, the same clamped number the disk
//     watchdog enforces) plus the absolute floor, so a unit buffered while disk
//     was ample doesn't start after the disk filled up (TB-24). A refused unit
//     waits in the buffer like a memory-refused one; the head's reservation
//     expiry reclaims it if space never frees.
//
// On refusal it returns the human-readable reason; it logs nothing itself —
// callers run it on a 1-second tick and own the throttling (TB-23).
func (d *Daemon) canAccommodateWU(wu *runtime.WorkUnit) (bool, string) {
	if d.slotManager == nil {
		return true, ""
	}

	// BG-16: book this WU at BookedMemMB — the same clamped number the runtime will
	// enforce — so admission and enforcement share one denominator. A declared 0 is
	// bounded to the per-task default; a huge declaration is clamped to the budget.
	maxMemoryMB := d.cfg.ResourceLimits.MaxMemoryMB
	wuMemoryMB := runtime.BookedMemMB(int(wu.ExecutionSpec.MaxMemoryMB), maxMemoryMB)

	// 1. Configured memory budget.
	if maxMemoryMB > 0 {
		activeMemoryMB := d.slotManager.TotalActiveMemoryMB(maxMemoryMB)
		if activeMemoryMB+wuMemoryMB > maxMemoryMB {
			return false, fmt.Sprintf("configured memory budget: %d MB active + %d MB unit exceeds max_memory_mb %d",
				activeMemoryMB, wuMemoryMB, maxMemoryMB)
		}
	}

	// 2. Real free system RAM (already reflects memory used by active containers).
	if freeMB, ok := freeSystemMemoryMB(); ok {
		if freeMB < wuMemoryMB+freeMemoryHeadroomMB {
			return false, fmt.Sprintf("free system RAM: %d MB free, unit needs %d MB + %d MB headroom",
				freeMB, wuMemoryMB, freeMemoryHeadroomMB)
		}
	}

	// 3. GPU exclusivity: one GPU work unit per physical GPU.
	if wu.ExecutionSpec.GPURequired {
		gpuCount := 0
		if d.cachedHW != nil {
			gpuCount = len(d.cachedHW.GetGpus())
		}
		if gpuCount > 0 && d.slotManager.ActiveGPUCount() >= gpuCount {
			return false, fmt.Sprintf("all GPUs busy: %d of %d running GPU work units",
				d.slotManager.ActiveGPUCount(), gpuCount)
		}
	}

	// 4. Disk workspace on the data-dir volume, at the unit's enforced ceiling.
	if d.limiter != nil {
		wuDiskMB := runtime.BookedDiskMB(int(wu.ExecutionSpec.MaxDiskMB), d.cfg.ResourceLimits.MaxDiskGB*1024)
		if err := d.limiter.CheckDiskSpace(d.cfg.DataDir, wuDiskMB+DiskFloorMB); err != nil {
			return false, fmt.Sprintf("disk workspace: unit's /work ceiling is %d MB + %d MB floor, but: %v",
				wuDiskMB, DiskFloorMB, err)
		}
	}

	return true, ""
}

// mayDelayAdmission reports whether starting candidate now could postpone
// blocked's own future admission — the delay test the backfill starvation cap
// gates on (TB-45; EASY backfilling's rule, specialized to this admission
// model). The declared bookings decide: when both units fit the configured
// budgets TOGETHER, capacity that frees enough to admit blocked can never be
// re-consumed by candidate, so candidate is harmless whatever the current
// running set looks like. Real free system RAM is deliberately not consulted:
// it cannot be projected to blocked's future admission moment, and candidate
// already passed the live free-RAM guard (canAccommodateWU) to be startable
// at all.
//
// Unknown shapes (nil units) count as delaying, keeping the cap's protection.
func (d *Daemon) mayDelayAdmission(blocked, candidate *runtime.WorkUnit) bool {
	if blocked == nil || candidate == nil {
		return true
	}

	// Configured memory budget: harmless iff both bookings fit it together.
	if maxMemoryMB := d.cfg.ResourceLimits.MaxMemoryMB; maxMemoryMB > 0 {
		blockedMemMB := runtime.BookedMemMB(int(blocked.ExecutionSpec.MaxMemoryMB), maxMemoryMB)
		candMemMB := runtime.BookedMemMB(int(candidate.ExecutionSpec.MaxMemoryMB), maxMemoryMB)
		if blockedMemMB+candMemMB > maxMemoryMB {
			return true
		}
	}

	// GPU exclusivity: two GPU units co-run only when two physical GPUs exist.
	if blocked.ExecutionSpec.GPURequired && candidate.ExecutionSpec.GPURequired {
		gpuCount := 0
		if d.cachedHW != nil {
			gpuCount = len(d.cachedHW.GetGpus())
		}
		if gpuCount < 2 {
			return true
		}
	}

	// Disk workspace: harmless iff the volume's free space covers both units'
	// enforced /work ceilings plus the floor, so candidate running at its
	// ceiling still leaves blocked's own admission headroom.
	if d.limiter != nil {
		maxDiskMB := d.cfg.ResourceLimits.MaxDiskGB * 1024
		blockedDiskMB := runtime.BookedDiskMB(int(blocked.ExecutionSpec.MaxDiskMB), maxDiskMB)
		candDiskMB := runtime.BookedDiskMB(int(candidate.ExecutionSpec.MaxDiskMB), maxDiskMB)
		if err := d.limiter.CheckDiskSpace(d.cfg.DataDir, blockedDiskMB+candDiskMB+DiskFloorMB); err != nil {
			return true
		}
	}

	return false
}

// itemMayDelay adapts mayDelayAdmission to the prefetch queue's item type.
func (d *Daemon) itemMayDelay(blocked, candidate *PreFetchItem) bool {
	var b, c *runtime.WorkUnit
	if blocked != nil {
		b = blocked.WU
	}
	if candidate != nil {
		c = candidate.WU
	}
	return d.mayDelayAdmission(b, c)
}

// shouldFetch checks whether the fetcher should request ANY work right now.
// Returns false when the scheduler says no, when the data-dir volume is below
// the absolute free-space floor, or when no enabled leaf currently passes its
// per-leaf disk gate (TB-24 — the gate is per leaf: what a fetch requires free
// is each leaf's own declared need, never the whole max_disk_gb allowance; see
// disk_gate.go). Which specific leafs are fetchable is the fetcher's per-leaf
// skip, driven by the same leafDiskGate.
func (d *Daemon) shouldFetch() bool {
	// Check scheduler.
	if d.scheduler != nil && !d.scheduler.ShouldRun() {
		d.logger.Debug("shouldFetch: scheduler says don't run")
		return false
	}

	if d.limiter == nil {
		return true
	}

	// The absolute floor: below this nothing runs at all.
	if err := d.limiter.CheckDiskSpace(d.cfg.DataDir, DiskFloorMB); err != nil {
		d.warnDiskGateOnce(fmt.Sprintf("free space on the data dir (%s) is below the %d MB floor the fetch gate needs to run any work: %v",
			d.cfg.DataDir, DiskFloorMB, err), "", "", 0)
		return false
	}

	leafs := d.allEnabledLeafs()
	if len(leafs) == 0 {
		// No cached leaf catalog (e.g. a head that doesn't surface GetHeadInfo):
		// gate the any-leaf request on the unknown-need fallback, since the
		// leaf's real requirement is unknowable here.
		if ok, reason := d.leafDiskGate(anyLeafInfo); !ok {
			d.warnDiskGateOnce(reason, "", "", d.leafRaiseToGB(anyLeafInfo))
			return false
		}
		d.clearDiskGateWarning()
		return true
	}

	var gatedLabel, gatedLeafID, gatedReason string
	var gatedRaiseToGB int
	sawFetchable := false
	for _, leaf := range leafs {
		// A leaf that requires a GPU this machine does not offer is refused by
		// the head whatever its disk verdict, so it neither justifies fetching
		// nor supplies the representative disk reason — a disk remedy quoted
		// for it (raise max_disk_gb) could not change the outcome (TB-30).
		if d.leafNeedsAbsentGPU(leaf) {
			continue
		}
		sawFetchable = true
		ok, reason := d.leafDiskGate(leaf)
		if ok {
			d.clearDiskGateWarning()
			return true
		}
		if gatedLabel == "" {
			gatedLabel = leaf.Slug
			if gatedLabel == "" {
				gatedLabel = leaf.ID
			}
			gatedLeafID = leaf.ID
			gatedReason = reason
			gatedRaiseToGB = d.leafRaiseToGB(leaf)
		}
	}
	if !sawFetchable {
		// Every enabled leaf needs a GPU this machine does not offer — a
		// permanent capability mismatch, not a disk stall. `leafs list` and
		// `doctor` name it per leaf (TB-21).
		d.logger.Debug("shouldFetch: every enabled leaf requires a GPU this machine does not offer")
		return false
	}
	// Every fetchable leaf is disk-gated; surface one representative reason,
	// naming its leaf — an unnamed "this leaf" sent a tester hunting through
	// the catalog for which leaf the numbers belonged to (TB-30).
	d.warnDiskGateOnce(fmt.Sprintf("every enabled leaf is disk-gated — e.g. %s: %s", gatedLabel, gatedReason),
		gatedLabel, gatedLeafID, gatedRaiseToGB)
	return false
}

// leafRequiresGPU reports whether this leaf's units need a GPU: either of the
// two gpu_required flags, because dispatch ORs them (TB-21).
func leafRequiresGPU(leaf CachedLeafInfo) bool {
	if leaf.ExecutionSpec != nil && leaf.ExecutionSpec.GPURequired {
		return true
	}
	return leaf.ResourceRequirements != nil && leaf.ResourceRequirements.GPURequired
}

// leafNeedsAbsentGPU reports whether this leaf requires a GPU on a machine that
// advertises none. Presence-only deliberately: VRAM, vendor and compute
// capability shortfalls stay the head's call, so this can never skip a leaf
// the head would actually dispatch.
func (d *Daemon) leafNeedsAbsentGPU(leaf CachedLeafInfo) bool {
	if d.HasGPU() {
		return false
	}
	return leafRequiresGPU(leaf)
}

// leafRuntimeVerdict classifies a leaf against what this machine advertised to
// one head, mirroring the head's own dispatch gate: a leaf's runtime must be
// among the runtimes the volunteer advertised to that head, and what it
// advertises is the registered runtimes filtered by per-head trust
// (advertisedForServer). It returns the runtime the leaf needs — "container",
// "native", or "" for a leaf that is never refused on runtime grounds (a WASM-
// capable leaf, since WASM is always registered and always trusted, or a leaf
// with no published spec, where the per-unit gates decide) — plus whether that
// runtime is missing from this machine's registry and whether the head is
// untrusted for it. A container leaf needs a registered container runtime and
// per-head CONTAINER trust; a native-only leaf (native binaries, no wasm) needs
// per-head NATIVE trust (a trusted head implies the runtime is registered —
// buildRuntimeRegistry constructs native when any head is trusted for it). A
// nil registry reports nothing missing. Shared by the readiness banner
// (readinessCounts, PB-5) and the fetcher's pre-request skip (TB-49), so the two
// cannot disagree about which leafs this machine can be handed.
func leafRuntimeVerdict(leaf CachedLeafInfo, registry *RuntimeRegistry, srv config.ServerConfig) (rt string, missing, untrusted bool) {
	es := leaf.ExecutionSpec
	if es == nil {
		return "", false, false
	}
	if es.Image != "" {
		rt = "container"
	} else {
		wasmCapable, nativeCapable := false, false
		for k := range es.Binaries {
			if strings.EqualFold(k, "wasm") {
				wasmCapable = true
			} else {
				nativeCapable = true
			}
		}
		if !nativeCapable || wasmCapable {
			return "", false, false
		}
		rt = "native"
	}
	missing = registry != nil && registry.GetRuntime(rt) == nil
	untrusted = !srv.TrustsRuntime(rt)
	return rt, missing, untrusted
}

// allEnabledLeafs returns the enabled leafs across every attached head.
func (d *Daemon) allEnabledLeafs() []CachedLeafInfo {
	if d.multiClient == nil {
		return nil
	}
	var out []CachedLeafInfo
	for _, srv := range d.multiClient.Servers() {
		out = append(out, d.enabledLeafs(srv.Name)...)
	}
	return out
}

// noteUnstattableImageStore logs — once per path per daemon run — that the
// engine-reported image-store path cannot be examined from this host, so the
// disk gate does not gate on it. INFO, not WARN: on the documented
// Windows/macOS podman-machine setup this is the normal state of the world.
func (d *Daemon) noteUnstattableImageStore(path string) {
	d.diskGateMu.Lock()
	if d.unstattableStores == nil {
		d.unstattableStores = make(map[string]bool)
	}
	already := d.unstattableStores[path]
	d.unstattableStores[path] = true
	d.diskGateMu.Unlock()
	if already {
		return
	}
	d.logger.Info("image-store free space cannot be determined from this host (engine-internal path); not gating work fetching on it — the engine enforces its own storage limits",
		"path", path)
}

// workBufferQueueDepth is the hard ceiling on the number of un-run descriptors
// the client work buffer may hold. The buffer's real "full" gate is hours-based
// (workBufferFull); this is only a safety cap so a misbehaving head or a leaf
// with tiny units cannot make the buffer grow without bound. Set generously high
// so it is not normally the binding constraint.
const workBufferQueueDepth = 256

// fallbackBufferUnitsPerSlot bounds the buffer when no per-unit time estimate is
// available (benchmark unknown, or leafs report rsc_fpops_est=0). Without a time
// estimate the hours-based target can't be computed, so we fall back to a small
// unit-count buffer (this many descriptors per slot) so the volunteer still
// pre-fetches a little without unboundedly hoarding reservations.
const fallbackBufferUnitsPerSlot = 2

// maxSlots returns the configured concurrent-task count (>= 1).
func (d *Daemon) maxSlots() int {
	n := d.cfg.MaxConcurrentTasks
	if n <= 0 {
		n = 1
	}
	return n
}

// estSecondsForUnit estimates wall-clock seconds for a unit from its FP-ops
// estimate and this host's benchmark, applying the leaf's learned duration
// correction factor when available. Returns 0 when no estimate is possible.
func (d *Daemon) estSecondsForUnit(leafID string, rscFpopsEst float64) float64 {
	if rscFpopsEst <= 0 || d.benchmarkFPOPS <= 0 {
		return 0
	}
	sec := rscFpopsEst / d.benchmarkFPOPS
	if d.dcfTracker != nil {
		if dcf := d.dcfTracker.Get(leafID); dcf > 0 {
			sec *= dcf
		}
	}
	return sec
}

// leafEstSeconds estimates wall-clock seconds for one unit of a leaf to size the
// FIRST batch request to it (#29), BEFORE any of that leaf's units have been
// buffered (so estSecondsForUnit, which needs a per-unit rsc_fpops_est, can't
// help yet). It uses the leaf-level, benchmark-INDEPENDENT estimate the head
// carries on CachedLeafInfo, refined by this leaf's learned duration correction
// factor when one is available. Because it does not divide by the local
// benchmark, it stays non-zero on un-benchmarked hosts — the exact case the old
// FP-ops-only seam tripped to 0, leaving the flat ceiling to bind. Returns 0 only
// when the head supplied no estimate.
func (d *Daemon) leafEstSeconds(leaf CachedLeafInfo) float64 {
	sec := leaf.EstimatedDurationSeconds
	if sec > 0 && d.dcfTracker != nil {
		if dcf := d.dcfTracker.Get(leaf.ID); dcf > 0 {
			sec *= dcf
		}
	}
	// TB-34: fold in what the last ARRIVED batch of this leaf actually measured
	// (per-unit FP-ops against this host's benchmark). Taking the max corrects the
	// over-ask case — a leaf-level estimate far below the units' real size asked for
	// 60× what the buffer could hold, and the DCF never corrects it because it learns
	// only from completions, which the returned tail never produces. When the head's
	// figure is the larger one it still wins (smaller asks are the safe direction).
	d.arrivalEstMu.Lock()
	if arr := d.arrivalEstSec[leaf.ID]; arr > sec {
		sec = arr
	}
	d.arrivalEstMu.Unlock()
	if sec <= 0 {
		return 0
	}
	return sec
}

// noteArrivalEstimate records the per-unit seconds a just-arrived unit of the leaf
// implies (its rsc_fpops_est against this host's benchmark, DCF applied — see
// arrivalEstSec). Called by the fetcher per arrival; a unit with no usable estimate
// records nothing (the previous figure stands). Units of one leaf are near-uniform,
// so the latest observation is the batch signal with no windowing machinery.
func (d *Daemon) noteArrivalEstimate(leafID string, rscFpopsEst float64) {
	if leafID == "" {
		return
	}
	sec := d.estSecondsForUnit(leafID, rscFpopsEst)
	if sec <= 0 {
		return
	}
	d.arrivalEstMu.Lock()
	if d.arrivalEstSec == nil {
		// Lazy init: test daemons are built as struct literals without the constructor.
		d.arrivalEstSec = make(map[string]float64)
	}
	d.arrivalEstSec[leafID] = sec
	d.arrivalEstMu.Unlock()
}

// bufferTargetSeconds is the total seconds of work the client work buffer aims
// to hold: work_buffer_hours hours per execution slot. Sizing in hours (rather
// than a unit count) keeps the buffer meaningful across leafs whose units span
// seconds to hours. Returns 0 when buffering is disabled by config (hours == 0).
func (d *Daemon) bufferTargetSeconds() float64 {
	hours := d.cfg.WorkBufferHours
	if hours < 0 {
		hours = 0
	}
	if hours == 0 {
		return 0
	}
	return hours * 3600 * float64(d.maxSlots())
}

// gpuSlots is how many execution slots GPU-required units can occupy at once:
// one per physical GPU (canAccommodateWU's exclusivity guard), never more than
// the slot count. With no GPU detected there is no GPU-specific bound — the
// head should not be dispatching GPU work here at all (leafNeedsAbsentGPU) —
// so the slot count is returned and the GPU class collapses into the whole.
func (d *Daemon) gpuSlots() int {
	slots := d.maxSlots()
	if d.cachedHW == nil {
		return slots
	}
	if n := len(d.cachedHW.GetGpus()); n > 0 && n < slots {
		return n
	}
	return slots
}

// gpuBufferTargetSeconds is the hours target for the GPU class of buffered work:
// work_buffer_hours per GPU-capable slot (TB-48). The global target sizes the
// buffer by slots alone, but GPU-required units can only ever drain through
// gpuSlots of them, so on a one-GPU host with many CPU slots the global figure
// let the buffer hold slots × hours of GPU units of which one ran, the rest
// waiting until the 90 %-of-deadline drop. GPU units count against BOTH this
// and the global target (they occupy slots too); everything else counts against
// the global target only. Returns 0 when buffering is disabled (hours == 0).
func (d *Daemon) gpuBufferTargetSeconds() float64 {
	hours := d.cfg.WorkBufferHours
	if hours <= 0 {
		return 0
	}
	return hours * 3600 * float64(d.gpuSlots())
}

// bufferedGPUSeconds is bufferedSeconds restricted to GPU-required units —
// the fill measured against gpuBufferTargetSeconds. Same full-booking
// (conservative, acceptance-side) view as bufferedSeconds.
func (d *Daemon) bufferedGPUSeconds() float64 {
	var total float64
	for _, wu := range d.heldWorkUnits() {
		if wu.ExecutionSpec.GPURequired {
			total += d.estSecondsForUnit(wu.LeafID, wu.RscFpopsEst)
		}
	}
	return total
}

// bufferedGPUUnitCount counts held GPU-required units (the unit-count fallback
// view of the GPU class).
func (d *Daemon) bufferedGPUUnitCount() int {
	n := 0
	for _, wu := range d.heldWorkUnits() {
		if wu.ExecutionSpec.GPURequired {
			n++
		}
	}
	return n
}

// fallbackGPUBufferUnits is the GPU class's unit-count cap when no hours
// estimate is available: the same per-slot multiple as fallbackBufferUnits,
// over GPU-capable slots.
func (d *Daemon) fallbackGPUBufferUnits() int {
	return fallbackBufferUnitsPerSlot * d.gpuSlots()
}

// gpuBufferHoursFull is workBufferHoursFull for the GPU class (TB-48): the GPU
// units held reach the GPU hours target, or — when none of them can be
// estimated — the GPU unit-count fallback. Like its global counterpart it says
// nothing about runnability; bufferAccepts and the fetcher's pre-request skip
// apply it, and the TB-32 idle-slot escape still governs acceptance over it.
func (d *Daemon) gpuBufferHoursFull() bool {
	target := d.gpuBufferTargetSeconds()
	if target <= 0 {
		return d.bufferedGPUUnitCount() >= d.fallbackGPUBufferUnits()
	}
	sec := d.bufferedGPUSeconds()
	if sec <= 0 && d.bufferedGPUUnitCount() >= d.fallbackGPUBufferUnits() {
		return true
	}
	return sec >= target
}

// leafClassBufferFull reports whether the resource class this leaf's units
// belong to has already reached its own hours target, so the fetcher can skip
// the leaf BEFORE issuing RequestWorkUnit (TB-48). Today the only class bounded
// tighter than the slot count is GPU work; a CPU leaf is never class-full here
// (the global target and workBufferFull govern it). Without this skip a one-GPU
// host under its global target asked for GPU units every round and returned
// each within seconds — the request-and-refuse churn TB-34 ended for the
// global buffer.
func (d *Daemon) leafClassBufferFull(leaf CachedLeafInfo) (bool, string) {
	if !leafRequiresGPU(leaf) || !d.gpuBufferHoursFull() {
		return false, ""
	}
	return true, fmt.Sprintf("GPU work buffer full (%.1f h of GPU units held against a target of %.1f h for %d GPU slot(s))",
		d.bufferedGPUSeconds()/3600, d.gpuBufferTargetSeconds()/3600, d.gpuSlots())
}

// bufferedSeconds sums the estimated seconds of work currently held: queued,
// mid queue→slot handoff (TB-33), and running (active slots). This is the
// "fill" measured against bufferTargetSeconds. Every unit is booked at its FULL
// estimate — including running ones — which is the right (conservative) measure
// for ACCEPTANCE: an arriving unit must fit under the target however far the
// running work has progressed. The refill trigger uses the remaining-time view
// (bufferedRemainingSeconds) instead.
func (d *Daemon) bufferedSeconds() float64 {
	var total float64
	for _, wu := range d.heldWorkUnits() {
		total += d.estSecondsForUnit(wu.LeafID, wu.RscFpopsEst)
	}
	return total
}

// bufferedRemainingSeconds is the buffer fill measured as REMAINING work: a
// running unit counts max(estimate − run time so far, 0) instead of its full
// booking (TB-34, from the tester's design input: "2 h buffered" can mean ~1 h of
// actual runway, so a refill trigger that reads full bookings starts later than
// the target implies). Queued and mid-handoff units still count in full — nothing
// has been spent on them. Only the hysteresis low-water comparison reads this;
// acceptance keeps the conservative full-booking view.
func (d *Daemon) bufferedRemainingSeconds() float64 {
	var elapsed map[string]time.Duration
	if d.slotManager != nil {
		elapsed = d.slotManager.ActiveElapsedByUnit()
	}
	var total float64
	for _, wu := range d.heldWorkUnits() {
		est := d.estSecondsForUnit(wu.LeafID, wu.RscFpopsEst)
		if ran, ok := elapsed[wu.ID]; ok {
			est -= ran.Seconds()
			if est < 0 {
				est = 0
			}
		}
		total += est
	}
	return total
}

// workBufferLowWaterFrac is the hysteresis low-water mark as a fraction of the
// hours target (TB-34): once the buffer has filled, fetching reopens only when
// the REMAINING buffered work drains below this fraction, so the interval
// between fetch rounds is about half the buffer of compute — the incumbent
// volunteer-computing platform's proven two-level ("min/max buffer") design.
// One shared threshold for fetch-gate and arrival-acceptance made the buffer
// hover at the line and request-and-refuse indefinitely.
const workBufferLowWaterFrac = 0.5

// workBufferFull reports whether the client work buffer holds enough work that
// the fetcher must issue ZERO RequestWorkUnit calls (Layer-1 DoD #2).
//
// Fullness is the hours/count verdict (workBufferHoursFull) with HYSTERESIS
// (TB-34): reaching the target latches the gate closed, and it stays closed —
// even as completions drop the fill back under the target — until the remaining
// buffered work sinks below the low-water mark (workBufferLowWaterFrac, measured
// in REMAINING time so booked-but-mostly-done running work does not overstate
// the runway). Then one refill round fills back to the target. Acceptance
// (bufferAccepts) deliberately keeps the raw single threshold: hysteresis shapes
// when we ASK, never how much we may hold.
//
// One escape, unchanged (TB-32): hours are not runnability. A buffer saturated
// with units admission refuses beside the running work — one big-memory leaf on
// a 2-slot host — used to pin the idle slot for hours while other attached
// leafs had admissible units the fetcher never asked for. So a full buffer that
// cannot occupy an idle slot does not gate fetching, latch or no latch; the
// fetcher then requests in starved-backfill mode (see starvedBackfill), which
// restricts it to leafs that could actually fill that slot.
func (d *Daemon) workBufferFull() bool {
	d.bufferFilledMu.Lock()
	if d.workBufferHoursFull() {
		d.bufferFilled = true
	} else if d.bufferFilled && d.bufferBelowLowWater() {
		d.bufferFilled = false
	}
	full := d.bufferFilled
	d.bufferFilledMu.Unlock()
	if !full {
		return false
	}
	return !d.idleSlotStarved()
}

// bufferBelowLowWater reports whether the buffer has drained enough for the
// hysteresis latch to reopen fetching: remaining buffered work under
// workBufferLowWaterFrac of the hours target. When the hours math is unusable
// (buffering disabled, or held units with no estimates) it reopens at the
// unit-count cap itself — count mode keeps its pre-hysteresis cadence, because a
// halved count threshold would idle the slot between the last spare unit
// starting and the next fetch round, and count-mode asks are already bounded by
// the batch-feedback cap rather than by an estimate.
func (d *Daemon) bufferBelowLowWater() bool {
	target := d.bufferTargetSeconds()
	if target <= 0 || d.bufferedSeconds() <= 0 {
		return d.bufferedUnitCount() < d.fallbackBufferUnits()
	}
	return d.bufferedRemainingSeconds() < target*workBufferLowWaterFrac
}

// workBufferHoursFull is the raw hours/count fullness verdict, with no regard
// for whether the buffered work can currently run (TB-32 splits the two).
//
// When a per-unit time estimate is available it uses the hours-based target;
// otherwise it falls back to a small per-slot unit count so the buffer can't
// grow without bound when estimates are missing.
func (d *Daemon) workBufferHoursFull() bool {
	target := d.bufferTargetSeconds()
	if target <= 0 {
		// Hours target unusable (buffering disabled) — fall back to a unit count.
		return d.bufferedUnitCount() >= d.fallbackBufferUnits()
	}
	// If we have buffered units but can't estimate ANY of their durations, the
	// hours math is meaningless; bound by the unit-count fallback instead.
	if d.bufferedSeconds() <= 0 && d.bufferedUnitCount() >= d.fallbackBufferUnits() {
		return true
	}
	return d.bufferedSeconds() >= target
}

// idleSlotStarved reports whether an execution slot is idle while the picker
// would start nothing from the work buffer — the TB-32 starvation state. The
// verdict is the PICKER's own reachability (PreFetchQueue.HasRunnable with the
// same predicates fillSlots hands PopFit), not a parallel scan: TB-45 froze a
// slot for 43 minutes beside a unit a whole-queue admission check found
// startable but the picker's capped scan never reached, so this returned
// false and the WARN specified for exactly that idle slot stayed silent. An
// empty buffer beside an idle slot counts: a single running unit whose
// estimate exceeds the whole hours target starves the other slot the same
// way. A slot whose unit is mid queue→slot handoff is occupied, not idle
// (TB-33). Cheap in the healthy case — when every slot is busy it returns
// false before touching the queue.
func (d *Daemon) idleSlotStarved() bool {
	if d.slotManager == nil || d.prefetchQueue == nil {
		return false
	}
	_, starting := d.prefetchQueue.HeldSnapshot()
	occupied := d.slotManager.ActiveCount()
	if len(starting) > 0 {
		// A unit in the handoff is about to occupy a slot; count it as if it
		// already had, minus any overlap with slots that just turned active.
		active := make(map[string]struct{})
		for _, wu := range d.slotManager.ActiveWorkUnits() {
			if wu != nil {
				active[wu.ID] = struct{}{}
			}
		}
		for _, item := range starting {
			if item.WU == nil {
				continue
			}
			if _, dup := active[item.WU.ID]; !dup {
				occupied++
			}
		}
	}
	if occupied >= d.maxSlots() {
		return false
	}
	return !d.prefetchQueue.HasRunnable(func(item *PreFetchItem) bool {
		if item.WU == nil {
			return false
		}
		ok, _ := d.canAccommodateWU(item.WU)
		return ok
	}, d.itemMayDelay)
}

// starvedBackfill reports whether the ONLY reason fetching is open is an idle
// slot starved by a full-by-hours buffer. The fetcher uses it to fetch
// precisely (skip leafs that could not fill the slot, see leafFitGate) instead
// of piling more inadmissible hours onto an already-full buffer.
func (d *Daemon) starvedBackfill() bool {
	return d.workBufferHoursFull() && d.idleSlotStarved()
}

// leafFitGate reports whether a unit shaped like this leaf's declared
// execution spec could currently be admitted to a slot (canAccommodateWU). In
// starved-backfill mode the fetcher skips leafs that fail it: requesting the
// big-memory leaf that saturated the buffer again cannot fill the idle slot,
// while another attached leaf's small units can (TB-32). Deliberately NOT
// applied to normal deficit fetching — buffering ahead a unit that cannot run
// beside the current mix but will run alone later is the buffer's job. A leaf
// with no published spec passes; the head's per-unit numbers stay authoritative.
func (d *Daemon) leafFitGate(leaf CachedLeafInfo) (bool, string) {
	if leaf.ExecutionSpec == nil {
		return true, ""
	}
	return d.canAccommodateWU(&runtime.WorkUnit{ExecutionSpec: runtime.ExecutionSpec{
		MaxMemoryMB: leaf.ExecutionSpec.MaxMemoryMB,
		MaxDiskMB:   leaf.ExecutionSpec.MaxDiskMB,
		GPURequired: leaf.ExecutionSpec.GPURequired,
	}})
}

// bufferAccepts decides whether one more ARRIVING unit may be buffered, so a
// batch reply cannot overshoot the hours target into deadline-drop territory
// (TB-32's churn half: ten units of one leaf fetched, never run, dropped at
// 90 % of deadline, re-issued elsewhere — zero compute). Under the target
// everything is accepted. Over it, a unit is accepted only to feed a starving
// idle slot — and only if it can start now; anything else is returned to the
// head immediately (abandon → instant re-dispatch) instead of being held for
// hours and dropped. Refusal reasons travel to the head as the abandon reason.
//
// A GPU-required unit is also measured against the GPU class's own target
// (gpuBufferHoursFull, TB-48): under the global target but over the GPU one it
// is refused the same way, because only gpuSlots of the slots can ever drain it
// — on a one-GPU host the global target admitted slots × hours of GPU units, of
// which one ran and the rest waited for the deadline drop.
func (d *Daemon) bufferAccepts(wu *runtime.WorkUnit) (bool, string) {
	full, reason := d.workBufferHoursFull(), "work buffer full (over the hours target)"
	if !full && wu.ExecutionSpec.GPURequired && d.gpuBufferHoursFull() {
		full = true
		reason = fmt.Sprintf("GPU work buffer full (over the hours target for %d GPU slot(s))", d.gpuSlots())
	}
	if !full {
		return true, ""
	}
	if !d.idleSlotStarved() {
		return false, reason
	}
	if ok, why := d.canAccommodateWU(wu); !ok {
		return false, fmt.Sprintf("work buffer full and the unit cannot start in the idle slot (%s)", why)
	}
	return true, ""
}

// fallbackBufferUnits is the unit-count cap used when an hours estimate is
// unavailable: a small multiple of the slot count.
func (d *Daemon) fallbackBufferUnits() int {
	return fallbackBufferUnitsPerSlot * d.maxSlots()
}

// bufferedUnitCount counts held units — queued, mid queue→slot handoff
// (TB-33), and running — each exactly once (the unit-count fallback view).
func (d *Daemon) bufferedUnitCount() int {
	return len(d.heldWorkUnits())
}

// requestBatchSize returns how many assignments the fetcher should ask a head
// for on the next RequestWorkUnit, given the remaining hours deficit and an
// estimate of seconds-per-unit for the leaf it is about to request. It is
// clamped to [1, maxBatchPerRequest].
//
// When a per-unit time estimate IS available, the count is the hours-deficit
// divided by that estimate (so a leaf with long units is requested fewer at a
// time than one with short units). When no estimate is available — common before
// the first unit of a leaf has been seen, since the leaf cache carries no
// rsc_fpops_est — it falls back to averaging the seconds-per-unit of work already
// buffered; failing that, it requests a full batch whenever the buffer is below
// its hours target so batching still happens, and 1 otherwise.
//
// For a GPU-required leaf the deficit is the smaller of the global deficit and
// the GPU class's own (gpuBufferTargetSeconds − bufferedGPUSeconds, TB-48), and
// the no-estimate fallback is bounded by the GPU unit-count cap rather than a
// full batch: a one-GPU host asking a head for 64 GPU units — the ask clamp the
// head's logs showed every five minutes — can drain them only one at a time.
func (d *Daemon) requestBatchSize(leaf CachedLeafInfo, estSecondsPerUnit float64) int32 {
	target := d.bufferTargetSeconds()
	if target <= 0 {
		// Buffering disabled (hours == 0): unit-count fallback, one at a time.
		return 1
	}
	deficit := target - d.bufferedSeconds()
	gpu := leafRequiresGPU(leaf)
	if gpu {
		if gpuDeficit := d.gpuBufferTargetSeconds() - d.bufferedGPUSeconds(); gpuDeficit < deficit {
			deficit = gpuDeficit
		}
	}
	if deficit <= 0 {
		return 1
	}

	per := estSecondsPerUnit
	if per <= 0 {
		per = d.avgBufferedSecondsPerUnit()
	}
	if per <= 0 {
		// No estimate at all: request a full batch to refill the deficit quickly —
		// except for the GPU class, whose unit-count fallback bounds the ask.
		if gpu {
			return clampBatch(int32(d.fallbackGPUBufferUnits() - d.bufferedGPUUnitCount()))
		}
		return maxBatchPerRequest
	}
	return clampBatch(int32(deficit / per))
}

// clampBatch bounds a computed ask to [1, maxBatchPerRequest].
func clampBatch(n int32) int32 {
	if n < 1 {
		return 1
	}
	if n > maxBatchPerRequest {
		return maxBatchPerRequest
	}
	return n
}

// avgBufferedSecondsPerUnit returns the mean estimated seconds per buffered or
// running unit, or 0 if nothing is buffered or no estimate is available.
func (d *Daemon) avgBufferedSecondsPerUnit() float64 {
	total := d.bufferedSeconds()
	n := d.bufferedUnitCount()
	if total <= 0 || n <= 0 {
		return 0
	}
	return total / float64(n)
}

// Slot-starvation WARN thresholds (TB-32). With the runnability-aware buffer
// gate in place a starved slot normally refills within one fetch round, so a
// starvation that PERSISTS means no attached head is serving anything this
// machine can run right now — worth a WARN, throttled on the TB-27 pattern
// rather than once-per-episode, because the state can hold for hours and a
// single line at hour zero is easy to lose.
const (
	// slotStarveWarnAfter is how long a slot must sit idle with only
	// inadmissible work buffered before the first WARN.
	slotStarveWarnAfter = 10 * time.Minute
	// slotStarveWarnInterval is the minimum spacing between repeat WARNs while
	// the starvation persists.
	slotStarveWarnInterval = 5 * time.Minute
)

// trackSlotStarvation runs on the coordinator tick: it watches for a slot
// idling while the buffer holds only units admission refuses, and WARNs —
// throttled — naming the head-of-buffer unit's blocking reason. Before TB-32
// nothing above INFO recorded the whole condition. The empty-buffer variant of
// starvation is deliberately excluded: with nothing buffered the fetcher's own
// "connected but getting no work" WARN owns the diagnosis.
func (d *Daemon) trackSlotStarvation() {
	if d.slotManager == nil || d.prefetchQueue == nil {
		return
	}
	starved := d.prefetchQueue.Len() > 0 && d.idleSlotStarved()

	d.slotStarveMu.Lock()
	defer d.slotStarveMu.Unlock()
	if !starved {
		if !d.slotStarveWarnedAt.IsZero() {
			// The starvation this WARNed about has ended: a buffered unit
			// started, or the buffer drained.
			d.notices.Resolve("buffer_unrunnable", "", "")
		}
		d.slotStarvedSince = time.Time{}
		d.slotStarveWarnedAt = time.Time{}
		return
	}
	now := time.Now()
	if d.slotStarvedSince.IsZero() {
		d.slotStarvedSince = now
		return
	}
	if now.Sub(d.slotStarvedSince) < slotStarveWarnAfter {
		return
	}
	if !d.slotStarveWarnedAt.IsZero() && now.Sub(d.slotStarveWarnedAt) < slotStarveWarnInterval {
		return
	}
	d.slotStarveWarnedAt = now

	items := d.prefetchQueue.Items()
	reason := ""
	if len(items) > 0 && items[0].WU != nil {
		_, reason = d.canAccommodateWU(items[0].WU)
	}
	idleFor := now.Sub(d.slotStarvedSince).Round(time.Second).String()
	d.logger.Warn("an execution slot is idle but none of the buffered work units can start on this machine — requesting admissible work from the attached heads, but none has served any",
		"idle_for", idleFor,
		"buffered_units", len(items),
		"head_of_buffer_reason", reason)
	d.notices.Notify(NoticeWarn, "buffer_unrunnable",
		fmt.Sprintf("An execution slot has been idle for %s: %d work unit(s) are buffered but none can start on this machine (%s). The daemon is asking the attached heads for work that fits, but none has served any yet.",
			idleFor, len(items), reason),
		"", "")
}

// warnDiskGateOnce surfaces the disk-space stall. The first time the gate
// blocks all fetching it logs a single actionable WARN carrying the gate's own
// reason (which names the numbers and the setting involved); subsequent blocked
// polls stay at Debug so the log isn't spammed. clearDiskGateWarning resets it
// so a later recovery and re-stall warns again.
//
// leafLabel and leafID name the representative gated leaf (empty when the
// stall is the absolute floor or the any-leaf fallback); raiseToGB is the
// max_disk_gb that would cover that leaf on this machine today (0 = not
// applicable). Both go into the volunteer-facing notice, which must name the
// leaf and the allowance that clears it — a refusal that names neither sent a
// tester on a raise-and-chase (TB-41).
func (d *Daemon) warnDiskGateOnce(reason, leafLabel, leafID string, raiseToGB int) {
	d.diskGateMu.Lock()
	already := d.diskGateWarned
	d.diskGateWarned = true
	d.diskGateMu.Unlock()

	if already {
		d.logger.Debug("shouldFetch: still disk-gated", "reason", reason)
		return
	}

	d.logger.Warn("not fetching work: disk-gated — this volunteer stays idle until it clears",
		"reason", reason,
		"data_dir_free_mb", client.DiskAvailableMB(d.cfg.DataDir))

	msg := "Not fetching work: " + reason + "."
	if leafLabel != "" && raiseToGB > 0 {
		msg += fmt.Sprintf(" Leaf %q would be covered by max_disk_gb = %d (currently %d).",
			leafLabel, raiseToGB, d.cfg.ResourceLimits.MaxDiskGB)
	} else if raiseToGB > 0 {
		msg += fmt.Sprintf(" The attached leafs would be covered by max_disk_gb = %d (currently %d).",
			raiseToGB, d.cfg.ResourceLimits.MaxDiskGB)
	}
	d.notices.Notify(NoticeWarn, "disk_gate_blocked", msg, "", leafID)
}

// clearDiskGateWarning re-arms the disk-gate WARN after the gate clears, and
// resolves the notice the stall raised — for every leaf, since the gate
// passing means no leaf is blocked any more.
func (d *Daemon) clearDiskGateWarning() {
	d.diskGateMu.Lock()
	wasWarned := d.diskGateWarned
	d.diskGateWarned = false
	d.diskGateMu.Unlock()
	if wasWarned {
		d.logger.Info("disk space recovered: resuming work fetching")
		d.notices.Resolve("disk_gate_blocked", "", "")
	}
}

// readinessCounts tallies, per attached head, how many enabled leafs this
// volunteer can ACTUALLY receive and run — applying the same gates the fetcher
// applies (leafRuntimeVerdict): a container leaf needs a registered container
// runtime AND per-head CONTAINER trust; a native-only leaf needs per-head
// NATIVE trust (native code is always machine-runnable, so trust is its only
// gate); WASM is always trusted. Counting a leaf the per-head trust would
// refuse produced the "eligible: 1" line for a volunteer that could never
// receive work (PB-5).
func (d *Daemon) readinessCounts() (total, eligible, containerBlocked, trustBlocked int) {
	for _, srv := range d.multiClient.Servers() {
		for _, lf := range d.enabledLeafs(srv.Name) {
			total++
			rt, missing, untrusted := leafRuntimeVerdict(lf, d.runtimeRegistry, srv.Config)
			switch {
			case rt == "container" && missing:
				containerBlocked++
			case untrusted:
				trustBlocked++
			default:
				eligible++
			}
		}
	}
	return total, eligible, containerBlocked, trustBlocked
}

// logReadiness logs a one-shot startup banner: the runtimes this volunteer can
// actually run, free disk vs the configured allowance, and how many of the
// attached leafs it is eligible for (per-head trust included — see
// readinessCounts). When nothing is runnable it escalates to WARN with the
// reason and remedy, so a misconfigured volunteer learns why in seconds instead
// of sitting silently idle.
func (d *Daemon) logReadiness() {
	if d.runtimeRegistry == nil || d.multiClient == nil {
		return
	}
	runtimes := d.runtimeRegistry.AvailableRuntimes()
	hasContainer := d.runtimeRegistry.GetRuntime("container") != nil

	availableMB := client.DiskAvailableMB(d.cfg.DataDir)
	allowanceMB := d.cfg.ResourceLimits.MaxDiskGB * 1024

	totalLeafs, eligibleLeafs, containerBlocked, trustBlocked := d.readinessCounts()

	d.logger.Info("volunteer ready",
		"runtimes", runtimes,
		"data_dir", d.cfg.DataDir,
		"disk_free_mb", availableMB,
		"disk_allowance_mb", allowanceMB,
		"eligible_leafs", eligibleLeafs,
		"trust_blocked_leafs", trustBlocked,
		"total_leafs", totalLeafs,
	)

	// "Connected, but you will get no work" — the actionable case worth a WARN.
	if totalLeafs > 0 && eligibleLeafs == 0 {
		switch {
		case containerBlocked == totalLeafs && !hasContainer:
			d.logger.Warn("no runnable leafs: every attached leaf needs a container runtime, but none is available here — install Docker or Podman (see the volunteer setup docs), or attach a head that has native leafs",
				"runtimes", runtimes, "container_leafs", containerBlocked)
		case trustBlocked > 0:
			d.logger.Warn("no runnable leafs: the attached leafs need runtimes this volunteer has not trusted their heads to run — opt in per head with 'lettuce-volunteer heads trust <head> <runtime>' if you accept running that head's code",
				"runtimes", runtimes, "trust_blocked_leafs", trustBlocked, "total_leafs", totalLeafs)
		default:
			d.logger.Warn("no runnable leafs: none of the attached leafs match this volunteer's available runtimes",
				"runtimes", runtimes, "total_leafs", totalLeafs)
		}
	}
}

// imageStorePaths returns the filesystem path(s) where the container backend
// stores images and extracts layers — normally one (Docker DockerRootDir /
// Podman graphroot), but more than one under the Docker containerd snapshotter
// (DockerRootDir plus the containerd content root, a different filesystem the
// blobs and overlayfs snapshots actually land on). ok is false when there is no
// container runtime, the backend can't be queried, or it reports no path —
// callers then skip the image-store disk gate rather than block, preserving the
// pre-#31 behavior for native-only volunteers or an unreachable engine. The
// result is cached for imageCacheCheckTTL so the fetch gate doesn't issue an
// /info call on every loop iteration. (TODO #31 + containerd-snapshotter
// follow-up.)
func (d *Daemon) imageStorePaths() ([]string, bool) {
	d.imgStoreMu.Lock()
	if !d.imgStoreChecked.IsZero() && time.Since(d.imgStoreChecked) < imageCacheCheckTTL {
		paths, known := d.imgStorePaths, d.imgStoreKnown
		d.imgStoreMu.Unlock()
		return paths, known
	}
	d.imgStoreMu.Unlock()

	paths, known := d.probeImageStorePaths()

	d.imgStoreMu.Lock()
	d.imgStoreChecked = time.Now()
	d.imgStorePaths = paths
	d.imgStoreKnown = known
	d.imgStoreMu.Unlock()
	return paths, known
}

func (d *Daemon) probeImageStorePaths() ([]string, bool) {
	if d.runtimeRegistry == nil {
		return nil, false
	}
	cr, ok := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime)
	if !ok || cr == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := cr.Client().Info(ctx)
	if err != nil || info == nil {
		return nil, false
	}
	paths := info.ImageStorePaths
	if len(paths) == 0 {
		// Older engine info / a backend that reports only the single data-root:
		// fall back to it so behavior is unchanged from pre-snapshotter resolution.
		if info.StoragePath == "" {
			return nil, false
		}
		paths = []string{info.StoragePath}
	}
	return paths, true
}

// allEnabledImageRefs returns the container image references of every enabled
// leaf across all attached heads — the set of images the volunteer wants cached.
// It is the keep-set for the container runtime's stale-image reaper, so a leaf's
// image is never reaped while another active leaf still needs it (e.g. grep-cpu
// :1.2 and grep-gpu :1.3-gpu, which share one repository).
func (d *Daemon) allEnabledImageRefs() []string {
	if d.multiClient == nil {
		return nil
	}
	seen := make(map[string]bool)
	var refs []string
	for _, srv := range d.multiClient.Servers() {
		for _, lf := range d.enabledLeafs(srv.Name) {
			if lf.ExecutionSpec == nil || lf.ExecutionSpec.Image == "" || seen[lf.ExecutionSpec.Image] {
				continue
			}
			seen[lf.ExecutionSpec.Image] = true
			refs = append(refs, lf.ExecutionSpec.Image)
		}
	}
	return refs
}

// abandonItem returns an un-run prefetched unit to the head so it isn't orphaned
// as ASSIGNED. Uses a detached context with a short timeout so it still reaches
// the head during shutdown, when the run context is already cancelled. See item 4.
// Flagged unrun_giveback: every caller returns a BUFFERED unit this volunteer
// never computed (shutdown clear, failed slot start), so the head closes the copy
// RETURNED — budget-neutral (TB-35). The head verifies the copy really never
// run-started before honoring the flag, so a start that failed AFTER StartWork
// landed still closes ABANDONED.
func (d *Daemon) abandonItem(item *PreFetchItem, reason string) {
	if item == nil || item.Conn == nil || item.Conn.Client == nil || item.WU == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := item.Conn.Client.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:    item.WU.ID,
		VolunteerId:   item.Conn.VolunteerID,
		PublicKey:     d.pubKey,
		Reason:        reason,
		UnrunGiveback: true,
	}); err != nil {
		d.logger.Warn("failed to abandon un-run work unit", "work_unit_id", item.WU.ID, "error", err)
		return
	}
	d.logger.Info("abandoned un-run work unit back to head", "work_unit_id", item.WU.ID, "reason", reason)
}

// abandonReasonLogTailBytes bounds the execution-log excerpt appended to an
// abandon reason. The reason crosses the wire and is written to the head's log,
// so it has to stay one readable line — long enough for the decisive last lines
// of a failing process, short enough not to swamp the operator's log.
const abandonReasonLogTailBytes = 500

// withLogTail appends an execution-log excerpt to an abandon reason, or returns
// the reason unchanged when there was nothing to read.
func withLogTail(reason, tail string) string {
	if tail == "" {
		return reason
	}
	return reason + "; output: " + tail
}

// logTailOrNone renders an execution-log excerpt for a log field, naming the
// absence explicitly so an empty tail is not mistaken for silent output.
func logTailOrNone(tail string) string {
	if tail == "" {
		return "(none captured)"
	}
	return tail
}

// noteLeafFailure records one local failure of a work unit's leaf against the
// per-leaf breaker, and emits the single loud WARN when this failure is the one
// that trips it. The volunteer is told what is happening and what it means —
// before this, a leaf that failed on every attempt produced nothing a volunteer
// would recognize as a problem with that leaf (TB-10).
func (d *Daemon) noteLeafFailure(wu *runtime.WorkUnit, reason string) {
	if d.leafFailures == nil || wu == nil {
		return
	}
	count, tripped := d.leafFailures.RecordFailure(wu.LeafID, reason)
	if !tripped {
		return
	}
	name, slug := d.resolveLeafInfo(wu.LeafID)
	d.logger.Warn("leaf keeps failing on this machine — pausing requests for it",
		"leaf_id", wu.LeafID,
		"leaf_name", name,
		"leaf_slug", slug,
		"consecutive_failures", count,
		"last_reason", reason,
		"cooldown", leafFailureCooldown,
		"remedy", "this leaf's work fails locally every time, so requesting more of it only churns units; the daemon will retry it after the cooldown. Check the log lines above for the process output, and report it to the head's operator if it persists")
	label := name
	if label == "" {
		label = slug
	}
	if label == "" {
		label = wu.LeafID
	}
	d.notices.Notify(NoticeWarn, "leaf_failing",
		fmt.Sprintf("Leaf %q keeps failing on this machine (%d consecutive failures; last reason: %s). Requests for it are paused for %s, then retried once. If it persists, report it to the head's operator.",
			label, count, reason, leafFailureCooldown),
		"", wu.LeafID)
}

// noteLeafSuccess clears a leaf's failure streak after a clean run, marking the
// recovery in the log when it un-pauses a leaf the breaker had tripped.
func (d *Daemon) noteLeafSuccess(wu *runtime.WorkUnit) {
	if d.leafFailures == nil || wu == nil {
		return
	}
	if d.leafFailures.RecordSuccess(wu.LeafID) {
		name, slug := d.resolveLeafInfo(wu.LeafID)
		d.logger.Info("leaf recovered, resuming requests for it",
			"leaf_id", wu.LeafID, "leaf_name", name, "leaf_slug", slug)
		d.notices.Resolve("leaf_failing", "", wu.LeafID)
	}
}

// AvailableRuntimeNames returns the runtime kinds this daemon has registered and
// can execute (lowercase). Exported for the management API so a client can tell
// which leafs this machine will ever be able to run.
func (d *Daemon) AvailableRuntimeNames() []string {
	if d.runtimeRegistry == nil {
		return nil
	}
	return d.runtimeRegistry.AvailableRuntimes()
}

// HasGPU reports whether this machine has a GPU the daemon can use: one must be
// present in the hardware detected at startup, and GPU work must not be disabled
// in config. Serves the management API rather than re-running detection, which
// on Windows means another exec.
func (d *Daemon) HasGPU() bool {
	if d.cfg != nil && d.cfg.ResourceLimits.MaxGPUVRAMPct == 0 {
		return false
	}
	return d.cachedHW != nil && len(d.cachedHW.GetGpus()) > 0
}

// GPUBudget reports the GPU capabilities this daemon ADVERTISED to heads, in the
// same terms dispatch matches leafs against (TB-21): the largest allowed VRAM
// across this machine's GPUs, and the vendors and compute capabilities it offers.
//
// vramMB is VRAM * max_vram_pct / 100 per card, taking the largest — deliberately
// identical to the head's own computation, because a client comparing a leaf's
// requirement against raw card capacity would report machines eligible that the
// head refuses, which is the entire bug this exists to end. Read from the cached
// advertised hardware rather than re-detecting, so the answer is what the head was
// actually told, and so Windows does not pay for another exec.
// cardVRAMMB and vramPct describe the SAME card that produced vramMB, so a caller
// explaining the allowance quotes that card's own percentage rather than the global
// default — a per-GPU override in config makes those differ.
func (d *Daemon) GPUBudget() (vramMB, cardVRAMMB, vramPct int, vendors []string, computeCapabilities []string) {
	if !d.HasGPU() {
		return 0, 0, 0, nil, nil
	}
	for _, g := range d.cachedHW.GetGpus() {
		if eff := int(g.GetVramMb()) * int(g.GetMaxVramPct()) / 100; eff > vramMB {
			vramMB, cardVRAMMB, vramPct = eff, int(g.GetVramMb()), int(g.GetMaxVramPct())
		}
		if v := strings.ToUpper(strings.TrimSpace(g.GetVendor())); v != "" {
			vendors = append(vendors, v)
		}
		if cc := g.GetComputeCapability(); cc != "" {
			computeCapabilities = append(computeCapabilities, cc)
		}
	}
	return vramMB, cardVRAMMB, vramPct, vendors, computeCapabilities
}

// leafFailurePaused reports whether the per-leaf breaker is holding a leaf back.
// Called from the fetcher goroutine; the tracker takes its own lock.
func (d *Daemon) leafFailurePaused(leafID string) bool {
	if d.leafFailures == nil {
		return false
	}
	return d.leafFailures.Paused(leafID)
}

// LeafFailureSnapshot reports every leaf that has failed locally since the daemon
// started, newest first. Read by the management API so `status` and `leafs list`
// can show repeated failures instead of leaving them to head-side forensics.
func (d *Daemon) LeafFailureSnapshot() []LeafFailureSummary {
	if d.leafFailures == nil {
		return nil
	}
	return d.leafFailures.Snapshot()
}

// abandonUnit closes this volunteer's live copy (RESERVED or RUNNING) of a locally
// failed unit — non-zero exit, deadline-exceeded, runtime failure — back to the head
// as ABANDONED. AbandonWorkUnit is the protocol's only failure signal (SubmitResult
// carries no status), so without it a failed unit strands until the head's fault-monitor
// deadline sweep closes it EXPIRED — a full deadline window of latency per failure.
// Abandoning requeues it immediately, and ABANDONED copies feed the max_error_copies
// dead-letter ceiling. Detached context with a short timeout so it still reaches the
// head during shutdown drain, when the run context is already cancelled.
func (d *Daemon) abandonUnit(wu *runtime.WorkUnit, conn *ServerConnection, reason string) {
	if wu == nil || conn == nil || conn.Client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := conn.Client.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  wu.ID,
		VolunteerId: conn.VolunteerID,
		PublicKey:   d.pubKey,
		Reason:      reason,
	}); err != nil {
		d.logger.Warn("failed to abandon failed work unit", "work_unit_id", wu.ID, "error", err)
		return
	}
	d.logger.Info("abandoned failed work unit back to head", "work_unit_id", wu.ID, "reason", reason)
}

// restoreCheckpoint downloads and extracts a checkpoint for a work unit.
func (d *Daemon) restoreCheckpoint(ctx context.Context, wu *runtime.WorkUnit, prep *runtime.PrepareResult, conn *ServerConnection) {
	resp, getErr := conn.Client.GetCheckpoint(ctx, &lettucev1.GetCheckpointRequest{
		WorkUnitId: wu.ID,
	})
	if getErr != nil {
		d.logger.Warn("failed to download checkpoint, starting fresh",
			"work_unit_id", wu.ID,
			"error", getErr,
		)
		return
	}
	if !resp.HasCheckpoint {
		return
	}
	checkpointDir := filepath.Join(prep.WorkDir, "checkpoint")
	if mkErr := os.MkdirAll(checkpointDir, 0755); mkErr != nil {
		d.logger.Warn("failed to create checkpoint dir", "error", mkErr)
		return
	}
	if exErr := extractTar(resp.CheckpointData, checkpointDir); exErr != nil {
		d.logger.Warn("failed to extract checkpoint", "error", exErr)
		return
	}
	d.logger.Info("restored checkpoint",
		"work_unit_id", wu.ID,
		"sequence", resp.CheckpointSequence,
	)
}

// runSlotHeartbeat (per-task heartbeat loop) is removed: per-task heartbeats no
// longer exist. Run-start is now an explicit StartWork RPC issued at slot handoff
// (see SlotManager.runSlot), and liveness is deadline-based. The abort/abandon
// responsibilities the heartbeat used to carry (the #20 reassigned-out drop,
// server-requested abort) are surfaced at StartWork (Ok=false / terminal error ->
// drop the unit) and SubmitResult (FailedPrecondition -> drop) instead.

// checkPauseSignals drains pause/resume signals from all sources.
func (d *Daemon) checkPauseSignals(pauseCh chan bool) {
	for {
		select {
		case shouldPause := <-pauseCh:
			d.mu.Lock()
			d.paused = shouldPause
			if shouldPause {
				d.pauseReason = "scheduled"
				d.logger.Info("daemon paused by resource monitor")
			}
			d.mu.Unlock()
		case shouldPause := <-d.thermalPauseCh:
			d.mu.Lock()
			d.paused = shouldPause
			if shouldPause {
				d.pauseReason = "thermal"
				d.logger.Info("daemon paused due to thermal throttle")
			} else {
				d.logger.Info("daemon resumed from thermal throttle")
			}
			d.mu.Unlock()
		case shouldPause := <-d.userPauseCh:
			d.mu.Lock()
			d.userPaused = shouldPause
			d.mu.Unlock()
			if shouldPause {
				d.logger.Info("daemon paused by user")
			} else {
				d.logger.Info("daemon resumed by user")
			}
		default:
			return
		}
	}
}

// waitForResume blocks until the daemon is unpaused or ctx is cancelled.
// Returns false if ctx was cancelled.
func (d *Daemon) waitForResume(ctx context.Context, pauseCh chan bool) bool {
	for {
		d.mu.Lock()
		paused := d.paused || d.userPaused
		d.mu.Unlock()
		if !paused {
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case shouldPause := <-pauseCh:
			d.mu.Lock()
			d.paused = shouldPause
			if shouldPause {
				d.pauseReason = "scheduled"
			}
			d.mu.Unlock()
			if !shouldPause {
				d.logger.Info("daemon resumed by resource monitor")
			}
		case shouldPause := <-d.thermalPauseCh:
			d.mu.Lock()
			d.paused = shouldPause
			if shouldPause {
				d.pauseReason = "thermal"
			}
			d.mu.Unlock()
			if !shouldPause {
				d.logger.Info("daemon resumed from thermal throttle")
			}
		case shouldPause := <-d.userPauseCh:
			d.mu.Lock()
			d.userPaused = shouldPause
			d.mu.Unlock()
			if !shouldPause {
				d.logger.Info("daemon resumed by user")
			}
		}
	}
}

// waitForScheduleActive blocks until the scheduler says the daemon may run, and
// returns false only if ctx is cancelled while waiting.
//
// Crucially, if the schedule is currently inactive it first SUSPENDS any active
// slots before waiting, and RESUMES them once the schedule reopens. This matters
// for tasks resumed from a previous session: such a task is adopted into a slot and
// is already executing by the time the main loop reaches this gate. The plain
// schedule gate only blocks NEW slot-filling — it does not freeze running slots, and
// while the loop is parked here it cannot observe the resource monitor's pause
// signal either. Without suspending here, a resumed task would run straight through
// the entire off-schedule (or thermal/disk/user) window, silently violating the
// schedule. The suspend/resume pair lives in this one block so it stays balanced
// regardless of how the wait ends.
func (d *Daemon) waitForScheduleActive(ctx context.Context) bool {
	if d.scheduler == nil || d.scheduler.ShouldRun() {
		return true
	}

	hadActive := d.slotManager != nil && d.slotManager.ActiveCount() > 0
	if hadActive {
		d.slotManager.SuspendAll()
		d.logger.Info("schedule inactive: suspended active tasks until it reopens",
			"active_slots", d.slotManager.ActiveCount())
	} else {
		d.logger.Debug("scheduler says not active, waiting")
	}

	err := d.scheduler.WaitUntilActive(ctx)

	if hadActive {
		// Resume even on cancellation so the suspended processes are unfrozen for the
		// shutdown path to clean up; the SuspendAll above is otherwise unbalanced.
		// On cancellation, mark the shutdown first so ResumeAll treats a container
		// already cleaned up by its executor's racing cancel path as the success it
		// is rather than WARNing (TB-29).
		if err != nil {
			d.slotManager.SetShuttingDown()
		}
		d.slotManager.ResumeAll()
		if err == nil {
			d.logger.Info("schedule active again: resumed previously suspended tasks")
		}
	}

	return err == nil
}

// Stop signals the daemon to stop. Active work units are cancelled so the
// daemon can shut down promptly. Work directories are preserved for resumption.
func (d *Daemon) Stop() {
	d.mu.Lock()
	d.stopping = true
	cancel := d.runCancel
	d.mu.Unlock()
	// Cancel the run context to interrupt all active slot execution.
	// The slot cleanup will preserve work directories (shuttingDown flag).
	if cancel != nil {
		cancel()
	}
}

// osExitFunc is the function called to exit the process. Defaults to os.Exit.
// Tests override this via SetOsExitFunc to prevent actual process termination.
var osExitFunc = os.Exit

// SetOsExitFunc overrides the os.Exit function used by SuspendAndQuit.
// Returns a restore function. Intended for testing only.
func SetOsExitFunc(fn func(int)) func() {
	prev := osExitFunc
	osExitFunc = fn
	return func() { osExitFunc = prev }
}

// SuspendAndQuit suspends all compute processes, saves their PIDs to disk,
// releases children from the process group (so they survive as frozen orphans),
// and exits the daemon process immediately. The next daemon launch will find
// the orphans by PID and resume them — zero work lost.
//
// We use os.Exit instead of d.Stop because Stop() cancels the run context,
// which causes exec.CommandContext to kill the suspended processes — defeating
// the entire purpose. os.Exit bypasses all defers, keeping orphans alive.
func (d *Daemon) SuspendAndQuit() {
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()

	// Suspend all running processes (NtSuspendProcess / SIGSTOP).
	if d.slotManager != nil {
		d.slotManager.SuspendAll()
		d.logger.Info("suspended all processes for quit",
			"active_slots", d.slotManager.ActiveCount())

		// Persist active tasks with PIDs so next launch can resume them.
		d.persistActiveTasks()
	}

	// Release children from process group so they survive daemon exit.
	// On Windows: removes KILL_ON_JOB_CLOSE from the Job Object.
	// On Unix: clears tracked pgids so Terminate() won't kill them.
	if d.processGroup != nil {
		d.processGroup.ReleaseChildren()
	}

	d.logger.Info("exiting daemon, orphan processes will survive frozen")

	// Exit immediately. Do NOT call d.Stop() — it cancels the run context,
	// which triggers exec.CommandContext to kill the suspended processes.
	// os.Exit skips all defers, which is intentional: the cleanup defer in
	// Run() would call processGroup.Terminate() and kill everything.
	osExitFunc(0)
}

// IsRunning returns true if the daemon loop is active.
func (d *Daemon) IsRunning() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

// Pause pauses the daemon from fetching new work units. Does not cancel
// the current work unit in progress.
func (d *Daemon) Pause() error {
	d.mu.Lock()
	if d.userPaused {
		d.mu.Unlock()
		return fmt.Errorf("already paused")
	}
	d.userPaused = true
	d.pauseReason = "user"
	d.mu.Unlock()
	// Signal the daemon loop (non-blocking).
	select {
	case d.userPauseCh <- true:
	default:
	}
	return nil
}

// Resume resumes the daemon after a user-initiated pause.
func (d *Daemon) Resume() error {
	d.mu.Lock()
	if !d.userPaused {
		d.mu.Unlock()
		return fmt.Errorf("not paused")
	}
	d.userPaused = false
	d.mu.Unlock()
	select {
	case d.userPauseCh <- false:
	default:
	}
	return nil
}

// IsPaused returns true if the daemon is paused by any source. The schedule
// verdict comes from LIVE policy, not only the signal-driven fields: the main
// loop's gate (waitForScheduleActive) parks and suspends slots without setting
// them, and it usually beats the resource monitor's pause signal to a closing
// window — always on a daemon booted inside one — so a gate-park was
// unrepresentable here and `status` showed an active, unexplained daemon for
// the whole window (TB-44).
func (d *Daemon) IsPaused() bool {
	d.mu.Lock()
	paused := d.paused || d.userPaused
	d.mu.Unlock()
	return paused || d.scheduleClosed()
}

// PauseReason returns the reason the daemon is paused, or empty string if not
// paused. A user pause outranks everything (it is the state `resume` undoes);
// then the signal-driven reason; then the live schedule verdict (TB-44).
func (d *Daemon) PauseReason() string {
	d.mu.Lock()
	if d.userPaused {
		d.mu.Unlock()
		return "user"
	}
	if d.paused {
		reason := d.pauseReason
		d.mu.Unlock()
		return reason
	}
	d.mu.Unlock()
	if d.scheduleClosed() {
		return "scheduled"
	}
	return ""
}

// scheduleClosed reports whether the scheduler currently forbids running —
// the live-policy half of IsPaused/PauseReason. The scheduler is set once at
// construction, so it is read without d.mu; ShouldRun takes no daemon locks.
func (d *Daemon) scheduleClosed() bool {
	return d.scheduler != nil && !d.scheduler.ShouldRun()
}

// CurrentTask holds info about an in-progress work unit.
type CurrentTask struct {
	WorkUnitID            string
	LeafID                string
	StartedAt             time.Time // original (first-ever) start, for reference
	ElapsedSeconds        int       // run time accrued across sessions (excludes daemon-down gap)
	WorkDir               string
	VizBundlePath         string
	CheckpointSequence    int32
	LastCheckpointAt      time.Time
	ResumedFromCheckpoint bool
	EstimatedSeconds      float64 // benchmark-based estimate (0 = unknown)
	Suspended             bool
	TotalPausedSeconds    int
	DeadlineSeconds       int32
	RuntimeType           string // "native", "container", or "wasm"
	ContainerImage        string
	ServerName            string
	ProcessID             int
	FetchedAt             time.Time
}

// GetCurrentTasks returns info about all in-progress work units across all slots.
func (d *Daemon) GetCurrentTasks() []CurrentTask {
	if d.slotManager == nil {
		return nil
	}
	var dcfFunc func(string) float64
	if d.dcfTracker != nil {
		dcfFunc = d.dcfTracker.Get
	}
	return d.slotManager.GetCurrentTasks(d.benchmarkFPOPS, dcfFunc)
}

// SuspendTask suspends a single task by work unit ID.
func (d *Daemon) SuspendTask(workUnitID string) error {
	if d.slotManager == nil {
		return ErrTaskNotFound
	}
	return d.slotManager.SuspendSlot(workUnitID)
}

// ResumeTask resumes a single suspended task by work unit ID.
// Returns ErrDaemonPaused if the daemon is paused (resume blocked at daemon level).
func (d *Daemon) ResumeTask(workUnitID string) error {
	if d.slotManager == nil {
		return ErrTaskNotFound
	}
	if d.IsPaused() {
		return ErrDaemonPaused
	}
	return d.slotManager.ResumeSlot(workUnitID)
}

// AbortTask cancels a single task by work unit ID, killing its process.
func (d *Daemon) AbortTask(workUnitID string) error {
	if d.slotManager == nil {
		return ErrTaskNotFound
	}
	return d.slotManager.AbortSlot(workUnitID)
}

// GetQueuedCount returns the number of work units in the prefetch queue.
func (d *Daemon) GetQueuedCount() int {
	if d.prefetchQueue == nil {
		return 0
	}
	return d.prefetchQueue.Len()
}

// QueuedTask describes a work unit waiting in the prefetch queue.
type QueuedTask struct {
	WorkUnitID      string
	LeafID          string
	DeadlineSeconds int32
	FetchedAt       time.Time
	ServerName      string
}

// GetQueuedTasks returns details of all work units in the prefetch queue.
func (d *Daemon) GetQueuedTasks() []QueuedTask {
	if d.prefetchQueue == nil {
		return nil
	}
	items := d.prefetchQueue.Items()
	tasks := make([]QueuedTask, 0, len(items))
	for _, item := range items {
		serverName := ""
		if item.Conn != nil {
			serverName = item.Conn.Config.DisplayName()
		}
		tasks = append(tasks, QueuedTask{
			WorkUnitID:      item.WU.ID,
			LeafID:          item.WU.LeafID,
			DeadlineSeconds: item.WU.DeadlineSeconds,
			FetchedAt:       item.FetchedAt,
			ServerName:      serverName,
		})
	}
	return tasks
}

// GetStartedAt returns when the daemon started running.
func (d *Daemon) GetStartedAt() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.startedAt
}

// GetConfig returns the current daemon configuration.
func (d *Daemon) GetConfig() *config.Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg
}

// GetMultiClient returns the multi-server client.
func (d *Daemon) GetMultiClient() *MultiServerClient {
	return d.multiClient
}

// ApplyConfig applies new configuration to the running daemon without restart.
// Changing max_concurrent_tasks requires a restart — slot count is fixed at init.
func (d *Daemon) ApplyConfig(newCfg *config.Config) {
	d.mu.Lock()
	oldMax := d.cfg.MaxConcurrentTasks
	d.cfg = newCfg
	d.mu.Unlock()

	if newCfg.MaxConcurrentTasks != oldMax && oldMax > 0 {
		d.logger.Warn("max_concurrent_tasks changed — restart daemon to apply",
			"old", oldMax,
			"new", newCfg.MaxConcurrentTasks,
		)
	}

	// Reinitialize weights from new config.
	d.initializeWeights()
}

// SetBackoff overrides backoff durations (for testing).
func (d *Daemon) SetBackoff(initial, max time.Duration) {
	d.initialBackoff = initial
	d.maxBackoff = max
}

// leafPreferences returns the leaf ID filter and block list from config.
func (d *Daemon) leafPreferences() (leafIDs, blockedIDs []string) {
	switch d.cfg.Leafs.Mode {
	case "SPECIFIC":
		leafIDs = d.cfg.Leafs.LeafIDs
	case "BLOCKLIST":
		blockedIDs = d.cfg.Leafs.BlockedIDs
	}
	return
}

// GetLeafCache returns the daemon's leaf cache (for management API access).
func (d *Daemon) GetLeafCache() *LeafCache {
	return d.leafCache
}

// GetWeightedSelector returns the daemon's weighted selector (for management API access).
func (d *Daemon) GetWeightedSelector() *WeightedSelector {
	return d.weightedSelector
}

// GetMachineManager returns the Podman machine manager, or nil if not configured.
func (d *Daemon) GetMachineManager() *runtime.PodmanMachineManager {
	return d.machineManager
}

// SetSlotManagerForTest injects a SlotManager into the daemon for testing.
// This allows external test packages (e.g., management) to exercise task
// visibility and per-task control endpoints without running the full daemon loop.
func (d *Daemon) SetSlotManagerForTest(sm *SlotManager) {
	d.slotManager = sm
}

// SetMultiClientForTest injects a MultiServerClient into the daemon for testing.
// This allows external test packages (e.g., management) to test GetHeads()
// volunteer ID population without running the full daemon loop.
func (d *Daemon) SetMultiClientForTest(mc *MultiServerClient) {
	d.multiClient = mc
}

// RecordLeafFailureForTest drives the per-leaf failure breaker directly, so an
// external test package (e.g. management) can assert that a recorded failure
// reaches the API without running a work unit.
func (d *Daemon) RecordLeafFailureForTest(leafID, reason string) {
	d.noteLeafFailure(&runtime.WorkUnit{LeafID: leafID}, reason)
}

// initializeWeights computes effective leaf weights from cache + config preferences.
func (d *Daemon) initializeWeights() {
	headWeights := make(map[string]int)
	for _, srv := range d.cfg.Servers {
		name := srv.DisplayName()
		w := srv.Weight
		if w <= 0 {
			w = 100
		}
		headWeights[name] = w

		// Compute effective leaf weights for this server.
		defaults := d.leafCache.GetDefaultWeights(name)
		lp := srv.LeafPreferences
		mode := lp.Mode
		if mode == "" {
			mode = "ALL"
		}

		effective := make(map[string]int)
		switch mode {
		case "ALL":
			// Start with researcher defaults.
			for slug, dw := range defaults {
				effective[slug] = dw
			}
			// Overlay any custom weights.
			for slug, cw := range lp.Weights {
				effective[slug] = cw
			}
		case "SPECIFIC":
			enabledSet := make(map[string]bool, len(lp.Enabled))
			for _, slug := range lp.Enabled {
				enabledSet[slug] = true
			}
			for slug := range enabledSet {
				if cw, ok := lp.Weights[slug]; ok {
					effective[slug] = cw
				} else if dw, ok := defaults[slug]; ok {
					effective[slug] = dw
				} else {
					effective[slug] = 100
				}
			}
		case "BLOCKLIST":
			disabledSet := make(map[string]bool, len(lp.Disabled))
			for _, slug := range lp.Disabled {
				disabledSet[slug] = true
			}
			for slug, dw := range defaults {
				if disabledSet[slug] {
					continue
				}
				if cw, ok := lp.Weights[slug]; ok {
					effective[slug] = cw
				} else {
					effective[slug] = dw
				}
			}
			// Also include leafs from cache that aren't in defaults but exist.
			leafs := d.leafCache.GetLeafs(name)
			for _, leaf := range leafs {
				if disabledSet[leaf.Slug] {
					continue
				}
				if _, ok := effective[leaf.Slug]; !ok {
					if cw, ok := lp.Weights[leaf.Slug]; ok {
						effective[leaf.Slug] = cw
					} else {
						effective[leaf.Slug] = 100
					}
				}
			}
		}

		d.weightedSelector.SetLeafWeights(name, effective)
	}
	d.weightedSelector.SetHeadWeights(headWeights)
}

// availableServers returns servers not currently in backoff.
func (d *Daemon) availableServers() []*ServerConnection {
	var available []*ServerConnection
	for _, srv := range d.multiClient.Servers() {
		if srv.Available || time.Since(srv.LastError) >= srv.Backoff {
			available = append(available, srv)
		}
	}
	return available
}

// enabledLeafs returns the leafs the fetcher should poll on a head: the cached
// public catalog filtered by the server's leaf preferences, PLUS the head's
// explicitly pinned leafs (attach --leaf, PB-16). Pins are appended even when
// the catalog does not list them — UNLISTED/PRIVATE leafs are absent from
// GetHeadInfo by design and are reachable only by requesting them by id — and
// they bypass the slug-based preference filters (an explicit attach is the
// stronger signal, and an unlisted leaf has no slug to filter on anyway).
func (d *Daemon) enabledLeafs(serverName string) []CachedLeafInfo {
	leafs := d.leafCache.GetLeafs(serverName)

	// Find the server config.
	var lp config.LeafPreferences
	var pinned []string
	for _, srv := range d.cfg.Servers {
		if srv.DisplayName() == serverName {
			lp = srv.LeafPreferences
			pinned = srv.PinnedLeafIDs
			break
		}
	}

	if leafs == nil && len(pinned) == 0 {
		return nil
	}

	mode := lp.Mode
	if mode == "" {
		mode = "ALL"
	}

	var result []CachedLeafInfo
	switch mode {
	case "SPECIFIC":
		enabledSet := make(map[string]bool, len(lp.Enabled))
		for _, slug := range lp.Enabled {
			enabledSet[slug] = true
		}
		for _, leaf := range leafs {
			if enabledSet[leaf.Slug] {
				result = append(result, leaf)
			}
		}
	case "BLOCKLIST":
		disabledSet := make(map[string]bool, len(lp.Disabled))
		for _, slug := range lp.Disabled {
			disabledSet[slug] = true
		}
		for _, leaf := range leafs {
			if !disabledSet[leaf.Slug] {
				result = append(result, leaf)
			}
		}
	default: // "ALL" and anything unrecognized
		result = append(result, leafs...)
	}

	// Append pins not already present. When the catalog knows the pinned leaf
	// (a PUBLIC leaf pinned explicitly) its cached info is used; otherwise a
	// minimal descriptor carries the id — the slug doubles as the id for
	// selector bookkeeping, and the per-unit prepare/trust gates do the rest.
	for _, pin := range pinned {
		already := false
		for _, leaf := range result {
			if leaf.ID == pin {
				already = true
				break
			}
		}
		if already {
			continue
		}
		info := CachedLeafInfo{ID: pin, Slug: pin, Name: pin, State: "ACTIVE"}
		for _, leaf := range leafs {
			if leaf.ID == pin {
				info = leaf
				break
			}
		}
		result = append(result, info)
	}
	return result
}

// serverBlockedLeafIDs returns the leaf IDs that a server's leaf_preferences
// exclude — every cached leaf for the server that enabledLeafs filters out
// (under SPECIFIC or BLOCKLIST mode). The steady-state fetch path already only
// requests enabled leaves by id, but the any-leaf fallback (used before the leaf
// cache is populated, or for heads that don't surface a catalog) would otherwise
// let the head dispatch a blocked leaf. Passing these as BlockedLeafIds makes the
// per-server preference authoritative at dispatch on every path. Returns nil when
// the cache is empty (nothing to translate slugs against yet).
func (d *Daemon) serverBlockedLeafIDs(serverName string) []string {
	all := d.leafCache.GetLeafs(serverName)
	if len(all) == 0 {
		return nil
	}
	enabled := make(map[string]bool, len(all))
	for _, lf := range d.enabledLeafs(serverName) {
		enabled[lf.ID] = true
	}
	var blocked []string
	for _, lf := range all {
		if lf.ID != "" && !enabled[lf.ID] {
			blocked = append(blocked, lf.ID)
		}
	}
	return blocked
}

// filterOut returns servers not in the excluded set.
func filterOut(servers []*ServerConnection, excluded map[string]bool) []*ServerConnection {
	var result []*ServerConnection
	for _, srv := range servers {
		if !excluded[srv.Name] {
			result = append(result, srv)
		}
	}
	return result
}

// buildSubmitRequest creates a SubmitResultRequest from a work unit, execution result, and server connection.
func (d *Daemon) buildSubmitRequest(wu *runtime.WorkUnit, result *runtime.ExecutionResult, conn *ServerConnection) *lettucev1.SubmitResultRequest {
	return &lettucev1.SubmitResultRequest{
		WorkUnitId:           wu.ID,
		VolunteerId:          conn.VolunteerID,
		PublicKey:            d.pubKey,
		OutputData:           result.OutputData,
		OutputChecksumSha256: result.OutputChecksum,
		Metadata:             runtime.MetricsToProto(&result.Metrics),
	}
}

// pendingResultRetryInterval is how often the retry worker resweeps persisted
// results that failed to submit.
const pendingResultRetryInterval = 60 * time.Second

// persistPendingResult marshals a completed result's submit request to disk so it
// can be retried after a submission failure. See item 6.
func (d *Daemon) persistPendingResult(wu *runtime.WorkUnit, result SlotResult, conn *ServerConnection, req *lettucev1.SubmitResultRequest) {
	blob, err := proto.Marshal(req)
	if err != nil {
		d.logger.Error("failed to marshal pending result", "work_unit_id", wu.ID, "error", err)
		return
	}
	wallClock := result.Result.Metrics.WallClockSeconds
	cpuSeconds := wallClock - int64(result.TotalPausedDur.Seconds())
	if cpuSeconds < 0 {
		cpuSeconds = 0
	}
	if err := SavePendingResult(d.cfg.DataDir, PendingResult{
		WorkUnitID:       wu.ID,
		LeafID:           wu.LeafID,
		ServerName:       conn.Name,
		RequestProto:     blob,
		WallClockSeconds: wallClock,
		CPUSeconds:       cpuSeconds,
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		d.logger.Error("failed to persist pending result", "work_unit_id", wu.ID, "error", err)
		return
	}
	d.logger.Info("persisted result for retry", "work_unit_id", wu.ID, "server", conn.Name)
}

// runPendingResultRetry resubmits persisted results until the head accepts them.
// It sweeps once on start (recovering results stranded by a previous run) and
// then every pendingResultRetryInterval until ctx is cancelled.
// bufferMaintenanceInterval is how often buffered units are re-checked for
// expiry independently of the fetcher. It only has to be short relative to the
// margins the sweep enforces — 10% of a unit's deadline, and
// reservationDropMargin (60s) before the head-side reservation lapses — not to
// the deadline itself.
const bufferMaintenanceInterval = 30 * time.Second

// runBufferMaintenance ages buffered work units out on a timer that is
// independent of the fetcher and of the coordinator loop (TB-19).
//
// Both of those stop during a pause: a thermal or resource pause cancels the
// fetcher's context, and the coordinator then blocks in waitForResume. The
// buffer, however, keeps ageing — a unit's head-side reservation lapses on wall
// clock whether or not this client is doing anything — so the sweep needs an
// owner that survives a pause. It is tied to the run context, so it stops only
// when the daemon does.
func (d *Daemon) runBufferMaintenance(ctx context.Context) {
	ticker := time.NewTicker(bufferMaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// The fetcher is replaced on every pause/resume, so read it under the
			// lock rather than capturing one at start-up. A nil fetcher (pre-start
			// or post-shutdown) simply means there is nothing to sweep yet.
			d.mu.Lock()
			f := d.fetcher
			d.mu.Unlock()
			if f != nil {
				f.sweepBuffer()
			}
		}
	}
}

func (d *Daemon) runPendingResultRetry(ctx context.Context) {
	d.retryPendingResults(ctx)
	ticker := time.NewTicker(pendingResultRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.retryPendingResults(ctx)
		}
	}
}

// retryPendingResults attempts one resubmission of each persisted result. A
// result is deleted once it reaches the head (accepted or rejected — either way
// retrying won't change the verdict); only transport failures keep it for the
// next sweep.
func (d *Daemon) retryPendingResults(ctx context.Context) {
	pending, err := ListPendingResults(d.cfg.DataDir)
	if err != nil {
		d.logger.Warn("failed to list pending results", "error", err)
		return
	}
	for _, pr := range pending {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn := d.serverByName(pr.ServerName)
		if conn == nil {
			d.logger.Debug("pending result: no connection for server, will retry later",
				"server", pr.ServerName, "work_unit_id", pr.WorkUnitID)
			continue
		}

		var req lettucev1.SubmitResultRequest
		if err := proto.Unmarshal(pr.RequestProto, &req); err != nil {
			d.logger.Error("pending result: corrupt request, dropping",
				"work_unit_id", pr.WorkUnitID, "error", err)
			_ = DeletePendingResult(d.cfg.DataDir, pr.WorkUnitID)
			continue
		}

		submitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		resp, err := conn.Client.SubmitResult(submitCtx, &req)
		cancel()
		if err != nil {
			// Classify the failure. A definitive (terminal) gRPC rejection means
			// the result reached the head and was rejected on its content/identity;
			// resending the identical bytes can never succeed, so drop the file to
			// stop the unbounded disk+RPC retry leak. Anything else (transport
			// failure, non-status error, or a transient/unclassified code) is kept
			// for the next sweep.
			if st, ok := status.FromError(err); ok && isTerminalSubmitCode(st.Code()) {
				d.logger.Warn("pending result: rejected by head, dropping",
					"work_unit_id", pr.WorkUnitID, "server", pr.ServerName,
					"code", st.Code(), "message", st.Message())
				if delErr := DeletePendingResult(d.cfg.DataDir, pr.WorkUnitID); delErr != nil {
					d.logger.Warn("pending result: failed to delete after rejection",
						"work_unit_id", pr.WorkUnitID, "error", delErr)
				}
				continue
			}
			d.logger.Warn("pending result: resubmit failed, will retry later",
				"work_unit_id", pr.WorkUnitID, "server", pr.ServerName, "error", err)
			continue
		}

		// Reached the head — stop retrying regardless of accept/reject.
		if err := DeletePendingResult(d.cfg.DataDir, pr.WorkUnitID); err != nil {
			d.logger.Warn("pending result: failed to delete after resubmit",
				"work_unit_id", pr.WorkUnitID, "error", err)
		}
		d.logger.Info("pending result resubmitted",
			"work_unit_id", pr.WorkUnitID, "accepted", resp.Accepted, "server", pr.ServerName)
		d.recordHistory(&runtime.WorkUnit{ID: pr.WorkUnitID, LeafID: pr.LeafID},
			pr.WallClockSeconds, pr.CPUSeconds, resp.Accepted, pr.ServerName)
	}
}

// isTerminalSubmitCode reports whether a gRPC status code from SubmitResult is a
// definitive, resend-invariant rejection of the result. For these codes the head
// reached a verdict on the request's fixed content or identity (parse/validation
// failure, checksum mismatch, missing entity, key mismatch, closed assignment, or
// an existing record), so resending the identical persisted bytes can never
// succeed and the file should be dropped. Every other code — transport/availability
// failures (Unavailable, DeadlineExceeded, Canceled), server-side faults (Internal,
// ResourceExhausted), and the catch-all codes.Unknown (which also covers non-status
// errors, since status.FromError yields Unknown for them) — is transient: the result
// may still land on a later sweep, so it is kept and retried.
func isTerminalSubmitCode(code codes.Code) bool {
	switch code {
	case codes.InvalidArgument,
		codes.NotFound,
		codes.PermissionDenied,
		codes.FailedPrecondition,
		codes.AlreadyExists:
		return true
	default:
		return false
	}
}

// serverByName returns the active server connection with the given name, or nil.
func (d *Daemon) serverByName(name string) *ServerConnection {
	if d.multiClient == nil {
		return nil
	}
	for _, srv := range d.multiClient.Servers() {
		if srv.Name == name {
			return srv
		}
	}
	return nil
}

// recordHistory appends a history entry and logs a warning on failure.
//
// The entry carries the leaf's display name alongside its id when the head cache
// knows it, so `history` can show "extract2-student-crowd" rather than a UUID
// prefix even with the daemon stopped (TB-46). resolveLeafInfo answers with the
// id itself when the leaf is unknown; that is not a name and is not recorded.
func (d *Daemon) recordHistory(wu *runtime.WorkUnit, wallClockSeconds int64, cpuSeconds int64, accepted bool, serverName string) {
	leafName, _ := d.resolveLeafInfo(wu.LeafID)
	if leafName == wu.LeafID {
		leafName = ""
	}
	if histErr := AppendHistory(d.cfg.DataDir, HistoryEntry{
		WorkUnitID:       wu.ID,
		LeafID:           wu.LeafID,
		LeafName:         leafName,
		ServerName:       serverName,
		CompletedAt:      time.Now().UTC(),
		WallClockSeconds: wallClockSeconds,
		CPUSeconds:       cpuSeconds,
		ResultAccepted:   accepted,
	}); histErr != nil {
		d.logger.Warn("failed to write history entry", "error", histErr)
	}
}

// resolveLeafInfo looks up the display name and slug for a leaf ID from the cache.
func (d *Daemon) resolveLeafInfo(leafID string) (name, slug string) {
	if d.leafCache != nil {
		for _, leafs := range d.leafCache.AllLeafs() {
			for _, l := range leafs {
				if l.ID == leafID {
					return l.Name, l.Slug
				}
			}
		}
	}
	return leafID, leafID
}

// --- PID file management ---

// WritePID writes the current process PID to {dataDir}/daemon.pid.
func WritePID(dataDir string) error {
	pidPath := filepath.Join(dataDir, "daemon.pid")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}
	return os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644)
}

// RemovePID removes the PID file.
func RemovePID(dataDir string) {
	os.Remove(filepath.Join(dataDir, "daemon.pid"))
}

// ReadPID reads the PID from {dataDir}/daemon.pid.
func ReadPID(dataDir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "daemon.pid"))
	if err != nil {
		return 0, fmt.Errorf("reading PID file: %w", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("parsing PID: %w", err)
	}
	return pid, nil
}

// resumePersistedTasks loads tasks saved from a previous session and resumes
// them. Work directories were preserved on shutdown; execution restarts from
// any checkpoint files found in the work dir.
func (d *Daemon) resumePersistedTasks(ctx context.Context) {
	state, err := LoadActiveState(d.cfg.DataDir)
	if err != nil {
		d.logger.Warn("failed to load persisted tasks", "error", err)
		ClearActiveState(d.cfg.DataDir)
		return
	}
	if state == nil || len(state.Tasks) == 0 {
		return
	}

	d.logger.Info("found persisted tasks from previous session",
		"count", len(state.Tasks),
		"saved_at", state.SavedAt,
	)

	// Build a server lookup by gRPC address.
	serverByAddr := make(map[string]*ServerConnection)
	if mc := d.multiClient; mc != nil {
		for _, srv := range mc.Servers() {
			serverByAddr[srv.Config.GRPCAddress] = srv
		}
	}

	resumed := 0
	for _, pt := range state.Tasks {
		// Try to resume a suspended orphan process by PID first.
		// This handles the "tray quit" case: processes are frozen in memory,
		// daemon exited, now we're back. Just wake them up.
		if pt.PID > 0 && isProcessAliveFunc(pt.PID) {
			slotID := d.slotManager.AvailableSlotID()
			if slotID < 0 {
				d.logger.Warn("no slots available for PID resume", "pid", pt.PID)
				break
			}

			// Resume the frozen process.
			handle := NewNativeProcessHandle(pt.PID)
			if err := handle.Resume(); err != nil {
				d.logger.Warn("failed to resume orphan process, will re-execute",
					"pid", pt.PID, "error", err)
				d.slotManager.ReturnSlotID(slotID)
				// Fall through to normal re-execution below.
			} else {
				// Re-attach to process group so it's tracked.
				if d.processGroup != nil {
					_ = d.processGroup.Add(pt.PID)
				}

				// Wire up a slot to monitor this resumed process.
				conn := serverByAddr[pt.ServerGRPCAddress]
				if conn == nil {
					d.logger.Warn("server gone for resumed orphan, killing",
						"pid", pt.PID, "server", pt.ServerGRPCAddress)
					_ = handle.Suspend() // re-freeze, let it die naturally or kill
					d.slotManager.ReturnSlotID(slotID)
					continue
				}

				rt := d.runtimeRegistry.GetRuntime(pt.RuntimeName)
				if rt == nil {
					rt = d.runtimeRegistry.GetRuntime("native")
				}

				wu := &runtime.WorkUnit{
					ID:                        pt.WorkUnitID,
					LeafID:                    pt.LeafID,
					Runtime:                   pt.RuntimeName,
					CodeArtifactURL:           pt.CodeArtifactURL,
					ParametersJSON:            pt.ParametersJSON,
					DeadlineSeconds:           pt.DeadlineSeconds,
					EnvVars:                   pt.EnvVars,
					ExecutionSpec:             pt.ExecutionSpec,
					RscFpopsEst:               pt.RscFpopsEst,
					CheckpointSequence:        pt.CheckpointSequence,
					CheckpointIntervalSeconds: pt.CheckpointIntervalSecs,
					// Re-stamp the dispatching head from the re-attached connection so
					// the artifact netguard opt-in keeps its per-head scope on resume
					// (PB-31; same config-derived name the fetcher stamps).
					SourceHead: conn.Name,
				}

				prep := &runtime.PrepareResult{
					WorkDir:           pt.WorkDir,
					BinaryPath:        pt.BinaryPath,
					InputPath:         pt.InputPath,
					VizBundlePath:     pt.VizBundlePath,
					OrphanPID:         pt.PID, // Tell the slot to poll instead of executing
					OriginalStartedAt: pt.StartedAt,
					ElapsedAccrued:    time.Duration(pt.ElapsedAccruedSeconds) * time.Second,
					PausedAccrued:     time.Duration(pt.PausedAccruedSeconds) * time.Second,
				}

				item := &PreFetchItem{
					WU:        wu,
					Prep:      prep,
					Runtime:   rt,
					Conn:      conn,
					WUResp:    &lettucev1.WorkUnitAssignment{}, // heartbeat interval removed
					FetchedAt: time.Now(),
				}

				if startErr := d.slotManager.StartSlot(ctx, slotID, item, d); startErr != nil {
					d.logger.Warn("failed to start slot for resumed orphan",
						"pid", pt.PID, "error", startErr)
					d.slotManager.ReturnSlotID(slotID)
					continue
				}

				// Set the process handle on the slot so suspend/resume works.
				d.slotManager.SetProcessHandle(slotID, handle)

				resumed++
				d.logger.Info("resumed orphan process by PID",
					"pid", pt.PID, "work_unit_id", pt.WorkUnitID)
				continue
			}
		}

		// Verify work directory still exists on disk.
		if _, statErr := os.Stat(pt.WorkDir); statErr != nil {
			d.logger.Warn("work directory missing, skipping persisted task",
				"work_unit_id", pt.WorkUnitID, "work_dir", pt.WorkDir)
			continue
		}

		// Find the matching server connection.
		conn := serverByAddr[pt.ServerGRPCAddress]
		if conn == nil {
			d.logger.Warn("server no longer configured, skipping persisted task",
				"work_unit_id", pt.WorkUnitID, "server", pt.ServerGRPCAddress)
			// Clean up orphaned work dir.
			os.RemoveAll(pt.WorkDir)
			continue
		}

		// Find the runtime.
		rtName := pt.RuntimeName
		if rtName == "" {
			rtName = "native"
		}
		rt := d.runtimeRegistry.GetRuntime(rtName)
		if rt == nil {
			d.logger.Warn("runtime not available, skipping persisted task",
				"work_unit_id", pt.WorkUnitID, "runtime", rtName)
			os.RemoveAll(pt.WorkDir)
			continue
		}

		// Reconstruct the work unit. InputData is nil — the input file is
		// already on disk in the work directory.
		wu := &runtime.WorkUnit{
			ID:                        pt.WorkUnitID,
			LeafID:                    pt.LeafID,
			Runtime:                   pt.RuntimeName,
			CodeArtifactURL:           pt.CodeArtifactURL,
			ParametersJSON:            pt.ParametersJSON,
			DeadlineSeconds:           pt.DeadlineSeconds,
			EnvVars:                   pt.EnvVars,
			ExecutionSpec:             pt.ExecutionSpec,
			RscFpopsEst:               pt.RscFpopsEst,
			CheckpointSequence:        pt.CheckpointSequence,
			CheckpointIntervalSeconds: pt.CheckpointIntervalSecs,
			// Re-stamp the dispatching head from the re-attached connection so the
			// artifact netguard opt-in keeps its per-head scope on resume (PB-31).
			SourceHead: conn.Name,
			// Don't set HasCheckpoint: the work dir was preserved on shutdown, so the
			// leaf's checkpoint state is still local in {workDir}/checkpoint and the
			// re-executed binary picks it up via LETTUCE_CHECKPOINT_DIR — no download
			// from the head is needed (that path is for cross-volunteer reassignment).
		}

		prep := &runtime.PrepareResult{
			WorkDir:           pt.WorkDir,
			BinaryPath:        pt.BinaryPath,
			InputPath:         pt.InputPath,
			VizBundlePath:     pt.VizBundlePath,
			OriginalStartedAt: pt.StartedAt,
			ElapsedAccrued:    time.Duration(pt.ElapsedAccruedSeconds) * time.Second,
			PausedAccrued:     time.Duration(pt.PausedAccruedSeconds) * time.Second,
		}

		// Get a slot.
		slotID := d.slotManager.AvailableSlotID()
		if slotID < 0 {
			d.logger.Warn("no slots available, cannot resume remaining tasks",
				"resumed_so_far", resumed)
			break
		}

		// Build a synthetic PreFetchItem for StartSlot.
		item := &PreFetchItem{
			WU:        wu,
			Prep:      prep,
			Runtime:   rt,
			Conn:      conn,
			WUResp:    &lettucev1.WorkUnitAssignment{}, // heartbeat interval removed
			FetchedAt: time.Now(),
		}

		if startErr := d.slotManager.StartSlot(ctx, slotID, item, d); startErr != nil {
			d.logger.Warn("failed to resume persisted task",
				"work_unit_id", pt.WorkUnitID, "error", startErr)
			d.slotManager.ReturnSlotID(slotID)
			continue
		}

		resumed++
		d.logger.Info("resumed persisted task",
			"work_unit_id", pt.WorkUnitID,
			"leaf_id", pt.LeafID,
			"work_dir", pt.WorkDir,
			"checkpoint_seq", pt.CheckpointSequence,
		)
	}

	// Clear the state file now that we've processed it.
	ClearActiveState(d.cfg.DataDir)

	if resumed > 0 {
		d.logger.Info("task resumption complete", "resumed", resumed, "total", len(state.Tasks))
	}
}

// resumePrefetchBuffer re-enqueues the prefetch-buffer units persisted from a previous
// session (a non-graceful exit) so the volunteer reports them as held on its first
// request and the head keeps their reservations. Unlike resumePersistedTasks these are
// buffered, NOT started: they are pushed back into the prefetch queue and run normally
// when a slot frees. A unit whose work directory, server, or runtime is gone is dropped
// (the head reclaims it via the buffer reconcile or its deadline).
func (d *Daemon) resumePrefetchBuffer(ctx context.Context) {
	state, err := LoadBufferState(d.cfg.DataDir)
	if err != nil {
		d.logger.Warn("failed to load persisted prefetch buffer", "error", err)
		ClearBufferState(d.cfg.DataDir)
		return
	}
	if state == nil || len(state.Tasks) == 0 {
		return
	}

	d.logger.Info("found persisted prefetch buffer from previous session",
		"count", len(state.Tasks), "saved_at", state.SavedAt)

	serverByAddr := make(map[string]*ServerConnection)
	if mc := d.multiClient; mc != nil {
		for _, srv := range mc.Servers() {
			serverByAddr[srv.Config.GRPCAddress] = srv
		}
	}

	restored := 0
	for _, pt := range state.Tasks {
		// The buffered unit was already prepared; its work dir must survive for us to
		// run it without re-fetching. If it is gone, drop the item.
		if _, statErr := os.Stat(pt.WorkDir); statErr != nil {
			d.logger.Warn("work directory missing, dropping buffered task",
				"work_unit_id", pt.WorkUnitID, "work_dir", pt.WorkDir)
			continue
		}
		conn := serverByAddr[pt.ServerGRPCAddress]
		if conn == nil {
			d.logger.Warn("server no longer configured, dropping buffered task",
				"work_unit_id", pt.WorkUnitID, "server", pt.ServerGRPCAddress)
			os.RemoveAll(pt.WorkDir)
			continue
		}
		rtName := pt.RuntimeName
		if rtName == "" {
			rtName = "native"
		}
		rt := d.runtimeRegistry.GetRuntime(rtName)
		if rt == nil {
			d.logger.Warn("runtime not available, dropping buffered task",
				"work_unit_id", pt.WorkUnitID, "runtime", rtName)
			os.RemoveAll(pt.WorkDir)
			continue
		}

		wu := &runtime.WorkUnit{
			ID:                        pt.WorkUnitID,
			LeafID:                    pt.LeafID,
			Runtime:                   pt.RuntimeName,
			CodeArtifactURL:           pt.CodeArtifactURL,
			ParametersJSON:            pt.ParametersJSON,
			DeadlineSeconds:           pt.DeadlineSeconds,
			EnvVars:                   pt.EnvVars,
			ExecutionSpec:             pt.ExecutionSpec,
			RscFpopsEst:               pt.RscFpopsEst,
			CheckpointIntervalSeconds: pt.CheckpointIntervalSecs,
			ReservedUntilUnix:         pt.ReservedUntilUnix,
			// Re-stamp the dispatching head from the re-attached connection so the
			// artifact netguard opt-in keeps its per-head scope on resume (PB-31).
			SourceHead: conn.Name,
		}
		prep := &runtime.PrepareResult{
			WorkDir:       pt.WorkDir,
			BinaryPath:    pt.BinaryPath,
			InputPath:     pt.InputPath,
			VizBundlePath: pt.VizBundlePath,
		}
		fetchedAt := pt.FetchedAt
		if fetchedAt.IsZero() {
			fetchedAt = time.Now()
		}
		item := &PreFetchItem{
			WU:        wu,
			Prep:      prep,
			Runtime:   rt,
			Conn:      conn,
			WUResp:    &lettucev1.WorkUnitAssignment{},
			FetchedAt: fetchedAt,
		}
		if pushErr := d.prefetchQueue.Push(item); pushErr != nil {
			d.logger.Warn("prefetch buffer full while restoring; dropping task",
				"work_unit_id", pt.WorkUnitID, "error", pushErr)
			os.RemoveAll(pt.WorkDir)
			continue
		}
		restored++
		d.logger.Info("restored buffered task",
			"work_unit_id", pt.WorkUnitID, "leaf_id", pt.LeafID, "work_dir", pt.WorkDir)
	}

	// Consume the file now that it has been processed.
	ClearBufferState(d.cfg.DataDir)
	if restored > 0 {
		d.logger.Info("prefetch buffer restoration complete", "restored", restored, "total", len(state.Tasks))
	}
}

// workDirTrees are the per-runtime work-dir trees under the data dir, each holding one
// `<work-unit-uuid>` subdir per prepared unit: native (work/), container (container-work/),
// wasm (wasm-work/). See runtime/{native,container,wasm}.go.
var workDirTrees = []string{"work", "container-work", "wasm-work"}

// gcOrphanedWorkDirs reaps per-unit work directories left behind by an unclean exit and
// never reclaimed by the resume loops (TODO #58): a SIGKILL / crash / power loss (cleanup
// defers don't run), the tray-quit fast-exit path (SuspendAndQuit -> os.Exit, which skips
// defers by design), a crash between Prepare creating the dir and the unit being persisted,
// or more persisted tasks than slots on resume. Nothing else ever scans these trees, so such
// dirs leak forever on the same volume shouldFetch measures and can silently trip the disk
// gate.
//
// It MUST run AFTER resumePersistedTasks + resumePrefetchBuffer: at that point the ONLY
// owned dirs are those of the active slots (resumed running tasks) and the prefetch queue
// (restored buffered units), so any other `<uuid>` dir is an orphan. Running it before the
// resumers would delete a dir that is about to be re-attached. Best-effort: read/remove
// failures are logged, never fatal.
// ownedWorkUnitIDs returns the set of work-unit IDs the volunteer currently owns
// — every active execution slot plus every buffered (un-run) prefetch unit. It
// is the spare-set for the startup stranded-container reaper, mirroring the
// owned-dir set gcOrphanedWorkDirs builds, so a just-resumed unit's freshly
// created container is never mistaken for a crash leftover and removed.
func (d *Daemon) ownedWorkUnitIDs() map[string]bool {
	owned := make(map[string]bool)
	for _, wu := range d.slotManager.ActiveWorkUnits() {
		if wu != nil && wu.ID != "" {
			owned[wu.ID] = true
		}
	}
	for _, it := range d.prefetchQueue.Items() {
		if it != nil && it.WU != nil && it.WU.ID != "" {
			owned[it.WU.ID] = true
		}
	}
	return owned
}

func (d *Daemon) gcOrphanedWorkDirs() {
	owned := make(map[string]struct{})
	for _, dir := range d.slotManager.ActiveWorkDirs() {
		if dir != "" {
			owned[filepath.Clean(dir)] = struct{}{}
		}
	}
	for _, it := range d.prefetchQueue.Items() {
		if it != nil && it.Prep != nil && it.Prep.WorkDir != "" {
			owned[filepath.Clean(it.Prep.WorkDir)] = struct{}{}
		}
	}
	reapOrphanWorkDirs(d.cfg.DataDir, owned, d.logger)
}

// reapOrphanWorkDirs is the IO core of the startup work-dir GC (#58): it removes every
// `<uuid>`-named child of the work-dir trees under dataDir that is not in owned. Returns the
// number of dirs removed. A child whose name is not a valid UUID is left untouched (a
// conservative guard so the sweep can never delete anything other than a per-unit work dir),
// as is a missing tree. Split out so it can be tested against a real temp dir with no slot
// manager.
func reapOrphanWorkDirs(dataDir string, owned map[string]struct{}, logger *slog.Logger) int {
	removed := 0
	for _, tree := range workDirTrees {
		treePath := filepath.Join(dataDir, tree)
		entries, err := os.ReadDir(treePath)
		if err != nil {
			if !os.IsNotExist(err) {
				logger.Warn("work-dir GC: failed to read tree", "tree", treePath, "error", err)
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			// Only ever touch `<uuid>` dirs — the exact shape the runtimes create. Anything
			// else (a stray file-as-dir, an operator's manual dir) is left alone.
			if _, perr := uuid.Parse(e.Name()); perr != nil {
				continue
			}
			dirPath := filepath.Clean(filepath.Join(treePath, e.Name()))
			if _, ok := owned[dirPath]; ok {
				continue
			}
			if err := os.RemoveAll(dirPath); err != nil {
				logger.Warn("work-dir GC: failed to remove orphan work dir", "dir", dirPath, "error", err)
				continue
			}
			removed++
			logger.Debug("work-dir GC: removed orphan work dir", "dir", dirPath)
		}
	}
	if removed > 0 {
		logger.Info("work-dir GC: removed orphaned work directories", "removed", removed)
	}
	return removed
}
