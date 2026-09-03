package management

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon/procmetrics"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// DaemonBridge provides thread-safe access to daemon state for the management API.
type DaemonBridge struct {
	daemon  *daemon.Daemon
	cfgPath string
	eta     *etaTracker
	// cfgMu serializes this bridge's config write-backs (UpdateConfig,
	// AttachLeaf, DetachLeaf) so two concurrent API writes cannot interleave
	// their load-modify-save cycles and drop each other's changes.
	cfgMu sync.Mutex
}

// NewDaemonBridge creates a bridge between the management API and the daemon.
func NewDaemonBridge(d *daemon.Daemon, cfgPath string) *DaemonBridge {
	return &DaemonBridge{
		daemon:  d,
		cfgPath: cfgPath,
		eta:     newETATracker(),
	}
}

// leafNameByID builds a leaf ID -> display name map from the leaf cache.
func (b *DaemonBridge) leafNameByID() map[string]string {
	m := make(map[string]string)
	lc := b.daemon.GetLeafCache()
	if lc == nil {
		return m
	}
	for _, leafs := range lc.AllLeafs() {
		for _, l := range leafs {
			if l.Name != "" {
				m[l.ID] = l.Name
			}
		}
	}
	return m
}

// resolveLeafName returns the leaf display name for a given ID, falling back to the ID itself.
func (b *DaemonBridge) resolveLeafName(leafID string) string {
	names := b.leafNameByID()
	if name, ok := names[leafID]; ok {
		return name
	}
	return leafID
}

// historyLeafName is the display name for a history entry: the name the daemon
// recorded at completion (TB-46), else the live cache's answer for the id, else
// the id itself. Entries written before names were recorded, or whose leaf has
// since left the cache, are why both fallbacks remain.
func (b *DaemonBridge) historyLeafName(e daemon.HistoryEntry) string {
	if e.LeafName != "" {
		return e.LeafName
	}
	return b.resolveLeafName(e.LeafID)
}

// StatusResponse is the response for GET /api/v1/status.
type StatusResponse struct {
	State            string           `json:"state"`
	UptimeSeconds    int              `json:"uptime_seconds"`
	ConnectedServers int              `json:"connected_servers"`
	ActiveTasks      []ActiveTaskInfo `json:"active_tasks"`
	QueuedTasks      []QueuedTaskInfo `json:"queued_tasks"`
	PausedReason     *string          `json:"paused_reason"`
	// ClientVersion is this volunteer build's version string (what
	// `lettuce-volunteer --version` prints), so a client can compare it with
	// each head's head_version on GET /api/v1/heads.
	ClientVersion string `json:"client_version"`
	// FailingLeafs lists every leaf whose units have failed locally since the
	// daemon started, newest failure first. It exists so a volunteer can see that
	// work IS arriving and failing, rather than concluding they are never sent
	// work of that kind — the exact misreading TB-10 was filed for.
	FailingLeafs []FailingLeafInfo `json:"failing_leafs"`
}

