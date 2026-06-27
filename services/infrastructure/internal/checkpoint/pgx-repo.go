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

	// Build storage key and filesystem path. The volunteer id is part of the key
	// so concurrent redundancy copies — distinct volunteers computing the same
	// work unit independently — never overwrite each other's checkpoint files.
	storageKey := fmt.Sprintf("checkpoints/%s/%s/%s/%d.tar",
		cp.LeafID, cp.WorkUnitID, cp.VolunteerID, cp.CheckpointSequence)
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

	// Delete this volunteer's existing checkpoint for the work unit (keep only its
	// latest). Scoped by volunteer_id so saving one redundancy copy's checkpoint
	// never deletes a corroborating volunteer's checkpoint for the same unit.
	// First, find old checkpoint files to clean up.
	rows, err := tx.Query(ctx, `
		SELECT storage_key FROM file_uploads
		WHERE work_unit_id = $1 AND volunteer_id = $2 AND file_type = 'CHECKPOINT'`,
		cp.WorkUnitID, cp.VolunteerID)
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

	// Delete old checkpoint DB records (this volunteer's only).
	_, err = tx.Exec(ctx, `
		DELETE FROM file_uploads
		WHERE work_unit_id = $1 AND volunteer_id = $2 AND file_type = 'CHECKPOINT'`,
		cp.WorkUnitID, cp.VolunteerID)
	if err != nil {
		return apierror.Internal("failed to delete old checkpoint records", err)
	}

	// Insert new checkpoint record.
	filename := fmt.Sprintf("checkpoint-%s-%s-%d.tar", cp.WorkUnitID, cp.VolunteerID, cp.CheckpointSequence)
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

	// Update work unit checkpoint tracking. With per-(work_unit, volunteer)
	// sequences this column no longer authorizes saves (LatestSequenceForVolunteer
	// does); it only drives the coarse "a checkpoint exists" gate on the
	// assignment, so keep it monotonic across volunteers via GREATEST.
	_, err = tx.Exec(ctx, `
		UPDATE work_units SET
			last_checkpoint_at = $2,
			last_checkpoint_sequence = GREATEST(last_checkpoint_sequence, $3)
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

// LatestSequenceForVolunteer returns the highest checkpoint sequence this
// volunteer has stored for the work unit, or 0 if it has none. Sequences are
// scoped per (work_unit, volunteer) so concurrent redundancy copies — each an
// independent computation — never collide on a shared counter.
func (r *PgxRepository) LatestSequenceForVolunteer(ctx context.Context, workUnitID, volunteerID types.ID) (int, error) {
	var seq int
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(checkpoint_sequence), 0)
		FROM file_uploads
		WHERE work_unit_id = $1 AND volunteer_id = $2 AND file_type = 'CHECKPOINT'`,
		workUnitID, volunteerID).Scan(&seq)
	if err != nil {
		return 0, apierror.Internal("failed to query volunteer checkpoint sequence", err)
	}
	return seq, nil
}

// GetLatestForVolunteer returns this volunteer's own latest checkpoint and its
// data for the work unit. Returns nil, nil, nil if this volunteer has none.
func (r *PgxRepository) GetLatestForVolunteer(ctx context.Context, workUnitID, volunteerID types.ID) (*Checkpoint, []byte, error) {
	var cp Checkpoint
	err := r.pool.QueryRow(ctx, `
		SELECT id, leaf_id, work_unit_id, volunteer_id,
			checkpoint_sequence, storage_key, size_bytes,
			checksum_sha256, created_at
		FROM file_uploads
		WHERE work_unit_id = $1 AND volunteer_id = $2 AND file_type = 'CHECKPOINT'
		ORDER BY checkpoint_sequence DESC
		LIMIT 1`, workUnitID, volunteerID).Scan(
		&cp.ID, &cp.LeafID, &cp.WorkUnitID, &cp.VolunteerID,
		&cp.CheckpointSequence, &cp.StorageKey, &cp.SizeBytes,
		&cp.ChecksumSHA256, &cp.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, apierror.Internal("failed to query volunteer checkpoint", err)
	}

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
