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

// The runtime half of the CPU grant (TB-75): a container is created
// with its unit's grant as its quota (not the whole budget) and told it
// through its environment; a native process is told it the same way and the
// limiter hooks receive the same grant. The grant is the one the daemon gave
// the unit when it started, and it is fixed for the run.

// TestContainerIsCreatedWithItsUnitsGrant: a unit granted 3 of 4 cores
// gets a 300000/100000 quota and LETTUCE_CPU_LIMIT=3 with the thread knobs at
// 3, whatever fixed budget the runtime holds. A unit with no grant (the audit
// runner) gets the runtime's whole budget; with neither, no quota and nothing
// told.
func TestContainerIsCreatedWithItsUnitsGrant(t *testing.T) {
	run := func(t *testing.T, budget int, grant CPUGrant) *ContainerConfig {
		t.Helper()
		mock := &MockDockerClient{}
		cr, _ := newTestContainerRuntime(t, mock)
		if budget > 0 {
			cr.SetCPUBudget(budget)
		}
		wu := &WorkUnit{ID: "2b1c6f3e-0d2a-4c5e-9f7b-1a2b3c4d5e6f", ExecutionSpec: ExecutionSpec{Image: "alpine:latest"}, CPUGrant: grant}
		prep, err := cr.Prepare(context.Background(), wu)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		defer cr.Cleanup(prep)
		os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644)
		if _, err := cr.Execute(context.Background(), wu, prep); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return mock.LastCreateConfig
	}

	cfg := run(t, 8, CPUGrant{Cores: 3, BudgetCores: 4})
	if cfg.CPUQuota != 300000 || cfg.CPUPeriod != 100000 {
		t.Errorf("CPUQuota/CPUPeriod = %d/%d, want 300000/100000 (the unit's 3 cores), not the runtime's 8-core budget", cfg.CPUQuota, cfg.CPUPeriod)
	}
	env := strings.Join(cfg.Env, "\n")
	for _, want := range []string{"LETTUCE_CPU_LIMIT=3", "OMP_NUM_THREADS=3", "OPENBLAS_NUM_THREADS=3", "MKL_NUM_THREADS=3", "NUMEXPR_MAX_THREADS=3", "DOCLING_NUM_THREADS=3"} {
		if !strings.Contains(env, want) {
			t.Errorf("container env lacks %q:\n%s", want, env)
		}
	}

	if cfg := run(t, 2, CPUGrant{}); cfg.CPUQuota != 200000 || !strings.Contains(strings.Join(cfg.Env, "\n"), "LETTUCE_CPU_LIMIT=2") {
		t.Errorf("an ungranted unit under a 2-core budget: quota %d, env %v; want 200000 and LETTUCE_CPU_LIMIT=2", cfg.CPUQuota, cfg.Env)
	}
	if cfg := run(t, 0, CPUGrant{}); cfg.CPUQuota != 0 || strings.Contains(strings.Join(cfg.Env, "\n"), "LETTUCE_CPU_LIMIT") {
		t.Errorf("with no CPU limit: quota %d, env %v", cfg.CPUQuota, cfg.Env)
	}
}

// TestNativeProcessIsToldItsUnitsGrant: the native runtime passes the
// unit's grant to the limiter hooks and into the process's environment, so a
// leaf can size its worker pool from LETTUCE_CPU_LIMIT instead of
// os.cpu_count().
func TestNativeProcessIsToldItsUnitsGrant(t *testing.T) {
	envBin := buildTestBinary(t, "envdump", envDumpSource)
	envBinData, _ := os.ReadFile(envBin)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(envBinData)
	}))
	defer ts.Close()

	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	nr.httpClient = ts.Client()
	nr.SetCPUBudget(8)
	var modifierGrant, notifierGrant CPUGrant
	nr.SetCommandModifier(func(cmd *exec.Cmd, _ int, cpu CPUGrant) error {
		modifierGrant = cpu
		return nil
	})
	nr.SetProcessNotifier(func(_ int, _ int, cpu CPUGrant) (func(), error) {
		notifierGrant = cpu
		return func() {}, nil
	})

	want := CPUGrant{Cores: 2, BudgetCores: 4}
	wu := &WorkUnit{ID: "7d3e9a10-5b2c-4f1e-8a6d-0c9b8a7f6e5d", Runtime: "native", DeadlineSeconds: 30, ExecutionSpec: nativeSpec(ts.URL+"/binary", envBinData), CPUGrant: want}
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
	for _, w := range []string{"LETTUCE_CPU_LIMIT=2", "OMP_NUM_THREADS=2", "OPENBLAS_NUM_THREADS=2", "MKL_NUM_THREADS=2", "NUMEXPR_MAX_THREADS=2", "DOCLING_NUM_THREADS=2"} {
		if !strings.Contains(env, w) {
			t.Errorf("process env lacks %q:\n%s", w, env)
		}
	}
	if modifierGrant != want || notifierGrant != want {
		t.Errorf("limiter hooks received grants %+v / %+v, want the unit's %+v", modifierGrant, notifierGrant, want)
	}
}
