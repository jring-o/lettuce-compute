//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// --- F29 E2E: WASM Leaf Execution ---

// TestWasmE2E_LeafCreationAndExecution verifies that WASM leafs can be created,
// activated, assigned to WASM-capable volunteers, executed, validated, and credited.
func TestWasmE2E_LeafCreationAndExecution(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "wasm-e2e")

	// Create a WASM leaf with redundancy=2.
	wasmExecConfig := leaf.ExecutionConfig{
		Runtime:       "WASM",
		Binaries:      map[string]string{"wasm": "https://example.com/bin/module.wasm"},
		MaxMemoryMB:   2048,
		MaxDiskMB:     5120,
		MaxCPUSeconds: 1800,
	}
	wasmValConfig := leaf.ValidationConfig{
		RedundancyFactor:   2,
		AgreementThreshold: 1.0,
		ComparisonMode:     "EXACT",
		MaxRetries:         3,
	}
	wasmLeaf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:            "WASM Test Leaf",
		TaskPattern:     leaf.PatternParameterSweep,
		ExecConfig:      wasmExecConfig,
		ValConfig:       wasmValConfig,
		FTConfig:        defaultFTConfig(),
		DataConfig:      defaultDataConfig(),
		CreditConfig:    leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	// Generate 2 WUs (parameter sweep with 2 combinations).
	generateLeafWUs(t, env, wasmLeaf.ID, 2)

	// Register volunteer 1 with NATIVE + WASM runtimes.
	vol1Pub := genVolunteerKey(t)
	vol1ID := registerHLVolunteerWithRuntimes(t, env, ctx, vol1Pub, "WASM Vol 1", []string{"NATIVE", "WASM"})

	// Volunteer 1 requests work → should get a WASM WU.
	wuResp1 := requestWUFromLeafs(t, env, ctx, vol1ID, vol1Pub, []string{wasmLeaf.ID.String()})
	if wuResp1.WorkUnitId == "" {
		t.Fatal("volunteer 1 should have received a work unit")
	}
	if wuResp1.Runtime != "WASM" {
		t.Errorf("work unit runtime = %q, want WASM", wuResp1.Runtime)
	}
	if es := wuResp1.GetExecutionSpec(); es == nil || es.Binaries["wasm"] == "" {
		t.Error("work unit execution_spec should have wasm binary URL")
	}

	// Volunteer 1 submits result.
	output1 := []byte(`{"result": "wasm-output-1"}`)
	submitWUResult(t, env, ctx, vol1ID, vol1Pub, wuResp1.WorkUnitId, output1)

	// Register volunteer 2 with NATIVE + WASM runtimes.
	vol2Pub := genVolunteerKey(t)
	vol2ID := registerHLVolunteerWithRuntimes(t, env, ctx, vol2Pub, "WASM Vol 2", []string{"NATIVE", "WASM"})

	// Volunteer 2 corroborates the SAME work unit (redundancy_factor=2). The scheduler
	// moves a WU out of QUEUED on first assignment (Alpha model), so a second volunteer
	// is placed on the same WU via a direct redundant assignment, mirroring the
	// maintained alpha/beta suites.
	createRedundantAssignment(t, env.pool, ctx, wuResp1.WorkUnitId, types.MustParseID(vol2ID))

	// Submit matching result from volunteer 2 for the same work unit.
	submitWUResult(t, env, ctx, vol2ID, vol2Pub, wuResp1.WorkUnitId, output1)

	// Verify credit was granted to both volunteers.
	vol1IDParsed := types.MustParseID(vol1ID)
	vol2IDParsed := types.MustParseID(vol2ID)
	assertCreditExists(t, env.pool, ctx, vol1IDParsed, wasmLeaf.ID, 1)
	assertCreditExists(t, env.pool, ctx, vol2IDParsed, wasmLeaf.ID, 1)
}

// TestWasmE2E_NonWasmVolunteerExcluded verifies that a volunteer without WASM
// in available_runtimes does not receive WASM work units.
func TestWasmE2E_NonWasmVolunteerExcluded(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "wasm-excl")

	// Create a WASM-only leaf.
	wasmLeaf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:        "WASM Only Leaf",
		TaskPattern: leaf.PatternParameterSweep,
		ExecConfig: leaf.ExecutionConfig{
			Runtime:       "WASM",
			Binaries:      map[string]string{"wasm": "https://example.com/bin/module.wasm"},
			MaxMemoryMB:   2048,
			MaxDiskMB:     5120,
			MaxCPUSeconds: 1800,
		},
		ValConfig:    defaultHLValConfig(),
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	generateLeafWUs(t, env, wasmLeaf.ID, 5)

	// Register a NATIVE-only volunteer (no WASM).
	volPub := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, volPub, "Native Only Vol")

	// Request work → should get NO_WORK_AVAILABLE (server returns NotFound).
	requestWUExpectNone(t, env, ctx, volID, volPub, []string{wasmLeaf.ID.String()})
}

