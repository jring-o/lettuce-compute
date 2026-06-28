package daemon

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
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

// Daemon manages the volunteer compute loop using concurrent execution slots
// and a pre-fetch queue.
type Daemon struct {
	cfg     *config.Config
	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey
	// hostID is this machine's stable host key (TODO #19), reported on each work
	// request so the head meters in-flight work + the work-send floor per machine.
	// Empty => the head falls back to per-account behavior.
	hostID          string
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

	// Podman machine lifecycle (Windows/macOS).
	machineManager   *runtime.PodmanMachineManager
	machineStartedBy bool // true if this daemon started the machine

	// Leaf discovery and weighted scheduling.
	leafCache        *LeafCache
	weightedSelector *WeightedSelector

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

	// Image-presence gate (item 5): caches whether an enabled leaf's image is
	// already pulled, so shouldFetch requires only workspace headroom for a
	// cached-image rerun instead of the full max_disk_gb allowance.
	imgCacheMu      sync.Mutex
	imgCacheChecked time.Time
	imgCacheResult  bool

	// Image-store path cache (TODO #31): the container backend's image/layer
	// store filesystem (Docker DockerRootDir / Podman graphroot), which the disk
	// gate checks in addition to the data dir because the image does not land
	// under the data dir. Cached for imageCacheCheckTTL to avoid an /info probe on
	// every fetch-loop iteration.
	imgStoreMu      sync.Mutex
	imgStoreChecked time.Time
	imgStorePath    string
	imgStoreKnown   bool

	// Disk-gate warning state: surfaces the otherwise-silent "no free disk, so
	// not fetching" stall as a one-time WARN, reset once the gate clears, so a
	// volunteer that's idle on disk space says so instead of only at Debug.
	diskGateMu     sync.Mutex
	diskGateWarned bool
}

