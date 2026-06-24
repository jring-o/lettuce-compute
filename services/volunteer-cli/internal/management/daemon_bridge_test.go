package management

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

func TestComputeTaskStatus_Running(t *testing.T) {
	task := daemon.CurrentTask{Suspended: false}
	status, reason := computeTaskStatus(task, "")
	if status != "running" {
		t.Errorf("status = %q, want %q", status, "running")
	}
	if reason != nil {
		t.Errorf("reason = %v, want nil", reason)
	}
}

func TestComputeTaskStatus_SuspendedUser(t *testing.T) {
	task := daemon.CurrentTask{Suspended: true}
	status, reason := computeTaskStatus(task, "user")
	if status != "suspended_user" {
		t.Errorf("status = %q, want %q", status, "suspended_user")
	}
	if reason == nil || *reason != "User paused" {
		t.Errorf("reason = %v, want %q", reason, "User paused")
	}
}

func TestComputeTaskStatus_SuspendedThermal(t *testing.T) {
	task := daemon.CurrentTask{Suspended: true}
	status, reason := computeTaskStatus(task, "thermal")
	if status != "suspended_thermal" {
		t.Errorf("status = %q, want %q", status, "suspended_thermal")
	}
	if reason == nil || *reason != "CPU temperature exceeded threshold" {
		t.Errorf("reason = %v, want %q", reason, "CPU temperature exceeded threshold")
	}
}

func TestComputeTaskStatus_SuspendedScheduled(t *testing.T) {
	task := daemon.CurrentTask{Suspended: true}
	status, reason := computeTaskStatus(task, "scheduled")
	if status != "suspended_scheduled" {
		t.Errorf("status = %q, want %q", status, "suspended_scheduled")
	}
	if reason == nil || *reason != "Outside scheduled computing hours" {
		t.Errorf("reason = %v, want %q", reason, "Outside scheduled computing hours")
	}
}

func TestComputeTaskStatus_PerSlotSuspendedNoDaemonPause(t *testing.T) {
	// Slot suspended but daemon not paused -> suspended_user (per-slot user action).
	task := daemon.CurrentTask{Suspended: true}
	status, reason := computeTaskStatus(task, "")
	if status != "suspended_user" {
		t.Errorf("status = %q, want %q", status, "suspended_user")
	}
	if reason == nil {
		t.Error("reason should not be nil")
	}
}

func TestHistoryEntryInfo_CPUSecondsAndHeadName(t *testing.T) {
	dir := t.TempDir()

	// Write a history entry with cpu_seconds.
	entry := daemon.HistoryEntry{
		WorkUnitID:       "wu-hist-1",
		LeafID:           "leaf-1",
		ServerName:       "test-head",
		CompletedAt:      time.Now().UTC(),
		WallClockSeconds: 600,
		CPUSeconds:       540,
		ResultAccepted:   true,
	}
	data, _ := json.Marshal(entry)
	histPath := filepath.Join(dir, "history.jsonl")
	os.WriteFile(histPath, append(data, '\n'), 0644)

	// Read it back.
	entries := readAllHistory(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].CPUSeconds != 540 {
		t.Errorf("CPUSeconds = %d, want 540", entries[0].CPUSeconds)
	}
	if entries[0].ServerName != "test-head" {
		t.Errorf("ServerName = %q, want %q", entries[0].ServerName, "test-head")
	}
}

