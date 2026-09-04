package management

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-59, management half: GET /api/v1/container-runtime must report what the
// daemon actually runs on, and POST /api/v1/container-runtime/redetect must
// queue a probe for an engine.

// tb59Env builds a management env around a daemon with the given registry and
// detector. cfg.ContainerBackend is left EMPTY — the auto-configured host the
// reproduction used, on which the old status route answered from the config
// preference alone.
func tb59Env(t *testing.T, registry *daemon.RuntimeRegistry, factory func(*config.Config, *slog.Logger) *daemon.ContainerRuntimeFactory, trusted []string) *testEnv {
	t.Helper()
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.Defaults()
	cfg.DataDir = tmpDir
	cfg.Servers = []config.ServerConfig{{GRPCAddress: "localhost:50051", Name: "test-server", TrustedRuntimes: trusted}}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("save config: %v", err)
	}

	dc := daemon.DaemonConfig{Config: cfg, Logger: logger, RuntimeRegistry: registry}
	if factory != nil {
		dc.ContainerFactory = factory(cfg, logger)
	}
	d := daemon.NewDaemon(dc)

	bridge := NewDaemonBridge(d, cfgPath)
	srv := NewServer(tmpDir, logger)
	if err := srv.Start(bridge); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return &testEnv{server: srv, bridge: bridge, daemon: d, dataDir: tmpDir, cfgPath: cfgPath,
		baseURL: "http://127.0.0.1:" + itoa(srv.Port()), token: srv.Token()}
}

// TestTB59_StatusReportsRegisteredRuntimeNotConfigPreference is the side catch
// from the reproduction: with `container_backend` empty (auto) and a Docker-
// backed container runtime registered and in use, the route answered
// `backend: none, status: not_installed`, which is what the app's runtime page
// showed beside running containers. The registered runtime is the truth.
func TestTB59_StatusReportsRegisteredRuntimeNotConfigPreference(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := daemon.NewRuntimeRegistry()
	cr := runtime.NewContainerRuntimeWithClient(t.TempDir(), logger, nil)
	cr.SetBackend(runtime.BackendDocker)
	registry.Register(cr)

	env := tb59Env(t, registry, nil, nil)
	resp := env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	result := decodeJSON(t, resp)
	if result["backend"] != "docker" {
		t.Errorf("backend = %v, want docker (the registered runtime), not the empty config preference", result["backend"])
	}
	if result["status"] != "running" {
		t.Errorf("status = %v, want running for a registered Docker runtime", result["status"])
	}
}

// tb59Switchable is an engine the detection seam can turn on.
type tb59Switchable struct {
	mu sync.Mutex
	up bool
}

func (e *tb59Switchable) factory(cfg *config.Config, logger *slog.Logger) *daemon.ContainerRuntimeFactory {
	return daemon.NewContainerRuntimeFactoryForTest(cfg, logger,
		func(runtime.ContainerBackend) runtime.BackendInfo {
			e.mu.Lock()
			defer e.mu.Unlock()
			if !e.up {
				return runtime.BackendInfo{Backend: runtime.BackendNone}
			}
			return runtime.BackendInfo{Backend: runtime.BackendDocker, Engine: "podman", Version: "5.3.1"}
		},
		func(runtime.BackendInfo) (runtime.Runtime, error) {
			cr := runtime.NewContainerRuntimeWithClient(cfg.DataDir, logger, nil)
			cr.SetBackend(runtime.BackendDocker)
			return cr, nil
		})
}

// TestTB59_RedetectRouteAndStatusLifecycle: a daemon that found no engine at
// start reports not_installed WITH redetecting=true (a head is trusted for
// containers, so it keeps probing); the redetect verb queues a probe (202);
// once an engine is detected and registered the status reports it and the
// verb answers 409 ALREADY_REGISTERED.
func TestTB59_RedetectRouteAndStatusLifecycle(t *testing.T) {
	registry := daemon.NewRuntimeRegistry()
	engine := &tb59Switchable{}
	env := tb59Env(t, registry, engine.factory, []string{"CONTAINER"})

	resp := env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	result := decodeJSON(t, resp)
	if result["backend"] != "none" || result["status"] != "not_installed" {
		t.Fatalf("before any engine: backend=%v status=%v, want none/not_installed", result["backend"], result["status"])
	}
	if result["redetecting"] != true {
		t.Errorf("redetecting = %v, want true: no runtime, a head trusted for CONTAINER", result["redetecting"])
	}

	resp = env.doRequest(t, "POST", "/api/v1/container-runtime/redetect", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("redetect: expected 202, got %d", resp.StatusCode)
	}
	if body := decodeJSON(t, resp); body["status"] != "checking" {
		t.Errorf("redetect body = %v, want status checking", body)
	}

	// The engine comes up; one attempt registers it (the loop is not running
	// in this test, so drive the attempt directly).
	engine.mu.Lock()
	engine.up = true
	engine.mu.Unlock()
	if !env.daemon.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}

	resp = env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	result = decodeJSON(t, resp)
	if result["backend"] != "docker" || result["status"] != "running" {
		t.Errorf("after detection: backend=%v status=%v, want docker/running", result["backend"], result["status"])
	}
	if result["version"] != "5.3.1" {
		t.Errorf("version = %v, want the detected 5.3.1", result["version"])
	}
	if result["redetecting"] != false {
		t.Errorf("redetecting = %v after registration, want false", result["redetecting"])
	}

	resp = env.doRequest(t, "POST", "/api/v1/container-runtime/redetect", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("redetect after registration: expected 409, got %d", resp.StatusCode)
	}
	if code := errorCode(decodeJSON(t, resp)); code != "ALREADY_REGISTERED" {
		t.Errorf("error code = %v, want ALREADY_REGISTERED", code)
	}
}

// TestTB59_RedetectRouteRefusedWithoutTrust: no head trusted for CONTAINER
// means start-up never probed and the loop has nothing to do; the verb says so
// instead of queueing a probe that would never run.
func TestTB59_RedetectRouteRefusedWithoutTrust(t *testing.T) {
	registry := daemon.NewRuntimeRegistry()
	engine := &tb59Switchable{}
	env := tb59Env(t, registry, engine.factory, nil)

	resp := env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	if result := decodeJSON(t, resp); result["redetecting"] != false {
		t.Errorf("redetecting = %v with no head trusted for CONTAINER, want false", result["redetecting"])
	}
	resp = env.doRequest(t, "POST", "/api/v1/container-runtime/redetect", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	if code := errorCode(decodeJSON(t, resp)); code != "NOT_TRUSTED" {
		t.Errorf("error code = %v, want NOT_TRUSTED", code)
	}
}
