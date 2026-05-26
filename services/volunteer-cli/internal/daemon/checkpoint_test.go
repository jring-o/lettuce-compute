package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

func newTestCheckpointManager(t *testing.T, client WorkClient, workDir string) *CheckpointManager {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	return &CheckpointManager{
		client:   client,
		logger:   logger,
		workDir:  workDir,
		wu:       &runtime.WorkUnit{ID: "dc5ff9da-f084-4dd7-86b8-e829669814f8", LeafID: "proj-1"},
		volID:    "vol-1",
		pubKey:   pub,
		interval: 50 * time.Millisecond,
		sequence: 0,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

func TestSaveOnce_Success(t *testing.T) {
	workDir := t.TempDir()
	checkpointDir := filepath.Join(workDir, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "state.bin"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var savedReq *lettucev1.SaveCheckpointRequest
	mc := &mockClient{
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			mu.Lock()
			savedReq = req
			mu.Unlock()
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	cm := newTestCheckpointManager(t, mc, workDir)
	cm.saveOnce(context.Background())

	if !cm.LastSaveSuccess() {
		t.Error("expected LastSaveSuccess to be true")
	}
	if cm.Sequence() != 1 {
		t.Errorf("expected sequence 1, got %d", cm.Sequence())
	}

	mu.Lock()
	req := savedReq
	mu.Unlock()
	if req == nil {
		t.Fatal("SaveCheckpoint was not called")
	}
	if req.WorkUnitId != "dc5ff9da-f084-4dd7-86b8-e829669814f8" {
		t.Errorf("expected work_unit_id 'wu-1', got %q", req.WorkUnitId)
	}
	if req.VolunteerId != "vol-1" {
		t.Errorf("expected volunteer_id 'vol-1', got %q", req.VolunteerId)
	}
	if req.CheckpointSequence != 1 {
		t.Errorf("expected sequence 1, got %d", req.CheckpointSequence)
	}
	if len(req.CheckpointData) == 0 {
		t.Error("expected non-empty checkpoint data")
	}
}

func TestSaveOnce_EmptyCheckpointDir(t *testing.T) {
	workDir := t.TempDir()
	// No checkpoint directory â€” nothing to save.

	saveCalled := false
	mc := &mockClient{
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			saveCalled = true
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	cm := newTestCheckpointManager(t, mc, workDir)
	cm.saveOnce(context.Background())

	if saveCalled {
		t.Error("SaveCheckpoint should not be called when checkpoint dir doesn't exist")
	}
	if cm.Sequence() != 0 {
		t.Errorf("sequence should remain 0, got %d", cm.Sequence())
	}
}

func TestSaveOnce_EmptyCheckpointDirExists(t *testing.T) {
	workDir := t.TempDir()
	// Create empty checkpoint directory.
	if err := os.MkdirAll(filepath.Join(workDir, "checkpoint"), 0755); err != nil {
		t.Fatal(err)
	}

	saveCalled := false
	mc := &mockClient{
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			saveCalled = true
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	cm := newTestCheckpointManager(t, mc, workDir)
	cm.saveOnce(context.Background())

	if saveCalled {
		t.Error("SaveCheckpoint should not be called for empty checkpoint dir")
	}
}

func TestSaveOnce_RPCFailure(t *testing.T) {
	workDir := t.TempDir()
	checkpointDir := filepath.Join(workDir, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "data.bin"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	mc := &mockClient{
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			return nil, fmt.Errorf("network error")
		},
	}

	cm := newTestCheckpointManager(t, mc, workDir)
	cm.saveOnce(context.Background())

	if cm.LastSaveSuccess() {
		t.Error("expected LastSaveSuccess to be false after failure")
	}
	if cm.Sequence() != 0 {
		t.Errorf("sequence should be reverted to 0, got %d", cm.Sequence())
	}
}

func TestSaveOnce_Rejected(t *testing.T) {
	workDir := t.TempDir()
	checkpointDir := filepath.Join(workDir, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "data.bin"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	mc := &mockClient{
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			return &lettucev1.SaveCheckpointResponse{Accepted: false, Message: "stale sequence"}, nil
		},
	}

	cm := newTestCheckpointManager(t, mc, workDir)
	cm.saveOnce(context.Background())

	if cm.LastSaveSuccess() {
		t.Error("expected LastSaveSuccess to be false after rejection")
	}
}

func TestCheckpointManagerRun_PeriodicSaves(t *testing.T) {
	workDir := t.TempDir()
	checkpointDir := filepath.Join(workDir, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "state.bin"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	saveCount := 0
	mc := &mockClient{
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			mu.Lock()
			saveCount++
			mu.Unlock()
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	cm := newTestCheckpointManager(t, mc, workDir)
	cm.interval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(cm.doneCh)
		cm.Run(ctx)
	}()

	// Wait for at least 2 saves.
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-cm.doneCh

	mu.Lock()
	count := saveCount
	mu.Unlock()

	// At least 2 periodic saves + 1 final save on context cancellation.
	if count < 2 {
		t.Errorf("expected at least 2 saves, got %d", count)
	}
}

func TestCheckpointManagerRun_FinalSaveOnShutdown(t *testing.T) {
	workDir := t.TempDir()
	checkpointDir := filepath.Join(workDir, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "state.bin"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	saveCount := 0
	mc := &mockClient{
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			mu.Lock()
			saveCount++
			mu.Unlock()
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	cm := newTestCheckpointManager(t, mc, workDir)
	cm.interval = 10 * time.Second // long interval so periodic save doesn't fire

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(cm.doneCh)
		cm.Run(ctx)
	}()

	// Cancel immediately â€” should trigger final save.
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-cm.doneCh

	mu.Lock()
	count := saveCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected exactly 1 final save, got %d", count)
	}
}

func TestSaveOnce_SequenceRecoveryAfterFailure(t *testing.T) {
	workDir := t.TempDir()
	checkpointDir := filepath.Join(workDir, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "state.bin"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	callCount := 0
	mc := &mockClient{
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			callCount++
			if callCount == 2 {
				// Second call fails.
				return nil, fmt.Errorf("network error")
			}
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	cm := newTestCheckpointManager(t, mc, workDir)

	// First save: success.
	cm.saveOnce(context.Background())
	if cm.Sequence() != 1 {
		t.Errorf("after first save: sequence = %d, want 1", cm.Sequence())
	}
	if !cm.LastSaveSuccess() {
		t.Error("after first save: expected success")
	}

	// Second save: failure.
	cm.saveOnce(context.Background())
	if cm.Sequence() != 1 {
		t.Errorf("after failed save: sequence = %d, want 1 (reverted)", cm.Sequence())
	}
	if cm.LastSaveSuccess() {
		t.Error("after failed save: expected failure")
	}

	// Third save: success (recovery).
	cm.saveOnce(context.Background())
	if cm.Sequence() != 2 {
		t.Errorf("after recovery save: sequence = %d, want 2", cm.Sequence())
	}
	if !cm.LastSaveSuccess() {
		t.Error("after recovery save: expected success")
	}
}

func TestSaveOnce_TarError(t *testing.T) {
	workDir := t.TempDir()
	// Create a file (not a directory) at the checkpoint path.
	checkpointPath := filepath.Join(workDir, "checkpoint")
	if err := os.WriteFile(checkpointPath, []byte("not-a-dir"), 0644); err != nil {
		t.Fatal(err)
	}

	saveCalled := false
	mc := &mockClient{
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			saveCalled = true
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	cm := newTestCheckpointManager(t, mc, workDir)
	cm.saveOnce(context.Background())

	if saveCalled {
		t.Error("SaveCheckpoint should not be called when tarDirectory fails")
	}
	if cm.LastSaveSuccess() {
		t.Error("LastSaveSuccess should be false when tarDirectory fails")
	}
	if cm.Sequence() != 0 {
		t.Errorf("sequence should remain 0, got %d", cm.Sequence())
	}
}

func TestSaveOnce_ResumedSequence(t *testing.T) {
	// When resuming from a checkpoint, sequence starts at the resumed value.
	workDir := t.TempDir()
	checkpointDir := filepath.Join(workDir, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "state.bin"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var lastSeq int32
	mc := &mockClient{
		saveCheckpointFn: func(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
			mu.Lock()
			lastSeq = req.CheckpointSequence
			mu.Unlock()
			return &lettucev1.SaveCheckpointResponse{Accepted: true}, nil
		},
	}

	cm := newTestCheckpointManager(t, mc, workDir)
	cm.sequence = 5 // Simulating resumption from checkpoint sequence 5.

	cm.saveOnce(context.Background())

	if cm.Sequence() != 6 {
		t.Errorf("sequence = %d, want 6 (resumed from 5)", cm.Sequence())
	}
	mu.Lock()
	seq := lastSeq
	mu.Unlock()
	if seq != 6 {
		t.Errorf("request sequence = %d, want 6", seq)
	}
}

func TestCheckpointManagerStop(t *testing.T) {
	workDir := t.TempDir()
	mc := &mockClient{}

	cm := newTestCheckpointManager(t, mc, workDir)
	cm.interval = 10 * time.Second

	ctx := context.Background()
	go func() {
		defer close(cm.doneCh)
		cm.Run(ctx)
	}()

	// Stop should return quickly.
	done := make(chan struct{})
	go func() {
		cm.Stop()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return in time")
	}
}
