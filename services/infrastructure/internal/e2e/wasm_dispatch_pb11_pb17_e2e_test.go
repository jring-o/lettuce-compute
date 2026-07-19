//go:build integration

package e2e_test

// PB-11 / PB-17 regression tests (Phase 3 local campaign): WASM dispatch to CLI
// volunteers through the dispatch cache, and artifact-version consistency on the
// browser immediate-assign path. Differential: this file uses only pre-fix test
// helpers, so it can be dropped onto the pre-fix tree and demonstrably FAILS there.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestWasmE2E_CacheDispatchesToCLIVolunteer (PB-11): a gRPC volunteer advertising
// WASM must receive WASM work through the production dispatch path — the in-process
// dispatch cache. Pre-fix the cache's dispatchable SQL excluded WASM leafs ("WASM is
// dispatched by the immediate-assign browser path, not the cache") while gRPC served
// ONLY from the cache, so the CLI's entire wazero/WASI runtime was unreachable: the
// CLI advertised WASM, doctor/readiness counted WASM leafs eligible, and the units
// sat QUEUED forever ("refill: nothing dispatchable" / "no work for leaf"). The
// pre-existing TestWasmE2E_LeafCreationAndExecution never caught it because it runs
// WITHOUT the cache, on the Layer-1 fallback path production does not use.
func TestWasmE2E_CacheDispatchesToCLIVolunteer(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "wasm-cache-dispatch")
	wasmLeaf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:        "WASM Cache Dispatch Leaf",
		TaskPattern: leaf.PatternParameterSweep,
		ExecConfig: leaf.ExecutionConfig{
			Runtime:       "WASM",
			Binaries:      map[string]string{"wasm": "https://example.com/bin/module.wasm"},
			MaxMemoryMB:   2048,
			MaxDiskMB:     5120,
			MaxCPUSeconds: 1800,
		},
		ValConfig:    defaultHLValConfig(), // redundancy 1: one result validates
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})
	generateLeafWUs(t, env, wasmLeaf.ID, 2)

	pubKey := genVolunteerKey(t)
	volID := registerHLVolunteerWithRuntimes(t, env, ctx, pubKey, "WASM Cache Vol", []string{"WASM"})

	// Poll the cache-backed gRPC dispatch. The refiller ticks every 250ms, so 15s is
	// generous; pre-fix the WASM leaf never stages and this loop times out empty.
	var assignment *lettucev1.WorkUnitAssignment
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    volID,
			PublicKey:      pubKey,
			LeafIds:        []string{wasmLeaf.ID.String()},
			MaxAssignments: 1,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit: %v", err)
		}
		if len(resp.Assignments) > 0 {
			assignment = resp.Assignments[0]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if assignment == nil {
		t.Fatal("cache never dispatched a WASM unit to a WASM-advertising gRPC volunteer: the CLI's WASI runtime is unreachable (PB-11 — the advertise-but-never-dispatch lie)")
	}
	if assignment.Runtime != "WASM" {
		t.Fatalf("assignment runtime = %q, want WASM", assignment.Runtime)
	}
	if es := assignment.GetExecutionSpec(); es == nil || es.Binaries["wasm"] == "" {
		t.Fatal("assignment execution_spec lacks the wasm binary URL the CLI runtime selects on")
	}

	// The full loop must work exactly like every other cache dispatch: run-start,
	// submit, validate.
	swResp, err := env.grpc.StartWork(signFor(t, ctx, pubKey), &lettucev1.StartWorkRequest{
		WorkUnitId: assignment.WorkUnitId, VolunteerId: volID,
	})
	if err != nil {
		t.Fatalf("StartWork: %v", err)
	}
	if !swResp.Ok {
		t.Fatalf("StartWork on the cache-dispatched WASM unit returned ok=false (%q)", swResp.Message)
	}
	out := []byte(`{"result":"wasm_cache_ok"}`)
	if _, err := env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: assignment.WorkUnitId, VolunteerId: volID, PublicKey: pubKey,
		OutputData: out, OutputChecksumSha256: sha256Hex(out),
		Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1},
	}); err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if st := pollWorkUnitState(t, ctx, env, assignment.WorkUnitId, "VALIDATED", 10*time.Second); st != "VALIDATED" {
		t.Fatalf("cache-dispatched WASM unit state = %q, want VALIDATED", st)
	}
}

