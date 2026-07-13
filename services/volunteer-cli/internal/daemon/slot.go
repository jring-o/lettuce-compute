package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Sentinel errors for per-task operations.
var (
	ErrTaskNotFound         = errors.New("task not found")
	ErrTaskAlreadySuspended = errors.New("task is already suspended")
	ErrTaskNotSuspended     = errors.New("task is not suspended")
	ErrDaemonPaused         = errors.New("cannot resume task while daemon is paused")

	// errStartWorkDropped is set as the slot's execErr when run-start (StartWork)
	// reports the unit is no longer ours (Ok=false) or fails terminally. It signals
	// "drop without submitting": the slot never executed, so there is nothing to
	// submit and nothing to abandon (the head already re-staged the unit via its
	// lapsed-reservation / deadline sweep). handleSlotResult treats a non-nil
	// result.Err as a no-submit outcome, which is exactly the drop behavior the
	// removed Heartbeat fast-path used to give the #20 reassigned-out case.
	errStartWorkDropped = errors.New("work unit dropped at run-start (no longer reserved for this volunteer)")
)

// startWorkTimeout bounds the run-start RPC so a slow/overloaded head can't stall
// a slot indefinitely before execution begins. On timeout the unit is dropped and
// reclaimed by the head's deadline/lapsed-reservation sweep.
const startWorkTimeout = 30 * time.Second

// ExecutionSlot represents a single execution slot that runs one WU at a time.
type ExecutionSlot struct {
	ID             int
	mu             sync.Mutex
	active         bool
	wu             *runtime.WorkUnit
	prep           *runtime.PrepareResult
	rt             runtime.Runtime
	conn           *ServerConnection
	cancel         context.CancelFunc
	startedAt      time.Time
	checkpoint     *CheckpointManager
	resumedFromCkp bool
	preserved      *PersistedTask // non-nil if work dir was preserved on shutdown
	processHandle  ProcessHandle  // for suspend/resume
	suspended      bool
	// suspendPending records that a suspend (schedule gate / pause) was requested
	// while this slot was active but had no process handle yet — the window between
	// StartSlot marking a re-executed resume active and the runtime registering the
	// spawned process via PIDCallback. The deferred suspend is applied the moment the
	// handle is attached (see attachProcessHandle); without it a resumed task whose
	// handle appears after a one-shot SuspendAll would run unfrozen through the whole
	// off-schedule/pause window.
	suspendPending bool
	pausedAt       time.Time     // when current pause began (zero if not paused)
	totalPausedDur time.Duration // accumulated pause time during THIS daemon session
	fetchedAt      time.Time     // when the WU was fetched from server (from prefetch queue)

	// originalStartedAt is the first-ever start time of this work unit, carried across
	// resumes for reference. startedAt above is the start of the CURRENT session's
	// wall-clock segment (reset to now on every (re)start). elapsedBase/pausedBase hold
	// the run/paused time accumulated under previous sessions, so displayed elapsed and
	// CPU time advance only while the task actually runs and never count the time the
	// daemon was stopped between sessions.
	originalStartedAt time.Time
	elapsedBase       time.Duration
	pausedBase        time.Duration
}

// sessionElapsed returns the run time accumulated across all sessions: the bases
// carried from previous sessions plus this session's wall-clock segment. Caller holds slot.mu.
func (s *ExecutionSlot) sessionElapsed() time.Duration {
	return s.elapsedBase + time.Since(s.startedAt)
}

// sessionPaused returns the total paused time across all sessions, including any
// pause currently in progress. Caller holds slot.mu.
func (s *ExecutionSlot) sessionPaused() time.Duration {
	paused := s.pausedBase + s.totalPausedDur
	if !s.pausedAt.IsZero() {
		paused += time.Since(s.pausedAt)
	}
	return paused
}

// SlotResult is the outcome of a slot's execution.
type SlotResult struct {
	SlotID         int
	WU             *runtime.WorkUnit
	Result         *runtime.ExecutionResult
	Conn           *ServerConnection
	Err            error
	TotalPausedDur time.Duration // accumulated pause time for CPU time calculation
	VizBundlePath  string        // from PrepareResult; non-empty if leaf has viz bundle
}

