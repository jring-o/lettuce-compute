//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// --- v0.9.3 Comprehensive E2E: CLI WASM + Browser Volunteering ---

// browserClient wraps Ed25519-authenticated HTTP calls against browser volunteer endpoints.
type browserClient struct {
	baseURL    string
	publicKey  ed25519.PublicKey
	privateKey ed25519.PrivateKey
}

func newBrowserClient(baseURL string) *browserClient {
	pub, priv, _ := ed25519.GenerateKey(nil)
	return &browserClient{baseURL: baseURL, publicKey: pub, privateKey: priv}
}

func (c *browserClient) pubKeyB64url() string {
	return base64.RawURLEncoding.EncodeToString(c.publicKey)
}

func (c *browserClient) signedPost(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	ts := fmt.Sprintf("%d", time.Now().Unix())
	bodyHash := sha256Hex(bodyBytes)
	message := fmt.Sprintf("%s:%s:%s:%s", ts, "POST", path, bodyHash)
	sig := ed25519.Sign(c.privateKey, []byte(message))
	sigB64url := base64.RawURLEncoding.EncodeToString(sig)
	authHeader := fmt.Sprintf("Ed25519 %s:%s:%s", c.pubKeyB64url(), sigB64url, ts)

	req, err := http.NewRequest("POST", c.baseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// signedPostWithKey does a signed POST using a specific private key (for testing bad signatures).
func signedPostWithKey(t *testing.T, baseURL, path string, body any, pub ed25519.PublicKey, priv ed25519.PrivateKey) *http.Response {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	ts := fmt.Sprintf("%d", time.Now().Unix())
	bodyHash := sha256Hex(bodyBytes)
	message := fmt.Sprintf("%s:%s:%s:%s", ts, "POST", path, bodyHash)
	sig := ed25519.Sign(priv, []byte(message))
	sigB64url := base64.RawURLEncoding.EncodeToString(sig)
	authHeader := fmt.Sprintf("Ed25519 %s:%s:%s", base64.RawURLEncoding.EncodeToString(pub), sigB64url, ts)

	req, err := http.NewRequest("POST", baseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func (c *browserClient) register(t *testing.T, runtimes []string, hasGPU bool, gpuVendors []string) string {
	t.Helper()
	body := map[string]any{
		"public_key": c.pubKeyB64url(),
		"hardware": map[string]any{
			"cpu_cores":          4,
			"memory_mb":          8192,
			"has_gpu":            hasGPU,
			"gpu_vendors":        gpuVendors,
			"available_runtimes": runtimes,
		},
	}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", c.baseURL+"/api/v1/volunteers/register", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		bodyOut, _ := io.ReadAll(resp.Body)
		t.Fatalf("register failed: status=%d body=%s", resp.StatusCode, bodyOut)
	}

	var result struct {
		VolunteerID string `json:"volunteer_id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.VolunteerID
}

func (c *browserClient) requestWork(t *testing.T, leafIDs []string) *browserWorkResponse {
	t.Helper()
	return c.requestWorkWithGPU(t, leafIDs, false, nil)
}

func (c *browserClient) requestWorkGPU(t *testing.T, leafIDs []string) *browserWorkResponse {
	t.Helper()
	return c.requestWorkWithGPU(t, leafIDs, true, []string{"WEBGPU"})
}

func (c *browserClient) requestWorkWithGPU(t *testing.T, leafIDs []string, hasGPU bool, gpuVendors []string) *browserWorkResponse {
	t.Helper()
	if gpuVendors == nil {
		gpuVendors = []string{}
	}
	body := map[string]any{
		"leaf_ids":      leafIDs,
		"max_memory_mb": 4096,
		"max_disk_mb":   51200,
		"has_gpu":       hasGPU,
		"gpu_vendors":   gpuVendors,
	}
	resp := c.signedPost(t, "/api/v1/volunteers/request-work", body)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		bodyOut, _ := io.ReadAll(resp.Body)
		t.Fatalf("request-work failed: status=%d body=%s", resp.StatusCode, bodyOut)
	}
	var wr browserWorkResponse
	json.NewDecoder(resp.Body).Decode(&wr)
	return &wr
}

func (c *browserClient) submitResult(t *testing.T, wuID string, output []byte) {
	t.Helper()
	outputB64 := base64.StdEncoding.EncodeToString(output)
	hash := sha256.Sum256(output)
	checksum := hex.EncodeToString(hash[:])
	body := map[string]any{
		"work_unit_id":    wuID,
		"output_data":     outputB64,
		"output_checksum": checksum,
		"exit_code":       0,
		"metrics": map[string]any{
			"wall_clock_seconds": 10,
			"cpu_seconds_user":   8.0,
			"peak_memory_mb":     256,
		},
	}
	resp := c.signedPost(t, "/api/v1/volunteers/submit-result", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyOut, _ := io.ReadAll(resp.Body)
		t.Fatalf("submit-result failed: status=%d body=%s", resp.StatusCode, bodyOut)
	}
}

type browserWorkResponse struct {
	WorkUnitID    string `json:"work_unit_id"`
	LeafID        string `json:"leaf_id"`
	Runtime       string `json:"runtime"`
	ExecutionSpec struct {
		Binaries    map[string]string `json:"binaries"`
		GPURequired bool              `json:"gpu_required"`
		GPUType     string            `json:"gpu_type"`
	} `json:"execution_spec"`
}

// --- Scenario 1: CLI Volunteer Executes WASM Leaf ---

func TestV093E2E_CLIVolunteerWASM(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v093-cli-wasm")

	// Create WASM leaf with redundancy=2.
	wasmLeaf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:            "CLI WASM Leaf",
		TaskPattern:     leaf.PatternParameterSweep,
		ExecConfig: leaf.ExecutionConfig{
			Runtime:       "WASM",
			Binaries:      map[string]string{"wasm": "https://example.com/bin/module.wasm"},
			MaxMemoryMB:   2048,
			MaxDiskMB:     5120,
			MaxCPUSeconds: 1800,
		},
		ValConfig: leaf.ValidationConfig{
			RedundancyFactor:   2,
			AgreementThreshold: 1.0,
			ComparisonMode:     "EXACT",
			MaxRetries:         3,
		},
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	generateLeafWUs(t, env, wasmLeaf.ID, 3)

	// Register CLI volunteer 1 with NATIVE + WASM.
	vol1Pub := genVolunteerKey(t)
	vol1ID := registerHLVolunteerWithRuntimes(t, env, ctx, vol1Pub, "CLI WASM Vol 1", []string{"NATIVE", "WASM"})

	// Volunteer 1 requests work via gRPC.
	wuResp1 := requestWUFromLeafs(t, env, ctx, vol1ID, vol1Pub, []string{wasmLeaf.ID.String()})
	if wuResp1.WorkUnitId == "" {
		t.Fatal("CLI volunteer 1 should have received a work unit")
	}
	if wuResp1.Runtime != "WASM" {
		t.Errorf("work unit runtime = %q, want WASM", wuResp1.Runtime)
	}
	es := wuResp1.GetExecutionSpec()
	if es == nil || es.Binaries["wasm"] == "" {
		t.Error("execution_spec should have wasm binary")
	}

	// Submit result.
	output := []byte(`{"result":"cli-wasm-v093"}`)
	submitWUResult(t, env, ctx, vol1ID, vol1Pub, wuResp1.WorkUnitId, output)

	// Register CLI volunteer 2.
	vol2Pub := genVolunteerKey(t)
	vol2ID := registerHLVolunteerWithRuntimes(t, env, ctx, vol2Pub, "CLI WASM Vol 2", []string{"NATIVE", "WASM"})

	// Volunteer 2 corroborates the SAME WU (redundancy_factor=2). The scheduler moves
	// a WU out of QUEUED on first assignment, so the second assignment is created
	// directly (mirrors the maintained alpha/beta suites).
	createRedundantAssignment(t, env.pool, ctx, wuResp1.WorkUnitId, types.MustParseID(vol2ID))

	// Submit matching result for the same work unit.
	submitWUResult(t, env, ctx, vol2ID, vol2Pub, wuResp1.WorkUnitId, output)

	// Verify credit granted.
	assertCreditExists(t, env.pool, ctx, types.MustParseID(vol1ID), wasmLeaf.ID, 1)
	assertCreditExists(t, env.pool, ctx, types.MustParseID(vol2ID), wasmLeaf.ID, 1)
}

// --- Scenario 2: Browser Volunteer Executes WASM Leaf via REST ---

func TestV093E2E_BrowserVolunteerWASM(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v093-browser-wasm")

	// Create WASM leaf with redundancy=1 (single result accepted).
	wasmLeaf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:        "Browser WASM Leaf",
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

	generateLeafWUs(t, env, wasmLeaf.ID, 3)

	// Create browser volunteer client.
	bc := newBrowserClient(env.httpURL)

	// Register via REST with available_runtimes: ["WASM"].
	volID := bc.register(t, []string{"WASM"}, false, nil)
	if volID == "" {
		t.Fatal("browser volunteer should have received an ID")
	}

	// Request work via REST with Ed25519 auth.
	wu := bc.requestWork(t, []string{wasmLeaf.ID.String()})
	if wu == nil {
		t.Fatal("browser volunteer should have received work")
	}
	if wu.Runtime != "WASM" {
		t.Errorf("work unit runtime = %q, want WASM", wu.Runtime)
	}
	if wu.ExecutionSpec.Binaries["wasm"] == "" {
		t.Error("execution_spec should have wasm binary URL")
	}

	// Submit result via REST with Ed25519 auth.
	output := []byte(`{"result":"browser-wasm-result"}`)
	bc.submitResult(t, wu.WorkUnitID, output)
}

// --- Scenario 3: Browser WebGPU Volunteer ---

func TestV093E2E_BrowserWebGPUVolunteer(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v093-browser-gpu")

	// Create WASM leaf with GPU required and both wasm + wgsl binaries.
	gpuLeaf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:        "GPU WASM Leaf",
		TaskPattern: leaf.PatternParameterSweep,
		ExecConfig: leaf.ExecutionConfig{
			Runtime: "WASM",
			Binaries: map[string]string{
				"wasm": "https://example.com/bin/module.wasm",
				"wgsl": "https://example.com/shader.wgsl",
			},
			GPURequired:   true,
			GPUType:       "WEBGPU",
			MaxMemoryMB:   4096,
			MaxDiskMB:     10240,
			MaxCPUSeconds: 3600,
		},
		ValConfig:    defaultHLValConfig(),
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 2.0},
	})

	generateLeafWUs(t, env, gpuLeaf.ID, 2)

	// Register browser volunteer with GPU capabilities.
	bc := newBrowserClient(env.httpURL)
	volID := bc.register(t, []string{"WASM"}, true, []string{"WEBGPU"})
	if volID == "" {
		t.Fatal("GPU browser volunteer should have received an ID")
	}

	// Request work with GPU capabilities.
	wu := bc.requestWorkGPU(t, []string{gpuLeaf.ID.String()})
	if wu == nil {
		t.Fatal("GPU browser volunteer should have received work")
	}

	// Verify execution spec has GPU fields.
	if !wu.ExecutionSpec.GPURequired {
		t.Error("execution_spec.gpu_required should be true")
	}
	if wu.ExecutionSpec.Binaries["wasm"] == "" {
		t.Error("execution_spec should have wasm binary")
	}
	if wu.ExecutionSpec.Binaries["wgsl"] == "" {
		t.Error("execution_spec should have wgsl shader URL")
	}
}

// --- Scenario 4: Mixed CLI + Browser Volunteers on Same Leaf ---

func TestV093E2E_MixedCLIBrowserVolunteers(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v093-mixed")

	// Create WASM leaf with redundancy=2 for cross-validation.
	mixedLeaf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:            "Mixed Validation Leaf",
		TaskPattern:     leaf.PatternParameterSweep,
		ExecConfig: leaf.ExecutionConfig{
			Runtime:       "WASM",
			Binaries:      map[string]string{"wasm": "https://example.com/bin/module.wasm"},
			MaxMemoryMB:   2048,
			MaxDiskMB:     5120,
			MaxCPUSeconds: 1800,
		},
		ValConfig: leaf.ValidationConfig{
			RedundancyFactor:   2,
			AgreementThreshold: 1.0,
			ComparisonMode:     "EXACT",
			MaxRetries:         3,
		},
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	generateLeafWUs(t, env, mixedLeaf.ID, 1)

	output := []byte(`{"result":"cross-validated"}`)

	// CLI volunteer requests work via gRPC.
	cliPub := genVolunteerKey(t)
	cliVolID := registerHLVolunteerWithRuntimes(t, env, ctx, cliPub, "CLI Mixed Vol", []string{"NATIVE", "WASM"})
	cliWU := requestWUFromLeafs(t, env, ctx, cliVolID, cliPub, []string{mixedLeaf.ID.String()})
	if cliWU.WorkUnitId == "" {
		t.Fatal("CLI volunteer should have received work")
	}

	// CLI submits via gRPC.
	submitWUResult(t, env, ctx, cliVolID, cliPub, cliWU.WorkUnitId, output)

	// Browser volunteer registers via REST.
	bc := newBrowserClient(env.httpURL)
	browserVolID := bc.register(t, []string{"WASM"}, false, nil)
	if browserVolID == "" {
		t.Fatal("browser volunteer should have received an ID")
	}

	// The CLI volunteer's request moved the WU out of QUEUED (Alpha scheduling), so the
	// browser volunteer corroborates the same WU via a direct redundant assignment
	// rather than requestWork (which would find no QUEUED work). This mirrors the
	// redundancy setup used by the maintained alpha/beta suites.
	createRedundantAssignment(t, env.pool, ctx, cliWU.WorkUnitId, types.MustParseID(browserVolID))

	// Browser submits via REST for the same work unit.
	bc.submitResult(t, cliWU.WorkUnitId, output)

	// Verify both volunteers got credit (cross-validation worked).
	assertCreditExists(t, env.pool, ctx, types.MustParseID(cliVolID), mixedLeaf.ID, 1)
	assertCreditExists(t, env.pool, ctx, types.MustParseID(browserVolID), mixedLeaf.ID, 1)
}

// --- Scenario 5: Non-WASM Volunteer Excluded ---

func TestV093E2E_NonWASMVolunteerExcluded(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "v093-excl")

	// Create WASM-only leaf.
	wasmLeaf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:        "WASM Exclusion Leaf",
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

	// Register NATIVE-only volunteer via gRPC.
	volPub := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, volPub, "Native Only Vol")

	// Request work → should get NO_WORK_AVAILABLE (server returns NotFound).
	requestWUExpectNone(t, env, ctx, volID, volPub, []string{wasmLeaf.ID.String()})
}

// --- Scenario 6: Ed25519 Auth Verification ---

func TestV093E2E_Ed25519AuthVerification(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	// Register a valid browser volunteer.
	bc := newBrowserClient(env.httpURL)
	volID := bc.register(t, []string{"WASM"}, false, nil)
	if volID == "" {
		t.Fatal("registration should succeed")
	}

	// Subtest: valid signature → 200 or 404 (no work, but auth passes).
	t.Run("valid_signature", func(t *testing.T) {
		body := map[string]any{
			"leaf_ids":      []string{},
			"max_memory_mb": 4096,
			"max_disk_mb":   51200,
			"has_gpu":       false,
			"gpu_vendors":   []string{},
		}
		resp := bc.signedPost(t, "/api/v1/volunteers/request-work", body)
		defer resp.Body.Close()
		// 200 (got work) or 404 (no work available) — both mean auth passed.
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
			t.Errorf("valid signature: want 200 or 404, got %d", resp.StatusCode)
		}
	})

	// Subtest: wrong signature → 401.
	t.Run("wrong_signature", func(t *testing.T) {
		// Sign with a different key than the one in the auth header.
		_, wrongPriv, _ := ed25519.GenerateKey(nil)
		body := map[string]any{
			"leaf_ids":      []string{},
			"max_memory_mb": 4096,
			"max_disk_mb":   51200,
		}
		// Use bc's public key but wrong private key to sign.
		resp := signedPostWithKey(t, env.httpURL, "/api/v1/volunteers/request-work", body, bc.publicKey, wrongPriv)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("wrong signature: want 401, got %d", resp.StatusCode)
		}
	})

	// Subtest: expired timestamp → 401.
	t.Run("expired_timestamp", func(t *testing.T) {
		body := map[string]any{
			"leaf_ids":      []string{},
			"max_memory_mb": 4096,
			"max_disk_mb":   51200,
		}
		bodyBytes, _ := json.Marshal(body)
		// Use a timestamp 10 minutes in the past.
		ts := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
		bodyHash := sha256Hex(bodyBytes)
		message := fmt.Sprintf("%s:%s:%s:%s", ts, "POST", "/api/v1/volunteers/request-work", bodyHash)
		sig := ed25519.Sign(bc.privateKey, []byte(message))
		authHeader := fmt.Sprintf("Ed25519 %s:%s:%s",
			base64.RawURLEncoding.EncodeToString(bc.publicKey),
			base64.RawURLEncoding.EncodeToString(sig),
			ts)

		req, _ := http.NewRequest("POST", env.httpURL+"/api/v1/volunteers/request-work",
			bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", authHeader)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("expired timestamp request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expired timestamp: want 401, got %d", resp.StatusCode)
		}
	})

	// Subtest: no auth header → 401.
	t.Run("no_auth_header", func(t *testing.T) {
		body := map[string]any{
			"leaf_ids":      []string{},
			"max_memory_mb": 4096,
			"max_disk_mb":   51200,
		}
		bodyBytes, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", env.httpURL+"/api/v1/volunteers/request-work",
			bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		// Deliberately omit Authorization header.
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("no auth header request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no auth header: want 401, got %d", resp.StatusCode)
		}
	})
}
