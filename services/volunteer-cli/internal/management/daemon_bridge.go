package management

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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

// StatusResponse is the response for GET /api/v1/status.
type StatusResponse struct {
	State            string           `json:"state"`
	UptimeSeconds    int              `json:"uptime_seconds"`
	ConnectedServers int              `json:"connected_servers"`
	ActiveTasks      []ActiveTaskInfo `json:"active_tasks"`
	QueuedTasks      []QueuedTaskInfo `json:"queued_tasks"`
	PausedReason     *string          `json:"paused_reason"`
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

// computeTaskStatus determines the status string and reason for an active task.
func computeTaskStatus(task daemon.CurrentTask, pauseReason string) (status string, reason *string) {
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
		default:
			status = "suspended_user"
			r := "User paused"
			return status, &r
		}
	}
	return "running", nil
}

// buildActiveTaskInfo converts a daemon.CurrentTask into the API's ActiveTaskInfo,
// computing derived fields (progress, elapsed, CPU seconds, ETA).
func (b *DaemonBridge) buildActiveTaskInfo(t daemon.CurrentTask, pauseReason string) ActiveTaskInfo {
	taskStatus, statusReason := computeTaskStatus(t, pauseReason)

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
	state := "stopped"
	if b.daemon.IsRunning() {
		if b.daemon.IsPaused() {
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
		activeTasks = append(activeTasks, b.buildActiveTaskInfo(t, pauseReason))
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
	}
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
	CPUUsagePct  float64 `json:"cpu_usage_pct"`
	GPUUsagePct  float64 `json:"gpu_usage_pct"`
	MemoryUsedMB int     `json:"memory_used_mb"`
	MemoryTotalMB int    `json:"memory_total_mb"`
	DiskUsedGB   float64 `json:"disk_used_gb"`
	DiskTotalGB  float64 `json:"disk_total_gb"`
	CPUTempC     int     `json:"cpu_temp_c"`
	GPUTempC     int     `json:"gpu_temp_c"`
}

// GetMetrics returns current resource usage metrics.
// Real system metrics require platform-specific code; this returns zeros for now.
// The desktop app will use these endpoints — real values will be populated when
// platform metric collection is integrated.
func (b *DaemonBridge) GetMetrics() MetricsResponse {
	return MetricsResponse{}
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

	b.cfgMu.Lock()
	defer b.cfgMu.Unlock()

	// Rebase onto the on-disk config (see loadWriteBase); the daemon's live
	// config is unchanged unless Save succeeds.
	newCfg, err := b.loadWriteBase()
	if err != nil {
		return err
	}

	// Check for duplicates.
	for _, s := range newCfg.Servers {
		if s.GRPCAddress == req.ServerAddress {
			return fmt.Errorf("already attached to %s", req.ServerAddress)
		}
	}

	name := req.Name
	if name == "" {
		name = req.ServerAddress
	}

	sc := config.ServerConfig{
		GRPCAddress: req.ServerAddress,
		Name:        name,
		// Explicit empty, not nil: attaching through the bridge has no consent
		// step, so the new head starts WASM-only and the trust migration must
		// never re-seed it from available_runtimes (PB-28).
		TrustedRuntimes: []string{},
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
	Pagination PaginationInfo    `json:"pagination"`
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
			LeafName:         b.resolveLeafName(e.LeafID),
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

// readAllHistory reads all history entries (newest first).
func readAllHistory(dataDir string) []daemon.HistoryEntry {
	path := daemon.HistoryFilePath(dataDir)
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []daemon.HistoryEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e daemon.HistoryEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}

	// Reverse for newest first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}

// ConfigResponse is the response for GET /api/v1/config.
type ConfigResponse struct {
	DataDir           string                    `json:"data_dir"`
	PublicKey         string                    `json:"public_key,omitempty"`
	ResourceLimits    config.ResourceLimits     `json:"resource_limits"`
	Scheduling        config.Scheduling         `json:"scheduling"`
	Leafs             config.LeafFilter `json:"leafs"`
	Thermal           config.ThermalConfig      `json:"thermal"`
	Notifications     config.NotificationConfig `json:"notifications"`
	Servers           []config.ServerConfig     `json:"servers"`
	LogLevel          string                    `json:"log_level"`
	MaxConcurrent     int                       `json:"max_concurrent_tasks"`
	AvailableRuntimes []string                  `json:"available_runtimes"`
}

// GetConfig returns the current configuration (with sensitive paths redacted).
func (b *DaemonBridge) GetConfig() ConfigResponse {
	cfg := b.daemon.GetConfig()

	var pubKeyStr string
	if pubBytes, err := os.ReadFile(cfg.PubKeyFilePath()); err == nil {
		pubKeyStr = identity.PublicKeyToBase64URL(pubBytes)
	}

	return ConfigResponse{
		DataDir:           cfg.DataDir,
		PublicKey:          pubKeyStr,
		ResourceLimits:    cfg.ResourceLimits,
		Scheduling:        cfg.Scheduling,
		Leafs:             cfg.Leafs,
		Thermal:           cfg.Thermal,
		Notifications:     cfg.Notifications,
		Servers:           cfg.Servers,
		LogLevel:          cfg.LogLevel,
		MaxConcurrent:     cfg.MaxConcurrentTasks,
		AvailableRuntimes: cfg.AvailableRuntimes,
	}
}

// UpdateConfig applies a partial config update, validates, persists, and applies.
func (b *DaemonBridge) UpdateConfig(partial map[string]any) (*ConfigResponse, error) {
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
	if v, ok := partial["max_concurrent_tasks"]; ok {
		newCfg.MaxConcurrentTasks = toInt(v)
	}
	if v, ok := partial["leafs"]; ok {
		if p, ok := v.(map[string]any); ok {
			applyLeafFilter(&newCfg.Leafs, p)
		}
	}
	if v, ok := partial["work_buffer_hours"]; ok {
		newCfg.WorkBufferHours = toFloat(v)
	}
	if v, ok := partial["servers"]; ok {
		if serverList, ok := v.([]any); ok {
			applyServers(newCfg, serverList)
		}
	}

	if err := newCfg.Validate(); err != nil {
		return nil, err
	}

	if err := newCfg.Save(b.cfgPath); err != nil {
		return nil, fmt.Errorf("saving config: %w", err)
	}

	b.daemon.ApplyConfig(newCfg)

	resp := b.GetConfig()
	return &resp, nil
}

// HeadInfo describes a connected head (server) with its leaf info.
type HeadInfo struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	URL         string       `json:"url,omitempty"`
	GRPCAddress string       `json:"grpc_address"`
	Status      string       `json:"status"` // "connected", "disconnected"
	Weight      int          `json:"weight"`
	VolunteerID string       `json:"volunteer_id,omitempty"`
	Leafs       []LeafDetail `json:"leafs"`
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
}

// GetHeads returns head info for all configured servers, with leaf details.
func (b *DaemonBridge) GetHeads() []HeadInfo {
	cfg := b.daemon.GetConfig()
	mc := b.daemon.GetMultiClient()
	lc := b.daemon.GetLeafCache()

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

		hi := HeadInfo{
			GRPCAddress:  srv.GRPCAddress,
			Status:       connStatus,
			Weight:       w,
			VolunteerID:  serverVolunteerID[name],
		}

		// Fill from cache if available.
		if cached := lc.GetHeadInfo(name); cached != nil {
			hi.Name = cached.Name
			hi.Description = cached.Description
			hi.URL = cached.URL

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
}

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

	summary := CreditSummary{Source: "head", ByLeaf: []LeafCredit{}, ByHead: []HeadCredit{}}
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

	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekStart := todayStart.AddDate(0, 0, -int(now.Weekday()))
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var total, today, week, month float64
	byLeaf := make(map[string]float64)

	for _, e := range entries {
		if !e.ResultAccepted {
			continue
		}
		total++
		if e.CompletedAt.After(todayStart) {
			today++
		}
		if e.CompletedAt.After(weekStart) {
			week++
		}
		if e.CompletedAt.After(monthStart) {
			month++
		}
		byLeaf[e.LeafID]++
	}

	leafCredits := make([]LeafCredit, 0, len(byLeaf))
	for pid, credit := range byLeaf {
		leafCredits = append(leafCredits, LeafCredit{
			LeafID:   pid,
			LeafName: b.resolveLeafName(pid),
			Credit:   credit,
		})
	}

	return CreditSummary{
		TotalCredit: total,
		Today:       today,
		ThisWeek:    week,
		ThisMonth:   month,
		ByLeaf:      leafCredits,
		ByHead:      []HeadCredit{},
		Source:      "local",
	}
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
	info := b.buildActiveTaskInfo(*t, pauseReason)

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

// applyServers merges incoming server updates into the config.
// Matches by server name; only updates weight and leaf_preferences.
func applyServers(cfg *config.Config, serverList []any) {
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
			break
		}
	}
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
	Backend        string  `json:"backend"`
	Status         string  `json:"status"`
	Version        string  `json:"version"`
	SocketPath     string  `json:"socket_path"`
	MachineRequired bool   `json:"machine_required"`
	MachineName    string  `json:"machine_name"`
	MachineCPUs    int     `json:"machine_cpus"`
	MachineMemoryMB int   `json:"machine_memory_mb"`
	MachineDiskGB  int     `json:"machine_disk_gb"`
	Error          *string `json:"error"`
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
