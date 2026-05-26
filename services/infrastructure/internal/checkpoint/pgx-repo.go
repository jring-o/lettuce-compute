package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// PgxRepository implements Repository using pgx and local filesystem storage.
type PgxRepository struct {
	pool       *pgxpool.Pool
	storageDir string
}

// NewPgxRepository creates a new PgxRepository.
// storageDir is the root directory for checkpoint file storage.
func NewPgxRepository(pool *pgxpool.Pool, storageDir string) *PgxRepository {
	return &PgxRepository{pool: pool, storageDir: storageDir}
}

// Save stores a checkpoint, replacing any existing checkpoint for the work unit.
// It writes the data to the filesystem and records metadata in file_uploads.
func (r *PgxRepository) Save(ctx context.Context, cp *Checkpoint, data []byte) error {
	// Verify checksum.
	hash := sha256.Sum256(data)
	computed := hex.EncodeToString(hash[:])
	if computed != cp.ChecksumSHA256 {
		return apierror.ValidationError("checkpoint checksum mismatch",
			map[string]string{"computed": computed, "provided": cp.ChecksumSHA256})
	}

	// Build storage key and filesystem path.
	storageKey := fmt.Sprintf("checkpoints/%s/%s/%d.tar",
		cp.LeafID, cp.WorkUnitID, cp.CheckpointSequence)
	cp.StorageKey = storageKey
	absPath := filepath.Join(r.storageDir, storageKey)

	// Ensure directory exists.
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return apierror.Internal("failed to create checkpoint directory", err)
	}

	// Write checkpoint data to filesystem.
	if err := os.WriteFile(absPath, data, 0o644); err != nil {
		return apierror.Internal("failed to write checkpoint file", err)
	}

	// Clean up the new file if the DB transaction fails.
	var committed bool
	defer func() {
		if !committed {
			os.Remove(absPath)
		}
	}()

	// Begin transaction for atomic DB operations.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return apierror.Internal("failed to begin transaction", err)
	}
	defer tx.Rollback(ctx)

	// Delete any existing checkpoint for this work unit (keep only latest).
	// First, find old checkpoint files to clean up.
	rows, err := tx.Query(ctx, `
		SELECT storage_key FROM file_uploads
		WHERE work_unit_id = $1 AND file_type = 'CHECKPOINT'`,
		cp.WorkUnitID)
	if err != nil {
		return apierror.Internal("failed to query old checkpoints", err)
	}
	var oldKeys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return apierror.Internal("failed to scan old checkpoint key", err)
		}
		oldKeys = append(oldKeys, key)
	}
	rows.Close()

	// Delete old checkpoint DB records.
	_, err = tx.Exec(ctx, `
		DELETE FROM file_uploads
		WHERE work_unit_id = $1 AND file_type = 'CHECKPOINT'`,
		cp.WorkUnitID)
	if err != nil {
		return apierror.Internal("failed to delete old checkpoint records", err)
	}

	// Insert new checkpoint record.
	filename := fmt.Sprintf("checkpoint-%s-%d.tar", cp.WorkUnitID, cp.CheckpointSequence)
	now := time.Now().UTC()
	err = tx.QueryRow(ctx, `
		INSERT INTO file_uploads (
			leaf_id, file_type, filename, storage_key, size_bytes,
			content_type, checksum_sha256, work_unit_id,
			checkpoint_sequence, volunteer_id, created_at
		) VALUES ($1, 'CHECKPOINT', $2, $3, $4, 'application/x-tar', $5, $6, $7, $8, $9)
		RETURNING id, created_at`,
		cp.LeafID, filename, storageKey, cp.SizeBytes,
		cp.ChecksumSHA256, cp.WorkUnitID,
		cp.CheckpointSequence, cp.VolunteerID, now,
	).Scan(&cp.ID, &cp.CreatedAt)
	if err != nil {
		return apierror.Internal("failed to insert checkpoint record", err)
	}

	// Update work unit checkpoint tracking.
	_, err = tx.Exec(ctx, `
		UPDATE work_units SET
			last_checkpoint_at = $2,
			last_checkpoint_sequence = $3
		WHERE id = $1`,
		cp.WorkUnitID, now, cp.CheckpointSequence)
	if err != nil {
		return apierror.Internal("failed to update work unit checkpoint metadata", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return apierror.Internal("failed to commit checkpoint", err)
	}
	committed = true

	// Clean up old checkpoint files (best effort, after commit).
	for _, key := range oldKeys {
		os.Remove(filepath.Join(r.storageDir, key))
	}

	return nil
}

// GetLatest returns the latest checkpoint and its data for a work unit.
// Returns nil, nil, nil if no checkpoint exists.
func (r *PgxRepository) GetLatest(ctx context.Context, workUnitID types.ID) (*Checkpoint, []byte, error) {
	var cp Checkpoint
	err := r.pool.QueryRow(ctx, `
		SELECT id, leaf_id, work_unit_id, volunteer_id,
			checkpoint_sequence, storage_key, size_bytes,
			checksum_sha256, created_at
		FROM file_uploads
		WHERE work_unit_id = $1 AND file_type = 'CHECKPOINT'
		ORDER BY checkpoint_sequence DESC
		LIMIT 1`, workUnitID).Scan(
		&cp.ID, &cp.LeafID, &cp.WorkUnitID, &cp.VolunteerID,
		&cp.CheckpointSequence, &cp.StorageKey, &cp.SizeBytes,
		&cp.ChecksumSHA256, &cp.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, apierror.Internal("failed to query checkpoint", err)
	}

	// Read data from filesystem.
	absPath := filepath.Join(r.storageDir, cp.StorageKey)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, nil, apierror.Internal("failed to read checkpoint file", err)
	}

	return &cp, data, nil
}

// Delete removes all checkpoints for a work unit from both DB and filesystem.
func (r *PgxRepository) Delete(ctx context.Context, workUnitID types.ID) error {
	// Find storage keys before deleting records.
	rows, err := r.pool.Query(ctx, `
		SELECT storage_key FROM file_uploads
		WHERE work_unit_id = $1 AND file_type = 'CHECKPOINT'`,
		workUnitID)
	if err != nil {
		return apierror.Internal("failed to query checkpoints for deletion", err)
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return apierror.Internal("failed to scan checkpoint key", err)
		}
		keys = append(keys, key)
	}
	rows.Close()

	if len(keys) == 0 {
		return nil
	}

	// Delete DB records.
	_, err = r.pool.Exec(ctx, `
		DELETE FROM file_uploads
		WHERE work_unit_id = $1 AND file_type = 'CHECKPOINT'`,
		workUnitID)
	if err != nil {
		return apierror.Internal("failed to delete checkpoint records", err)
	}

	// Remove files (best effort).
	for _, key := range keys {
		os.Remove(filepath.Join(r.storageDir, key))
	}

	return nil
}
