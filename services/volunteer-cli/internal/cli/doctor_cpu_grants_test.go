package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestDoctorListsEachRunningTasksCores: with the daemon running, doctor
// lists the cores each running task was given against the CPU limit, and says
// a changed limit reaches tasks started afterwards; with nothing running, or
// no daemon, it prints no such line.
func TestDoctorListsEachRunningTasksCores(t *testing.T) {
	dataDir := stubStatusAPI(t, map[string]any{
		"active_tasks": []map[string]any{
			{"leaf_name": "GREP", "cpu_cores": 2},
			{"leaf_name": "Beyblade", "cpu_cores": 1},
			{"leaf_name": "Beyblade", "cpu_cores": 1},
		},
	})
	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	checkCPUGrants(rep, 4, dataDir)
	out := buf.String()
	for _, want := range []string{"cpu grants", "GREP 2, Beyblade 1, Beyblade 1", "4 of 4 cores granted", "a changed limit applies to tasks started afterwards"} {
		if !strings.Contains(out, want) {
			t.Errorf("cpu grants line lacks %q:\n%s", want, out)
		}
	}
	if rep.warns != 0 {
		t.Errorf("warns = %d, want 0 (information)", rep.warns)
	}

	buf.Reset()
	rep = &doctorReport{w: &buf}
	checkCPUGrants(rep, 4, stubStatusAPI(t, map[string]any{"active_tasks": []any{}}))
	checkCPUGrants(rep, 4, t.TempDir()) // no daemon
	if buf.Len() != 0 {
		t.Errorf("with nothing running or no daemon doctor printed:\n%s", buf.String())
	}
}

// TestStatusShowsEachTasksCores: `status` has a CORES column with the
// cores each running task was given, and a dash for a task a daemon older than
// per-task grants reports without a figure.
func TestStatusShowsEachTasksCores(t *testing.T) {
	dataDir := stubStatusAPI(t, map[string]any{
		"active_tasks": []any{
			map[string]any{"leaf_name": "GREP", "runtime_type": "container", "cpu_cores": 3, "task_status": "running"},
			map[string]any{"leaf_name": "Older", "runtime_type": "native", "task_status": "running"},
		},
		"queued_tasks": []any{},
	})
	out := captureStdout(t, func() { printActiveTasks(dataDir) })
	var header, grep, older string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "LEAF"):
			header = line
		case strings.Contains(line, "GREP"):
			grep = line
		case strings.Contains(line, "Older"):
			older = line
		}
	}
	if !strings.Contains(header, "CORES") {
		t.Fatalf("no CORES column:\n%s", out)
	}
	col := strings.Index(header, "CORES")
	if len(grep) <= col || !strings.HasPrefix(grep[col:], "3") {
		t.Errorf("GREP row does not show 3 under CORES:\n%s", out)
	}
	if len(older) <= col || !strings.HasPrefix(older[col:], "—") {
		t.Errorf("a task with no figure should show a dash under CORES:\n%s", out)
	}
}
