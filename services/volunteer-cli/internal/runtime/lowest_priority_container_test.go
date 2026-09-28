package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Every task container runs at the lowest CPU weight the engines accept, so a
// contended machine serves its owner's programs first. Its CPU quota is kept:
// the weight decides who yields, the quota how much Lettuce may use at most.

// TestContainerIsCreatedAtTheLowestCPUWeight drives the runtime's normal create
// path over the mocked engine: the create request carries LowestCPUShares (2,
// cgroup v2 cpu.weight 1) beside the task's quota.
func TestContainerIsCreatedAtTheLowestCPUWeight(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)

	wu := &WorkUnit{ID: "5e0c1a2b-3d4e-4f60-8a7b-9c0d1e2f3a4b", ExecutionSpec: ExecutionSpec{Image: "alpine:latest"}, CPUGrant: CPUGrant{Cores: 2, BudgetCores: 4}}
	prep, err := cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)
	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)
	if _, err := cr.Execute(context.Background(), wu, prep); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cfg := mock.LastCreateConfig
	if cfg.CPUShares != 2 || LowestCPUShares != 2 {
		t.Errorf("create request CPUShares = %d (LowestCPUShares %d), want 2: the container runs at the engine's default weight, level with the owner's programs", cfg.CPUShares, LowestCPUShares)
	}
	if cfg.CPUQuota != 200000 || cfg.CPUPeriod != 100000 {
		t.Errorf("CPUQuota/CPUPeriod = %d/%d, want 200000/100000: the weight must not replace the quota", cfg.CPUQuota, cfg.CPUPeriod)
	}
}

// TestContainerCreateSendsTheCPUWeightToTheEngine drives the production engine
// client against a fake Docker Engine API: the weight reaches the wire as
// HostConfig.CpuShares, and a config that sets none sends the engine default.
func TestContainerCreateSendsTheCPUWeightToTheEngine(t *testing.T) {
	var mu sync.Mutex
	var sent []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := fakeEngineVersionPrefix.ReplaceAllString(r.URL.Path, "")
		switch path {
		case "/_ping":
			w.Header().Set("Api-Version", "1.41")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		case "/containers/create":
			var body struct {
				HostConfig struct {
					CPUShares int64 `json:"CpuShares"`
					CPUQuota  int64 `json:"CpuQuota"`
				} `json:"HostConfig"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			mu.Lock()
			sent = append(sent, body.HostConfig.CPUShares)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "c1", "Warnings": []string{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dc, err := NewDockerClientWrapperWithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://"), slog.Default())
	if err != nil {
		t.Fatalf("engine client: %v", err)
	}
	defer dc.Close()

	ctx := context.Background()
	if _, err := dc.ContainerCreate(ctx, &ContainerConfig{Image: "alpine", CPUQuota: 100000, CPUPeriod: 100000, CPUShares: LowestCPUShares}); err != nil {
		t.Fatalf("create with the lowest weight: %v", err)
	}
	if _, err := dc.ContainerCreate(ctx, &ContainerConfig{Image: "alpine"}); err != nil {
		t.Fatalf("create with no weight: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[0] != 2 || sent[1] != 0 {
		t.Fatalf("HostConfig.CpuShares on the wire = %v, want [2 0] (the lowest weight, then the engine default)", sent)
	}
}
