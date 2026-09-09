package management

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-80, management half: while a container engine that was in service is
// not answering, GET /api/v1/container-runtime reports `unreachable` with the
// transport error and the engine it was reached through, and says the daemon
// is re-probing — not `running` (the registered runtime's answer before the
// outage took it out of service) and not `not_installed` (what an
// unregistered runtime answered before this).

func TestTB80_StatusReportsUnreachableDuringOutageAndRunningAfter(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := daemon.NewRuntimeRegistry()
	cr := runtime.NewContainerRuntimeWithClient(t.TempDir(), logger, nil)
	cr.SetBackend(runtime.BackendPodman)
	registry.Register(cr)

	engineUp := true
	factory := func(cfg *config.Config, logger *slog.Logger) *daemon.ContainerRuntimeFactory {
		return daemon.NewContainerRuntimeFactoryForTest(cfg, logger,
			func(runtime.ContainerBackend) runtime.BackendInfo {
				if !engineUp {
					return runtime.BackendInfo{Backend: runtime.BackendNone}
				}
				// A Docker-compatible socket served by Podman (podman-mac-helper,
				// Podman Desktop's compatibility): the Docker probe, so the
				// production factory creates no Podman machine manager here —
				// on this test host that would drive a real `podman machine`.
				return runtime.BackendInfo{Backend: runtime.BackendDocker, Engine: "podman", Version: "5.3.1", SocketPath: "/run/user/1000/podman/podman.sock"}
			},
			func(runtime.BackendInfo) (runtime.Runtime, error) {
				fresh := runtime.NewContainerRuntimeWithClient(t.TempDir(), logger, nil)
				fresh.SetBackend(runtime.BackendDocker)
				return fresh, nil
			})
	}
	env := tb59Env(t, registry, factory, []string{"CONTAINER"})

	resp := env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	before := decodeJSON(t, resp)
	if before["status"] != "running" {
		t.Fatalf("status before the outage = %v, want running", before["status"])
	}

	// The engine stops answering under the registered runtime.
	outage := &runtime.EngineUnreachableError{Backend: runtime.BackendPodman, Socket: "/run/user/1000/podman/podman.sock",
		Err: errors.New("docker ping: Cannot connect to the Docker daemon at unix:///run/user/1000/podman/podman.sock. Is the docker daemon running?")}
	if !env.daemon.NoteContainerEngineUnreachable(cr, outage) {
		t.Fatal("NoteContainerEngineUnreachable did not take the registered runtime out of service")
	}
	engineUp = false

	resp = env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	during := decodeJSON(t, resp)
	if during["status"] != "unreachable" {
		t.Errorf("status during the outage = %v, want unreachable (pre-fix: not_installed, the answer for any unregistered runtime)", during["status"])
	}
	if during["backend"] != "podman" {
		t.Errorf("backend during the outage = %v, want podman (the engine that stopped answering)", during["backend"])
	}
	errText, _ := during["error"].(string)
	if errText == "" || !strings.Contains(errText, "Cannot connect to the Docker daemon") {
		t.Errorf("error during the outage = %q, want the transport error the engine produced", errText)
	}
	if during["redetecting"] != true {
		t.Errorf("redetecting during the outage = %v, want true: the daemon probes the engine every minute", during["redetecting"])
	}

	// The engine answers again: a probe registers a fresh runtime and the
	// route reports running.
	engineUp = true
	if !env.daemon.RedetectContainerRuntime(t.Context(), false) {
		t.Fatal("RedetectContainerRuntime = false after the engine came back")
	}
	resp = env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	after := decodeJSON(t, resp)
	if after["status"] != "running" {
		t.Errorf("status after recovery = %v, want running", after["status"])
	}
	if after["backend"] != "docker" || after["engine"] != "podman" {
		t.Errorf("backend/engine after recovery = %v/%v, want docker/podman (what the probe found)", after["backend"], after["engine"])
	}
	if after["error"] != nil {
		t.Errorf("error after recovery = %v, want none", after["error"])
	}
}
