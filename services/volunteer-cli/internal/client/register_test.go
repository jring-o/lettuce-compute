package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestBuildRegistrationRequest(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cfg := config.Defaults()
	cfg.Scheduling.Mode = "WHEN_IDLE"

	hw := &lettucev1.HardwareCapabilities{
		CpuCores:      8,
		CpuModel:      "Test CPU",
		MaxCpuCores:   4,
		MemoryTotalMb: 16384,
		MaxMemoryMb:   8192,
	}

	req := BuildRegistrationRequest(pub, "host-abc", hw, cfg, "NATIVE", "CONTAINER")

	if len(req.PublicKey) != ed25519.PublicKeySize {
		t.Errorf("PublicKey length = %d, want %d", len(req.PublicKey), ed25519.PublicKeySize)
	}
	if req.HostId != "host-abc" {
		t.Errorf("HostId = %q, want %q", req.HostId, "host-abc")
	}
	if req.DisplayName == "" {
		t.Error("DisplayName is empty (expected hostname)")
	}
	if req.Hardware != hw {
		t.Error("Hardware does not match")
	}
	if len(req.AvailableRuntimes) != 2 {
		t.Errorf("AvailableRuntimes length = %d, want 2", len(req.AvailableRuntimes))
	}
	if req.AvailableRuntimes[0] != "NATIVE" || req.AvailableRuntimes[1] != "CONTAINER" {
		t.Errorf("AvailableRuntimes = %v, want [NATIVE, CONTAINER]", req.AvailableRuntimes)
	}
	if req.SchedulingMode != "WHEN_IDLE" {
		t.Errorf("SchedulingMode = %q, want %q", req.SchedulingMode, "WHEN_IDLE")
	}
}

// The advertised runtimes are exactly what the caller passes (derived from the
// live runtime registry and per-head trust) — there is no config fallback, so a
// stale config record can never be advertised as capability (TB-25).
func TestBuildRegistrationRequest_AdvertisesExactlyWhatCallerPasses(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	cfg := config.Defaults()

	req := BuildRegistrationRequest(pub, "host-abc", nil, cfg, "NATIVE", "WASM")

	if len(req.AvailableRuntimes) != 2 || req.AvailableRuntimes[0] != "NATIVE" || req.AvailableRuntimes[1] != "WASM" {
		t.Errorf("AvailableRuntimes = %v, want [NATIVE WASM] (exactly the caller's list)", req.AvailableRuntimes)
	}

	// No runtimes passed advertises none — never a value resurrected from config.
	req = BuildRegistrationRequest(pub, "host-abc", nil, cfg)
	if len(req.AvailableRuntimes) != 0 {
		t.Errorf("AvailableRuntimes = %v, want none when the caller passes none", req.AvailableRuntimes)
	}
}

func TestRegisterNewVolunteer(t *testing.T) {
	withMockHardware(t)
	mock := &mockVolunteerService{
		registerResp: &lettucev1.RegisterVolunteerResponse{
			VolunteerId: "vol-new-123",
			Registered:  true,
		},
	}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cfg := config.Defaults()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	volID, isNew, _, err := Register(context.Background(), client, pub, nil, "", cfg, configPath, DetectHardware(cfg))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if volID != "vol-new-123" {
		t.Errorf("volunteerID = %q, want %q", volID, "vol-new-123")
	}
	if !isNew {
		t.Error("isNew = false, want true")
	}
	if cfg.VolunteerID != "vol-new-123" {
		t.Errorf("cfg.VolunteerID = %q, want %q", cfg.VolunteerID, "vol-new-123")
	}

	// Verify config was persisted.
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if loaded.VolunteerID != "vol-new-123" {
		t.Errorf("persisted VolunteerID = %q, want %q", loaded.VolunteerID, "vol-new-123")
	}
}

func TestRegisterUpdateExisting(t *testing.T) {
	withMockHardware(t)
	mock := &mockVolunteerService{
		registerResp: &lettucev1.RegisterVolunteerResponse{
			VolunteerId: "vol-existing-456",
			Registered:  false,
		},
	}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cfg := config.Defaults()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	volID, isNew, _, err := Register(context.Background(), client, pub, nil, "", cfg, configPath, DetectHardware(cfg))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if volID != "vol-existing-456" {
		t.Errorf("volunteerID = %q, want %q", volID, "vol-existing-456")
	}
	if isNew {
		t.Error("isNew = true, want false (update existing)")
	}
}