// TestWasmE2E_GetHeadInfoExecutionSpec verifies that GetHeadInfo returns
// ExecutionSpec in LeafInfo for both WASM and NATIVE leafs.
func TestWasmE2E_GetHeadInfoExecutionSpec(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "wasm-headinfo")

	// Create a WASM leaf.
	createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:        "WASM Leaf for HeadInfo",
		TaskPattern: leaf.PatternParameterSweep,
		ExecConfig: leaf.ExecutionConfig{
			Runtime:       "WASM",
			Binaries:      map[string]string{"wasm": "https://example.com/bin/module.wasm"},
			MaxMemoryMB:   2048,
			MaxDiskMB:     5120,
			MaxCPUSeconds: 1800,
		},
		ValConfig:    defaultHLValConfig(),
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	// Create a NATIVE leaf.
	createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:        "Native Leaf for HeadInfo",
		TaskPattern: leaf.PatternParameterSweep,
		ExecConfig:  defaultExecConfig(),
		ValConfig:   defaultHLValConfig(),
		FTConfig:    defaultFTConfig(),
		DataConfig:  defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	// Call GetHeadInfo via gRPC.
	resp, err := env.grpc.GetHeadInfo(ctx, &lettucev1.GetHeadInfoRequest{})
	if err != nil {
		t.Fatalf("GetHeadInfo: %v", err)
	}

	if len(resp.Leafs) < 2 {
		t.Fatalf("expected at least 2 leafs in GetHeadInfo, got %d", len(resp.Leafs))
	}

	var foundWasm, foundNative bool
	for _, li := range resp.Leafs {
		es := li.GetExecutionSpec()
		if es == nil {
			t.Errorf("leaf %q has nil execution_spec", li.Name)
			continue
		}

		switch li.Name {
		case "WASM Leaf for HeadInfo":
			foundWasm = true
			if es.Binaries["wasm"] != "https://example.com/bin/module.wasm" {
				t.Errorf("WASM leaf binaries[wasm] = %q, want %q", es.Binaries["wasm"], "https://example.com/bin/module.wasm")
			}
		case "Native Leaf for HeadInfo":
			foundNative = true
			if es.Binaries["linux-amd64"] != "https://example.com/bin/linux-amd64" {
				t.Errorf("NATIVE leaf binaries[linux-amd64] = %q, want %q", es.Binaries["linux-amd64"], "https://example.com/bin/linux-amd64")
			}
		}
	}

	if !foundWasm {
		t.Error("WASM leaf not found in GetHeadInfo response")
	}
	if !foundNative {
		t.Error("NATIVE leaf not found in GetHeadInfo response")
	}
}

// TestHeadInfo_CarriesResourceRequirements is the head half of the TB-15
// regression. Dispatch refuses a leaf whose resource_requirements.min_disk_mb or
// min_cpu_cores exceeds the volunteer's advertised budgets, but GetHeadInfo did
// not report either number, so no client could tell an ineligible leaf from an
// eligible one — every volunteer-side diagnostic reported "eligible" and the
// machine idled. The two values must reach the wire, and must be the
// resource_requirements ones, not execution_config's max_disk_mb.
func TestHeadInfo_CarriesResourceRequirements(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "headinfo-resreqs")

	// min_disk_mb (15 GB) is deliberately different from the execution config's
	// max_disk_mb (5 GB): they are separate fields with no synchronisation, and
	// reading the wrong one is the trap this test exists to catch.
	execCfg := defaultExecConfig()
	execCfg.MaxDiskMB = 5120
	createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:         "Disk Hungry Leaf",
		TaskPattern:  leaf.PatternParameterSweep,
		ExecConfig:   execCfg,
		ValConfig:    defaultHLValConfig(),
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
		ResourceReqs: &leaf.ResourceRequirements{MinCPUCores: 4, MinDiskMB: 15360},
	})

	resp, err := env.grpc.GetHeadInfo(ctx, &lettucev1.GetHeadInfoRequest{})
	if err != nil {
		t.Fatalf("GetHeadInfo: %v", err)
	}

	var found bool
	for _, li := range resp.Leafs {
		if li.Name != "Disk Hungry Leaf" {
			continue
		}
		found = true
		rr := li.GetResourceRequirements()
		if rr == nil {
			t.Fatal("leaf has nil resource_requirements: a volunteer cannot check what it is never sent")
		}
		if rr.GetMinDiskMb() != 15360 {
			t.Errorf("min_disk_mb = %d, want 15360 (5120 would mean execution_config.max_disk_mb was read instead)", rr.GetMinDiskMb())
		}
		if rr.GetMinCpuCores() != 4 {
			t.Errorf("min_cpu_cores = %d, want 4", rr.GetMinCpuCores())
		}
	}
	if !found {
		t.Fatal("Disk Hungry Leaf not found in GetHeadInfo response")
	}
}