// FailingLeafInfo is one leaf's local failure record, as reported by `status`.
type FailingLeafInfo struct {
	LeafID              string `json:"leaf_id"`
	LeafName            string `json:"leaf_name"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	TotalFailures       int    `json:"total_failures"`
	LastReason          string `json:"last_reason,omitempty"`
	LastFailedAt        string `json:"last_failed_at,omitempty"`
	// Paused reports that the per-leaf breaker has stopped requesting this leaf;
	// PausedUntil is when it will be retried.
	Paused      bool   `json:"paused"`
	PausedUntil string `json:"paused_until,omitempty"`
}

// QueuedTaskInfo describes a work unit waiting in the prefetch queue.
type QueuedTaskInfo struct {
	WorkUnitID      string `json:"work_unit_id"`
	LeafName        string `json:"leaf_name"`
	DeadlineSeconds int32  `json:"deadline_seconds"`
	FetchedAt       string `json:"fetched_at"`
	ServerName      string `json:"server_name"`
}

// ActiveTaskInfo describes an in-progress work unit.
type ActiveTaskInfo struct {
	WorkUnitID            string  `json:"work_unit_id"`
	LeafName              string  `json:"leaf_name"`
	ProgressPct           int     `json:"progress_pct"`
	ElapsedSeconds        int     `json:"elapsed_seconds"`
	EstimatedRemainingSec *int    `json:"estimated_remaining_seconds,omitempty"`
	WorkDir               string  `json:"work_dir"`
	VizBundlePath         *string `json:"viz_bundle_path"`
	CheckpointSequence    int32   `json:"checkpoint_sequence,omitempty"`
	LastCheckpointAt      *string `json:"last_checkpoint_at,omitempty"`
	ResumedFromCheckpoint bool    `json:"resumed_from_checkpoint,omitempty"`
	CPUSeconds            int     `json:"cpu_seconds"`
	TaskStatus            string  `json:"task_status"`
	StatusReason          *string `json:"status_reason"`
	DeadlineSeconds       int     `json:"deadline_seconds"`
	HeadName              string  `json:"head_name"`
	RuntimeType           string  `json:"runtime_type"`
	ProcessID             *int    `json:"process_id"`
}

// computeTaskStatus determines the status string and reason for an active
// task. "User paused" is claimed only when it is the one remaining
// explanation: with the daemon itself paused for a reason nobody named, an
// unexplained suspension must say the reason is unknown rather than invent a
// user action — the default arm is exactly how schedule-gate-suspended tasks
// spent months displaying "User paused" (TB-44). With the daemon NOT paused,
// a suspended slot is the per-task suspend verb, which IS a user action.
func computeTaskStatus(task daemon.CurrentTask, pauseReason string, daemonPaused bool) (status string, reason *string) {
	if task.Suspended {
		switch pauseReason {
		case "thermal":
			status = "suspended_thermal"
			r := "CPU temperature exceeded threshold"
			return status, &r
		case "scheduled":
			status = "suspended_scheduled"
			r := "Outside scheduled computing hours"
			return status, &r
		case "user":
			status = "suspended_user"
			r := "User paused"
			return status, &r
		default:
			if daemonPaused {
				status = "suspended"
				r := "Paused (reason not reported)"
				return status, &r
			}
			status = "suspended_user"
			r := "User paused"
			return status, &r
		}
	}
	return "running", nil
}

// buildActiveTaskInfo converts a daemon.CurrentTask into the API's ActiveTaskInfo,
// computing derived fields (progress, elapsed, CPU seconds, ETA).
func (b *DaemonBridge) buildActiveTaskInfo(t daemon.CurrentTask, pauseReason string, daemonPaused bool) ActiveTaskInfo {
	taskStatus, statusReason := computeTaskStatus(t, pauseReason, daemonPaused)

	info := ActiveTaskInfo{
		WorkUnitID:            t.WorkUnitID,
		LeafName:              b.resolveLeafName(t.LeafID),
		WorkDir:               t.WorkDir,
		CheckpointSequence:    t.CheckpointSequence,
		ResumedFromCheckpoint: t.ResumedFromCheckpoint,
		TaskStatus:            taskStatus,
		StatusReason:          statusReason,
		HeadName:              t.ServerName,
		RuntimeType:           t.RuntimeType,
	}
	if t.ProcessID != 0 {
		pid := t.ProcessID
		info.ProcessID = &pid
	}
	if t.VizBundlePath != "" {
		vbp := t.VizBundlePath
		info.VizBundlePath = &vbp
	}
	if t.WorkDir != "" {
		info.ProgressPct = int(daemon.ReadProgressFile(t.WorkDir))
	}
	// Run time accrued only while actually executing under a live daemon — excludes the
	// wall-clock gap during which the daemon was stopped (see CurrentTask.ElapsedSeconds).
	info.ElapsedSeconds = t.ElapsedSeconds
	if !t.LastCheckpointAt.IsZero() {
		ts := t.LastCheckpointAt.UTC().Format(time.RFC3339)
		info.LastCheckpointAt = &ts
	}
	// CPU seconds = elapsed minus paused time.
	info.CPUSeconds = info.ElapsedSeconds - t.TotalPausedSeconds
	if info.CPUSeconds < 0 {
		info.CPUSeconds = 0
	}
	// Deadline: remaining seconds until deadline expires.
	if t.DeadlineSeconds > 0 {
		info.DeadlineSeconds = int(t.DeadlineSeconds) - info.ElapsedSeconds
	}
	// Estimated remaining time: a smoothed recent-progress-rate estimate blended with
	// the benchmark estimate (see etaTracker), falling back to the benchmark estimate
	// alone before there is live progress.
	if remaining, ok := b.eta.estimate(t.WorkUnitID, info.ProgressPct, info.ElapsedSeconds, t.EstimatedSeconds); ok {
		info.EstimatedRemainingSec = &remaining
	}
	return info
}

// GetStatus returns the current daemon state.
func (b *DaemonBridge) GetStatus() StatusResponse {
	daemonPaused := b.daemon.IsPaused()
	state := "stopped"
	if b.daemon.IsRunning() {
		if daemonPaused {
			state = "paused"
		} else {
			state = "active"
		}
	}

	var uptime int
	startedAt := b.daemon.GetStartedAt()
	if !startedAt.IsZero() {
		uptime = int(time.Since(startedAt).Seconds())
	}

	connectedServers := 0
	if mc := b.daemon.GetMultiClient(); mc != nil {
		for _, s := range mc.Servers() {
			if s.Available {
				connectedServers++
			}
		}
	}

	pauseReason := b.daemon.PauseReason()

	var activeTasks []ActiveTaskInfo
	for _, t := range b.daemon.GetCurrentTasks() {
		activeTasks = append(activeTasks, b.buildActiveTaskInfo(t, pauseReason, daemonPaused))
	}
	if activeTasks == nil {
		activeTasks = []ActiveTaskInfo{}
	}
	// Drop ETA state for work units that are no longer active.
	activeIDs := make(map[string]bool, len(activeTasks))
	for _, at := range activeTasks {
		activeIDs[at.WorkUnitID] = true
	}
	b.eta.retain(activeIDs)

	var pausedReasonPtr *string
	if pauseReason != "" {
		pausedReasonPtr = &pauseReason
	}

	var queuedTasks []QueuedTaskInfo
	for _, qt := range b.daemon.GetQueuedTasks() {
		queuedTasks = append(queuedTasks, QueuedTaskInfo{
			WorkUnitID:      qt.WorkUnitID,
			LeafName:        b.resolveLeafName(qt.LeafID),
			DeadlineSeconds: qt.DeadlineSeconds,
			FetchedAt:       qt.FetchedAt.UTC().Format(time.RFC3339),
			ServerName:      qt.ServerName,
		})
	}
	if queuedTasks == nil {
		queuedTasks = []QueuedTaskInfo{}
	}

	return StatusResponse{
		State:            state,
		UptimeSeconds:    uptime,
		ConnectedServers: connectedServers,
		ActiveTasks:      activeTasks,
		QueuedTasks:      queuedTasks,
		PausedReason:     pausedReasonPtr,
		FailingLeafs:     b.failingLeafs(),
		ClientVersion:    b.daemon.ClientVersion(),
	}
}

// NoticesResponse is the response for GET /api/v1/notices.
type NoticesResponse struct {
	// Notices is most recently updated first.
	Notices []daemon.Notice `json:"notices"`
	// LatestID is the highest notice id ever assigned by this daemon run (0
	// when none). A client polls with ?since=<latest_id> to receive only
	// notices created after its last poll.
	LatestID uint64 `json:"latest_id"`
}

// GetNotices returns the volunteer-facing notices created after since (all of
// them when since is 0).
func (b *DaemonBridge) GetNotices(since uint64) NoticesResponse {
	notices, latest := b.daemon.Notices().Since(since)
	return NoticesResponse{Notices: notices, LatestID: latest}
}

// failingLeafs renders the daemon's per-leaf failure records for the API,
// resolving each leaf id to its display name.
func (b *DaemonBridge) failingLeafs() []FailingLeafInfo {
	snap := b.daemon.LeafFailureSnapshot()
	out := make([]FailingLeafInfo, 0, len(snap))
	for _, s := range snap {
		info := FailingLeafInfo{
			LeafID:              s.LeafID,
			LeafName:            b.resolveLeafName(s.LeafID),
			ConsecutiveFailures: s.Consecutive,
			TotalFailures:       s.Total,
			LastReason:          s.LastReason,
			Paused:              s.Paused,
		}
		if !s.LastFailedAt.IsZero() {
			info.LastFailedAt = s.LastFailedAt.UTC().Format(time.RFC3339)
		}
		if !s.PausedUntil.IsZero() {
			info.PausedUntil = s.PausedUntil.UTC().Format(time.RFC3339)
		}
		out = append(out, info)
	}
	return out
}

// Pause pauses the daemon. Returns error if already paused.
func (b *DaemonBridge) Pause() error {
	return b.daemon.Pause()
}

// Resume resumes the daemon. Returns error if not paused.
func (b *DaemonBridge) Resume() error {
	return b.daemon.Resume()
}

// SuspendAndQuit suspends all compute processes, saves PIDs, releases children,
// and stops the daemon. Frozen processes survive as orphans for the next launch.
func (b *DaemonBridge) SuspendAndQuit() {
	b.daemon.SuspendAndQuit()
}

// MetricsResponse is the response for GET /api/v1/metrics.
type MetricsResponse struct {
	CPUUsagePct   float64 `json:"cpu_usage_pct"`
	GPUUsagePct   float64 `json:"gpu_usage_pct"`
	MemoryUsedMB  int     `json:"memory_used_mb"`
	MemoryTotalMB int     `json:"memory_total_mb"`
	// The disk figures are the fetch gate's own (TB-24): DiskUsedMB is
	// Lettuce's measured footprint — the data-dir tree plus cached container
	// images — and DiskAllowanceMB is the max_disk_gb allowance it is budgeted
	// against. DiskUsageKnown false means the daemon cannot measure usage right
	// now; consumers must treat DiskUsedMB as absent then, never as zero —
	// doctor quotes these instead of measuring for itself (TB-30). MB integers,
	// not GB floats, so the reader and the gate quote identical numbers.
	DiskUsedMB      int64 `json:"disk_used_mb"`
	DiskAllowanceMB int64 `json:"disk_allowance_mb"`
	DiskUsageKnown  bool  `json:"disk_usage_known"`
	CPUTempC        int   `json:"cpu_temp_c"`
	GPUTempC        int   `json:"gpu_temp_c"`
}

// GetMetrics returns current resource usage metrics. The CPU/GPU/memory/
// temperature fields still require platform-specific collection and remain
// zero until that is integrated.
func (b *DaemonBridge) GetMetrics() MetricsResponse {
	resp := MetricsResponse{}
	usedMB, allowanceMB, ok := b.daemon.DiskUsage()
	resp.DiskAllowanceMB = allowanceMB
	resp.DiskUsageKnown = ok
	if ok {
		resp.DiskUsedMB = usedMB
	}
	return resp
}

// LeafInfo describes an attached leaf/server.
type LeafInfo struct {
	ServerName         string `json:"server_name"`
	ServerAddress      string `json:"server_address"`
	LeafID             string `json:"leaf_id,omitempty"`
	LeafName           string `json:"leaf_name,omitempty"`
	Status             string `json:"status"`
	CreditEarned       int    `json:"credit_earned"`
	WorkUnitsCompleted int    `json:"work_units_completed"`
}

// GetLeafs returns the list of attached leafs/servers.
func (b *DaemonBridge) GetLeafs() []LeafInfo {
	cfg := b.daemon.GetConfig()
	mc := b.daemon.GetMultiClient()

	serverStatus := make(map[string]bool)
	if mc != nil {
		for _, s := range mc.Servers() {
			serverStatus[s.Name] = s.Available
		}
	}

	// Aggregate credit/WU counts from history.
	entries := readAllHistory(cfg.DataDir)
	serverCredit := make(map[string]int)
	serverWUs := make(map[string]int)
	// Default server name for history entries that predate server_name tracking.
	defaultServer := ""
	if len(cfg.Servers) > 0 {
		defaultServer = cfg.Servers[0].DisplayName()
	}
	for _, e := range entries {
		if e.ResultAccepted {
			name := e.ServerName
			if name == "" {
				name = defaultServer
			}
			serverCredit[name]++
			serverWUs[name]++
		}
	}

	var leafs []LeafInfo
	for _, srv := range cfg.Servers {
		name := srv.DisplayName()
		status := "disconnected"
		if serverStatus[name] {
			status = "connected"
		}
		info := LeafInfo{
			ServerName:         name,
			ServerAddress:      srv.GRPCAddress,
			Status:             status,
			CreditEarned:       serverCredit[name],
			WorkUnitsCompleted: serverWUs[name],
		}
		if len(srv.PinnedLeafIDs) == 0 {
			leafs = append(leafs, info)
			continue
		}
		// One row per explicitly pinned leaf (PB-16: pins live ON the head entry
		// now, several per head).
		for _, pin := range srv.PinnedLeafIDs {
			row := info
			row.LeafID = pin
			leafs = append(leafs, row)
		}
	}
	if leafs == nil {
		leafs = []LeafInfo{}
	}
	return leafs
}

// AttachRequest is the request body for POST /api/v1/leafs/attach.
type AttachRequest struct {
	ServerAddress string `json:"server_address"`
	LeafID        string `json:"leaf_id,omitempty"`
	Name          string `json:"name,omitempty"`
	// TrustedRuntimes is the runtime trust to record for the new head: which
	// runtime kinds beyond WASM ("CONTAINER", "NATIVE"; case-insensitive) its
	// operator may run on this machine. Absent or empty means WASM only — the
	// safe default when the client offered no consent step. See
	// parseTrustedRuntimes for validation.
	TrustedRuntimes []string `json:"trusted_runtimes,omitempty"`
}

// parseTrustedRuntimes validates and normalises a trusted_runtimes list from
// an API request into the stored form: UPPERCASE, de-duplicated, sorted, and
// never nil — an explicit empty list is the recorded "WASM only" decision
// (PB-28), and a nil would be re-pinned by the load-time migration as a
// legacy blank. Only CONTAINER and NATIVE are accepted: WASM is always
// trusted and is never stored, so listing it is a caller error rather than a
// no-op, and any other token is rejected so a typo cannot silently narrow or
// widen trust. Mirrors the CLI's `heads trust` semantics: the list REPLACES
// the head's trust rather than merging into it.
func parseTrustedRuntimes(raw []string) ([]string, error) {
	seen := make(map[string]bool, len(raw))
	out := []string{}
	for _, r := range raw {
		u := strings.ToUpper(strings.TrimSpace(r))
		switch u {
		case "CONTAINER", "NATIVE":
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		default:
			return nil, fmt.Errorf("trusted_runtimes: unknown runtime %q (valid: CONTAINER, NATIVE; WASM is always allowed and is not listed)", strings.TrimSpace(r))
		}
	}
	sort.Strings(out)
	return out, nil
}

// trustedRuntimesEqual reports whether two trust lists grant the same
// runtimes, ignoring case, order and duplicates — so a PUT that re-sends the
// current trust is not reported as a change needing a restart.
func trustedRuntimesEqual(a, b []string) bool {
	set := func(list []string) map[string]bool {
		m := make(map[string]bool, len(list))
		for _, r := range list {
			u := strings.ToUpper(strings.TrimSpace(r))
			if u != "" && u != "WASM" {
				m[u] = true
			}
		}
		return m
	}
	sa, sb := set(a), set(b)
	if len(sa) != len(sb) {
		return false
	}
	for r := range sa {
		if !sb[r] {
			return false
		}
	}
	return true
}

// loadWriteBase returns the config a bridge write-back must start from: the
// CURRENT on-disk file, not the daemon's in-memory copy. The two diverge
// whenever the CLI edits config.yaml while the daemon runs — most critically
// `heads trust <head> none`, which revokes runtime trust on disk and tells the
// user to restart. Persisting the daemon's boot-time snapshot here silently
// overwrote that revocation with the stale, wider trust (PB-28), so disk state
// is authoritative and every bridge write is rebased onto it. The in-memory
// config is the base only when no config file exists at all — then there is no
// disk-side decision to preserve.
//
// DataDir is always carried over from the live config: it is resolved at
// startup (--data-dir applied, made absolute) and the running daemon's paths
// must not move because a write-back re-read a relative or stale value from
// the file.
func (b *DaemonBridge) loadWriteBase() (*config.Config, error) {
	live := b.daemon.GetConfig()
	if _, err := os.Stat(b.cfgPath); os.IsNotExist(err) {
		base := *live
		base.Servers = make([]config.ServerConfig, len(live.Servers))
		copy(base.Servers, live.Servers)
		return &base, nil
	}
	base, err := config.Load(b.cfgPath)
	if err != nil {
		return nil, fmt.Errorf("reading current config from disk: %w", err)
	}
	base.DataDir = live.DataDir
	return base, nil
}

// AttachLeaf adds a server to the configuration and persists it.
func (b *DaemonBridge) AttachLeaf(req AttachRequest) error {
	if req.ServerAddress == "" {
		return fmt.Errorf("server_address is required")
	}
	// Validate before taking the lock or touching disk: a bad trust list must
	// leave the config untouched.
	trusted, err := parseTrustedRuntimes(req.TrustedRuntimes)
	if err != nil {
		return err
	}
	// The desktop app hands over whatever the volunteer typed into its address
	// field — possibly "https://host/" (its own Test Connection accepts that).
	// Derive the gRPC target, the HTTP base and the default name from the
	// parsed address, as `init` and `attach` do, instead of storing the raw
	// string as a gRPC target that can never resolve (TB-51).
	addr, err := config.ParseHeadAddress(req.ServerAddress)
	if err != nil {
		return err
	}

	b.cfgMu.Lock()
	defer b.cfgMu.Unlock()

	// Rebase onto the on-disk config (see loadWriteBase); the daemon's live
	// config is unchanged unless Save succeeds.
	newCfg, err := b.loadWriteBase()
	if err != nil {
		return err
	}

	// Check for duplicates.
	grpcAddr := addr.GRPCAddress()
	for _, s := range newCfg.Servers {
		if s.GRPCAddress == grpcAddr {
			return fmt.Errorf("already attached to %s", grpcAddr)
		}
	}

	name := req.Name
	if name == "" {
		name = addr.Host
	}

	sc := config.ServerConfig{
		GRPCAddress: grpcAddr,
		HTTPAddress: addr.HTTPAddress(),
		Name:        name,
		Insecure:    addr.Insecure,
		// Exactly what the request granted — the client's consent step, if it
		// had one — and an explicit empty list otherwise, so the new head starts
		// WASM-only as a recorded decision, not a legacy blank (PB-28).
		TrustedRuntimes: trusted,
	}
	if req.LeafID != "" {
		sc.PinnedLeafIDs = []string{req.LeafID}
	}
	newCfg.Servers = append(newCfg.Servers, sc)

	if err := newCfg.Save(b.cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	b.daemon.ApplyConfig(newCfg)
	return nil
}

// DetachRequest is the request body for POST /api/v1/leafs/detach.
type DetachRequest struct {
	ServerName    string `json:"server_name,omitempty"`
	ServerAddress string `json:"server_address,omitempty"`
}

// DetachLeaf removes a server from the configuration.
func (b *DaemonBridge) DetachLeaf(req DetachRequest) error {
	if req.ServerName == "" && req.ServerAddress == "" {
		return fmt.Errorf("server_name or server_address is required")
	}

	b.cfgMu.Lock()
	defer b.cfgMu.Unlock()

	// Rebase onto the on-disk config (see loadWriteBase); the daemon's live
	// config is unchanged unless Save succeeds.
	newCfg, err := b.loadWriteBase()
	if err != nil {
		return err
	}

	found := false
	var remaining []config.ServerConfig

	for _, s := range newCfg.Servers {
		name := s.DisplayName()
		if (req.ServerName != "" && name == req.ServerName) ||
			(req.ServerAddress != "" && s.GRPCAddress == req.ServerAddress) {
			found = true
			continue
		}
		remaining = append(remaining, s)
	}

	if !found {
		return errNotFound
	}

	newCfg.Servers = remaining

	if err := newCfg.Save(b.cfgPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	b.daemon.ApplyConfig(newCfg)
	return nil
}

// sentinel error for not-found detach.
var errNotFound = fmt.Errorf("server not found")

// AvailableLeaf describes a leaf available on a connected server.
type AvailableLeaf struct {
	ServerName   string `json:"server_name"`
	LeafID       string `json:"leaf_id"`
	LeafName     string `json:"leaf_name"`
	Description  string `json:"description,omitempty"`
	ResearchArea string `json:"research_area,omitempty"`
}

// GetAvailableLeafsLegacy queries all connected servers for available leafs.
// For now returns a list based on configured servers — full gRPC browsing
// requires the ListLeafs RPC which will be connected in a future session.
func (b *DaemonBridge) GetAvailableLeafsLegacy(search, area string) []AvailableLeaf {
	cfg := b.daemon.GetConfig()
	var leafs []AvailableLeaf

	for _, srv := range cfg.Servers {
		name := srv.DisplayName()

		for _, pin := range srv.PinnedLeafIDs {
			p := AvailableLeaf{
				ServerName: name,
				LeafID:     pin,
				LeafName:   name,
			}

			if search != "" && !containsIgnoreCase(p.LeafName, search) && !containsIgnoreCase(p.LeafID, search) {
				continue
			}
			if area != "" && !containsIgnoreCase(p.ResearchArea, area) {
				continue
			}
			leafs = append(leafs, p)
		}
	}

	if leafs == nil {
		leafs = []AvailableLeaf{}
	}
	return leafs
}

func containsIgnoreCase(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// HistoryResponse is the response for GET /api/v1/history.
type HistoryResponse struct {
	Entries    []HistoryEntryInfo `json:"entries"`
	Pagination PaginationInfo     `json:"pagination"`
}

// HistoryEntryInfo describes a completed work unit.
type HistoryEntryInfo struct {
	WorkUnitID       string `json:"work_unit_id"`
	LeafName         string `json:"leaf_name"`
	CompletedAt      string `json:"completed_at"`
	DurationSeconds  int64  `json:"duration_seconds"`
	CPUSeconds       int64  `json:"cpu_seconds"`
	CreditEarned     int    `json:"credit_earned"`
	ValidationStatus string `json:"validation_status"`
	HeadName         string `json:"head_name"`
}

// PaginationInfo provides cursor-based pagination info.
type PaginationInfo struct {
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

// GetHistory returns completed work units with cursor-based pagination.
func (b *DaemonBridge) GetHistory(cursor string, limit int, leafID, from, to string) HistoryResponse {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	cfg := b.daemon.GetConfig()
	entries := readAllHistory(cfg.DataDir)

	// Apply filters.
	var filtered []daemon.HistoryEntry
	var fromTime, toTime time.Time
	if from != "" {
		fromTime, _ = time.Parse(time.RFC3339, from)
	}
	if to != "" {
		toTime, _ = time.Parse(time.RFC3339, to)
	}

	for _, e := range entries {
		if leafID != "" && e.LeafID != leafID {
			continue
		}
		if !fromTime.IsZero() && e.CompletedAt.Before(fromTime) {
			continue
		}
		if !toTime.IsZero() && e.CompletedAt.After(toTime) {
			continue
		}
		filtered = append(filtered, e)
	}

	// Apply cursor (cursor is the index as string).
	startIdx := 0
	if cursor != "" {
		if idx, err := strconv.Atoi(cursor); err == nil {
			startIdx = idx
		}
	}

	if startIdx >= len(filtered) {
		return HistoryResponse{
			Entries:    []HistoryEntryInfo{},
			Pagination: PaginationInfo{},
		}
	}

	end := startIdx + limit
	hasMore := end < len(filtered)
	if end > len(filtered) {
		end = len(filtered)
	}

	page := filtered[startIdx:end]
	result := make([]HistoryEntryInfo, len(page))
	for i, e := range page {
		validationStatus := "rejected"
		if e.ResultAccepted {
			validationStatus = "accepted"
		}
		result[i] = HistoryEntryInfo{
			WorkUnitID:       e.WorkUnitID,
			LeafName:         b.historyLeafName(e),
			CompletedAt:      e.CompletedAt.Format(time.RFC3339),
			DurationSeconds:  e.WallClockSeconds,
			CPUSeconds:       e.CPUSeconds,
			CreditEarned:     0, // Credit tracking not yet integrated
			ValidationStatus: validationStatus,
			HeadName:         e.ServerName,
		}
	}

	var nextCursor string
	if hasMore {
		nextCursor = strconv.Itoa(end)
	}

	return HistoryResponse{
		Entries: result,
		Pagination: PaginationInfo{
			NextCursor: nextCursor,
			HasMore:    hasMore,
		},
	}
}

// readAllHistory reads all history entries (newest first). A file that cannot be
// read is an empty history here: the surfaces built on it are secondary.
func readAllHistory(dataDir string) []daemon.HistoryEntry {
	entries, _ := daemon.ReadAllHistory(dataDir)
	return entries
}

// ConfigResponse is the response for GET /api/v1/config.
type ConfigResponse struct {
	DataDir        string                    `json:"data_dir"`
	PublicKey      string                    `json:"public_key,omitempty"`
	ResourceLimits config.ResourceLimits     `json:"resource_limits"`
	Scheduling     config.Scheduling         `json:"scheduling"`
	Leafs          config.LeafFilter         `json:"leafs"`
	Thermal        config.ThermalConfig      `json:"thermal"`
	Notifications  config.NotificationConfig `json:"notifications"`
	Servers        []config.ServerConfig     `json:"servers"`
	LogLevel       string                    `json:"log_level"`
	MaxConcurrent  int                       `json:"max_concurrent_tasks"`
	// WorkBufferHours is how many hours of work the daemon keeps buffered per
	// execution slot (0 = a small unit-count fallback). PUT already accepted it;
	// it is returned here so a client can show the current value it writes.
	WorkBufferHours float64 `json:"work_buffer_hours"`
}

// GetConfig returns the current configuration (with sensitive paths redacted).
func (b *DaemonBridge) GetConfig() ConfigResponse {
	cfg := b.daemon.GetConfig()

	var pubKeyStr string
	if pubBytes, err := os.ReadFile(cfg.PubKeyFilePath()); err == nil {
		pubKeyStr = identity.PublicKeyToBase64URL(pubBytes)
	}

	return ConfigResponse{
		DataDir:         cfg.DataDir,
		PublicKey:       pubKeyStr,
		ResourceLimits:  cfg.ResourceLimits,
		Scheduling:      cfg.Scheduling,
		Leafs:           cfg.Leafs,
		Thermal:         cfg.Thermal,
		Notifications:   cfg.Notifications,
		Servers:         cfg.Servers,
		LogLevel:        cfg.LogLevel,
		MaxConcurrent:   cfg.MaxConcurrentTasks,
		WorkBufferHours: cfg.WorkBufferHours,
	}
}

// UpdateConfigResponse is the response for PUT /api/v1/config: the resulting
// configuration plus whether the daemon must be restarted for part of the
// change to take effect.
type UpdateConfigResponse struct {
	ConfigResponse
	// RestartRequired is true when a setting that is only read at start-up
	// changed: a head's trusted_runtimes (applied when the daemon builds its
	// runtime registry and advertises runtimes to each head), or
	// max_concurrent_tasks (the slot count). Such a change is on disk but not
	// yet in force — exactly what `heads trust` tells the user after saving.
	RestartRequired bool `json:"restart_required"`
}

// UpdateConfig applies a partial config update, validates, persists, and applies.
func (b *DaemonBridge) UpdateConfig(partial map[string]any) (*UpdateConfigResponse, error) {
	b.cfgMu.Lock()
	defer b.cfgMu.Unlock()

	// Rebase the partial onto the on-disk config (see loadWriteBase); the
	// daemon's live config is unchanged unless validation and Save succeed.
	newCfg, err := b.loadWriteBase()
	if err != nil {
		return nil, err
	}

	// Apply partial updates to recognized fields.
	if v, ok := partial["resource_limits"]; ok {
		if rl, ok := v.(map[string]any); ok {
			applyResourceLimits(&newCfg.ResourceLimits, rl)
		}
	}
	if v, ok := partial["scheduling"]; ok {
		if sched, ok := v.(map[string]any); ok {
			applyScheduling(&newCfg.Scheduling, sched)
		}
	}
	if v, ok := partial["thermal"]; ok {
		if th, ok := v.(map[string]any); ok {
			applyThermal(&newCfg.Thermal, th)
		}
	}
	if v, ok := partial["notifications"]; ok {
		if n, ok := v.(map[string]any); ok {
			applyNotifications(&newCfg.Notifications, n)
		}
	}
	if v, ok := partial["log_level"]; ok {
		if s, ok := v.(string); ok {
			newCfg.LogLevel = s
		}
	}
	// The slot count is fixed when the daemon starts, so a change here is on
	// disk but not in force until restart (ApplyConfig logs the same).
	maxConcurrentChanged := false
	if v, ok := partial["max_concurrent_tasks"]; ok {
		if n := toInt(v); n != newCfg.MaxConcurrentTasks {
			newCfg.MaxConcurrentTasks = n
			maxConcurrentChanged = true
		}
	}
	if v, ok := partial["leafs"]; ok {
		if p, ok := v.(map[string]any); ok {
			applyLeafFilter(&newCfg.Leafs, p)
		}
	}
	if v, ok := partial["work_buffer_hours"]; ok {
		newCfg.WorkBufferHours = toFloat(v)
	}
	trustChanged := false
	if v, ok := partial["servers"]; ok {
		if serverList, ok := v.([]any); ok {
			changed, err := applyServers(newCfg, serverList)
			if err != nil {
				return nil, err
			}
			trustChanged = changed
		}
	}

	if err := newCfg.Validate(); err != nil {
		return nil, err
	}

	if err := newCfg.Save(b.cfgPath); err != nil {
		return nil, fmt.Errorf("saving config: %w", err)
	}

	b.daemon.ApplyConfig(newCfg)

	return &UpdateConfigResponse{
		ConfigResponse:  b.GetConfig(),
		RestartRequired: trustChanged || maxConcurrentChanged,
	}, nil
}

// HeadInfo describes a connected head (server) with its leaf info.
type HeadInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	GRPCAddress string `json:"grpc_address"`
	Status      string `json:"status"` // "connected", "disconnected"
	Weight      int    `json:"weight"`
	VolunteerID string `json:"volunteer_id,omitempty"`
	// HeadVersion is the head's build version as it reported it at start-up
	// (empty when unknown). UpdateRequired is true once this head has
	// rejected this volunteer build as too old — at registration or on a
	// work request — and stays true until a later request to the head
	// succeeds; the remedy is `lettuce-volunteer update`.
	HeadVersion    string       `json:"head_version"`
	UpdateRequired bool         `json:"update_required"`
	Leafs          []LeafDetail `json:"leafs"`
	// LeafsRefreshedAt is when this head's leaf figures below were last fetched.
	// The daemon refreshes the leaf cache only inside the fetch path, so their age
	// is unbounded: a host with a full buffer, or one slot held by a long unit,
	// carries the same numbers for hours. Zero when nothing has been cached yet
	// (rendered as unknown, never as "now") — TB-14.
	LeafsRefreshedAt time.Time `json:"leafs_refreshed_at,omitzero"`
}

// LeafExecutionSpec is the JSON representation of a leaf's execution spec for the management API.
type LeafExecutionSpec struct {
	Binaries      map[string]string `json:"binaries,omitempty"`
	Image         string            `json:"image,omitempty"`
	GPURequired   bool              `json:"gpu_required,omitempty"`
	GPUType       string            `json:"gpu_type,omitempty"`
	MaxMemoryMB   int32             `json:"max_memory_mb,omitempty"`
	MaxDiskMB     int32             `json:"max_disk_mb,omitempty"`
	NetworkAccess bool              `json:"network_access,omitempty"`
}

// LeafResourceRequirements is the JSON representation of the machine budgets the
// head's dispatch gate matches this leaf against (TB-15). Absent from a head too
// old to report them.
type LeafResourceRequirements struct {
	MinDiskMB   int64 `json:"min_disk_mb,omitempty"`
	MinCPUCores int32 `json:"min_cpu_cores,omitempty"`
	// GPU dimensions (TB-21). MinGPUVRAMMB is compared against the machine's
	// ALLOWED VRAM, not its card size.
	MinGPUVRAMMB         int32  `json:"min_gpu_vram_mb,omitempty"`
	GPUType              string `json:"gpu_type,omitempty"`
	GPUComputeCapability string `json:"gpu_compute_capability,omitempty"`
	// resource_requirements.gpu_required — read together with the execution spec's
	// flag, never alone; dispatch requires a GPU when either is set.
	GPURequired bool `json:"gpu_required,omitempty"`
}

// LeafDiskGate is the RUNNING daemon's live disk-gate verdict for one leaf
// (TB-41): whether its fetcher is skipping this leaf right now and why, plus
// the max_disk_gb that would clear both the head's dispatch gate and this
// machine's current budget. Computed daemon-side because the inputs — measured
// usage and image cachedness — exist only there; a client recomputing the
// verdict from the leaf requirement alone is how three surfaces came to give
// three different answers (TB-41/TB-42).
type LeafDiskGate struct {
	Blocked   bool   `json:"blocked"`
	Reason    string `json:"reason,omitempty"`
	RaiseToGB int    `json:"raise_to_gb,omitempty"`
}

// LeafDetail describes a single leaf on a head, including effective config.
type LeafDetail struct {
	ID               string             `json:"id"`
	Slug             string             `json:"slug"`
	Name             string             `json:"name"`
	Description      string             `json:"description,omitempty"`
	ResearchArea     []string           `json:"research_area,omitempty"`
	TaskPattern      string             `json:"task_pattern"`
	State            string             `json:"state"`
	QueuedWorkUnits  int                `json:"queued_work_units"`
	ActiveVolunteers int                `json:"active_volunteers"`
	ActiveHosts      int                `json:"active_hosts"`
	Enabled          bool               `json:"enabled"`
	EffectiveWeight  int                `json:"effective_weight"`
	ExecutionSpec    *LeafExecutionSpec `json:"execution_spec,omitempty"`
	// ResourceRequirements is what the head requires of this machine before it
	// will dispatch this leaf's work. Carried so `leafs list` can say "your disk
	// allowance is below what this leaf needs" instead of promising a fetch that
	// the head will refuse (TB-15).
	ResourceRequirements *LeafResourceRequirements `json:"resource_requirements,omitempty"`
	// Failures is this leaf's local failure record, or nil if it has never
	// failed on this machine. Carried alongside the leaf so `leafs list` can say
	// "arriving and failing" in the same row that says "active" (TB-10).
	Failures *FailingLeafInfo `json:"failures,omitempty"`
	// DiskGate is the daemon's own live fetch-gate verdict for this leaf, so
	// WILL FETCH answers with the arithmetic that actually decides (TB-41).
	DiskGate *LeafDiskGate `json:"disk_gate,omitempty"`
}

// MachineCapabilities is what this machine can actually do, as the RUNNING
// daemon sees it. A client deciding whether a given leaf will ever be fetched
// here needs the leaf's requirements AND these; deriving them again from
// config.yaml would answer for a configuration that may not be the one the
// daemon loaded, and would re-run hardware detection the daemon already did
// once at startup (TB-4).
type MachineCapabilities struct {
	// Runtimes are the runtime kinds registered in the daemon's runtime
	// registry, lowercase (e.g. ["container","native","wasm"]).
	Runtimes []string `json:"runtimes"`
	HasGPU   bool     `json:"has_gpu"`
	// MaxMemoryMB is the per-unit memory ceiling the daemon is enforcing.
	MaxMemoryMB int `json:"max_memory_mb"`
	// MaxDiskMB and MaxCPUCores are the other two budgets the head matches leafs
	// against, in the same units it receives them (max_disk_gb is advertised as
	// MB). Reported here so the client checks a leaf against what this daemon
	// actually advertised, not against a config file that may have moved on
	// since (TB-15).
	MaxDiskMB   int64 `json:"max_disk_mb"`
	MaxCPUCores int   `json:"max_cpu_cores"`
	// The GPU side of the same idea (TB-21). MaxGPUVRAMMB is the ALLOWED VRAM —
	// card capacity * max_gpu_vram_pct / 100, the figure dispatch compares a leaf
	// against — not the size of the card. GPUVendors are uppercase ("NVIDIA").
	// GPUCardVRAMMB and GPUVRAMPct describe the same card MaxGPUVRAMMB came from,
	// so a blocked-leaf message can say "70% of your 6144 MB card" rather than
	// quoting an allowance that reads as a hardware fault.
	MaxGPUVRAMMB           int      `json:"max_gpu_vram_mb"`
	GPUCardVRAMMB          int      `json:"gpu_card_vram_mb"`
	GPUVRAMPct             int      `json:"gpu_vram_pct"`
	GPUVendors             []string `json:"gpu_vendors"`
	GPUComputeCapabilities []string `json:"gpu_compute_capabilities"`
}

// MachineRuntimes returns the runtime kinds this daemon has registered and can
// execute, lowercase and sorted.
func (b *DaemonBridge) MachineRuntimes() []string {
	names := b.daemon.AvailableRuntimeNames()
	sort.Strings(names)
	return names
}

// MachineCaps reports this machine's capabilities as the running daemon sees them.
func (b *DaemonBridge) MachineCaps() MachineCapabilities {
	rl := b.daemon.GetConfig().ResourceLimits
	vramMB, cardVRAMMB, vramPct, vendors, computeCaps := b.daemon.GPUBudget()
	return MachineCapabilities{
		Runtimes:    b.MachineRuntimes(),
		HasGPU:      b.daemon.HasGPU(),
		MaxMemoryMB: rl.MaxMemoryMB,
		// max_disk_gb is advertised to the head in MB (client/hardware.go), so it
		// is converted here rather than at the comparison, where a GB-vs-MB slip
		// would silently pass every leaf.
		MaxDiskMB:              int64(rl.MaxDiskGB) * 1024,
		MaxCPUCores:            rl.MaxCPUCores,
		MaxGPUVRAMMB:           vramMB,
		GPUCardVRAMMB:          cardVRAMMB,
		GPUVRAMPct:             vramPct,
		GPUVendors:             vendors,
		GPUComputeCapabilities: computeCaps,
	}
}

// GetHeads returns head info for all configured servers, with leaf details.
func (b *DaemonBridge) GetHeads() []HeadInfo {
	cfg := b.daemon.GetConfig()
	mc := b.daemon.GetMultiClient()
	lc := b.daemon.GetLeafCache()

	// Per-leaf failure records, keyed by leaf id, so each leaf row can carry its
	// own (TB-10).
	failures := make(map[string]FailingLeafInfo)
	for _, f := range b.failingLeafs() {
		failures[f.LeafID] = f
	}

	serverStatus := make(map[string]bool)
	serverVolunteerID := make(map[string]string)
	if mc != nil {
		for _, s := range mc.Servers() {
			serverStatus[s.Name] = s.Available
			serverVolunteerID[s.Name] = s.VolunteerID
		}
	}

	var heads []HeadInfo
	for _, srv := range cfg.Servers {
		name := srv.DisplayName()

		connStatus := "disconnected"
		if serverStatus[name] {
			connStatus = "connected"
		}

		w := srv.Weight
		if w <= 0 {
			w = 100
		}

		hs := b.daemon.HeadStatus().Get(srv.GRPCAddress)
		hi := HeadInfo{
			GRPCAddress:    srv.GRPCAddress,
			Status:         connStatus,
			Weight:         w,
			VolunteerID:    serverVolunteerID[name],
			HeadVersion:    hs.HeadVersion,
			UpdateRequired: hs.UpdateRequired,
		}

		// Fill from cache if available.
		if cached := lc.GetHeadInfo(name); cached != nil {
			hi.Name = cached.Name
			hi.Description = cached.Description
			hi.URL = cached.URL
			hi.LeafsRefreshedAt = cached.LastRefreshed

			lp := srv.LeafPreferences
			mode := lp.Mode
			if mode == "" {
				mode = "ALL"
			}

			for _, leaf := range cached.Leafs {
				enabled := true
				switch mode {
				case "SPECIFIC":
					enabled = false
					for _, slug := range lp.Enabled {
						if slug == leaf.Slug {
							enabled = true
							break
						}
					}
				case "BLOCKLIST":
					for _, slug := range lp.Disabled {
						if slug == leaf.Slug {
							enabled = false
							break
						}
					}
				}

				ew := 100
				if dw, ok := cached.DefaultWeights[leaf.Slug]; ok {
					ew = dw
				}
				if cw, ok := lp.Weights[leaf.Slug]; ok {
					ew = cw
				}

				ld := LeafDetail{
					ID:               leaf.ID,
					Slug:             leaf.Slug,
					Name:             leaf.Name,
					Description:      leaf.Description,
					ResearchArea:     leaf.ResearchArea,
					TaskPattern:      leaf.TaskPattern,
					State:            leaf.State,
					QueuedWorkUnits:  leaf.QueuedWorkUnits,
					ActiveVolunteers: leaf.ActiveVolunteers,
					ActiveHosts:      leaf.ActiveHosts,
					Enabled:          enabled,
					EffectiveWeight:  ew,
				}
				if leaf.ExecutionSpec != nil {
					ld.ExecutionSpec = &LeafExecutionSpec{
						Binaries:      leaf.ExecutionSpec.Binaries,
						Image:         leaf.ExecutionSpec.Image,
						GPURequired:   leaf.ExecutionSpec.GPURequired,
						GPUType:       leaf.ExecutionSpec.GPUType,
						MaxMemoryMB:   leaf.ExecutionSpec.MaxMemoryMB,
						MaxDiskMB:     leaf.ExecutionSpec.MaxDiskMB,
						NetworkAccess: leaf.ExecutionSpec.NetworkAccess,
					}
				}
				if rr := leaf.ResourceRequirements; rr != nil {
					ld.ResourceRequirements = &LeafResourceRequirements{
						MinDiskMB:            rr.MinDiskMB,
						MinCPUCores:          rr.MinCPUCores,
						MinGPUVRAMMB:         rr.MinGPUVRAMMB,
						GPUType:              rr.GPUType,
						GPUComputeCapability: rr.GPUComputeCapability,
						GPURequired:          rr.GPURequired,
					}
				}
				if f, ok := failures[leaf.ID]; ok {
					ld.Failures = &f
				}
				gs := b.daemon.LeafDiskGateStatus(leaf)
				ld.DiskGate = &LeafDiskGate{Blocked: gs.Blocked, Reason: gs.Reason, RaiseToGB: gs.RaiseToGB}
				hi.Leafs = append(hi.Leafs, ld)
			}
		} else {
			hi.Name = name
		}

		if hi.Leafs == nil {
			hi.Leafs = []LeafDetail{}
		}
		heads = append(heads, hi)
	}

	if heads == nil {
		heads = []HeadInfo{}
	}
	return heads
}

// GetAvailableLeafs returns all leafs from all connected servers.
func (b *DaemonBridge) GetAvailableLeafs() []LeafDetail {
	heads := b.GetHeads()
	var leafs []LeafDetail
	for _, h := range heads {
		leafs = append(leafs, h.Leafs...)
	}
	if leafs == nil {
		leafs = []LeafDetail{}
	}
	return leafs
}

// CreditSummary is the response for GET /api/v1/credit. Credit is reported by the
// head(s) the volunteer is attached to — authoritative and account-wide, so it
// already sums all of the volunteer's machines. It falls back to a local
// history.jsonl proxy only when no head can be reached.
type CreditSummary struct {
	TotalCredit float64      `json:"total_credit"`
	Today       float64      `json:"today"`
	ThisWeek    float64      `json:"this_week"`
	ThisMonth   float64      `json:"this_month"`
	ByLeaf      []LeafCredit `json:"by_leaf"`
	ByHead      []HeadCredit `json:"by_head"`
	// Source is "head" when at least one attached head answered (authoritative),
	// or "local" when the summary was derived from the local history.jsonl proxy
	// (no head reachable, or every head predates the GetMyContribution RPC).
	Source string `json:"source"`
	// DayBoundary names the calendar rule behind Today/ThisWeek/ThisMonth, so a
	// client can label them instead of leaving the reader to guess (TB-57).
	// "utc": the buckets came from the head's daily timeline, which the head
	// records by UTC date, so they cannot be re-cut to this machine's day. "local":
	// the buckets were cut from the local history file by this machine's clock,
	// the same rule the desktop app's history list groups by.
	DayBoundary string `json:"day_boundary"`
}

// Values of CreditSummary.DayBoundary.
const (
	DayBoundaryUTC   = "utc"
	DayBoundaryLocal = "local"
)

// LeafCredit holds credit for a single leaf.
type LeafCredit struct {
	LeafID   string  `json:"leaf_id"`
	LeafName string  `json:"leaf_name"`
	Credit   float64 `json:"credit"`
}

// HeadCredit holds the account's total credit on a single head.
type HeadCredit struct {
	HeadName    string  `json:"head_name"`
	VolunteerID string  `json:"volunteer_id"`
	TotalCredit float64 `json:"total_credit"`
	Available   bool    `json:"available"` // false if the head was unreachable or predates GetMyContribution
}

// GetCredit returns the volunteer ACCOUNT's credit. It asks each attached head for
// the account's own contribution (the authoritative GetMyContribution RPC, already
// aggregated across the account's machines) and sums the results. If no head can be
// reached — or every head predates that RPC — it falls back to the local
// history.jsonl proxy so the number is still useful offline.
func (b *DaemonBridge) GetCredit() CreditSummary {
	if summary, ok := b.creditFromHeads(); ok {
		return summary
	}
	return b.creditFromHistory()
}

// creditFromHeads queries every attached head's GetMyContribution and aggregates
// the results. The bool is false (caller falls back to local history) when there is
// no multi-client or no head answered.
func (b *DaemonBridge) creditFromHeads() (CreditSummary, bool) {
	mc := b.daemon.GetMultiClient()
	if mc == nil {
		return CreditSummary{}, false
	}

	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekStart := todayStart.AddDate(0, 0, -int(now.Weekday()))
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	summary := CreditSummary{Source: "head", DayBoundary: DayBoundaryUTC, ByLeaf: []LeafCredit{}, ByHead: []HeadCredit{}}
	leafByID := make(map[string]*LeafCredit)
	var leafOrder []string
	anyAnswered := false

	for _, s := range mc.Servers() {
		if s == nil || s.Client == nil {
			continue
		}
		hc := HeadCredit{HeadName: s.Name, VolunteerID: s.VolunteerID}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := s.Client.GetMyContribution(ctx, &lettucev1.GetMyContributionRequest{})
		cancel()
		if err != nil {
			// Unreachable, or an old head that returns Unimplemented: record the
			// head as unavailable and keep going — one head must not poison the rest.
			summary.ByHead = append(summary.ByHead, hc)
			continue
		}

		anyAnswered = true
		hc.Available = true
		hc.TotalCredit = resp.GetTotalCredit()
		if hc.VolunteerID == "" {
			hc.VolunteerID = resp.GetVolunteerId()
		}
		summary.ByHead = append(summary.ByHead, hc)
		summary.TotalCredit += resp.GetTotalCredit()

		for _, lc := range resp.GetByLeaf() {
			if existing, ok := leafByID[lc.GetLeafId()]; ok {
				existing.Credit += lc.GetCredit()
				continue
			}
			leafOrder = append(leafOrder, lc.GetLeafId())
			leafByID[lc.GetLeafId()] = &LeafCredit{
				LeafID:   lc.GetLeafId(),
				LeafName: lc.GetLeafName(),
				Credit:   lc.GetCredit(),
			}
		}

		// Derive today/this-week/this-month from the head's daily timeline (the
		// finest granularity it returns). The daily series spans the last 30 days,
		// so a calendar month can be undercounted by at most its first day or two —
		// the per-head totals above are always exact.
		for _, dc := range resp.GetDaily() {
			day, perr := time.Parse("2006-01-02", dc.GetDate())
			if perr != nil {
				continue
			}
			if !day.Before(todayStart) {
				summary.Today += dc.GetCredit()
			}
			if !day.Before(weekStart) {
				summary.ThisWeek += dc.GetCredit()
			}
			if !day.Before(monthStart) {
				summary.ThisMonth += dc.GetCredit()
			}
		}
	}

	if !anyAnswered {
		return CreditSummary{}, false
	}

	for _, id := range leafOrder {
		summary.ByLeaf = append(summary.ByLeaf, *leafByID[id])
	}
	return summary, true
}

// creditFromHistory is the offline fallback: it counts accepted work units in the
// local history.jsonl as a credit proxy (one unit ~= one credit). It is used only
// when no head answered, so the volunteer still sees a number while disconnected.
func (b *DaemonBridge) creditFromHistory() CreditSummary {
	cfg := b.daemon.GetConfig()
	entries := readAllHistory(cfg.DataDir)

	buckets := bucketAcceptedHistory(entries, time.Now())

	leafCredits := make([]LeafCredit, 0, len(buckets.byLeaf))
	for _, lid := range buckets.leafOrder {
		leafCredits = append(leafCredits, LeafCredit{
			LeafID:   lid,
			LeafName: b.historyLeafName(buckets.leafSample[lid]),
			Credit:   buckets.byLeaf[lid],
		})
	}

	return CreditSummary{
		TotalCredit: buckets.total,
		Today:       buckets.today,
		ThisWeek:    buckets.week,
		ThisMonth:   buckets.month,
		ByLeaf:      leafCredits,
		ByHead:      []HeadCredit{},
		Source:      "local",
		DayBoundary: DayBoundaryLocal,
	}
}

// historyBuckets is bucketAcceptedHistory's tally of accepted history entries.
type historyBuckets struct {
	total, today, week, month float64
	byLeaf                    map[string]float64
	leafOrder                 []string                       // leaf ids in first-seen order, for a stable response
	leafSample                map[string]daemon.HistoryEntry // one entry per leaf, for its recorded name
}

// bucketAcceptedHistory tallies the accepted entries into all-time, today,
// this-week and this-month counts, cutting the day boundaries in the location
// of now.
//
// The daemon and the desktop app run on the same machine, so time.Now() here is
// the same clock and zone the app's history list groups by, and "today" means
// the same day on both surfaces (TB-57). Cutting these buckets by UTC day, as the
// head's timeline must, made a volunteer east of Greenwich see units completed
// after local midnight listed under Today and yet not counted in it. The week
// starts on Sunday, matching the head-derived buckets.
func bucketAcceptedHistory(entries []daemon.HistoryEntry, now time.Time) historyBuckets {
	loc := now.Location()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	weekStart := todayStart.AddDate(0, 0, -int(now.Weekday()))
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)

	b := historyBuckets{
		byLeaf:     make(map[string]float64),
		leafSample: make(map[string]daemon.HistoryEntry),
	}
	for _, e := range entries {
		if !e.ResultAccepted {
			continue
		}
		b.total++
		if !e.CompletedAt.Before(todayStart) {
			b.today++
		}
		if !e.CompletedAt.Before(weekStart) {
			b.week++
		}
		if !e.CompletedAt.Before(monthStart) {
			b.month++
		}
		if _, seen := b.byLeaf[e.LeafID]; !seen {
			b.leafOrder = append(b.leafOrder, e.LeafID)
			b.leafSample[e.LeafID] = e
		}
		b.byLeaf[e.LeafID]++
	}
	return b
}

// TaskDetail is the response for GET /api/v1/tasks/{work_unit_id}/details.
type TaskDetail struct {
	ActiveTaskInfo
	MemoryRSSMB                *float64 `json:"memory_rss_mb"`
	VirtualMemoryMB            *float64 `json:"virtual_memory_mb"`
	CPUUsagePct                *float64 `json:"cpu_usage_pct"`
	DiskReadMB                 *float64 `json:"disk_read_mb"`
	DiskWrittenMB              *float64 `json:"disk_written_mb"`
	TimeSinceCheckpointSeconds *int     `json:"time_since_checkpoint_seconds"`
	EstimatedCompletionAt      *string  `json:"estimated_completion_at"`
	ProgressRatePctPerHour     *float64 `json:"progress_rate_pct_per_hour"`
	FractionDone               float64  `json:"fraction_done"`
	ContainerImage             *string  `json:"container_image"`
}

// SuspendTask suspends a single task by work unit ID.
func (b *DaemonBridge) SuspendTask(workUnitID string) error {
	return b.daemon.SuspendTask(workUnitID)
}

// ResumeTask resumes a single suspended task by work unit ID.
func (b *DaemonBridge) ResumeTask(workUnitID string) error {
	return b.daemon.ResumeTask(workUnitID)
}

// AbortTask cancels a single task by work unit ID, killing its process.
func (b *DaemonBridge) AbortTask(workUnitID string) error {
	return b.daemon.AbortTask(workUnitID)
}

// GetTaskDetails returns full details for a single active task including per-process metrics.
func (b *DaemonBridge) GetTaskDetails(workUnitID string) (*TaskDetail, error) {
	pauseReason := b.daemon.PauseReason()
	daemonPaused := b.daemon.IsPaused()

	// Find the matching task in the active tasks list.
	var found *daemon.CurrentTask
	for _, t := range b.daemon.GetCurrentTasks() {
		if t.WorkUnitID == workUnitID {
			t := t // capture loop variable
			found = &t
			break
		}
	}
	if found == nil {
		return nil, daemon.ErrTaskNotFound
	}

	t := found
	info := b.buildActiveTaskInfo(*t, pauseReason, daemonPaused)

	detail := &TaskDetail{
		ActiveTaskInfo: info,
		FractionDone:   float64(info.ProgressPct),
	}

	// Container image
	if t.ContainerImage != "" {
		img := t.ContainerImage
		detail.ContainerImage = &img
	}

	// Per-process metrics
	if t.ProcessID > 0 {
		reader := readProcessMetrics
		if pm, err := reader(t.ProcessID); err == nil && pm != nil {
			detail.MemoryRSSMB = pm.MemoryRSSMB
			detail.VirtualMemoryMB = pm.VirtualMemoryMB
			detail.CPUUsagePct = pm.CPUUsagePct
			detail.DiskReadMB = pm.DiskReadMB
			detail.DiskWrittenMB = pm.DiskWrittenMB
		}
	}

	// Time since checkpoint
	if !t.LastCheckpointAt.IsZero() {
		secs := int(time.Since(t.LastCheckpointAt).Seconds())
		detail.TimeSinceCheckpointSeconds = &secs
	}

	// Estimated completion at
	if info.EstimatedRemainingSec != nil && *info.EstimatedRemainingSec > 0 {
		est := time.Now().Add(time.Duration(*info.EstimatedRemainingSec) * time.Second).UTC().Format(time.RFC3339)
		detail.EstimatedCompletionAt = &est
	}

	// Progress rate (pct per hour)
	if info.ProgressPct > 0 && info.CPUSeconds > 0 {
		rate := float64(info.ProgressPct) / (float64(info.CPUSeconds) / 3600.0)
		detail.ProgressRatePctPerHour = &rate
	}

	return detail, nil
}

// readProcessMetrics is the function used to read per-process metrics.
// Tests override this to avoid calling real OS APIs.
var readProcessMetrics = defaultReadProcessMetrics

func defaultReadProcessMetrics(pid int) (*procmetrics.ProcessMetrics, error) {
	return procmetrics.NewReader().Read(pid)
}

// Helper functions for partial config application.

func applyResourceLimits(rl *config.ResourceLimits, m map[string]any) {
	if v, ok := m["max_cpu_cores"]; ok {
		rl.MaxCPUCores = toInt(v)
	}
	if v, ok := m["max_memory_mb"]; ok {
		rl.MaxMemoryMB = toInt(v)
	}
	if v, ok := m["max_disk_gb"]; ok {
		rl.MaxDiskGB = toInt(v)
	}
	if v, ok := m["max_bandwidth_mbps"]; ok {
		rl.MaxBandwidthMbps = toInt(v)
	}
	if v, ok := m["max_gpu_vram_pct"]; ok {
		rl.MaxGPUVRAMPct = toInt(v)
	}
}

func applyScheduling(s *config.Scheduling, m map[string]any) {
	if v, ok := m["mode"]; ok {
		if str, ok := v.(string); ok {
			s.Mode = strings.ToUpper(str)
		}
	}
	if v, ok := m["idle_threshold_mins"]; ok {
		s.IdleThresholdMins = toInt(v)
	}
	if v, ok := m["cron_expression"]; ok {
		if str, ok := v.(string); ok {
			s.CronExpression = str
		}
	}
	if v, ok := m["schedule_ranges"]; ok {
		if arr, ok := v.([]any); ok {
			var ranges []config.ScheduleRange
			for _, item := range arr {
				if obj, ok := item.(map[string]any); ok {
					r := config.ScheduleRange{
						StartHour: toInt(obj["start_hour"]),
						EndHour:   toInt(obj["end_hour"]),
					}
					if days, ok := obj["days"].([]any); ok {
						for _, d := range days {
							r.Days = append(r.Days, toInt(d))
						}
					}
					ranges = append(ranges, r)
				}
			}
			s.ScheduleRanges = ranges
		}
	}
}

func applyThermal(t *config.ThermalConfig, m map[string]any) {
	if v, ok := m["enabled"]; ok {
		if b, ok := v.(bool); ok {
			t.Enabled = b
		}
	}
	if v, ok := m["cpu_pause_threshold"]; ok {
		t.CPUPauseThresholdC = toInt(v)
	}
	if v, ok := m["cpu_resume_threshold"]; ok {
		t.CPUResumeThresholdC = toInt(v)
	}
	if v, ok := m["gpu_pause_threshold"]; ok {
		t.GPUPauseThresholdC = toInt(v)
	}
	if v, ok := m["gpu_resume_threshold"]; ok {
		t.GPUResumeThresholdC = toInt(v)
	}
	if v, ok := m["poll_interval_seconds"]; ok {
		t.PollIntervalSeconds = toInt(v)
	}
}

func applyLeafFilter(p *config.LeafFilter, m map[string]any) {
	if v, ok := m["mode"]; ok {
		if str, ok := v.(string); ok {
			p.Mode = strings.ToUpper(str)
		}
	}
	if v, ok := m["leaf_ids"]; ok {
		if arr, ok := v.([]any); ok {
			p.LeafIDs = toStringSlice(arr)
		}
	}
	if v, ok := m["blocked_ids"]; ok {
		if arr, ok := v.([]any); ok {
			p.BlockedIDs = toStringSlice(arr)
		}
	}
}

func applyNotifications(n *config.NotificationConfig, m map[string]any) {
	if v, ok := m["credit_milestones"]; ok {
		if b, ok := v.(bool); ok {
			n.CreditMilestones = b
		}
	}
	if v, ok := m["credit_milestone_threshold"]; ok {
		n.CreditMilestoneThreshold = toInt(v)
	}
	if v, ok := m["work_unit_completed"]; ok {
		if b, ok := v.(bool); ok {
			n.WorkUnitCompleted = b
		}
	}
	if v, ok := m["errors"]; ok {
		if b, ok := v.(bool); ok {
			n.Errors = b
		}
	}
	if v, ok := m["updates"]; ok {
		if b, ok := v.(bool); ok {
			n.Updates = b
		}
	}
}

// applyServers merges incoming server updates into the config. Entries are
// matched by server name; weight, leaf_preferences and trusted_runtimes are
// updated when present and left untouched when absent — so a PUT that does
// not mention trust can never revert an on-disk `heads trust <head> none`
// (PB-28). trusted_runtimes, when present, REPLACES the head's trust with
// exactly the validated list (widening or narrowing alike: the client's
// consent step decided). It reports whether any head's trust actually
// changed, which is what makes a restart necessary; an invalid trust token
// fails the whole update before anything is saved.
func applyServers(cfg *config.Config, serverList []any) (trustChanged bool, err error) {
	for _, item := range serverList {
		sm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := sm["name"].(string)
		if name == "" {
			continue
		}
		for i := range cfg.Servers {
			if cfg.Servers[i].Name != name && cfg.Servers[i].DisplayName() != name {
				continue
			}
			if v, ok := sm["weight"]; ok {
				cfg.Servers[i].Weight = toInt(v)
			}
			if v, ok := sm["leaf_preferences"]; ok {
				if lp, ok := v.(map[string]any); ok {
					applyLeafPreferences(&cfg.Servers[i].LeafPreferences, lp)
				}
			}
			if v, ok := sm["trusted_runtimes"]; ok {
				arr, ok := v.([]any)
				if !ok {
					return false, fmt.Errorf("servers[%q].trusted_runtimes must be a list of runtime names", name)
				}
				raw := make([]string, 0, len(arr))
				for _, item := range arr {
					s, ok := item.(string)
					if !ok {
						return false, fmt.Errorf("servers[%q].trusted_runtimes must contain only runtime names, got %v", name, item)
					}
					raw = append(raw, s)
				}
				trusted, perr := parseTrustedRuntimes(raw)
				if perr != nil {
					return false, fmt.Errorf("servers[%q].%w", name, perr)
				}
				if !trustedRuntimesEqual(cfg.Servers[i].TrustedRuntimes, trusted) {
					trustChanged = true
				}
				cfg.Servers[i].TrustedRuntimes = trusted
			}
			break
		}
	}
	return trustChanged, nil
}

func applyLeafPreferences(lp *config.LeafPreferences, m map[string]any) {
	if v, ok := m["mode"].(string); ok {
		lp.Mode = v
	}
	if v, ok := m["enabled"]; ok {
		if arr, ok := v.([]any); ok {
			lp.Enabled = make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					lp.Enabled = append(lp.Enabled, s)
				}
			}
		}
	}
	if v, ok := m["disabled"]; ok {
		if arr, ok := v.([]any); ok {
			lp.Disabled = make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					lp.Disabled = append(lp.Disabled, s)
				}
			}
		}
	}
	if v, ok := m["weights"]; ok {
		if wm, ok := v.(map[string]any); ok {
			lp.Weights = make(map[string]int, len(wm))
			for k, val := range wm {
				lp.Weights[k] = toInt(val)
			}
		}
	}
	// Clear fields not relevant for the current mode
	if lp.Mode == "ALL" {
		lp.Enabled = nil
		lp.Disabled = nil
	}
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

func toStringSlice(arr []any) []string {
	result := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

// ContainerRuntimeStatusResponse is the response for GET /api/v1/container-runtime.
type ContainerRuntimeStatusResponse struct {
	Backend         string  `json:"backend"`
	Status          string  `json:"status"`
	Version         string  `json:"version"`
	SocketPath      string  `json:"socket_path"`
	MachineRequired bool    `json:"machine_required"`
	MachineName     string  `json:"machine_name"`
	MachineCPUs     int     `json:"machine_cpus"`
	MachineMemoryMB int     `json:"machine_memory_mb"`
	MachineDiskGB   int     `json:"machine_disk_gb"`
	Error           *string `json:"error"`
}

// GetContainerRuntimeStatus returns the current container runtime state.
func (b *DaemonBridge) GetContainerRuntimeStatus() ContainerRuntimeStatusResponse {
	cfg := b.daemon.GetConfig()
	mm := b.daemon.GetMachineManager()

	resp := ContainerRuntimeStatusResponse{
		Backend: cfg.ContainerBackend,
	}

	if cfg.ContainerBackend == "" {
		resp.Backend = "none"
		resp.Status = "not_installed"
		return resp
	}

	if mm == nil {
		// No machine manager — check the backend string only.
		resp.Status = "running" // assume running if configured but no manager
		return resp
	}

	resp.MachineRequired = mm.NeedsMachine()
	info := mm.Status()
	resp.Status = string(info.Status)
	resp.SocketPath = info.SocketPath
	resp.MachineName = info.Name
	resp.MachineCPUs = info.CPUs
	resp.MachineMemoryMB = info.MemoryMB
	resp.MachineDiskGB = info.DiskGB

	if info.Error != "" {
		resp.Error = &info.Error
	}

	return resp
}

// SetupContainerRuntime initializes and starts the container runtime.
func (b *DaemonBridge) SetupContainerRuntime(cpus, memoryMB, diskGB int) error {
	mm := b.daemon.GetMachineManager()
	if mm == nil {
		return fmt.Errorf("no container runtime configured")
	}

	// Use config defaults if not specified, with hard minimums.
	cfg := b.daemon.GetConfig()
	if cpus <= 0 {
		cpus = cfg.ResourceLimits.MaxCPUCores
	}
	if memoryMB <= 0 {
		memoryMB = cfg.ResourceLimits.MaxMemoryMB
	}
	if diskGB <= 0 {
		diskGB = cfg.ResourceLimits.MaxDiskGB
	}
	// Hard minimums (same as cli/start.go).
	if cpus <= 0 {
		cpus = 2
	}
	if memoryMB <= 0 {
		memoryMB = 4096
	}
	if diskGB <= 0 {
		diskGB = 20
	}
	// Reasonable upper bounds.
	if cpus > 128 {
		cpus = 128
	}
	if memoryMB > 1048576 { // 1 TB
		memoryMB = 1048576
	}
	if diskGB > 10000 { // 10 TB
		diskGB = 10000
	}

	return mm.Setup(cpus, memoryMB, diskGB)
}

// StartContainerRuntime starts the Podman machine (if applicable).
func (b *DaemonBridge) StartContainerRuntime() error {
	mm := b.daemon.GetMachineManager()
	if mm == nil {
		return fmt.Errorf("no container runtime configured")
	}
	status := mm.Status()
	if status.Status == runtime.MachineRunning {
		return runtime.ErrAlreadyRunning
	}
	if status.Status == runtime.MachineNotInitialized {
		return runtime.ErrNotInitialized
	}
	return mm.Start()
}

// StopContainerRuntime stops the Podman machine (if applicable).
func (b *DaemonBridge) StopContainerRuntime() error {
	mm := b.daemon.GetMachineManager()
	if mm == nil {
		return fmt.Errorf("no container runtime configured")
	}
	status := mm.Status()
	if status.Status != runtime.MachineRunning {
		return runtime.ErrNotRunning
	}
	return mm.Stop()
}

// RegenerateKeypair generates a new Ed25519 keypair, saves it, and returns the new public key.
// Rejects the operation while tasks are active to prevent identity mismatch.
func (b *DaemonBridge) RegenerateKeypair() (string, error) {
	if tasks := b.daemon.GetCurrentTasks(); len(tasks) > 0 {
		return "", fmt.Errorf("cannot regenerate keypair while %d task(s) are active", len(tasks))
	}
	cfg := b.daemon.GetConfig()
	pub, priv, err := identity.Generate()
	if err != nil {
		return "", fmt.Errorf("generating keypair: %w", err)
	}
	if err := identity.SaveKeyPair(cfg.KeyFilePath(), cfg.PubKeyFilePath(), priv, pub); err != nil {
		return "", fmt.Errorf("saving keypair: %w", err)
	}
	return identity.PublicKeyToBase64URL(pub), nil
}

// SignChallengeResponse is the response for POST /api/v1/identity/sign.
type SignChallengeResponse struct {
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

// SignChallenge signs a hex-encoded challenge with the volunteer's Ed25519 private key.
func (b *DaemonBridge) SignChallenge(challengeHex string) (*SignChallengeResponse, error) {
	challengeBytes, err := hex.DecodeString(challengeHex)
	if err != nil {
		return nil, fmt.Errorf("invalid challenge hex: %w", err)
	}

	cfg := b.daemon.GetConfig()
	pub, priv, err := identity.LoadKeyPair(cfg.KeyFilePath(), cfg.PubKeyFilePath())
	if err != nil {
		return nil, fmt.Errorf("loading keypair: %w", err)
	}

	sig := ed25519.Sign(priv, challengeBytes)

	return &SignChallengeResponse{
		PublicKey: identity.PublicKeyToBase64URL(pub),
		Signature: base64.RawURLEncoding.EncodeToString(sig),
	}, nil
}

// ResultEntryResponse is the API response model for a persisted result.
type ResultEntryResponse struct {
	WorkUnitID    string `json:"work_unit_id"`
	LeafName      string `json:"leaf_name"`
	LeafSlug      string `json:"leaf_slug"`
	HeadName      string `json:"head_name"`
	CompletedAt   string `json:"completed_at"`
	VizBundlePath string `json:"viz_bundle_path"`
	SizeBytes     int64  `json:"size_bytes"`
}

// ListResults returns all locally persisted result entries.
func (b *DaemonBridge) ListResults() ([]ResultEntryResponse, error) {
	cfg := b.daemon.GetConfig()
	entries, err := daemon.ListResults(cfg.DataDir)
	if err != nil {
		return nil, err
	}

	results := make([]ResultEntryResponse, 0, len(entries))
	for _, e := range entries {
		results = append(results, ResultEntryResponse{
			WorkUnitID:    e.WorkUnitID,
			LeafName:      e.LeafName,
			LeafSlug:      e.LeafSlug,
			HeadName:      e.HeadName,
			CompletedAt:   e.CompletedAt.UTC().Format(time.RFC3339),
			VizBundlePath: e.VizBundlePath,
			SizeBytes:     e.SizeBytes,
		})
	}
	return results, nil
}

// GetResultData returns the raw result JSON for a work unit.
func (b *DaemonBridge) GetResultData(workUnitID string) ([]byte, error) {
	cfg := b.daemon.GetConfig()
	return daemon.GetResultData(cfg.DataDir, workUnitID)
}