// DaemonConfig holds all dependencies for creating a Daemon.
type DaemonConfig struct {
	Config  *config.Config
	PubKey  ed25519.PublicKey
	PrivKey ed25519.PrivateKey
	// HostID is this machine's stable host key (TODO #19). Empty is valid (the head
	// then treats the volunteer as a single per-account host).
	HostID string

	// Multi-server: preferred way to configure servers.
	Servers []*ServerConnection

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

	// Wire resource limiter and process group hooks into any NativeRuntime.
	limits := &cfg.Config.ResourceLimits
	for _, rt := range registry.runtimes {
		if nr, ok := rt.(*runtime.NativeRuntime); ok {
			nr.SetCommandModifier(func(cmd *exec.Cmd) error {
				if pg != nil {
					pg.ConfigureCommand(cmd)
				}
				return limiter.Apply(cmd, limits)
			})
			nr.SetProcessNotifier(func(pid int) (func(), error) {
				if pg != nil {
					if err := pg.Add(pid); err != nil {
						cfg.Logger.Warn("failed to add process to group", "pid", pid, "error", err)
					}
				}
				return limiter.Enforce(pid, limits)
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
	}
	thermalMonitor := runtime.NewThermalMonitor(thermalCfg, thermalPauseCh, cfg.Logger)

	// Build multi-server client. Support both new Servers field and legacy
	// Client/VolunteerID for backward compatibility with existing tests.
	servers := cfg.Servers
	if len(servers) == 0 && cfg.Client != nil {
		servers = []*ServerConnection{{
			Client:      cfg.Client,
			VolunteerID: cfg.VolunteerID,
			Name:        "default",
			Available:   true,
		}}
	}
	multiClient := NewMultiServerClient(servers, cfg.Logger)

	// Detect hardware once at startup (avoid repeated exec calls that
	// trigger DiskPart/UAC popups on Windows).
	hw := client.DetectHardware(cfg.Config)

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
		hostID:           cfg.HostID,
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
		userPauseCh:      make(chan bool, 1),
		processGroup:     pg,
		benchmarkFPOPS:   benchFPOPS,
		dcfTracker:       dcfTracker,
	}
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
				// Shutting down — resume processes so they can be cleaned up.
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
		d.logger.Error("slot execution failed",
			"work_unit_id", wu.ID,
			"slot", result.SlotID,
			"error", result.Err,
		)
		return
	}

	if result.Result == nil {
		d.logger.Error("slot returned nil result", "work_unit_id", wu.ID, "slot", result.SlotID)
		return
	}

	if result.Result.ExitCode != 0 {
		d.logger.Error("slot execution non-zero exit",
			"work_unit_id", wu.ID,
			"slot", result.SlotID,
			"exit_code", result.Result.ExitCode,
		)
		return
	}

	// Persist result JSON for replay if the leaf has a viz bundle.
	if result.VizBundlePath != "" && len(result.Result.OutputData) > 0 {
		leafName, leafSlug := d.resolveLeafInfo(wu.LeafID)
		maxBytes := int64(d.cfg.ResultCacheMaxMB) * 1024 * 1024
		if err := SaveResult(d.cfg.DataDir, wu.ID, leafName, leafSlug, conn.Name, result.Result.OutputData, result.VizBundlePath, maxBytes); err != nil {
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

	// Update duration correction factor from actual vs estimated time.
	if d.dcfTracker != nil && wu.RscFpopsEst > 0 && d.benchmarkFPOPS > 0 {
		estimatedSec := wu.RscFpopsEst / d.benchmarkFPOPS
		actualSec := float64(result.Result.Metrics.WallClockSeconds)
		if actualSec > 0 {
			d.dcfTracker.Update(wu.LeafID, estimatedSec, actualSec)
		}
	}

	wallClock := result.Result.Metrics.WallClockSeconds
	cpuSeconds := wallClock - int64(result.TotalPausedDur.Seconds())
	if cpuSeconds < 0 {
		cpuSeconds = 0
	}
	d.recordHistory(wu, wallClock, cpuSeconds, submitResp.Accepted, conn.Name)
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
// reservation the volunteer no longer holds. The set deliberately includes running
// units too: the head never reaps a started copy, but reporting it closes the window
// between popping a unit from the buffer and the head recording its run-start.
func (d *Daemon) heldWorkUnitIDs() []string {
	var ids []string
	if d.prefetchQueue != nil {
		for _, item := range d.prefetchQueue.Items() {
			if item.WU != nil {
				ids = append(ids, item.WU.ID)
			}
		}
	}
	if d.slotManager != nil {
		for _, wu := range d.slotManager.ActiveWorkUnits() {
			if wu != nil {
				ids = append(ids, wu.ID)
			}
		}
	}
	return ids
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

// fillSlots fills available execution slots from the pre-fetch queue.
func (d *Daemon) fillSlots(ctx context.Context) {
	for {
		slotID := d.slotManager.AvailableSlotID()
		if slotID < 0 {
			d.logger.Debug("fillSlots: no available slots", "active", d.slotManager.ActiveCount())
			return // no available slots
		}

		item := d.prefetchQueue.Pop()
		if item == nil {
			// No items in queue — return slot.
			d.logger.Debug("fillSlots: queue empty, returning slot", "slot", slotID, "queue_len", d.prefetchQueue.Len())
			d.slotManager.ReturnSlotID(slotID)
			return
		}

		// Check resource availability.
		if !d.canAccommodateWU(item.WU) {
			// Can't accommodate — push item back and return slot.
			d.logger.Debug("fillSlots: can't accommodate WU, pushing back", "work_unit_id", item.WU.ID)
			d.prefetchQueue.PushBack(item)
			d.slotManager.ReturnSlotID(slotID)
			return
		}

		d.logger.Info("starting work unit in slot",
			"work_unit_id", item.WU.ID,
			"leaf_id", item.WU.LeafID,
			"slot", slotID,
			"server", item.Conn.Name,
		)

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
	}
}

// canAccommodateWU checks whether there are enough resources to run the WU
// before admitting it to a slot. It applies three guards (only run
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
func (d *Daemon) canAccommodateWU(wu *runtime.WorkUnit) bool {
	if d.slotManager == nil {
		return true
	}

	wuMemoryMB := int(wu.ExecutionSpec.MaxMemoryMB)
	if wuMemoryMB <= 0 {
		wuMemoryMB = defaultWUMemoryMB
	}

	// 1. Configured memory budget.
	if maxMemoryMB := d.cfg.ResourceLimits.MaxMemoryMB; maxMemoryMB > 0 {
		activeMemoryMB := d.slotManager.TotalActiveMemoryMB()
		if activeMemoryMB+wuMemoryMB > maxMemoryMB {
			d.logger.Info("canAccommodateWU: exceeds configured memory budget; buffered work waiting for capacity",
				"work_unit_id", wu.ID, "active_mb", activeMemoryMB, "wu_mb", wuMemoryMB, "max_mb", maxMemoryMB)
			return false
		}
	}

	// 2. Real free system RAM (already reflects memory used by active containers).
	if freeMB, ok := freeSystemMemoryMB(); ok {
		if freeMB < wuMemoryMB+freeMemoryHeadroomMB {
			d.logger.Info("canAccommodateWU: insufficient free system RAM; buffered work waiting for capacity",
				"work_unit_id", wu.ID, "free_mb", freeMB, "wu_mb", wuMemoryMB, "headroom_mb", freeMemoryHeadroomMB)
			return false
		}
	}

	// 3. GPU exclusivity: one GPU work unit per physical GPU.
	if wu.ExecutionSpec.GPURequired {
		gpuCount := 0
		if d.cachedHW != nil {
			gpuCount = len(d.cachedHW.GetGpus())
		}
		if gpuCount > 0 && d.slotManager.ActiveGPUCount() >= gpuCount {
			d.logger.Info("canAccommodateWU: all GPUs busy; buffered work waiting for capacity",
				"work_unit_id", wu.ID, "gpu_count", gpuCount, "active_gpu_units", d.slotManager.ActiveGPUCount())
			return false
		}
	}

	return true
}

// cachedImageWorkspaceHeadroomMB is the disk headroom required on the data-dir
// volume to start a unit whose image is already pulled. A rerun only writes a
// small workspace — the image itself is already on disk (or in the container
// backend's VM disk) — so the full max_disk_gb allowance isn't needed.
const cachedImageWorkspaceHeadroomMB = 10 * 1024 // 10 GB

// imageCacheCheckTTL bounds how often shouldFetch probes the container backend
// for image presence, so a disk-gated daemon doesn't spawn an "image exists"
// call on every loop iteration.
const imageCacheCheckTTL = 30 * time.Second

// DiskGateThresholds returns the two disk-space thresholds (both in MB) the
// fetch gate applies to the data-dir volume for a given max_disk_gb:
//
//   - fullRequiredMB is the allowance needed to pull a fresh image and run a
//     unit — the configured max_disk_gb, or a 1 GB floor when it is unset.
//   - cachedHeadroomMB is the smaller workspace headroom that suffices to rerun
//     a unit whose container image is already cached.
//
// shouldFetch and the `doctor` preflight both derive their numbers from this one
// function so the live gate and the diagnostic can never disagree (TODO #24).
func DiskGateThresholds(maxDiskGB int) (fullRequiredMB, cachedHeadroomMB int) {
	fullRequiredMB = maxDiskGB * 1024
	if fullRequiredMB <= 0 {
		fullRequiredMB = 1024
	}
	return fullRequiredMB, cachedImageWorkspaceHeadroomMB
}

// DiskGateVerdict classifies how a free-space reading sits relative to the fetch
// gate's thresholds.
type DiskGateVerdict int

const (
	// DiskAmple: at or above the full allowance — the gate always fetches.
	DiskAmple DiskGateVerdict = iota
	// DiskCachedOnly: below the full allowance but at or above the cached-image
	// workspace headroom — the gate fetches only for leafs whose image is
	// already cached; a fresh image pull is still gated. Only reachable when
	// max_disk_gb exceeds the headroom.
	DiskCachedOnly
	// DiskBlocked: below even the cached-image workspace headroom — the gate
	// blocks all fetching.
	DiskBlocked
)

// ClassifyDiskGate maps a free-space reading (MB) on the data-dir volume to the
// fetch gate's verdict for a given max_disk_gb. It is the shared, side-effect-
// free core that the `doctor` preflight uses so its disk check can never
// contradict the daemon's live shouldFetch gate. The DiskCachedOnly band exists
// only when max_disk_gb exceeds the cached-image headroom; otherwise the gate is
// a single threshold and a reading is either DiskAmple or DiskBlocked.
func ClassifyDiskGate(availableMB int64, maxDiskGB int) DiskGateVerdict {
	fullRequiredMB, cachedHeadroomMB := DiskGateThresholds(maxDiskGB)
	floorMB := cachedHeadroomMB
	if fullRequiredMB < floorMB {
		floorMB = fullRequiredMB
	}
	switch {
	case availableMB >= int64(fullRequiredMB):
		return DiskAmple
	case availableMB >= int64(floorMB):
		return DiskCachedOnly
	default:
		return DiskBlocked
	}
}

// shouldFetch checks whether the fetcher should request work.
// Returns false if disk space is insufficient or the scheduler says not active.
func (d *Daemon) shouldFetch() bool {
	// Check scheduler.
	if d.scheduler != nil && !d.scheduler.ShouldRun() {
		d.logger.Debug("shouldFetch: scheduler says don't run")
		return false
	}

	if d.limiter == nil {
		return true
	}

	fullRequiredMB, cachedHeadroomMB := DiskGateThresholds(d.cfg.ResourceLimits.MaxDiskGB)
	cached := d.hasCachedRunnableImage()

	// Gate 1 — the data-dir volume: holds per-unit work dirs (inputs/outputs) and
	// checkpoints. The full allowance is needed to pull-and-run; a repeat run on
	// an already-cached image needs only workspace headroom, not room to pull the
	// image again (the leaf's min_disk_mb, which sizes max_disk_gb, bundles the
	// image, so demanding the whole allowance free forever would block every
	// cached-image rerun).
	if err := d.limiter.CheckDiskSpace(d.cfg.DataDir, fullRequiredMB); err != nil {
		if !cached {
			d.warnDiskGateOnce("data dir", d.cfg.DataDir, fullRequiredMB)
			return false
		}
		if err := d.limiter.CheckDiskSpace(d.cfg.DataDir, cachedHeadroomMB); err != nil {
			d.warnDiskGateOnce("data dir", d.cfg.DataDir, cachedHeadroomMB)
			return false
		}
		d.logger.Debug("shouldFetch: cached image present, requiring workspace headroom only",
			"required_mb", cachedImageWorkspaceHeadroomMB)
	}

	// Gate 2 — the container image-store volume: the image does NOT live under the
	// data dir, it lands in the engine's store (Docker DockerRootDir / Podman
	// graphroot), often a different filesystem. When a fresh pull is required (no
	// enabled leaf's image is cached), gate that filesystem too — otherwise a
	// roomy data dir lets the fetch pass and the pull then dies with ENOSPC on a
	// volume Gate 1 never looked at (TODO #31). A cached image needs no pull, so
	// the store gate is skipped then.
	if !cached {
		if path, ok := d.imageStorePath(); ok {
			if err := d.limiter.CheckDiskSpace(path, fullRequiredMB); err != nil {
				d.warnDiskGateOnce("image store", path, fullRequiredMB)
				return false
			}
		}
	}

	d.clearDiskGateWarning()
	return true
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
	if sec <= 0 {
		return 0
	}
	if d.dcfTracker != nil {
		if dcf := d.dcfTracker.Get(leaf.ID); dcf > 0 {
			sec *= dcf
		}
	}
	return sec
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

// bufferedSeconds sums the estimated seconds of work currently buffered (queued,
// un-run descriptors) and running (active slots). This is the "fill" measured
// against bufferTargetSeconds.
func (d *Daemon) bufferedSeconds() float64 {
	var total float64
	if d.prefetchQueue != nil {
		for _, item := range d.prefetchQueue.Items() {
			if item.WU == nil {
				continue
			}
			total += d.estSecondsForUnit(item.WU.LeafID, item.WU.RscFpopsEst)
		}
	}
	if d.slotManager != nil {
		for _, wu := range d.slotManager.ActiveWorkUnits() {
			if wu == nil {
				continue
			}
			total += d.estSecondsForUnit(wu.LeafID, wu.RscFpopsEst)
		}
	}
	return total
}

// workBufferFull reports whether the client work buffer holds enough work that
// the fetcher must issue ZERO RequestWorkUnit calls (Layer-1 DoD #2).
//
// When a per-unit time estimate is available it uses the hours-based target;
// otherwise it falls back to a small per-slot unit count so the buffer can't
// grow without bound when estimates are missing.
func (d *Daemon) workBufferFull() bool {
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

// fallbackBufferUnits is the unit-count cap used when an hours estimate is
// unavailable: a small multiple of the slot count.
func (d *Daemon) fallbackBufferUnits() int {
	return fallbackBufferUnitsPerSlot * d.maxSlots()
}

// bufferedUnitCount counts queued + running units (the unit-count fallback view).
func (d *Daemon) bufferedUnitCount() int {
	n := 0
	if d.prefetchQueue != nil {
		n += d.prefetchQueue.Len()
	}
	if d.slotManager != nil {
		n += d.slotManager.ActiveCount()
	}
	return n
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
func (d *Daemon) requestBatchSize(estSecondsPerUnit float64) int32 {
	target := d.bufferTargetSeconds()
	if target <= 0 {
		// Buffering disabled (hours == 0): unit-count fallback, one at a time.
		return 1
	}
	deficit := target - d.bufferedSeconds()
	if deficit <= 0 {
		return 1
	}

	per := estSecondsPerUnit
	if per <= 0 {
		per = d.avgBufferedSecondsPerUnit()
	}
	if per <= 0 {
		// No estimate at all: request a full batch to refill the deficit quickly.
		return maxBatchPerRequest
	}

	n := int32(deficit / per)
	if n < 1 {
		n = 1
	}
	if n > maxBatchPerRequest {
		n = maxBatchPerRequest
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

// warnDiskGateOnce surfaces the disk-space stall. The first time the gate blocks
// fetching it logs a single actionable WARN (naming the path, the required and
// available space, and the remedies); subsequent blocked polls stay at Debug so
// the log isn't spammed. clearDiskGateWarning resets it so a later recovery and
// re-stall warns again.
func (d *Daemon) warnDiskGateOnce(volume, path string, requiredMB int) {
	d.diskGateMu.Lock()
	already := d.diskGateWarned
	d.diskGateWarned = true
	d.diskGateMu.Unlock()

	if already {
		d.logger.Debug("shouldFetch: still disk-gated", "volume", volume, "path", path, "required_mb", requiredMB)
		return
	}

	availableMB := client.DiskAvailableMB(path)
	d.logger.Warn("not fetching work: not enough free disk space — this volunteer stays idle until it clears",
		"volume", volume,
		"path", path,
		"required_mb", requiredMB,
		"available_mb", availableMB,
		"remedy", diskGateRemedy(volume))
}

// diskGateRemedy returns the remedy text for the short volume — the image store
// can't be moved from Lettuce's config (the engine owns it), so its advice is to
// repoint the engine's storage or enlarge the Podman-machine disk (TODO #31).
func diskGateRemedy(volume string) string {
	if volume == "image store" {
		return "free space on the container image-store filesystem, repoint the engine's storage (Docker data-root / Podman graphroot) to a roomier volume, or enlarge the Podman-machine disk"
	}
	return "free disk space, lower resource_limits.max_disk_gb, or restart with --data-dir on a roomier volume"
}

// clearDiskGateWarning re-arms the disk-gate WARN after the gate clears.
func (d *Daemon) clearDiskGateWarning() {
	d.diskGateMu.Lock()
	wasWarned := d.diskGateWarned
	d.diskGateWarned = false
	d.diskGateMu.Unlock()
	if wasWarned {
		d.logger.Info("disk space recovered: resuming work fetching")
	}
}

// logReadiness logs a one-shot startup banner: the runtimes this volunteer can
// actually run, free disk vs the configured allowance, and how many of the
// attached leafs it is eligible for. When nothing is runnable (e.g. every leaf
// needs a container runtime that isn't installed) it escalates to WARN with the
// reason and remedy, so a misconfigured volunteer learns why in seconds instead
// of sitting silently idle. A leaf with a container image requires the container
// runtime; everything else runs on the always-present native/wasm runtimes.
func (d *Daemon) logReadiness() {
	if d.runtimeRegistry == nil || d.multiClient == nil {
		return
	}
	runtimes := d.runtimeRegistry.AvailableRuntimes()
	hasContainer := d.runtimeRegistry.GetRuntime("container") != nil

	availableMB := client.DiskAvailableMB(d.cfg.DataDir)
	allowanceMB := d.cfg.ResourceLimits.MaxDiskGB * 1024

	var totalLeafs, eligibleLeafs, containerBlocked int
	for _, srv := range d.multiClient.Servers() {
		for _, lf := range d.enabledLeafs(srv.Name) {
			totalLeafs++
			if lf.ExecutionSpec != nil && lf.ExecutionSpec.Image != "" && !hasContainer {
				containerBlocked++
				continue
			}
			eligibleLeafs++
		}
	}

	d.logger.Info("volunteer ready",
		"runtimes", runtimes,
		"data_dir", d.cfg.DataDir,
		"disk_free_mb", availableMB,
		"disk_allowance_mb", allowanceMB,
		"eligible_leafs", eligibleLeafs,
		"total_leafs", totalLeafs,
	)

	// "Connected, but you will get no work" — the actionable case worth a WARN.
	if totalLeafs > 0 && eligibleLeafs == 0 {
		if containerBlocked == totalLeafs && !hasContainer {
			d.logger.Warn("no runnable leafs: every attached leaf needs a container runtime, but none is available here — install Docker or Podman (see the volunteer setup docs), or attach a head that has native leafs",
				"runtimes", runtimes, "container_leafs", containerBlocked)
		} else {
			d.logger.Warn("no runnable leafs: none of the attached leafs match this volunteer's available runtimes",
				"runtimes", runtimes, "total_leafs", totalLeafs)
		}
	}
}

// hasCachedRunnableImage reports whether at least one enabled leaf's container
// image is already present locally. The result is cached for imageCacheCheckTTL
// to avoid repeated backend calls while disk-gated. Returns false when no
// container runtime is registered (native-only volunteers, or tests using mock
// runtimes — which keeps the disk gate from ever shelling out under test).
func (d *Daemon) hasCachedRunnableImage() bool {
	d.imgCacheMu.Lock()
	if !d.imgCacheChecked.IsZero() && time.Since(d.imgCacheChecked) < imageCacheCheckTTL {
		r := d.imgCacheResult
		d.imgCacheMu.Unlock()
		return r
	}
	d.imgCacheMu.Unlock()

	result := d.checkCachedRunnableImage()

	d.imgCacheMu.Lock()
	d.imgCacheChecked = time.Now()
	d.imgCacheResult = result
	d.imgCacheMu.Unlock()
	return result
}

func (d *Daemon) checkCachedRunnableImage() bool {
	if d.runtimeRegistry == nil || d.multiClient == nil {
		return false
	}
	cr, ok := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime)
	if !ok || cr == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seen := make(map[string]bool)
	for _, srv := range d.multiClient.Servers() {
		for _, lf := range d.enabledLeafs(srv.Name) {
			if lf.ExecutionSpec == nil || lf.ExecutionSpec.Image == "" {
				continue
			}
			img := lf.ExecutionSpec.Image
			if seen[img] {
				continue
			}
			seen[img] = true
			if exists, err := cr.Client().ImageExists(ctx, img); err == nil && exists {
				return true
			}
		}
	}
	return false
}

// imageStorePath returns the filesystem path where the container backend stores
// images and extracts layers (Docker DockerRootDir / Podman graphroot). ok is
// false when there is no container runtime, the backend can't be queried, or it
// reports no path — callers then skip the image-store disk gate rather than
// block, preserving the pre-#31 behavior for native-only volunteers or an
// unreachable engine. The result is cached for imageCacheCheckTTL so the fetch
// gate doesn't issue an /info call on every loop iteration. (TODO #31)
func (d *Daemon) imageStorePath() (string, bool) {
	d.imgStoreMu.Lock()
	if !d.imgStoreChecked.IsZero() && time.Since(d.imgStoreChecked) < imageCacheCheckTTL {
		path, known := d.imgStorePath, d.imgStoreKnown
		d.imgStoreMu.Unlock()
		return path, known
	}
	d.imgStoreMu.Unlock()

	path, known := d.probeImageStorePath()

	d.imgStoreMu.Lock()
	d.imgStoreChecked = time.Now()
	d.imgStorePath = path
	d.imgStoreKnown = known
	d.imgStoreMu.Unlock()
	return path, known
}

func (d *Daemon) probeImageStorePath() (string, bool) {
	if d.runtimeRegistry == nil {
		return "", false
	}
	cr, ok := d.runtimeRegistry.GetRuntime("container").(*runtime.ContainerRuntime)
	if !ok || cr == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := cr.Client().Info(ctx)
	if err != nil || info == nil || info.StoragePath == "" {
		return "", false
	}
	return info.StoragePath, true
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
func (d *Daemon) abandonItem(item *PreFetchItem, reason string) {
	if item == nil || item.Conn == nil || item.Conn.Client == nil || item.WU == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := item.Conn.Client.AbandonWorkUnit(ctx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  item.WU.ID,
		VolunteerId: item.Conn.VolunteerID,
		PublicKey:   d.pubKey,
		Reason:      reason,
	}); err != nil {
		d.logger.Warn("failed to abandon un-run work unit", "work_unit_id", item.WU.ID, "error", err)
		return
	}
	d.logger.Info("abandoned un-run work unit back to head", "work_unit_id", item.WU.ID, "reason", reason)
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

// IsPaused returns true if the daemon is paused by any source.
func (d *Daemon) IsPaused() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.paused || d.userPaused
}

// PauseReason returns the reason the daemon is paused, or empty string if not paused.
func (d *Daemon) PauseReason() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.userPaused {
		return "user"
	}
	if d.paused {
		return d.pauseReason
	}
	return ""
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

// SetMachineStartedByDaemon marks that the daemon started the Podman machine,
// so it can be stopped on daemon shutdown.
func (d *Daemon) SetMachineStartedByDaemon(started bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.machineStartedBy = started
}

// MachineStartedByDaemon returns whether the daemon started the Podman machine.
func (d *Daemon) MachineStartedByDaemon() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.machineStartedBy
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

// enabledLeafs returns cached leafs filtered by the server's leaf preferences.
func (d *Daemon) enabledLeafs(serverName string) []CachedLeafInfo {
	leafs := d.leafCache.GetLeafs(serverName)
	if leafs == nil {
		return nil
	}

	// Find the server config.
	var lp config.LeafPreferences
	for _, srv := range d.cfg.Servers {
		if srv.DisplayName() == serverName {
			lp = srv.LeafPreferences
			break
		}
	}

	mode := lp.Mode
	if mode == "" {
		mode = "ALL"
	}

	switch mode {
	case "ALL":
		return leafs
	case "SPECIFIC":
		enabledSet := make(map[string]bool, len(lp.Enabled))
		for _, slug := range lp.Enabled {
			enabledSet[slug] = true
		}
		var result []CachedLeafInfo
		for _, leaf := range leafs {
			if enabledSet[leaf.Slug] {
				result = append(result, leaf)
			}
		}
		return result
	case "BLOCKLIST":
		disabledSet := make(map[string]bool, len(lp.Disabled))
		for _, slug := range lp.Disabled {
			disabledSet[slug] = true
		}
		var result []CachedLeafInfo
		for _, leaf := range leafs {
			if !disabledSet[leaf.Slug] {
				result = append(result, leaf)
			}
		}
		return result
	default:
		return leafs
	}
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
func (d *Daemon) recordHistory(wu *runtime.WorkUnit, wallClockSeconds int64, cpuSeconds int64, accepted bool, serverName string) {
	if histErr := AppendHistory(d.cfg.DataDir, HistoryEntry{
		WorkUnitID:       wu.ID,
		LeafID:           wu.LeafID,
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
