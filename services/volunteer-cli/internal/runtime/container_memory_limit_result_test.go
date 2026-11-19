package runtime

import (
	"context"
	"testing"
)

// TestContainerExecute_ReportsTheMemoryLimitTheContainerWasCreatedWith: the
// result carries the memory limit the container was created with, which is
// what a kill at exit 137 is explained by. Lowering the allowance while the
// container runs does not change it, and a declaration above the allowance at
// start reports the clamped figure the engine was given.
func TestContainerExecute_ReportsTheMemoryLimitTheContainerWasCreatedWith(t *testing.T) {
	var cr *ContainerRuntime
	mock := &MockDockerClient{
		ContainerWaitFn: func(context.Context, string) (int64, error) {
			// The volunteer lowers the allowance while the container runs, and
			// the container is then killed at the limit it was created with.
			cr.SetMemoryCeilingMB(6000)
			return 137, nil
		},
	}
	cr, _ = newTestContainerRuntime(t, mock)

	run := func(id string, declaredMB int32, ceilingMB int) (created, reported int) {
		t.Helper()
		cr.SetMemoryCeilingMB(ceilingMB)
		wu := &WorkUnit{ID: id, ExecutionSpec: ExecutionSpec{Image: "alpine:latest", MaxMemoryMB: declaredMB}}
		prep, err := cr.Prepare(context.Background(), wu)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		defer cr.Cleanup(prep)
		res, err := cr.Execute(context.Background(), wu, prep)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.ExitCode != 137 {
			t.Fatalf("exit code %d, want the 137 the engine reported", res.ExitCode)
		}
		return int(mock.LastCreateConfig.MemoryBytes / (1024 * 1024)), res.MemoryLimitMB
	}

	if created, reported := run("0d6f3f8e-1c1a-4b9e-9a51-0b7d3c2e4f10", 7000, 463872); created != 7000 || reported != 7000 {
		t.Errorf("7000 MB leaf on a 463872 MB allowance: created with %d MB, result reports %d MB; want 7000 for both, though the allowance fell to 6000 during the run", created, reported)
	}
	if created, reported := run("0d6f3f8e-1c1a-4b9e-9a51-0b7d3c2e4f11", 7000, 4096); created != 4096 || reported != 4096 {
		t.Errorf("7000 MB leaf on a 4096 MB allowance: created with %d MB, result reports %d MB; want the clamped 4096 for both", created, reported)
	}
	if created, reported := run("0d6f3f8e-1c1a-4b9e-9a51-0b7d3c2e4f12", 0, 463872); created != DefaultPerTaskMemMB || reported != DefaultPerTaskMemMB {
		t.Errorf("leaf declaring no memory: created with %d MB, result reports %d MB; want the default %d for both", created, reported, DefaultPerTaskMemMB)
	}
}
