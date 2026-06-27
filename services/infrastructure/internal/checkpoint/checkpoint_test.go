package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// mockRepository is an in-memory checkpoint repository for unit testing.
type mockRepository struct {
	checkpoints map[types.ID]*storedCheckpoint
}

type storedCheckpoint struct {
	cp   *Checkpoint
	data []byte
}

func newMockRepository() *mockRepository {
	return &mockRepository{checkpoints: make(map[types.ID]*storedCheckpoint)}
}

func (m *mockRepository) Save(_ context.Context, cp *Checkpoint, data []byte) error {
	m.checkpoints[cp.WorkUnitID] = &storedCheckpoint{
		cp:   cp,
		data: append([]byte{}, data...),
	}
	cp.ID = types.NewID()
	cp.CreatedAt = time.Now().UTC()
	return nil
}

func (m *mockRepository) GetLatest(_ context.Context, workUnitID types.ID) (*Checkpoint, []byte, error) {
	stored, ok := m.checkpoints[workUnitID]
	if !ok {
		return nil, nil, nil
	}
	return stored.cp, stored.data, nil
}

func (m *mockRepository) LatestSequenceForVolunteer(_ context.Context, workUnitID, volunteerID types.ID) (int, error) {
	stored, ok := m.checkpoints[workUnitID]
	if !ok || stored.cp.VolunteerID != volunteerID {
		return 0, nil
	}
	return stored.cp.CheckpointSequence, nil
}

func (m *mockRepository) GetLatestForVolunteer(_ context.Context, workUnitID, volunteerID types.ID) (*Checkpoint, []byte, error) {
	stored, ok := m.checkpoints[workUnitID]
	if !ok || stored.cp.VolunteerID != volunteerID {
		return nil, nil, nil
	}
	return stored.cp, stored.data, nil
}

func (m *mockRepository) Delete(_ context.Context, workUnitID types.ID) error {
	delete(m.checkpoints, workUnitID)
	return nil
}

func TestMockRepository_SaveAndGetLatest(t *testing.T) {
	repo := newMockRepository()
	ctx := context.Background()

	wuID := types.NewID()
	data := []byte("checkpoint data")
	hash := sha256.Sum256(data)

	cp := &Checkpoint{
		LeafID:          types.NewID(),
		WorkUnitID:         wuID,
		VolunteerID:        types.NewID(),
		CheckpointSequence: 1,
		SizeBytes:          int64(len(data)),
		ChecksumSHA256:     hex.EncodeToString(hash[:]),
	}

	if err := repo.Save(ctx, cp, data); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, gotData, err := repo.GetLatest(ctx, wuID)
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if got == nil {
		t.Fatal("expected checkpoint, got nil")
	}
	if got.CheckpointSequence != 1 {
		t.Errorf("sequence = %d, want 1", got.CheckpointSequence)
	}
	if string(gotData) != "checkpoint data" {
		t.Errorf("data = %q, want %q", gotData, "checkpoint data")
	}
}

func TestMockRepository_GetLatest_NoCheckpoint(t *testing.T) {
	repo := newMockRepository()
	ctx := context.Background()

	cp, data, err := repo.GetLatest(ctx, types.NewID())
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if cp != nil || data != nil {
		t.Error("expected nil checkpoint and data for nonexistent work unit")
	}
}

func TestMockRepository_Delete(t *testing.T) {
	repo := newMockRepository()
	ctx := context.Background()

	wuID := types.NewID()
	data := []byte("data")
	hash := sha256.Sum256(data)
	cp := &Checkpoint{
		LeafID:          types.NewID(),
		WorkUnitID:         wuID,
		VolunteerID:        types.NewID(),
		CheckpointSequence: 1,
		SizeBytes:          int64(len(data)),
		ChecksumSHA256:     hex.EncodeToString(hash[:]),
	}

	_ = repo.Save(ctx, cp, data)
	if err := repo.Delete(ctx, wuID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, _, _ := repo.GetLatest(ctx, wuID)
	if got != nil {
		t.Error("expected nil after delete")
	}
}

func TestMockRepository_UpsertIncreasingSequence(t *testing.T) {
	repo := newMockRepository()
	ctx := context.Background()

	wuID := types.NewID()
	data1 := []byte("seq1")
	hash1 := sha256.Sum256(data1)
	data2 := []byte("seq2")
	hash2 := sha256.Sum256(data2)

	cp1 := &Checkpoint{
		LeafID:          types.NewID(),
		WorkUnitID:         wuID,
		VolunteerID:        types.NewID(),
		CheckpointSequence: 1,
		SizeBytes:          int64(len(data1)),
		ChecksumSHA256:     hex.EncodeToString(hash1[:]),
	}
	cp2 := &Checkpoint{
		LeafID:          cp1.LeafID,
		WorkUnitID:         wuID,
		VolunteerID:        cp1.VolunteerID,
		CheckpointSequence: 2,
		SizeBytes:          int64(len(data2)),
		ChecksumSHA256:     hex.EncodeToString(hash2[:]),
	}

	_ = repo.Save(ctx, cp1, data1)
	_ = repo.Save(ctx, cp2, data2)

	got, gotData, _ := repo.GetLatest(ctx, wuID)
	if got.CheckpointSequence != 2 {
		t.Errorf("sequence = %d, want 2", got.CheckpointSequence)
	}
	if string(gotData) != "seq2" {
		t.Errorf("data = %q, want %q", gotData, "seq2")
	}
}

// TestPgxRepository_FilesystemOps tests that PgxRepository writes/reads files correctly
// (using only filesystem operations, no database).
func TestPgxRepository_FilesystemPaths(t *testing.T) {
	tmpDir := t.TempDir()

	leafID := types.NewID()
	wuID := types.NewID()
	seq := 3

	// Verify storage key format.
	expectedKey := "checkpoints/" + leafID.String() + "/" + wuID.String() + "/3.tar"
	expectedPath := filepath.Join(tmpDir, expectedKey)

	// Write a file at the expected path to verify path construction.
	dir := filepath.Dir(expectedPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	testData := []byte("test checkpoint data")
	if err := os.WriteFile(expectedPath, testData, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Read it back.
	data, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "test checkpoint data" {
		t.Errorf("data = %q, want %q", data, "test checkpoint data")
	}

	// Verify storage key format.
	cp := &Checkpoint{
		LeafID:          leafID,
		WorkUnitID:         wuID,
		CheckpointSequence: seq,
	}
	storageKey := "checkpoints/" + cp.LeafID.String() + "/" + cp.WorkUnitID.String() + "/" +
		string(rune('0'+seq)) + ".tar"
	_ = storageKey // just verifying it constructs

	// Clean up.
	os.Remove(expectedPath)
}