// SlotManager owns a fixed pool of execution slots.
type SlotManager struct {
	slots        []*ExecutionSlot
	available    chan int        // IDs of available slots
	results      chan SlotResult // completed slot notifications
	logger       *slog.Logger
	shuttingDown atomic.Bool
}

// NewSlotManager creates a slot manager with the given number of slots.
func NewSlotManager(maxSlots int, logger *slog.Logger) *SlotManager {
	if maxSlots <= 0 {
		maxSlots = 1
	}
	slots := make([]*ExecutionSlot, maxSlots)
	available := make(chan int, maxSlots)
	for i := 0; i < maxSlots; i++ {
		slots[i] = &ExecutionSlot{ID: i}
		available <- i
	}
	return &SlotManager{
		slots:     slots,
		available: available,
		results:   make(chan SlotResult, maxSlots),
		logger:    logger,
	}
}

// StartSlot begins execution in the given slot with the provided pre-fetched item.
// The slot goroutine handles: checkpoint restore, run-start (StartWork), execution,
// cleanup.
func (sm *SlotManager) StartSlot(ctx context.Context, slotID int, item *PreFetchItem, d *Daemon) error {
	slot := sm.slots[slotID]
	slot.mu.Lock()
	slot.active = true
	slot.wu = item.WU
	slot.prep = item.Prep
	slot.rt = item.Runtime
	slot.conn = item.Conn
	// Start a fresh wall-clock segment for this session; carry forward the original
	// start time and the run/paused time accrued under previous sessions so elapsed/CPU
	// time excludes any daemon-down gap (see ExecutionSlot.sessionElapsed/sessionPaused).
	slot.startedAt = time.Now()
	if item.Prep != nil {
		slot.elapsedBase = item.Prep.ElapsedAccrued
		slot.pausedBase = item.Prep.PausedAccrued
		if !item.Prep.OriginalStartedAt.IsZero() {
			slot.originalStartedAt = item.Prep.OriginalStartedAt
		} else {
			slot.originalStartedAt = slot.startedAt
		}
	} else {
		slot.elapsedBase = 0
		slot.pausedBase = 0
		slot.originalStartedAt = slot.startedAt
	}
	slot.resumedFromCkp = false
	slot.preserved = nil
	slot.fetchedAt = item.FetchedAt
	slot.totalPausedDur = 0
	slot.pausedAt = time.Time{}
	slot.mu.Unlock()

	go sm.runSlot(ctx, slot, item, d)
	return nil
}

