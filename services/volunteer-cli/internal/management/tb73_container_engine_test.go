package management

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TestTB73_StatusNamesTheEngineBehindTheDockerSocket: a Podman engine reached
// through the Docker-compatible socket registers as backend "docker" (the
// probe that found it), and the status route now says which engine answers,
// so the app's runtime card can stop calling it Docker and stop advising the
// install of the Podman that is already running (TB-73). The daemon has known
// the engine since TB-54; the route dropped it.
func TestTB73_StatusNamesTheEngineBehindTheDockerSocket(t *testing.T) {
	registry := daemon.NewRuntimeRegistry()
	engine := &tb59Switchable{up: true}
	env := tb59Env(t, registry, engine.factory, []string{"CONTAINER"})
	if !env.daemon.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}

	resp := env.doRequest(t, "GET", "/api/v1/container-runtime", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	result := decodeJSON(t, resp)
	if result["backend"] != "docker" {
		t.Fatalf("backend = %v, want docker (the socket the probe found)", result["backend"])
	}
	if result["engine"] != "podman" {
		t.Errorf("engine = %v, want podman: the engine answering the Docker-compatible socket (TB-73)", result["engine"])
	}
}

// TestTB73_StatusEngineIsEmptyWhenUnknown: a runtime registered without the
// detector has no engine to name; the field is present and empty rather than
// a guess.
func TestTB73_StatusEngineIsEmptyWhenUnknown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	registry := daemon.NewRuntimeRegistry()
	cr := runtime.NewContainerRuntimeWithClient(t.TempDir(), logger, nil)
	cr.SetBackend(runtime.BackendDocker)
	registry.Register(cr)

	env := tb59Env(t, registry, nil, nil)
	result := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/container-runtime", ""))
	engine, present := result["engine"]
	if !present {
		t.Fatal("engine field missing from the status route")
	}
	if engine != "" {
		t.Errorf("engine = %v, want \"\" for a runtime whose socket was never asked", engine)
	}
}