// TestWasmE2E_CacheStillExcludesNonWasmVolunteer (PB-11 control): a NATIVE-only
// volunteer must NOT receive WASM work from the cache — the runtime capability gate,
// not the old blanket exclusion, is what scopes WASM dispatch.
func TestWasmE2E_CacheStillExcludesNonWasmVolunteer(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "wasm-cache-excl")
	wasmLeaf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:        "WASM Cache Exclusion Leaf",
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
	generateLeafWUs(t, env, wasmLeaf.ID, 2)

	pubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pubKey, "Native Only Cache Vol") // NATIVE only

	// Give the refiller ample time to stage the WASM units, then assert the
	// NATIVE-only volunteer still gets nothing across several polls.
	time.Sleep(1 * time.Second)
	for i := 0; i < 5; i++ {
		requestWUExpectNone(t, env, ctx, volID, pubKey, []string{wasmLeaf.ID.String()})
		time.Sleep(100 * time.Millisecond)
	}
}

// TestBrowserRequestWork_ServesCurrentArtifactConfig (PB-17): the browser
// immediate-assign response must be internally consistent and current. Pre-fix it
// mixed the GENERATION-time artifact reference (code_artifact_url =
// wu.CodeArtifactRef) with CURRENT-config execution_spec.binaries, so every
// already-QUEUED browser/WASM unit kept serving the OLD artifact URL forever after
// the leaf's artifact was updated (observed live: units generated under a dead
// tunnel URL kept handing that dead URL after the config moved on).
func TestBrowserRequestWork_ServesCurrentArtifactConfig(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		urlA = "https://example.com/bin/module-v1.wasm"
		urlB = "https://example.com/bin/module-v2.wasm"
	)

	userID := createTestUser(t, env.pool, ctx, "browser-current-artifact")
	wasmLeaf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name:        "Browser Current Artifact Leaf",
		TaskPattern: leaf.PatternParameterSweep,
		ExecConfig: leaf.ExecutionConfig{
			Runtime:       "WASM",
			Binaries:      map[string]string{"wasm": urlA},
			MaxMemoryMB:   2048,
			MaxDiskMB:     5120,
			MaxCPUSeconds: 1800,
		},
		ValConfig:    defaultHLValConfig(),
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})
	// Units generated NOW stamp code_artifact_ref = urlA (generation-time).
	generateLeafWUs(t, env, wasmLeaf.ID, 2)

	// The leaf's artifact moves on (the E9 no-restart artifact-update flow).
	newEC := leaf.ExecutionConfig{
		Runtime:       "WASM",
		Binaries:      map[string]string{"wasm": urlB},
		MaxMemoryMB:   2048,
		MaxDiskMB:     5120,
		MaxCPUSeconds: 1800,
	}
	resp := httpReq(t, "PUT", env.httpURL+"/api/v1/leafs/"+wasmLeaf.ID.String(),
		leaf.UpdateLeafRequest{ExecutionConfig: &newEC})
	requireStatus(t, resp, http.StatusOK, "update leaf artifact")
	resp.Body.Close()

	// A browser volunteer picks up one of the ALREADY-QUEUED units: every artifact
	// field of the response must carry the CURRENT config, and agree.
	bc := newBrowserClient(env.httpURL)
	bc.register(t, []string{"WASM"}, false, nil)

	body := map[string]any{
		"leaf_ids":      []string{wasmLeaf.ID.String()},
		"max_memory_mb": 4096,
		"max_disk_mb":   51200,
		"has_gpu":       false,
		"gpu_vendors":   []string{},
	}
	workResp := bc.signedPost(t, "/api/v1/volunteers/request-work", body)
	defer workResp.Body.Close()
	if workResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(workResp.Body)
		t.Fatalf("request-work failed: status=%d body=%s", workResp.StatusCode, b)
	}
	var wr struct {
		WorkUnitID      string `json:"work_unit_id"`
		CodeArtifactURL string `json:"code_artifact_url"`
		ExecutionSpec   struct {
			Binaries map[string]string `json:"binaries"`
		} `json:"execution_spec"`
	}
	if err := json.NewDecoder(workResp.Body).Decode(&wr); err != nil {
		t.Fatalf("decode request-work response: %v", err)
	}

	if wr.ExecutionSpec.Binaries["wasm"] != urlB {
		t.Fatalf("execution_spec.binaries[wasm] = %q, want the current %q", wr.ExecutionSpec.Binaries["wasm"], urlB)
	}
	if wr.CodeArtifactURL != urlB {
		t.Fatalf("code_artifact_url = %q (the generation-time artifact), want the current %q: queued browser/WASM units serve stale artifacts forever after a publish/rollback (PB-17)", wr.CodeArtifactURL, urlB)
	}
}