// runSlot is the slot goroutine lifecycle.
func (sm *SlotManager) runSlot(ctx context.Context, slot *ExecutionSlot, item *PreFetchItem, d *Daemon) {
	wu := item.WU
	prep := item.Prep
	rt := item.Runtime
	conn := item.Conn

	var execResult *runtime.ExecutionResult
	var execErr error

	defer func() {
		// If daemon is shutting down and execution was interrupted (not completed
		// successfully), preserve the work directory for resumption on next startup.
		// A unit dropped at run-start (errStartWorkDropped) is no longer ours, so it
		// is never preserved — resuming it would only fail StartWork again.
		if sm.shuttingDown.Load() && execErr != nil && !errors.Is(execErr, errStartWorkDropped) {
			var ckptSeq int32
			slot.mu.Lock()
			if slot.checkpoint != nil {
				ckptSeq = slot.checkpoint.Sequence()
			}
			slot.preserved = &PersistedTask{
				WorkUnitID:             wu.ID,
				LeafID:                 wu.LeafID,
				ServerGRPCAddress:      conn.Config.GRPCAddress,
				ServerName:             conn.Name,
				VolunteerID:            conn.VolunteerID,
				RuntimeName:            wu.Runtime,
				WorkDir:                prep.WorkDir,
				BinaryPath:             prep.BinaryPath,
				InputPath:              prep.InputPath,
				CodeArtifactURL:        wu.CodeArtifactURL,
				ParametersJSON:         wu.ParametersJSON,
				DeadlineSeconds:        wu.DeadlineSeconds,
				EnvVars:                wu.EnvVars,
				ExecutionSpec:          wu.ExecutionSpec,
				RscFpopsEst:            wu.RscFpopsEst,
				VizBundlePath:          prep.VizBundlePath,
				CheckpointSequence:     ckptSeq,
				CheckpointIntervalSecs: wu.CheckpointIntervalSeconds,
				StartedAt:              slot.originalStartedAt,
				ElapsedAccruedSeconds:  int64(slot.sessionElapsed().Seconds()),
				PausedAccruedSeconds:   int64(slot.sessionPaused().Seconds()),
			}
			slot.mu.Unlock()

			sm.logger.Info("preserving work directory for resume",
				"work_unit_id", wu.ID, "slot", slot.ID, "work_dir", prep.WorkDir)
		} else {
			// Normal completion or error without shutdown — clean up.
			if cleanErr := rt.Cleanup(prep); cleanErr != nil {
				sm.logger.Warn("cleanup failed", "work_unit_id", wu.ID, "slot", slot.ID, "error", cleanErr)
			}
		}

		// Capture pause duration and mark slot inactive in one lock acquisition.
		slot.mu.Lock()
		pausedDur := slot.totalPausedDur
		if !slot.pausedAt.IsZero() {
			pausedDur += time.Since(slot.pausedAt)
		}
		slot.active = false
		slot.wu = nil
		slot.prep = nil
		slot.rt = nil
		slot.conn = nil
		slot.cancel = nil
		slot.checkpoint = nil
		slot.resumedFromCkp = false
		slot.processHandle = nil
		slot.suspended = false
		slot.suspendPending = false
		slot.pausedAt = time.Time{}
		slot.totalPausedDur = 0
		slot.fetchedAt = time.Time{}
		slot.originalStartedAt = time.Time{}
		slot.elapsedBase = 0
		slot.pausedBase = 0
		slot.mu.Unlock()

		// Send result.
		sm.results <- SlotResult{
			SlotID:         slot.ID,
			WU:             wu,
			Result:         execResult,
			Conn:           conn,
			Err:            execErr,
			TotalPausedDur: pausedDur,
			VizBundlePath:  prep.VizBundlePath,
		}

		// Return slot to available pool.
		sm.available <- slot.ID
	}()

	// Restore checkpoint if applicable.
	if wu.HasCheckpoint && wu.CheckpointIntervalSeconds > 0 {
		d.restoreCheckpoint(ctx, wu, prep, conn)
		slot.mu.Lock()
		slot.resumedFromCkp = true
		slot.mu.Unlock()
	}

	// Create cancellable execution context.
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	slot.mu.Lock()
	slot.cancel = cancelExec
	slot.mu.Unlock()

	// Start checkpoint manager if enabled.
	var checkpointMgr *CheckpointManager
	if wu.CheckpointIntervalSeconds > 0 {
		checkpointMgr = &CheckpointManager{
			client:   conn.Client,
			logger:   sm.logger,
			workDir:  prep.WorkDir,
			wu:       wu,
			volID:    conn.VolunteerID,
			pubKey:   d.pubKey,
			interval: time.Duration(wu.CheckpointIntervalSeconds) * time.Second,
			sequence: wu.CheckpointSequence,
			stopCh:   make(chan struct{}),
			doneCh:   make(chan struct{}),
		}
		slot.mu.Lock()
		slot.checkpoint = checkpointMgr
		slot.mu.Unlock()
		go func() {
			defer close(checkpointMgr.doneCh)
			checkpointMgr.Run(execCtx)
		}()
	}

	// Run-start: the slot is now actually executing a unit pulled from the work
	// buffer. With per-task heartbeats removed, the head's QUEUED -> ASSIGNED
	// transition (and the deadline clock start) is performed by an explicit
	// StartWork RPC here, replacing the old first-RUNNING-heartbeat run-start.
	// Liveness from here is deadline-based.
	//
	// If StartWork reports the unit is no longer ours (Ok=false) or fails
	// terminally, the head has already re-staged it (its reservation lapsed while
	// it sat in the buffer, or it was reassigned). We DROP it instead of wasting an
	// execution: cancel the (unused) exec context and return errStartWorkDropped so
	// handleSlotResult skips submission. This relocates the removed Heartbeat
	// fast-path's #20 reassigned-out drop to run-start. Signing is handled by the
	// client's signing interceptor, so the request carries only the IDs.
	if d != nil && conn != nil && conn.Client != nil {
		swCtx, swCancel := context.WithTimeout(execCtx, startWorkTimeout)
		swResp, swErr := conn.Client.StartWork(swCtx, &lettucev1.StartWorkRequest{
			WorkUnitId:  wu.ID,
			VolunteerId: conn.VolunteerID,
		})
		swCancel()
		if swErr != nil {
			// A terminal rejection (NotFound/FailedPrecondition/PermissionDenied/
			// InvalidArgument) means the unit is definitively not run-startable for us;
			// drop it. Anything else (transport blip, ResourceExhausted shed,
			// DeadlineExceeded) also drops THIS attempt — the unit stays QUEUED/reserved
			// at the head and is reclaimed by its reservation window, so we never run a
			// unit the head doesn't believe is ours.
			sm.logger.Warn("run-start StartWork failed; dropping unit",
				"work_unit_id", wu.ID, "slot", slot.ID, "error", swErr)
			cancelExec()
			execErr = errStartWorkDropped
			return
		}
		if !swResp.GetOk() {
			sm.logger.Info("run-start denied (unit reassigned or reservation lapsed); dropping unit",
				"work_unit_id", wu.ID, "slot", slot.ID, "message", swResp.GetMessage())
			cancelExec()
			execErr = errStartWorkDropped
			return
		}
		sm.logger.Info("run-start StartWork ok", "work_unit_id", wu.ID, "slot", slot.ID, "server", conn.Name, "leaf_id", wu.LeafID)
	}

	// Wire process handle callbacks for suspend/resume.
	if prep != nil {
		prep.PIDCallback = func(pid int) {
			sm.attachProcessHandle(slot, NewNativeProcessHandle(pid))
		}
		if cr, ok := rt.(*runtime.ContainerRuntime); ok {
			prep.ContainerIDCallback = func(containerID string) {
				sm.attachProcessHandle(slot, NewContainerProcessHandle(cr.Client(), containerID))
			}
		}
	}

	// Execute or adopt orphan — blocks until completion or context cancellation.
	if prep.OrphanPID > 0 {
		// Orphan resume: process is already running (we resumed it from frozen).
		// Poll until it exits instead of starting a new one.
		execResult, execErr = waitForOrphan(execCtx, prep)
	} else {
		execResult, execErr = rt.Execute(execCtx, wu, prep)
	}

	// Stop checkpoint manager once execution completes so a final checkpoint is saved.
	if checkpointMgr != nil {
		checkpointMgr.Stop()
	}
}