func TestRegisterSendsPublicKey(t *testing.T) {
	withMockHardware(t)
	mock := &mockVolunteerService{}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cfg := config.Defaults()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	_, _, _, err = Register(context.Background(), client, pub, nil, "", cfg, configPath, DetectHardware(cfg))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Verify the mock received the correct public key.
	if mock.registerReq == nil {
		t.Fatal("registerReq is nil (server didn't receive the request)")
	}
	if len(mock.registerReq.PublicKey) != ed25519.PublicKeySize {
		t.Errorf("sent PublicKey length = %d, want %d", len(mock.registerReq.PublicKey), ed25519.PublicKeySize)
	}
	for i := range pub {
		if mock.registerReq.PublicKey[i] != pub[i] {
			t.Errorf("PublicKey mismatch at byte %d", i)
			break
		}
	}
}

func TestRegisterDetectsHardware(t *testing.T) {
	withMockHardware(t)
	mock := &mockVolunteerService{}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cfg := config.Defaults()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	_, _, _, err = Register(context.Background(), client, pub, nil, "", cfg, configPath, DetectHardware(cfg))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	hw := mock.registerReq.Hardware
	if hw == nil {
		t.Fatal("hardware is nil in registration request")
	}
	if hw.CpuCores <= 0 {
		t.Errorf("CpuCores = %d, want > 0", hw.CpuCores)
	}
	if hw.MemoryTotalMb <= 0 {
		t.Errorf("MemoryTotalMb = %d, want > 0", hw.MemoryTotalMb)
	}
}

func TestRegisterRPCError(t *testing.T) {
	withMockHardware(t)
	mock := &mockVolunteerService{
		registerErr: status.Error(codes.Unavailable, "server down"),
	}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cfg := config.Defaults()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	_, _, _, err = Register(context.Background(), client, pub, nil, "", cfg, configPath, DetectHardware(cfg))
	if err == nil {
		t.Fatal("expected error from Register when RPC fails")
	}
	if cfg.VolunteerID != "" {
		t.Errorf("VolunteerID should be empty after failed registration, got %q", cfg.VolunteerID)
	}
}

func TestRegisterConfigSaveError(t *testing.T) {
	withMockHardware(t)
	mock := &mockVolunteerService{
		registerResp: &lettucev1.RegisterVolunteerResponse{
			VolunteerId: "vol-save-fail",
			Registered:  true,
		},
	}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cfg := config.Defaults()
	// Use a path that can't be written to (file as directory).
	tmpFile := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(tmpFile, []byte("x"), 0444); err != nil {
		t.Fatalf("creating blocker file: %v", err)
	}
	// Try to save config inside a file (not a directory) — should fail.
	configPath := filepath.Join(tmpFile, "subdir", "config.yaml")

	volID, _, _, err := Register(context.Background(), client, pub, nil, "", cfg, configPath, DetectHardware(cfg))
	if err == nil {
		t.Fatal("expected error when config save fails")
	}
	// The volunteer ID should still be returned even though save failed.
	if volID != "vol-save-fail" {
		t.Errorf("volunteerID = %q, want %q", volID, "vol-save-fail")
	}
}

func TestBuildRegistrationRequestNilHardware(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cfg := config.Defaults()

	// Nil hardware should not panic — it's a valid proto field.
	req := BuildRegistrationRequest(pub, "host-abc", nil, cfg)

	if req.Hardware != nil {
		t.Error("expected nil hardware when nil is passed")
	}
	if req.DisplayName == "" {
		t.Error("DisplayName should be set to hostname")
	}
}

func TestRegisterCancelledContext(t *testing.T) {
	withMockHardware(t)
	mock := &mockVolunteerService{}
	addr, cleanup := startMockServer(t, mock)
	defer cleanup()

	client := newTestClient(t, addr)
	defer client.Close()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cfg := config.Defaults()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, _, _, err = Register(ctx, client, pub, nil, "", cfg, configPath, DetectHardware(cfg))
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}
