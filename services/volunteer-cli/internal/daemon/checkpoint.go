package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// CheckpointManager periodically saves checkpoint data from the work directory
// to the infrastructure server via gRPC.
type CheckpointManager struct {
	client   WorkClient
	logger   *slog.Logger
	workDir  string
	wu       *runtime.WorkUnit
	volID    string
	pubKey   ed25519.PublicKey
	interval time.Duration
	sequence int32

	stopCh chan struct{}
	doneCh chan struct{}

	mu              sync.Mutex
	lastSaveSuccess bool
	lastSaveAt      time.Time
}

// Run starts the checkpoint loop. It saves checkpoints at the configured
// interval and performs one final save when ctx is cancelled (graceful shutdown).
func (cm *CheckpointManager) Run(ctx context.Context) {
	ticker := time.NewTicker(cm.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final checkpoint on shutdown.
			cm.saveOnce(context.Background())
			return
		case <-cm.stopCh:
			// Final checkpoint before stopping (use Background since ctx may
			// already be cancelled).
			cm.saveOnce(context.Background())
			return
		case <-ticker.C:
			cm.saveOnce(ctx)
		}
	}
}

// Stop signals the checkpoint manager to stop and waits for it to finish.
func (cm *CheckpointManager) Stop() {
	close(cm.stopCh)
	<-cm.doneCh
}

// LastSaveSuccess returns true if the most recent checkpoint save succeeded.
func (cm *CheckpointManager) LastSaveSuccess() bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.lastSaveSuccess
}

// LastSaveAt returns the timestamp of the last successful checkpoint save.
func (cm *CheckpointManager) LastSaveAt() time.Time {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.lastSaveAt
}

// Sequence returns the current checkpoint sequence number.
func (cm *CheckpointManager) Sequence() int32 {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.sequence
}

// saveOnce tars the checkpoint directory and uploads it to the server.
func (cm *CheckpointManager) saveOnce(ctx context.Context) {
	checkpointDir := filepath.Join(cm.workDir, "checkpoint")

	blob, err := tarDirectory(checkpointDir)
	if err != nil {
		cm.logger.Warn("failed to tar checkpoint directory",
			"work_unit_id", cm.wu.ID,
			"error", err,
		)
		cm.mu.Lock()
		cm.lastSaveSuccess = false
		cm.mu.Unlock()
		return
	}

	if blob == nil {
		// Empty or nonexistent checkpoint dir — nothing to upload.
		return
	}

	cm.mu.Lock()
	cm.sequence++
	seq := cm.sequence
	cm.mu.Unlock()

	resp, err := cm.client.SaveCheckpoint(ctx, &lettucev1.SaveCheckpointRequest{
		WorkUnitId:         cm.wu.ID,
		VolunteerId:        cm.volID,
		PublicKey:          base64.RawURLEncoding.EncodeToString(cm.pubKey),
		CheckpointData:     blob,
		CheckpointSequence: seq,
	})
	if err != nil {
		cm.logger.Warn("failed to save checkpoint",
			"work_unit_id", cm.wu.ID,
			"sequence", seq,
			"error", err,
		)
		cm.mu.Lock()
		cm.lastSaveSuccess = false
		cm.sequence-- // revert increment on failure
		cm.mu.Unlock()
		return
	}

	if !resp.Accepted {
		cm.logger.Warn("checkpoint rejected by server",
			"work_unit_id", cm.wu.ID,
			"sequence", seq,
			"message", resp.Message,
		)
		cm.mu.Lock()
		cm.lastSaveSuccess = false
		cm.sequence--
		cm.mu.Unlock()
		return
	}

	cm.logger.Info("checkpoint saved",
		"work_unit_id", cm.wu.ID,
		"sequence", seq,
		"size_bytes", len(blob),
	)
	cm.mu.Lock()
	cm.lastSaveSuccess = true
	cm.lastSaveAt = time.Now()
	cm.mu.Unlock()
}