// TestHeadInfo_CarriesGPURequirements is the TB-21 protocol half: the three GPU
// dimensions dispatch matches on have to reach the volunteer, or its eligibility
// check reports GPU leafs runnable on any machine with a GPU of any size or make.
func TestHeadInfo_CarriesGPURequirements(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "headinfo-gpureqs")

	execCfg := defaultExecConfig()
	execCfg.Runtime = leaf.RuntimeContainer
	img := "example.test/model:1.0"
	execCfg.Image = &img
	execCfg.Binaries = nil
	execCfg.BinaryChecksums = nil
	execCfg.GPURequired = true
	execCfg.GPUType = leaf.GPUTypeNvidia

	cc := "8.6"
	createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:         "GPU Hungry Leaf",
		TaskPattern:  leaf.PatternParameterSweep,
		ExecConfig:   execCfg,
		ValConfig:    defaultHLValConfig(),
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
		ResourceReqs: &leaf.ResourceRequirements{
			MinCPUCores: 1, MinDiskMB: 1024,
			GPURequired: true, MinGPUVRAMMB: 4096, GPUComputeCapability: &cc,
		},
	})

	resp, err := env.grpc.GetHeadInfo(ctx, &lettucev1.GetHeadInfoRequest{})
	if err != nil {
		t.Fatalf("GetHeadInfo: %v", err)
	}

	var found bool
	for _, li := range resp.Leafs {
		if li.Name != "GPU Hungry Leaf" {
			continue
		}
		found = true
		rr := li.GetResourceRequirements()
		if rr == nil {
			t.Fatal("leaf has nil resource_requirements: a volunteer cannot check what it is never sent")
		}
		if rr.GetMinGpuVramMb() != 4096 {
			t.Errorf("min_gpu_vram_mb = %d, want 4096", rr.GetMinGpuVramMb())
		}
		if rr.GetGpuType() != leaf.GPUTypeNvidia {
			t.Errorf("gpu_type = %q, want %q", rr.GetGpuType(), leaf.GPUTypeNvidia)
		}
		if rr.GetGpuComputeCapability() != cc {
			t.Errorf("gpu_compute_capability = %q, want %q", rr.GetGpuComputeCapability(), cc)
		}
	}
	if !found {
		t.Fatal("GPU Hungry Leaf not found in GetHeadInfo response")
	}
}

// registerHLVolunteerWithRuntimes registers a volunteer with specific available runtimes.
func registerHLVolunteerWithRuntimes(t *testing.T, env *headsLeafsEnv, ctx context.Context, pubKey []byte, name string, runtimes []string) string {
	t.Helper()
	hw := &lettucev1.HardwareCapabilities{
		CpuCores:        8,
		CpuModel:        "Test CPU",
		MaxCpuCores:     4,
		MemoryTotalMb:   32768,
		MaxMemoryMb:     16384,
		DiskAvailableMb: 102400,
		MaxDiskMb:       51200,
	}
	regResp, err := env.grpc.RegisterVolunteer(signFor(t, ctx, pubKey), &lettucev1.RegisterVolunteerRequest{
		PublicKey:         pubKey,
		DisplayName:       name,
		Hardware:          hw,
		AvailableRuntimes: runtimes,
		SchedulingMode:    "ALWAYS",
	})
	if err != nil {
		t.Fatalf("register volunteer %s: %v", name, err)
	}
	return regResp.VolunteerId
}