func TestHistoryEntry_BackwardCompat_MissingCPUSeconds(t *testing.T) {
	dir := t.TempDir()

	// Write a pre-S100 history entry WITHOUT cpu_seconds or server_name fields.
	// This simulates entries from before these fields were added.
	legacyJSON := `{"work_unit_id":"wu-old","leaf_id":"leaf-1","completed_at":"2026-03-28T12:00:00Z","wall_clock_seconds":300,"result_accepted":true}`
	histPath := filepath.Join(dir, "history.jsonl")
	os.WriteFile(histPath, []byte(legacyJSON+"\n"), 0644)

	entries := readAllHistory(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	// CPUSeconds should default to 0 for old entries.
	if entries[0].CPUSeconds != 0 {
		t.Errorf("CPUSeconds = %d, want 0 for legacy entry", entries[0].CPUSeconds)
	}
	// ServerName should default to "" for old entries.
	if entries[0].ServerName != "" {
		t.Errorf("ServerName = %q, want empty for legacy entry", entries[0].ServerName)
	}
}

func TestComputeTaskStatus_NotSuspendedIgnoresPauseReason(t *testing.T) {
	// Even if pauseReason is set (e.g., "thermal"), a non-suspended task should be "running".
	task := daemon.CurrentTask{Suspended: false}
	status, reason := computeTaskStatus(task, "thermal")
	if status != "running" {
		t.Errorf("status = %q, want %q", status, "running")
	}
	if reason != nil {
		t.Errorf("reason = %v, want nil", reason)
	}
}

func TestSuspendTask_NoSlotManager(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})
	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))

	// Daemon not running -> slotManager is nil -> ErrTaskNotFound.
	err := bridge.SuspendTask("wu-1")
	if err != daemon.ErrTaskNotFound {
		t.Errorf("SuspendTask = %v, want ErrTaskNotFound", err)
	}
}

func TestResumeTask_NoSlotManager(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})
	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))

	err := bridge.ResumeTask("wu-1")
	if err != daemon.ErrTaskNotFound {
		t.Errorf("ResumeTask = %v, want ErrTaskNotFound", err)
	}
}

func TestAbortTask_NoSlotManager(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})
	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))

	err := bridge.AbortTask("wu-1")
	if err != daemon.ErrTaskNotFound {
		t.Errorf("AbortTask = %v, want ErrTaskNotFound", err)
	}
}

func TestGetTaskDetails_NotFound(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})
	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))

	_, err := bridge.GetTaskDetails("wu-1")
	if err != daemon.ErrTaskNotFound {
		t.Errorf("GetTaskDetails = %v, want ErrTaskNotFound", err)
	}
}

func TestBuildActiveTaskInfo_Running(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})
	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))

	task := daemon.CurrentTask{
		WorkUnitID:      "wu-test",
		LeafID:          "leaf-1",
		StartedAt:       time.Now().Add(-60 * time.Second),
		ServerName:      "test-head",
		RuntimeType:     "native",
		ProcessID:       1234,
		DeadlineSeconds: 3600,
	}

	info := bridge.buildActiveTaskInfo(task, "")
	if info.WorkUnitID != "wu-test" {
		t.Errorf("WorkUnitID = %q, want %q", info.WorkUnitID, "wu-test")
	}
	if info.TaskStatus != "running" {
		t.Errorf("TaskStatus = %q, want %q", info.TaskStatus, "running")
	}
	if info.StatusReason != nil {
		t.Errorf("StatusReason = %v, want nil", info.StatusReason)
	}
	if info.HeadName != "test-head" {
		t.Errorf("HeadName = %q, want %q", info.HeadName, "test-head")
	}
	if info.RuntimeType != "native" {
		t.Errorf("RuntimeType = %q, want %q", info.RuntimeType, "native")
	}
	if info.ProcessID == nil || *info.ProcessID != 1234 {
		t.Errorf("ProcessID = %v, want 1234", info.ProcessID)
	}
	if info.ElapsedSeconds < 59 {
		t.Errorf("ElapsedSeconds = %d, want >= 59", info.ElapsedSeconds)
	}
	// CPUSeconds = elapsed - 0 paused = elapsed.
	if info.CPUSeconds < 59 {
		t.Errorf("CPUSeconds = %d, want >= 59", info.CPUSeconds)
	}
	// DeadlineSeconds = 3600 - elapsed.
	if info.DeadlineSeconds > 3541 || info.DeadlineSeconds < 3530 {
		t.Errorf("DeadlineSeconds = %d, want ~3540", info.DeadlineSeconds)
	}
}

func TestBuildActiveTaskInfo_CPUSecondsClamped(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})
	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))

	// TotalPausedSeconds > elapsed should clamp CPUSeconds to 0.
	task := daemon.CurrentTask{
		WorkUnitID:         "wu-clamped",
		LeafID:             "leaf-1",
		StartedAt:          time.Now().Add(-10 * time.Second),
		TotalPausedSeconds: 999,
	}

	info := bridge.buildActiveTaskInfo(task, "")
	if info.CPUSeconds != 0 {
		t.Errorf("CPUSeconds = %d, want 0 (clamped)", info.CPUSeconds)
	}
}