// isProcessAliveFunc is the function used to check whether a PID is still running.
// It defaults to the platform-specific isProcessAlive. Tests override this to
// avoid calling real system APIs.
var isProcessAliveFunc = isProcessAlive

// waitForOrphan polls a previously-frozen orphan process until it exits or the
// context is cancelled. Used when resuming processes from a previous "tray quit"
// session — the process is already running, we just need to wait for it to finish.
func waitForOrphan(ctx context.Context, prep *runtime.PrepareResult) (*runtime.ExecutionResult, error) {
	start := time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if !isProcessAliveFunc(prep.OrphanPID) {
				// Process completed. Read output from work directory.
				wallClock := int64(time.Since(start).Seconds())
				outputPath := filepath.Join(prep.WorkDir, "output.dat")
				// BG-15c: an orphan-resumed unit's output.dat is leaf-controlled; read
				// it through the shared symlink-safe, size-capped reader so a planted
				// symlink cannot exfiltrate its target (and a giant file cannot balloon
				// daemon RAM). A refusal yields empty output, matching the prior
				// best-effort semantics rather than crashing the resume.
				output, _ := runtime.ReadRegularNoFollow(outputPath)

				return &runtime.ExecutionResult{
					ExitCode:   0,
					OutputData: output,
					Metrics: runtime.ExecutionMetrics{
						WallClockSeconds: wallClock,
					},
				}, nil
			}
		}
	}
}

// WaitForCompletion blocks until a slot completes and returns its result.
func (sm *SlotManager) WaitForCompletion(ctx context.Context) (SlotResult, error) {
	select {
	case <-ctx.Done():
		return SlotResult{}, ctx.Err()
	case result := <-sm.results:
		return result, nil
	}
}

// TryGetResult returns a completed slot result if one is available, or false.
func (sm *SlotManager) TryGetResult() (SlotResult, bool) {
	select {
	case result := <-sm.results:
		return result, true
	default:
		return SlotResult{}, false
	}
}

