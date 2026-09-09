package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TB-75 regression tests, runtime half: a container is created with its
// SHARE of the CPU budget (not the whole budget), is told the share through
// its environment, and can have its quota rewritten while it runs; a native
// process is told its share the same way and the limiter hooks receive the
// same grant.

// TestTB75_ContainerGetsItsShareNotTheBudget: with the daemon's grant source
// answering "1.5 of 3 cores", the container's quota is 150000/100000 and its
// environment carries LETTUCE_CPU_LIMIT=1.5 with the thread knobs at 2.
// Pre-fix the quota was max_cpu_cores × period for every container and no
// environment entry existed.
func TestTB75_ContainerGetsItsShareNotTheBudget(t *testing.T) {
	mock := &MockDockerClient{}
	cr, _ := newTestContainerRuntime(t, mock)
	cr.SetCPUGrantSource(func() CPUGrant { return CPUGrant{ShareCores: 1.5, BudgetCores: 3} })

	wu := &WorkUnit{ID: "2b1c6f3e-0d2a-4c5e-9f7b-1a2b3c4d5e6f", ExecutionSpec: ExecutionSpec{Image: "alpine:latest"}}
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
	if cfg.CPUQuota != 150000 || cfg.CPUPeriod != 100000 {
		t.Errorf("CPUQuota/CPUPeriod = %d/%d, want 150000/100000 (1.5 cores), not the 3-core budget", cfg.CPUQuota, cfg.CPUPeriod)
	}
	env := strings.Join(cfg.Env, "\n")
	for _, want := range []string{"LETTUCE_CPU_LIMIT=1.5", "OMP_NUM_THREADS=2", "OPENBLAS_NUM_THREADS=2", "MKL_NUM_THREADS=2", "NUMEXPR_MAX_THREADS=2"} {
		if !strings.Contains(env, want) {
			t.Errorf("container env lacks %q:\n%s", want, env)
		}
	}

	// The live adjustment goes through the engine's update call with the
	// new share's quota.
	if err := cr.SetContainerCPU(context.Background(), "abc123", 1); err != nil {
		t.Fatalf("SetContainerCPU: %v", err)
	}
	if len(mock.CPUUpdates) != 1 || mock.CPUUpdates[0] != (CPUUpdateCall{ContainerID: "abc123", Quota: 100000, Period: 100000}) {
		t.Errorf("ContainerUpdateCPU calls = %+v, want one call for abc123 at 100000/100000", mock.CPUUpdates)
	}

	// No CPU limit: no quota and nothing told.
	mock = &MockDockerClient{}
	cr, _ = newTestContainerRuntime(t, mock)
	prep, err = cr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)
	os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)
	if _, err := cr.Execute(context.Background(), wu, prep); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if mock.LastCreateConfig.CPUQuota != 0 || strings.Contains(strings.Join(mock.LastCreateConfig.Env, "\n"), "LETTUCE_CPU_LIMIT") {
		t.Errorf("with no CPU limit: quota %d, env %v", mock.LastCreateConfig.CPUQuota, mock.LastCreateConfig.Env)
	}
}

// TestTB75_NativeProcessIsToldItsShare: the native runtime passes the grant
// to the limiter hooks and into the process's environment, so a leaf can
// size its worker pool from LETTUCE_CPU_LIMIT instead of os.cpu_count().
func TestTB75_NativeProcessIsToldItsShare(t *testing.T) {
	envBin := buildTestBinary(t, "envdump", envDumpSource)
	envBinData, _ := os.ReadFile(envBin)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(envBinData)
	}))
	defer ts.Close()

	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	nr.httpClient = ts.Client()
	nr.SetCPUGrantSource(func() CPUGrant { return CPUGrant{ShareCores: 1.5, BudgetCores: 3} })
	var modifierGrant, notifierGrant CPUGrant
	nr.SetCommandModifier(func(cmd *exec.Cmd, _ int, cpu CPUGrant) error {
		modifierGrant = cpu
		return nil
	})
	nr.SetProcessNotifier(func(_ int, _ int, cpu CPUGrant) (func(), error) {
		notifierGrant = cpu
		return func() {}, nil
	})

	wu := &WorkUnit{ID: "7d3e9a10-5b2c-4f1e-8a6d-0c9b8a7f6e5d", Runtime: "native", DeadlineSeconds: 30, ExecutionSpec: nativeSpec(ts.URL+"/binary", envBinData)}
	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)
	result, err := nr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	env := string(result.OutputData)
	for _, want := range []string{"LETTUCE_CPU_LIMIT=1.5", "OMP_NUM_THREADS=2", "OPENBLAS_NUM_THREADS=2", "MKL_NUM_THREADS=2", "NUMEXPR_MAX_THREADS=2"} {
		if !strings.Contains(env, want) {
			t.Errorf("process env lacks %q:\n%s", want, env)
		}
	}
	want := CPUGrant{ShareCores: 1.5, BudgetCores: 3}
	if modifierGrant != want || notifierGrant != want {
		t.Errorf("limiter hooks received grants %+v / %+v, want %+v", modifierGrant, notifierGrant, want)
	}
}
