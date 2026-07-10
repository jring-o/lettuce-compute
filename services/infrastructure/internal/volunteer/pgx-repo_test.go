//go:build integration

package volunteer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

func setupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}

	cleanup := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM leaf_stats_snapshots")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_adjustments")
		_, _ = pool.Exec(ctx, "DELETE FROM credit_ledger")
		_, _ = pool.Exec(ctx, "DELETE FROM results")
		_, _ = pool.Exec(ctx, "DELETE FROM work_unit_assignment_history")
		_, _ = pool.Exec(ctx, "DELETE FROM work_units")
		_, _ = pool.Exec(ctx, "DELETE FROM batches")
		_, _ = pool.Exec(ctx, "DELETE FROM leafs")
		_, _ = pool.Exec(ctx, "DELETE FROM volunteers")
		_, _ = pool.Exec(ctx, "DELETE FROM users")
		pool.Close()
	}

	return pool, cleanup
}

func newTestPublicKey() []byte {
	key := make([]byte, 32)
	id1 := uuid.New()
	id2 := uuid.New()
	copy(key, id1[:])
	copy(key[16:], id2[:])
	return key
}

func newTestVolunteer() *Volunteer {
	now := time.Now().UTC()
	return &Volunteer{
		PublicKey: newTestPublicKey(),
		HardwareCapabilities: HardwareCapabilities{
			CPUCores:        8,
			CPUModel:        "AMD Ryzen 7 5800X",
			MaxCPUCores:     4,
			MemoryTotalMB:   32768,
			MaxMemoryMB:     16384,
			DiskAvailableMB: 102400,
			MaxDiskMB:       10240,
			GPUs: []GpuInfo{
				{
					Model:             "NVIDIA RTX 3080",
					Vendor:            "nvidia",
					VRAMMB:            10240,
					MaxVRAMPct:        50,
					ComputeCapability: "8.6",
				},
			},
		},
		AvailableRuntimes: []string{"NATIVE", "CONTAINER"},
		SchedulingMode:    ScheduleAlways,
		IsActive:          true,
		LastSeenAt:        &now,
	}
}

func TestCreate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	err := repo.Create(ctx, v)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if types.IsNilID(v.ID) {
		t.Error("ID should be set after Create")
	}
	if v.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if v.RegisteredAt.IsZero() {
		t.Error("RegisteredAt should be set")
	}
	if v.SchedulingMode != ScheduleAlways {
		t.Errorf("SchedulingMode = %q, want %q", v.SchedulingMode, ScheduleAlways)
	}
}

func TestCreateDuplicatePublicKey(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v1 := newTestVolunteer()
	if err := repo.Create(ctx, v1); err != nil {
		t.Fatalf("Create v1: %v", err)
	}

	v2 := newTestVolunteer()
	v2.PublicKey = v1.PublicKey // same key
	err := repo.Create(ctx, v2)
	if err == nil {
		t.Fatal("expected conflict error for duplicate public key")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d, want 409", apiErr.HTTPStatus)
	}
}

func TestGetByID(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	if err := repo.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != v.ID {
		t.Errorf("ID = %v, want %v", got.ID, v.ID)
	}
	if got.HardwareCapabilities.CPUCores != 8 {
		t.Errorf("CPUCores = %d, want 8", got.HardwareCapabilities.CPUCores)
	}
	if len(got.HardwareCapabilities.GPUs) != 1 {
		t.Fatalf("GPUs count = %d, want 1", len(got.HardwareCapabilities.GPUs))
	}
	if got.HardwareCapabilities.GPUs[0].Model != "NVIDIA RTX 3080" {
		t.Errorf("GPU model = %q, want %q", got.HardwareCapabilities.GPUs[0].Model, "NVIDIA RTX 3080")
	}
}

func TestGetByIDNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	_, err := repo.GetByID(ctx, types.NewID())
	if err == nil {
		t.Fatal("expected error for non-existent ID")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestGetByPublicKey(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	if err := repo.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByPublicKey(ctx, v.PublicKey)
	if err != nil {
		t.Fatalf("GetByPublicKey: %v", err)
	}
	if got.ID != v.ID {
		t.Errorf("ID = %v, want %v", got.ID, v.ID)
	}
}

func TestGetByPublicKeyNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	_, err := repo.GetByPublicKey(ctx, newTestPublicKey())
	if err == nil {
		t.Fatal("expected error for non-existent public key")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestUpdate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	if err := repo.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}

	origUpdatedAt := v.UpdatedAt
	time.Sleep(50 * time.Millisecond)

	// Update hardware capabilities.
	v.HardwareCapabilities.MaxCPUCores = 8
	v.HardwareCapabilities.MaxMemoryMB = 32768
	displayName := "Updated Volunteer"
	v.DisplayName = &displayName
	if err := repo.Update(ctx, v); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if !v.UpdatedAt.After(origUpdatedAt) {
		t.Error("UpdatedAt should have changed")
	}

	got, err := repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.HardwareCapabilities.MaxCPUCores != 8 {
		t.Errorf("MaxCPUCores = %d, want 8", got.HardwareCapabilities.MaxCPUCores)
	}
	if got.HardwareCapabilities.MaxMemoryMB != 32768 {
		t.Errorf("MaxMemoryMB = %d, want 32768", got.HardwareCapabilities.MaxMemoryMB)
	}
	if got.DisplayName == nil || *got.DisplayName != "Updated Volunteer" {
		t.Errorf("DisplayName = %v, want %q", got.DisplayName, "Updated Volunteer")
	}
}

func TestUpdateNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	v.ID = types.NewID()
	err := repo.Update(ctx, v)
	if err == nil {
		t.Fatal("expected error for non-existent volunteer")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestUpdateLastSeen(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	if err := repo.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if err := repo.UpdateLastSeen(ctx, v.ID); err != nil {
		t.Fatalf("UpdateLastSeen: %v", err)
	}

	got, err := repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.LastSeenAt == nil {
		t.Fatal("LastSeenAt should be set")
	}
	if !got.LastSeenAt.After(*v.LastSeenAt) {
		t.Error("LastSeenAt should have been updated to a later time")
	}
}

func TestUpdateLastSeenNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	err := repo.UpdateLastSeen(ctx, types.NewID())
	if err == nil {
		t.Fatal("expected error for non-existent volunteer")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestSetActive(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	v.IsActive = true
	if err := repo.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Deactivate.
	if err := repo.SetActive(ctx, v.ID, false); err != nil {
		t.Fatalf("SetActive(false): %v", err)
	}
	got, err := repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.IsActive {
		t.Error("expected is_active = false")
	}

	// Reactivate.
	if err := repo.SetActive(ctx, v.ID, true); err != nil {
		t.Fatalf("SetActive(true): %v", err)
	}
	got, err = repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.IsActive {
		t.Error("expected is_active = true")
	}
}

func TestSetActiveNotFound(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	err := repo.SetActive(ctx, types.NewID(), true)
	if err == nil {
		t.Fatal("expected error for non-existent volunteer")
	}
	apiErr, ok := err.(*apierror.APIError)
	if !ok {
		t.Fatalf("expected *apierror.APIError, got %T", err)
	}
	if apiErr.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", apiErr.HTTPStatus)
	}
}

func TestListAll(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		v := newTestVolunteer()
		if err := repo.Create(ctx, v); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	volunteers, pagination, err := repo.List(ctx, VolunteerListFilters{}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(volunteers) < 3 {
		t.Errorf("expected at least 3 volunteers, got %d", len(volunteers))
	}
	if pagination.HasMore {
		t.Error("HasMore should be false for small set")
	}
}

func TestListFilterByIsActive(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	active := newTestVolunteer()
	active.IsActive = true
	if err := repo.Create(ctx, active); err != nil {
		t.Fatalf("Create active: %v", err)
	}

	inactive := newTestVolunteer()
	inactive.IsActive = false
	if err := repo.Create(ctx, inactive); err != nil {
		t.Fatalf("Create inactive: %v", err)
	}

	isActive := true
	volunteers, _, err := repo.List(ctx, VolunteerListFilters{
		IsActive: &isActive,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, v := range volunteers {
		if !v.IsActive {
			t.Errorf("expected is_active = true, got false for volunteer %v", v.ID)
		}
	}
}

func TestListFilterBySchedulingMode(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	always := newTestVolunteer()
	always.SchedulingMode = ScheduleAlways
	if err := repo.Create(ctx, always); err != nil {
		t.Fatalf("Create always: %v", err)
	}

	whenIdle := newTestVolunteer()
	whenIdle.SchedulingMode = ScheduleWhenIdle
	if err := repo.Create(ctx, whenIdle); err != nil {
		t.Fatalf("Create when_idle: %v", err)
	}

	mode := ScheduleWhenIdle
	volunteers, _, err := repo.List(ctx, VolunteerListFilters{
		SchedulingMode: &mode,
	}, types.PaginationRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(volunteers) != 1 {
		t.Fatalf("expected 1 volunteer, got %d", len(volunteers))
	}
	if volunteers[0].ID != whenIdle.ID {
		t.Errorf("expected volunteer %v, got %v", whenIdle.ID, volunteers[0].ID)
	}
}

func TestListPagination(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		v := newTestVolunteer()
		if err := repo.Create(ctx, v); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Page 1: 3 items.
	volunteers, pagination, err := repo.List(ctx, VolunteerListFilters{}, types.PaginationRequest{PageSize: 3})
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(volunteers) != 3 {
		t.Fatalf("page 1: got %d volunteers, want 3", len(volunteers))
	}
	if !pagination.HasMore {
		t.Error("page 1: HasMore should be true")
	}

	// Page 2: remaining 2 items.
	volunteers2, pagination2, err := repo.List(ctx, VolunteerListFilters{}, types.PaginationRequest{PageSize: 3, Cursor: pagination.NextCursor})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(volunteers2) != 2 {
		t.Fatalf("page 2: got %d volunteers, want 2", len(volunteers2))
	}
	if pagination2.HasMore {
		t.Error("page 2: HasMore should be false")
	}

	// Ensure no overlap.
	seen := make(map[types.ID]bool)
	for _, v := range volunteers {
		seen[v.ID] = true
	}
	for _, v := range volunteers2 {
		if seen[v.ID] {
			t.Errorf("duplicate volunteer %v across pages", v.ID)
		}
	}
}

func TestHardwareCapabilitiesRoundTrip(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewPgxRepository(pool)
	ctx := context.Background()

	v := newTestVolunteer()
	v.HardwareCapabilities = HardwareCapabilities{
		CPUCores:         16,
		CPUModel:         "Intel Core i9-12900K",
		MaxCPUCores:      8,
		MemoryTotalMB:    65536,
		MaxMemoryMB:      32768,
		DiskAvailableMB:  500000,
		MaxDiskMB:        50000,
		MaxBandwidthMbps: 100,
		GPUs: []GpuInfo{
			{
				Model:             "NVIDIA RTX 4090",
				Vendor:            "nvidia",
				VRAMMB:            24576,
				MaxVRAMPct:        75,
				ComputeCapability: "8.9",
			},
			{
				Model:             "AMD RX 7900 XTX",
				Vendor:            "amd",
				VRAMMB:            24576,
				MaxVRAMPct:        100,
				ComputeCapability: "",
			},
		},
	}

	if err := repo.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	hw := got.HardwareCapabilities
	if hw.CPUCores != 16 {
		t.Errorf("CPUCores = %d, want 16", hw.CPUCores)
	}
	if hw.CPUModel != "Intel Core i9-12900K" {
		t.Errorf("CPUModel = %q, want %q", hw.CPUModel, "Intel Core i9-12900K")
	}
	if hw.MaxBandwidthMbps != 100 {
		t.Errorf("MaxBandwidthMbps = %d, want 100", hw.MaxBandwidthMbps)
	}
	if len(hw.GPUs) != 2 {
		t.Fatalf("GPUs count = %d, want 2", len(hw.GPUs))
	}
	if hw.GPUs[0].Model != "NVIDIA RTX 4090" {
		t.Errorf("GPU[0].Model = %q, want %q", hw.GPUs[0].Model, "NVIDIA RTX 4090")
	}
	if hw.GPUs[1].Vendor != "amd" {
		t.Errorf("GPU[1].Vendor = %q, want %q", hw.GPUs[1].Vendor, "amd")
	}
}