// ActiveCount returns the number of currently active slots.
func (sm *SlotManager) ActiveCount() int {
	count := 0
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active {
			count++
		}
		slot.mu.Unlock()
	}
	return count
}

// ActiveWorkUnits returns the work units of all currently active slots. Used by
// the client work buffer to account for in-flight (running) work toward the
// hours-based buffer target.
func (sm *SlotManager) ActiveWorkUnits() []*runtime.WorkUnit {
	var wus []*runtime.WorkUnit
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil {
			wus = append(wus, slot.wu)
		}
		slot.mu.Unlock()
	}
	return wus
}

// ActiveWorkDirs returns the per-unit work directories of all currently active slots.
// Used by the startup orphaned-work-dir GC (#58) to know which dirs are owned by a
// running slot (and so must NOT be reaped). A slot with no prep (not yet started) or an
// empty WorkDir contributes nothing.
func (sm *SlotManager) ActiveWorkDirs() []string {
	var dirs []string
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.prep != nil && slot.prep.WorkDir != "" {
			dirs = append(dirs, slot.prep.WorkDir)
		}
		slot.mu.Unlock()
	}
	return dirs
}

// GetCurrentTasks returns info about all active slots' work units.
// benchmarkFPOPS is the volunteer's CPU benchmark score for time estimation.
// dcfFunc returns the duration correction factor for a given leaf ID.
func (sm *SlotManager) GetCurrentTasks(benchmarkFPOPS float64, dcfFunc func(leafID string) float64) []CurrentTask {
	var tasks []CurrentTask
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil {
			// Run/paused time accrued across all sessions (excludes any daemon-down gap).
			elapsedDur := slot.sessionElapsed()
			pausedDur := slot.sessionPaused()

			task := CurrentTask{
				WorkUnitID:            slot.wu.ID,
				LeafID:                slot.wu.LeafID,
				StartedAt:             slot.originalStartedAt,
				ElapsedSeconds:        int(elapsedDur.Seconds()),
				ResumedFromCheckpoint: slot.resumedFromCkp,
				Suspended:             slot.suspended,
				TotalPausedSeconds:    int(pausedDur.Seconds()),
				DeadlineSeconds:       slot.wu.DeadlineSeconds,
				RuntimeType:           slot.wu.Runtime,
				FetchedAt:             slot.fetchedAt,
			}
			if slot.wu.ExecutionSpec.Image != "" {
				task.ContainerImage = slot.wu.ExecutionSpec.Image
			}
			if slot.conn != nil {
				task.ServerName = slot.conn.Config.DisplayName()
			}
			if slot.processHandle != nil {
				task.ProcessID = slot.processHandle.PID()
			}
			if slot.prep != nil {
				task.WorkDir = slot.prep.WorkDir
				task.VizBundlePath = slot.prep.VizBundlePath
			}
			if slot.checkpoint != nil {
				task.CheckpointSequence = slot.checkpoint.Sequence()
				task.LastCheckpointAt = slot.checkpoint.LastSaveAt()
			}
			// Compute estimated duration from benchmark + fpops + DCF.
			if slot.wu.RscFpopsEst > 0 && benchmarkFPOPS > 0 {
				dcf := 1.0
				if dcfFunc != nil {
					dcf = dcfFunc(slot.wu.LeafID)
				}
				task.EstimatedSeconds = (slot.wu.RscFpopsEst / benchmarkFPOPS) * dcf
			}
			tasks = append(tasks, task)
		}
		slot.mu.Unlock()
	}
	return tasks
}

// StopAll waits for all active slots to complete their current WU.
// Does not cancel them — graceful wait. Results remain in the results channel
// for the caller to drain via TryGetResult.
func (sm *SlotManager) StopAll() {
	for sm.ActiveCount() > 0 {
		time.Sleep(50 * time.Millisecond)
	}
}

// TotalActiveMemoryMB returns the sum of booked memory across all active slots' WUs.
// BG-16: books each unit at BookedMemMB (the same clamped number each runtime
// enforces), so admission and enforcement share one denominator. memCeilingMB is the
// volunteer's configured budget (config.ResourceLimits.MaxMemoryMB).
func (sm *SlotManager) TotalActiveMemoryMB(memCeilingMB int) int {
	total := 0
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil {
			total += runtime.BookedMemMB(int(slot.wu.ExecutionSpec.MaxMemoryMB), memCeilingMB)
		}
		slot.mu.Unlock()
	}
	return total
}

