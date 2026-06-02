package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Sentinel errors for per-task operations.
var (
	ErrTaskNotFound         = errors.New("task not found")
	ErrTaskAlreadySuspended = errors.New("task is already suspended")
	ErrTaskNotSuspended     = errors.New("task is not suspended")
	ErrDaemonPaused         = errors.New("cannot resume task while daemon is paused")
)

// defaultWUMemoryMB is the conservative memory estimate for WUs that don't specify MaxMemoryMB.
const defaultWUMemoryMB = 512

// ExecutionSlot represents a single execution slot that runs one WU at a time.
type ExecutionSlot struct {
	ID                int
	mu                sync.Mutex
	active            bool
	wu                *runtime.WorkUnit
	prep              *runtime.PrepareResult
	rt                runtime.Runtime
	conn              *ServerConnection
	cancel            context.CancelFunc
	startedAt         time.Time
	checkpoint        *CheckpointManager
	resumedFromCkp    bool
	heartbeatInterval int32
	preserved         *PersistedTask // non-nil if work dir was preserved on shutdown
	processHandle     ProcessHandle  // for suspend/resume
	suspended         bool
	pausedAt          time.Time     // when current pause began (zero if not paused)
	totalPausedDur    time.Duration // accumulated pause time across all pause/resume cycles
	fetchedAt         time.Time     // when the WU was fetched from server (from prefetch queue)
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
// The slot goroutine handles: checkpoint restore, heartbeat, execution, cleanup.
func (sm *SlotManager) StartSlot(ctx context.Context, slotID int, item *PreFetchItem, d *Daemon) error {
	slot := sm.slots[slotID]
	slot.mu.Lock()
	slot.active = true
	slot.wu = item.WU
	slot.prep = item.Prep
	slot.rt = item.Runtime
	slot.conn = item.Conn
	if item.Prep != nil && !item.Prep.OriginalStartedAt.IsZero() {
		slot.startedAt = item.Prep.OriginalStartedAt
	} else {
		slot.startedAt = time.Now()
	}
	slot.resumedFromCkp = false
	slot.heartbeatInterval = item.WUResp.HeartbeatIntervalSeconds
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
	heartbeatInterval := item.WUResp.HeartbeatIntervalSeconds

	var execResult *runtime.ExecutionResult
	var execErr error

	defer func() {
		// If daemon is shutting down and execution was interrupted (not completed
		// successfully), preserve the work directory for resumption on next startup.
		if sm.shuttingDown.Load() && execErr != nil {
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
				HeartbeatIntervalSecs:  slot.heartbeatInterval,
				StartedAt:              slot.startedAt,
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
		slot.pausedAt = time.Time{}
		slot.totalPausedDur = 0
		slot.fetchedAt = time.Time{}
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

	// Hand off heartbeating: stop the PREPARING heartbeat (kept the unit fresh
	// during the pull and queue wait) now that the slot's RUNNING heartbeat —
	// which also transitions the unit ASSIGNED -> RUNNING — takes over.
	item.stopHeartbeat()

	// Start heartbeat goroutine.
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	workDir := ""
	if prep != nil {
		workDir = prep.WorkDir
	}
	go d.runSlotHeartbeat(heartbeatCtx, wu, workDir, heartbeatInterval, cancelExec, conn, checkpointMgr)

	// Wire process handle callbacks for suspend/resume.
	if prep != nil {
		prep.PIDCallback = func(pid int) {
			slot.mu.Lock()
			slot.processHandle = NewNativeProcessHandle(pid)
			slot.mu.Unlock()
		}
		if cr, ok := rt.(*runtime.ContainerRuntime); ok {
			prep.ContainerIDCallback = func(containerID string) {
				slot.mu.Lock()
				slot.processHandle = NewContainerProcessHandle(cr.Client(), containerID)
				slot.mu.Unlock()
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

	// Stop checkpoint manager before heartbeat so final checkpoint is saved.
	if checkpointMgr != nil {
		checkpointMgr.Stop()
	}

	// Stop heartbeat.
	heartbeatCancel()
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
				output, _ := os.ReadFile(outputPath)

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

// GetCurrentTasks returns info about all active slots' work units.
// benchmarkFPOPS is the volunteer's CPU benchmark score for time estimation.
// dcfFunc returns the duration correction factor for a given leaf ID.
func (sm *SlotManager) GetCurrentTasks(benchmarkFPOPS float64, dcfFunc func(leafID string) float64) []CurrentTask {
	var tasks []CurrentTask
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil {
			// Compute total paused seconds including any ongoing pause.
			pausedDur := slot.totalPausedDur
			if !slot.pausedAt.IsZero() {
				pausedDur += time.Since(slot.pausedAt)
			}

			task := CurrentTask{
				WorkUnitID:            slot.wu.ID,
				LeafID:                slot.wu.LeafID,
				StartedAt:             slot.startedAt,
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

// TotalActiveMemoryMB returns the sum of MaxMemoryMB across all active slots' WUs.
func (sm *SlotManager) TotalActiveMemoryMB() int {
	total := 0
	for _, slot := range sm.slots {
		slot.mu.Lock()
		if slot.active && slot.wu != nil {
			mem := int(slot.wu.ExecutionSpec.MaxMemoryMB)
			if mem <= 0 {
				mem = defaultWUMemoryMB
			}
			total += mem
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
		if slot.active && slot.processHandle != nil && !slot.suspended {
			if err := slot.processHandle.Suspend(); err != nil {
				sm.logger.Warn("failed to suspend process", "slot", slot.ID, "error", err)
			} else {
				slot.suspended = true
				slot.pausedAt = time.Now()
			}
		}
		slot.mu.Unlock()
	}
}

// ResumeAll unfreezes all suspended slot processes.
func (sm *SlotManager) ResumeAll() {
	for _, slot := range sm.slots {
		slot.mu.Lock()
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
	slot := sm.slots[slotID]
	slot.mu.Lock()
	slot.processHandle = handle
	slot.mu.Unlock()
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
				HeartbeatIntervalSecs: slot.heartbeatInterval,
				StartedAt:             slot.startedAt,
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
