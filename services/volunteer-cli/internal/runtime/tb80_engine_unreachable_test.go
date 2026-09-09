package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// TB-80, runtime half: a container engine that does not answer is reported
// as an EngineUnreachableError by every engine-facing step, and the
// classifier tells a transport failure from an engine's own error text.

// TestTB80_IsEngineConnectionError_Classification pins the boundary: the
// Docker client's connection-failed error (however deeply wrapped), an EOF
// from an engine that died mid-request, and a raw dial failure are outages;
// an error the engine ANSWERED with — a pull the registry refused, a pull
// whose text mentions a dial failure of the daemon's own — is not, nor is a
// cancellation.
func TestTB80_IsEngineConnectionError_Classification(t *testing.T) {
	connFailed := client.ErrorConnectionFailed("unix:///var/folders/82/T/podman/podman-machine-default-api.sock")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"docker client connection-failed", connFailed, true},
		{"connection-failed wrapped as the prepare path wraps it", fmt.Errorf("docker is not available: %w", fmt.Errorf("docker ping: %w", connFailed)), true},
		{"connection-failed wrapped as the execute path wraps it", fmt.Errorf("create container: %w", fmt.Errorf("container create: %w", connFailed)), true},
		{"EOF from an engine that died mid-request", fmt.Errorf("container wait: %w", io.EOF), true},
		{"unexpected EOF on a pull stream", io.ErrUnexpectedEOF, true},
		{"raw dial failure", &net.OpError{Op: "dial", Net: "unix", Err: errors.New("connect: connection refused")}, true},
		{"chain broken by %v keeps the client's wording", errors.New("prepare: Cannot connect to the Docker daemon at unix:///run/user/1000/podman/podman.sock. Is the docker daemon running?"), true},
		{"permission-denied socket", errors.New("permission denied while trying to connect to the Docker daemon socket at unix:///run/podman/podman.sock: Get \"http://%2Frun%2Fpodman%2Fpodman.sock/_ping\": dial unix /run/podman/podman.sock: connect: permission denied"), true},
		{"pull refused by the registry", errors.New("pull access denied for ghcr.io/example/img, repository does not exist or may require authorization"), false},
		{"pull failed inside the daemon: registry unresolvable", errors.New("Error response from daemon: Get \"https://ghcr.io/v2/\": dial tcp: lookup ghcr.io: no such host"), false},
		{"manifest unknown", errors.New("manifest for ghcr.io/example/img:tag not found: manifest unknown"), false},
		{"cancelled by the caller", context.Canceled, false},
		{"deadline", fmt.Errorf("docker ping: %w", context.DeadlineExceeded), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isEngineConnectionError(tc.err); got != tc.want {
			t.Errorf("%s: isEngineConnectionError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestTB80_RealDialFailureIsAnOutage runs the production Docker client
// against a socket path nothing listens on and checks the error the ping
// actually produces classifies as an outage — the reproduction's shape, no
// engine needed.
func TestTB80_RealDialFailureIsAnOutage(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nobody-home.sock")
	dc, err := NewDockerClientWrapperWithHost("unix://"+sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer dc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pingErr := dc.Ping(ctx)
	if pingErr == nil {
		t.Fatalf("ping of %s succeeded; nothing should listen there", sock)
	}
	if !isEngineConnectionError(pingErr) {
		t.Errorf("a real dial failure is not classified as an outage: %v", pingErr)
	}
}

// TestTB80_PrepareReportsEngineUnreachableWhenPingFails: Prepare's ping
// failure names the engine and the socket and is an EngineUnreachableError,
// so the fetcher can return the unit un-run instead of billing a "prepare
// failure". Pre-fix Prepare returned a plain "docker is not available"
// error the fetcher could not tell from a broken image.
func TestTB80_PrepareReportsEngineUnreachableWhenPingFails(t *testing.T) {
	connFailed := client.ErrorConnectionFailed("unix:///tmp/podman.sock")
	mock := &MockDockerClient{PingFn: func(context.Context) error { return fmt.Errorf("docker ping: %w", connFailed) }}
	cr := NewContainerRuntimeWithClient(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), mock)
	cr.SetBackend(BackendPodman)
	cr.engineSocket = "/tmp/podman.sock"

	wu := &WorkUnit{ID: "11111111-2222-4333-8444-555555555555", LeafID: "leaf-1", Runtime: "container",
		ExecutionSpec: ExecutionSpec{Image: "ghcr.io/example/img:tag"}}
	_, err := cr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("Prepare succeeded with a dead engine")
	}
	if !IsEngineUnreachable(err) {
		t.Fatalf("Prepare error is not an EngineUnreachableError: %v", err)
	}
	var eu *EngineUnreachableError
	if !errors.As(err, &eu) || eu.Backend != BackendPodman || eu.Socket != "/tmp/podman.sock" {
		t.Errorf("outage does not name the engine and socket: %+v", eu)
	}
	if !strings.Contains(err.Error(), "container engine unreachable (podman at /tmp/podman.sock)") {
		t.Errorf("abandon reason does not name the engine: %q", err.Error())
	}
	if !errors.Is(err, connFailed) {
		t.Errorf("the transport error is not in the chain: %v", err)
	}
}

// TestTB80_PrepareCancelledIsNotAnOutage: a ping cut short by the daemon's
// own cancellation (shutdown) stays the plain error it was — an outage
// notice at every quit would be noise.
func TestTB80_PrepareCancelledIsNotAnOutage(t *testing.T) {
	mock := &MockDockerClient{PingFn: func(ctx context.Context) error { return fmt.Errorf("docker ping: %w", context.Canceled) }}
	cr := NewContainerRuntimeWithClient(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), mock)
	wu := &WorkUnit{ID: "11111111-2222-4333-8444-555555555555", Runtime: "container", ExecutionSpec: ExecutionSpec{Image: "img:tag"}}
	_, err := cr.Prepare(context.Background(), wu)
	if err == nil || IsEngineUnreachable(err) {
		t.Errorf("a cancelled ping classified as an outage: %v", err)
	}
}

// TestTB80_ExecuteReportsEngineUnreachableWhenCreateIsRefused: the
// reproduction's execute-time shape — `create container: container create:
// Cannot connect to the Docker daemon at unix:///…` — is an outage, not an
// execution failure of the leaf. A create the engine ANSWERED with a refusal
// stays an ordinary execution error.
func TestTB80_ExecuteReportsEngineUnreachableWhenCreateIsRefused(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	workDir := t.TempDir()
	for _, dir := range []string{"input", "output", "checkpoint"} {
		if err := os.MkdirAll(filepath.Join(workDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wu := &WorkUnit{ID: "11111111-2222-4333-8444-555555555555", LeafID: "leaf-1", Runtime: "container",
		ExecutionSpec: ExecutionSpec{Image: "ghcr.io/example/img:tag"}}
	prep := &PrepareResult{WorkDir: workDir}

	connFailed := client.ErrorConnectionFailed("unix:///var/folders/82/T/podman/podman-machine-default-api.sock")
	dead := &MockDockerClient{ContainerCreateFn: func(context.Context, *ContainerConfig) (string, error) {
		return "", fmt.Errorf("container create: %w", connFailed)
	}}
	cr := NewContainerRuntimeWithClient(workDir, logger, dead)
	cr.SetBackend(BackendPodman)
	_, err := cr.Execute(context.Background(), wu, prep)
	if err == nil {
		t.Fatal("Execute succeeded with a dead engine")
	}
	if !IsEngineUnreachable(err) {
		t.Errorf("create-time connection refusal is not an EngineUnreachableError: %v", err)
	}

	refused := &MockDockerClient{ContainerCreateFn: func(context.Context, *ContainerConfig) (string, error) {
		return "", fmt.Errorf("container create: %w", errors.New("Error response from daemon: invalid CPU quota"))
	}}
	cr2 := NewContainerRuntimeWithClient(workDir, logger, refused)
	cr2.SetBackend(BackendPodman)
	_, err = cr2.Execute(context.Background(), wu, prep)
	if err == nil || IsEngineUnreachable(err) {
		t.Errorf("a create the engine answered with a refusal classified as an outage: %v", err)
	}
}

// TestTB80_EngineUnreachableError_Message pins the wording the head records
// as the abandon reason and the app shows.
func TestTB80_EngineUnreachableError_Message(t *testing.T) {
	e := &EngineUnreachableError{Backend: BackendDocker, Socket: "unix:///var/run/docker.sock", Err: errors.New("boom")}
	if got, want := e.Error(), "container engine unreachable (docker at unix:///var/run/docker.sock): boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	bare := &EngineUnreachableError{Err: errors.New("boom")}
	if got, want := bare.Error(), "container engine unreachable: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if IsEngineUnreachable(errors.New("something else")) {
		t.Error("an unrelated error classified as an outage")
	}
}