// ActiveGPUCount returns the number of active slots running GPU work units.
// Used by admission to keep at most one GPU work unit per physical GPU so
// concurrent units never oversubscribe VRAM.
func (sm *SlotManager) ActiveGPUCount() int {
	count := 0
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil && slot.wu.ExecutionSpec.GPURequired {
			count++
		}
		slot.mu.Unlock()
	}
	return count
}

// AvailableSlotID returns an available slot ID without blocking, or -1 if none available.
func (sm *SlotManager) AvailableSlotID() int {
	select {
	case id := <-sm.available:
		return id
	default:
		return -1
	}
}

// ReturnSlotID returns a slot ID to the available pool (used when we decide not to use it).
func (sm *SlotManager) ReturnSlotID(id int) {
	sm.available <- id
}

// SetShuttingDown signals that the daemon is shutting down. Active slots will
// preserve their work directories instead of cleaning up.
func (sm *SlotManager) SetShuttingDown() {
	sm.shuttingDown.Store(true)
}

// SuspendAll freezes all active slot processes via OS-level suspension.
func (sm *SlotManager) SuspendAll() {
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && !slot.suspended {
			if slot.processHandle != nil {
				if err := slot.processHandle.Suspend(); err != nil {
					sm.logger.Warn("failed to suspend process", "slot", slot.ID, "error", err)
				} else {
					slot.suspended = true
					slot.pausedAt = time.Now()
				}
			} else {
				// Active but no process handle yet: a task resumed via re-execution is
				// marked active before its process spawns. Defer the suspend so it is
				// applied the instant the handle is registered (attachProcessHandle);
				// otherwise the process runs unfrozen through the whole window.
				slot.suspendPending = true
			}
		}
		slot.mu.Unlock()
	}
}

// ResumeAll unfreezes all suspended slot processes.
func (sm *SlotManager) ResumeAll() {
	for _, slot := range sm.slots {
		slot.mu.Lock()
		// Cancel any suspend that was deferred while the handle was missing: the window
		// reopened before the process ever registered its handle, so it should run.
		slot.suspendPending = false
		if slot.active && slot.processHandle != nil && slot.suspended {
			// Accumulate pause duration before resuming.
			if !slot.pausedAt.IsZero() {
				slot.totalPausedDur += time.Since(slot.pausedAt)
				slot.pausedAt = time.Time{}
			}
			if err := slot.processHandle.Resume(); err != nil {
				sm.logger.Warn("failed to resume process", "slot", slot.ID, "error", err)
			} else {
				slot.suspended = false
			}
		}
		slot.mu.Unlock()
	}
}

// SetProcessHandle stores a process handle on a slot for suspend/resume.
func (sm *SlotManager) SetProcessHandle(slotID int, handle ProcessHandle) {
	if slotID < 0 || slotID >= len(sm.slots) {
		return
	}
	sm.attachProcessHandle(sm.slots[slotID], handle)
}

// attachProcessHandle stores a process handle on a slot and, if a suspend was
// deferred while the slot had no handle (suspendPending), applies it immediately.
// All handle registration — the runtime's PIDCallback/ContainerIDCallback and the
// orphan-resume SetProcessHandle path — flows through here, so a handle that
// appears after a one-shot SuspendAll (e.g. a re-executed task resumed into an
// already-inactive schedule window) is frozen the moment it exists rather than
// running on unsuspended. Best-effort: a failed suspend is logged and the pending
// flag cleared, matching SuspendAll's behavior.
func (sm *SlotManager) attachProcessHandle(slot *ExecutionSlot, handle ProcessHandle) {
	slot.mu.Lock()
	defer slot.mu.Unlock()
	slot.processHandle = handle
	if handle == nil || !slot.suspendPending || slot.suspended {
		slot.suspendPending = false
		return
	}
	if err := handle.Suspend(); err != nil {
		sm.logger.Warn("failed to apply deferred suspend on handle registration",
			"slot", slot.ID, "error", err)
	} else {
		slot.suspended = true
		slot.pausedAt = time.Now()
	}
	slot.suspendPending = false
}