func TestBuildActiveTaskInfo_SuspendedThermal(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})
	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))

	task := daemon.CurrentTask{
		WorkUnitID: "wu-thermal",
		LeafID:     "leaf-1",
		Suspended:  true,
		StartedAt:  time.Now().Add(-30 * time.Second),
	}

	info := bridge.buildActiveTaskInfo(task, "thermal")
	if info.TaskStatus != "suspended_thermal" {
		t.Errorf("TaskStatus = %q, want %q", info.TaskStatus, "suspended_thermal")
	}
	if info.StatusReason == nil || *info.StatusReason != "CPU temperature exceeded threshold" {
		t.Errorf("StatusReason = %v, want thermal reason", info.StatusReason)
	}
}

func TestBuildActiveTaskInfo_BenchmarkEstimate(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})
	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))

	// No progress, but benchmark-based estimate of 100s, started 30s ago.
	task := daemon.CurrentTask{
		WorkUnitID:       "wu-bench-est",
		LeafID:           "leaf-1",
		StartedAt:        time.Now().Add(-30 * time.Second),
		EstimatedSeconds: 100,
	}

	info := bridge.buildActiveTaskInfo(task, "")
	if info.EstimatedRemainingSec == nil {
		t.Fatal("EstimatedRemainingSec should not be nil for benchmark estimate")
	}
	// Remaining = 100 - ~30 = ~70.
	if *info.EstimatedRemainingSec < 65 || *info.EstimatedRemainingSec > 75 {
		t.Errorf("EstimatedRemainingSec = %d, want ~70", *info.EstimatedRemainingSec)
	}
}

func TestBuildActiveTaskInfo_VizBundleAndCheckpoint(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})
	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))

	ckTime := time.Now().Add(-5 * time.Minute)
	task := daemon.CurrentTask{
		WorkUnitID:            "wu-viz",
		LeafID:                "leaf-1",
		VizBundlePath:         "/tmp/viz-bundle.tar.gz",
		CheckpointSequence:    3,
		LastCheckpointAt:      ckTime,
		ResumedFromCheckpoint: true,
		StartedAt:             time.Now().Add(-10 * time.Minute),
	}

	info := bridge.buildActiveTaskInfo(task, "")
	if info.VizBundlePath == nil || *info.VizBundlePath != "/tmp/viz-bundle.tar.gz" {
		t.Errorf("VizBundlePath = %v, want /tmp/viz-bundle.tar.gz", info.VizBundlePath)
	}
	if info.CheckpointSequence != 3 {
		t.Errorf("CheckpointSequence = %d, want 3", info.CheckpointSequence)
	}
	if info.LastCheckpointAt == nil {
		t.Fatal("LastCheckpointAt should not be nil")
	}
	if !info.ResumedFromCheckpoint {
		t.Error("ResumedFromCheckpoint should be true")
	}
}

func TestGetCreditFromHead(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Servers = []config.ServerConfig{{GRPCAddress: "localhost:50051", Name: "head-alpha"}}

	d := daemon.NewDaemon(daemon.DaemonConfig{Config: cfg, Logger: logger})

	today := time.Now().UTC().Format("2006-01-02")
	mockClient := &e2eMockWorkClient{
		getMyContributionFn: func(ctx context.Context, req *lettucev1.GetMyContributionRequest) (*lettucev1.GetMyContributionResponse, error) {
			return &lettucev1.GetMyContributionResponse{
				VolunteerId: "vol-aaa-111",
				TotalCredit: 5.5,
				ByLeaf: []*lettucev1.LeafContribution{
					{LeafId: "leaf-a", LeafName: "Leaf A", Credit: 2.0},
					{LeafId: "leaf-b", LeafName: "Leaf B", Credit: 3.5},
				},
				Daily: []*lettucev1.DailyContribution{{Date: today, Credit: 5.5}},
			}, nil
		},
	}

	mc := daemon.NewMultiServerClient([]*daemon.ServerConnection{
		{Name: "head-alpha", VolunteerID: "vol-aaa-111", Available: true, Client: mockClient},
	}, logger)
	d.SetMultiClientForTest(mc)

	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))
	summary := bridge.GetCredit()

	if summary.Source != "head" {
		t.Errorf("source = %q, want head", summary.Source)
	}
	if summary.TotalCredit != 5.5 {
		t.Errorf("total_credit = %v, want 5.5", summary.TotalCredit)
	}
	if summary.Today != 5.5 {
		t.Errorf("today = %v, want 5.5", summary.Today)
	}
	if len(summary.ByLeaf) != 2 {
		t.Fatalf("by_leaf len = %d, want 2", len(summary.ByLeaf))
	}
	if len(summary.ByHead) != 1 || !summary.ByHead[0].Available || summary.ByHead[0].TotalCredit != 5.5 {
		t.Errorf("by_head = %+v, want one available head with 5.5", summary.ByHead)
	}
}

