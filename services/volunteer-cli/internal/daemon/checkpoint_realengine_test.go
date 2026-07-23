package daemon

import (
	"archive/tar"
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

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// PB-29 regression coverage against a REAL container engine, extending the
// LETTUCE_TEST_REAL_ENGINE=1 suite from the PB-23 fix (see
// runtime/container_realengine_test.go for why a mocked engine cannot catch
// this class: no mock enforces the hardened container's non-root user against
// host file modes; and Windows drvfs binds mask modes entirely, so a Windows
// host run is NOT evidence — run inside the podman machine or on a Linux
// host).
//
// The scenario is cross-volunteer checkpoint reassignment: a checkpointing
// CONTAINER unit dies on volunteer A, the head hands it to volunteer B, and
// B's daemon downloads and extracts A's checkpoint (extractTar — the exact
// call restoreCheckpoint makes) into the fresh work dir. The resumed leaf,
// running hardened as 65534:65534, must be able to OVERWRITE the restored
// files in place. Pre-fix, extractTar restored them volunteer-owned
// 0644/0755 and the leaf died with EACCES on its first checkpoint write —
// PB-23's fix covered only the top-level dirs, not restored contents.

const realEngineEnv = "LETTUCE_TEST_REAL_ENGINE"

// realEngineExecutors re-enables real command execution (TestMain blocks it)
// for the duration of one gated test.
func realEngineExecutors(t *testing.T) {
	t.Helper()
	oldExec, oldExecCtx := runtime.CommandExecutor, runtime.CommandExecutorCtx
	runtime.CommandExecutor = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
	runtime.CommandExecutorCtx = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	t.Cleanup(func() {
		runtime.CommandExecutor, runtime.CommandExecutorCtx = oldExec, oldExecCtx
	})
}

// buildRealEngineImage builds a throwaway local image whose CMD is cmd (shell
// form), returning the image ID. Engine-CLI plumbing, mirroring the PB-23
// real-engine harness.
func buildRealEngineImage(t *testing.T, cmd string) string {
	t.Helper()
	engineBin := "docker"
	if backend := runtime.DetectContainerBackend(runtime.BundledPodmanPath()); backend.Backend == runtime.BackendPodman {
		engineBin = backend.BinaryPath
	}

	dir := t.TempDir()
	dockerfile := "FROM docker.io/library/alpine:3.20\nCMD [\"/bin/sh\", \"-c\", " + fmt.Sprintf("%q", cmd) + "]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}

	tag := "lettuce-pb29-e2e:test"
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

// TestRealEngine_ReassignedCheckpointOverwritableByHardenedContainer drives
// the reassignment-restore path end to end: extractTar restores a foreign
// checkpoint into the prepared work dir, then the hardened container
// overwrites BOTH restored files in place (a top-level checkpoint.dat and a
// nested state file) and performs the leaf contract's final output write.
func TestRealEngine_ReassignedCheckpointOverwritableByHardenedContainer(t *testing.T) {
	if os.Getenv(realEngineEnv) == "" {
		t.Skipf("real-engine test: set %s=1 (requires a working podman or docker)", realEngineEnv)
	}
	realEngineExecutors(t)

	backend := runtime.DetectContainerBackend(runtime.BundledPodmanPath())
	if backend.Backend == runtime.BackendNone {
		t.Skip("no container backend detected on this host")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cr, err := runtime.NewContainerRuntimeForBackend(t.TempDir(), logger, backend)
	if err != nil {
		t.Skipf("container backend %s detected but not initializable: %v", backend.Backend, err)
	}
	t.Cleanup(func() { cr.Client().Close() })

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pingCancel()
	if err := cr.Client().Ping(pingCtx); err != nil {
		t.Skipf("container backend %s socket not reachable: %v", backend.Backend, err)
	}

	// The leaf: resume from the restored checkpoint by OVERWRITING it in place
	// (the standard checkpoint contract), then write the final output.
	imageID := buildRealEngineImage(t,
		`echo seq=8 > /work/checkpoint/checkpoint.dat && echo inner2 > /work/checkpoint/state/nested.dat && echo '{"result": 29}' > "$LETTUCE_OUTPUT_DIR/output.json"`)

	wu := &runtime.WorkUnit{
		ID:      "5b1c7e9a-3d2f-4b8c-9e0d-6f5a4b3c2d1e",
		LeafID:  "5b1c7e9a-3d2f-4b8c-9e0d-6f5a4b3c2d1f",
		Runtime: "container",
		ExecutionSpec: runtime.ExecutionSpec{
			Image: imageID,
		},
		HasCheckpoint:             true,
		CheckpointSequence:        7,
		CheckpointIntervalSeconds: 30,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	prep, err := cr.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)

	// Restore the foreign volunteer's checkpoint exactly as restoreCheckpoint
	// does: extractTar of the head-downloaded blob into {workDir}/checkpoint.
	// The blob's entries carry the restrictive modes the ORIGINAL volunteer's
	// leaf wrote them with.
	var blob bytes.Buffer
	tw := tar.NewWriter(&blob)
	for _, e := range []struct {
		name     string
		typeflag byte
		mode     int64
		body     string
	}{
		{name: "checkpoint.dat", typeflag: tar.TypeReg, mode: 0o644, body: "seq=7\n"},
		{name: "state", typeflag: tar.TypeDir, mode: 0o755},
		{name: "state/nested.dat", typeflag: tar.TypeReg, mode: 0o600, body: "inner\n"},
	} {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: e.mode, Size: int64(len(e.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	checkpointDir := filepath.Join(prep.WorkDir, "checkpoint")
	if err := extractTar(blob.Bytes(), checkpointDir); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	result, err := cr.Execute(ctx, wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("hardened container exited %d overwriting a restored checkpoint in place (PB-29); execution.log:\n%s",
			result.ExitCode, realEngineLogTail(filepath.Join(prep.WorkDir, "execution.log")))
	}
	ckpt, err := os.ReadFile(filepath.Join(checkpointDir, "checkpoint.dat"))
	if err != nil {
		t.Fatalf("read overwritten checkpoint: %v", err)
	}
	if !strings.Contains(string(ckpt), "seq=8") {
		t.Errorf("checkpoint.dat = %q, want the container's in-place overwrite (seq=8)", ckpt)
	}
	nested, err := os.ReadFile(filepath.Join(checkpointDir, "state", "nested.dat"))
	if err != nil {
		t.Fatalf("read overwritten nested state: %v", err)
	}
	if !strings.Contains(string(nested), "inner2") {
		t.Errorf("state/nested.dat = %q, want the container's in-place overwrite (inner2)", nested)
	}
	if !strings.Contains(string(result.OutputData), "29") {
		t.Errorf("output not captured; got %q", result.OutputData)
	}
}

// realEngineLogTail returns up to the last 4 KB of a file for diagnostics.
func realEngineLogTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no captured container log)"
	}
	if len(data) > 4096 {
		data = data[len(data)-4096:]
	}
	return string(data)
}