// GetActivePersistableTasks returns PersistedTask data for all currently active
// slots. Called after every task start/completion to keep active-tasks.json
// up to date — no graceful shutdown needed for persistence.
func (sm *SlotManager) GetActivePersistableTasks() []PersistedTask {
	var tasks []PersistedTask
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil && slot.prep != nil && slot.conn != nil {
			pt := PersistedTask{
				WorkUnitID:            slot.wu.ID,
				LeafID:                slot.wu.LeafID,
				ServerGRPCAddress:     slot.conn.Config.GRPCAddress,
				ServerName:            slot.conn.Name,
				VolunteerID:           slot.conn.VolunteerID,
				RuntimeName:           slot.wu.Runtime,
				WorkDir:               slot.prep.WorkDir,
				BinaryPath:            slot.prep.BinaryPath,
				InputPath:             slot.prep.InputPath,
				CodeArtifactURL:       slot.wu.CodeArtifactURL,
				ParametersJSON:        slot.wu.ParametersJSON,
				DeadlineSeconds:       slot.wu.DeadlineSeconds,
				EnvVars:               slot.wu.EnvVars,
				ExecutionSpec:         slot.wu.ExecutionSpec,
				RscFpopsEst:           slot.wu.RscFpopsEst,
				VizBundlePath:         slot.prep.VizBundlePath,
				StartedAt:             slot.originalStartedAt,
				ElapsedAccruedSeconds: int64(slot.sessionElapsed().Seconds()),
				PausedAccruedSeconds:  int64(slot.sessionPaused().Seconds()),
			}
			if slot.checkpoint != nil {
				pt.CheckpointSequence = slot.checkpoint.Sequence()
				pt.CheckpointIntervalSecs = int32(slot.wu.CheckpointIntervalSeconds)
			}
			if slot.processHandle != nil {
				pt.PID = slot.processHandle.PID()
			}
			tasks = append(tasks, pt)
		}
		slot.mu.Unlock()
	}
	return tasks
}

// GetPreservedTasks returns persisted task info for any slots that preserved
// their work directories during shutdown (instead of cleaning up).
func (sm *SlotManager) GetPreservedTasks() []PersistedTask {
	var tasks []PersistedTask
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.preserved != nil {
			tasks = append(tasks, *slot.preserved)
		}
		slot.mu.Unlock()
	}
	return tasks
}

// SuspendSlot suspends a single slot identified by work unit ID.
func (sm *SlotManager) SuspendSlot(workUnitID string) error {
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil && slot.wu.ID == workUnitID {
			if slot.suspended {
				slot.mu.Unlock()
				return ErrTaskAlreadySuspended
			}
			if slot.processHandle != nil {
				if err := slot.processHandle.Suspend(); err != nil {
					slot.mu.Unlock()
					return err
				}
			}
			slot.suspended = true
			slot.pausedAt = time.Now()
			slot.mu.Unlock()
			return nil
		}
		slot.mu.Unlock()
	}
	return ErrTaskNotFound
}

// ResumeSlot resumes a single suspended slot identified by work unit ID.
func (sm *SlotManager) ResumeSlot(workUnitID string) error {
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil && slot.wu.ID == workUnitID {
			if !slot.suspended {
				slot.mu.Unlock()
				return ErrTaskNotSuspended
			}
			// Accumulate pause duration before resuming.
			if !slot.pausedAt.IsZero() {
				slot.totalPausedDur += time.Since(slot.pausedAt)
				slot.pausedAt = time.Time{}
			}
			if slot.processHandle != nil {
				if err := slot.processHandle.Resume(); err != nil {
					slot.mu.Unlock()
					return err
				}
			}
			slot.suspended = false
			slot.mu.Unlock()
			return nil
		}
		slot.mu.Unlock()
	}
	return ErrTaskNotFound
}

// AbortSlot cancels a single slot identified by work unit ID, killing its process.
func (sm *SlotManager) AbortSlot(workUnitID string) error {
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil && slot.wu.ID == workUnitID {
			if slot.cancel != nil {
				slot.cancel()
			}
			slot.mu.Unlock()
			return nil
		}
		slot.mu.Unlock()
	}
	return ErrTaskNotFound
}