// TestGetCreditFallsBackToHistory proves that when no head answers (e.g. an old
// head returning Unimplemented, or an unreachable head), GetCredit falls back to
// the local history.jsonl proxy instead of erroring.
func TestGetCreditFallsBackToHistory(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Servers = []config.ServerConfig{{GRPCAddress: "localhost:50051", Name: "head-alpha"}}

	d := daemon.NewDaemon(daemon.DaemonConfig{Config: cfg, Logger: logger})

	mockClient := &e2eMockWorkClient{
		getMyContributionFn: func(ctx context.Context, req *lettucev1.GetMyContributionRequest) (*lettucev1.GetMyContributionResponse, error) {
			return nil, fmt.Errorf("rpc error: code = Unimplemented")
		},
	}
	mc := daemon.NewMultiServerClient([]*daemon.ServerConnection{
		{Name: "head-alpha", Available: true, Client: mockClient},
	}, logger)
	d.SetMultiClientForTest(mc)

	daemon.AppendHistory(dir, daemon.HistoryEntry{
		WorkUnitID: "wu-1", LeafID: "proj-a", CompletedAt: time.Now().UTC(), ResultAccepted: true,
	})
	daemon.AppendHistory(dir, daemon.HistoryEntry{
		WorkUnitID: "wu-2", LeafID: "proj-a", CompletedAt: time.Now().UTC(), ResultAccepted: true,
	})

	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))
	summary := bridge.GetCredit()

	if summary.Source != "local" {
		t.Errorf("source = %q, want local", summary.Source)
	}
	if summary.TotalCredit != 2 {
		t.Errorf("total_credit = %v, want 2 (from history fallback)", summary.TotalCredit)
	}
}

func TestGetHeads_VolunteerIDPopulation(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Servers = []config.ServerConfig{
		{GRPCAddress: "localhost:50051", Name: "head-alpha"},
		{GRPCAddress: "localhost:50052", Name: "head-beta"},
	}

	d := daemon.NewDaemon(daemon.DaemonConfig{
		Config: cfg,
		Logger: logger,
	})

	// Inject a MultiServerClient with VolunteerIDs set on each connection.
	mc := daemon.NewMultiServerClient([]*daemon.ServerConnection{
		{Name: "head-alpha", VolunteerID: "vol-aaa-111", Available: true},
		{Name: "head-beta", VolunteerID: "vol-bbb-222", Available: true},
	}, logger)
	d.SetMultiClientForTest(mc)

	bridge := NewDaemonBridge(d, filepath.Join(dir, "config.yaml"))
	heads := bridge.GetHeads()

	if len(heads) != 2 {
		t.Fatalf("expected 2 heads, got %d", len(heads))
	}

	// Build a map by grpc_address for reliable lookup.
	byAddr := make(map[string]HeadInfo)
	for _, h := range heads {
		byAddr[h.GRPCAddress] = h
	}

	alphaHead, ok := byAddr["localhost:50051"]
	if !ok {
		t.Fatal("missing head for localhost:50051")
	}
	if alphaHead.VolunteerID != "vol-aaa-111" {
		t.Errorf("head-alpha VolunteerID = %q, want %q", alphaHead.VolunteerID, "vol-aaa-111")
	}

	betaHead, ok := byAddr["localhost:50052"]
	if !ok {
		t.Fatal("missing head for localhost:50052")
	}
	if betaHead.VolunteerID != "vol-bbb-222" {
		t.Errorf("head-beta VolunteerID = %q, want %q", betaHead.VolunteerID, "vol-bbb-222")
	}
}
