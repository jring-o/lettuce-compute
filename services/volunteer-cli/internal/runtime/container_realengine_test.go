package runtime

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PB-23 regression coverage against a REAL container engine.
//
// The mocked client in container_runtime_e2e_test.go is exactly why CI missed
// PB-23: no mocked engine ever enforces the hardened container's non-root user
// against the bind-mounted output dir's host permissions. These tests run the
// production ContainerRuntime — hardening included — against a real
// podman/docker, building a throwaway local image whose entrypoint performs the
// leaf contract's final output write.
//
// They are GATED: set LETTUCE_TEST_REAL_ENGINE=1 to run (CI has no engine; the
// implement/closeout sessions run them locally — note the bug itself only
// manifests where bind mounts preserve POSIX ownership, i.e. a Linux host or
// inside the podman machine VM; Windows drvfs binds mask everything 0777).
const realEngineEnv = "LETTUCE_TEST_REAL_ENGINE"

// realEngineExecutors re-enables real command execution (TestMain blocks it)
// for the duration of one gated test.
func realEngineExecutors(t *testing.T) {
	t.Helper()
	oldExec, oldExecCtx := CommandExecutor, CommandExecutorCtx
	CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
	CommandExecutorCtx = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	t.Cleanup(func() {
		CommandExecutor, CommandExecutorCtx = oldExec, oldExecCtx
	})
}

// newRealEngineRuntime detects the real backend and constructs the production
// runtime over it, logging to logBuf when non-nil (so a test can assert on the
// daemon-side WARN content).
func newRealEngineRuntime(t *testing.T, logBuf *bytes.Buffer) *ContainerRuntime {
	t.Helper()
	if os.Getenv(realEngineEnv) == "" {
		t.Skipf("real-engine test: set %s=1 (requires a working podman or docker)", realEngineEnv)
	}
	realEngineExecutors(t)

	backend := DetectContainerBackend(BundledPodmanPath())
	if backend.Backend == BackendNone {
		t.Skip("no container backend detected on this host")
	}

	var handler slog.Handler
	if logBuf != nil {
		handler = slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	} else {
		handler = slog.NewTextHandler(os.Stderr, nil)
	}
	cr, err := NewContainerRuntimeForBackend(t.TempDir(), slog.New(handler), backend)
	if err != nil {
		t.Skipf("container backend %s detected but not initializable: %v", backend.Backend, err)
	}
	t.Cleanup(func() { cr.Client().Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cr.Client().Ping(ctx); err != nil {
		t.Skipf("container backend %s socket not reachable: %v", backend.Backend, err)
	}
	return cr
}

// buildRealEngineImage builds a throwaway local image whose CMD is cmd (shell
// form), returning the image ID (content-addressed, so the runtime's tag-pull
// logic falls back to the local copy). Uses the engine CLI directly — this is
// harness plumbing, not product surface.
func buildRealEngineImage(t *testing.T, cmd string) string {
	t.Helper()
	engineBin := "docker"
	if backend := DetectContainerBackend(BundledPodmanPath()); backend.Backend == BackendPodman {
		engineBin = backend.BinaryPath
	}

	dir := t.TempDir()
	dockerfile := "FROM docker.io/library/alpine:3.20\nCMD [\"/bin/sh\", \"-c\", " + fmt.Sprintf("%q", cmd) + "]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}

	tag := "lettuce-pb23-e2e:test"
	out, err := exec.Command(engineBin, "build", "-t", tag, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("building test image: %v\n%s", err, out)
	}
	idOut, err := exec.Command(engineBin, "image", "inspect", "--format", "{{.Id}}", tag).CombinedOutput()
	if err != nil {
		t.Fatalf("inspecting test image: %v\n%s", err, idOut)
	}
	return strings.TrimSpace(string(idOut))
}

// TestRealEngine_HardenedCPUContainerCanWriteOutput is the PB-23 regression
// test: a CPU leaf under the full hardened posture (read-only rootfs, all
// capabilities dropped, non-root 65534:65534 user) must be able to perform the
// leaf contract's final write to $LETTUCE_OUTPUT_DIR. Pre-fix, the bind-mounted
// output dir was 0o755 owned by the volunteer's uid, so this exact write failed
// EACCES and every real CPU container unit exited 1.
func TestRealEngine_HardenedCPUContainerCanWriteOutput(t *testing.T) {
	cr := newRealEngineRuntime(t, nil)
	imageID := buildRealEngineImage(t, `echo '{"result": 42}' > "$LETTUCE_OUTPUT_DIR/output.json"`)

	wu := &WorkUnit{
		ID:      "7d4a2f10-9a1b-4c3d-8e5f-aa0102030405",
		LeafID:  "7d4a2f10-9a1b-4c3d-8e5f-aa0102030406",
		Runtime: "container",
		ExecutionSpec: ExecutionSpec{
			Image: imageID,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	prep, err := cr.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	result, err := cr.Execute(ctx, wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("hardened container exited %d — the leaf contract's output write failed (PB-23); execution.log:\n%s",
			result.ExitCode, tailOfFile(filepath.Join(prep.WorkDir, "execution.log"), containerLogTailBytes))
	}
	if !strings.Contains(string(result.OutputData), "42") {
		t.Fatalf("output not captured; got %q", result.OutputData)
	}
}

// TestRealEngine_NonZeroExitSurfacesLogTail covers PB-23's diagnosability half:
// when the container exits non-zero, the runtime must surface a bounded tail of
// the container's stdout/stderr in the daemon log — the bare exit code told a
// leaf author nothing.
func TestRealEngine_NonZeroExitSurfacesLogTail(t *testing.T) {
	var logBuf bytes.Buffer
	cr := newRealEngineRuntime(t, &logBuf)
	imageID := buildRealEngineImage(t, `echo pb23-stderr-marker >&2; exit 3`)

	wu := &WorkUnit{
		ID:      "7d4a2f10-9a1b-4c3d-8e5f-aa0102030407",
		LeafID:  "7d4a2f10-9a1b-4c3d-8e5f-aa0102030408",
		Runtime: "container",
		ExecutionSpec: ExecutionSpec{
			Image: imageID,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	prep, err := cr.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	result, err := cr.Execute(ctx, wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", result.ExitCode)
	}
	if !strings.Contains(logBuf.String(), "pb23-stderr-marker") {
		t.Fatalf("container stderr not surfaced in the daemon log on non-zero exit; log:\n%s", logBuf.String())
	}
}
