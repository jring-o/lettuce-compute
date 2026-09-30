package cli

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TestDoctorNamesTheCPUTimeLimitAndTheOverrides: doctor reports a CPU time
// limit below 100 and every per-leaf override.
func TestDoctorNamesTheCPUTimeLimitAndTheOverrides(t *testing.T) {
	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	checkCPUTimeLimit(rep, config.ResourceLimits{MaxCPUTimePct: 50})
	checkLeafOverrides(rep, []config.ServerConfig{{Name: "lettuce.science", LeafPreferences: config.LeafPreferences{
		Cores: map[string]int{"grep": 3}, MaxRunning: map[string]int{"grep": 1}}}}, nil)
	out := buf.String()
	for _, want := range []string{"cpu time", "Runs 50 % of the time (5 s of every 10 s)", "2.0 times as long", "grep on lettuce.science: 3 cores per task, at most 1 running"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	var quietBuf bytes.Buffer
	quiet := &doctorReport{w: &quietBuf}
	checkCPUTimeLimit(quiet, config.ResourceLimits{MaxCPUTimePct: 100})
	checkLeafOverrides(quiet, []config.ServerConfig{{Name: "x"}}, nil)
	if quietBuf.Len() != 0 {
		t.Errorf("doctor reported lines with no limit and no overrides:\n%s", quietBuf.String())
	}
}

// TestDoctorShowsWhatRunsTogether: doctor prints the running daemon's
// preview — the tasks that start together and each leaf alone.
func TestDoctorShowsWhatRunsTogether(t *testing.T) {
	dataDir := stubManagement(t, map[string]http.HandlerFunc{
		"GET /api/v1/run-preview": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"cpu_limit":4,"together_cores":3,"together":[{"leaf_name":"GREP","cores":2},{"leaf_name":"Beyblade","cores":1}],"waiting_for_cores":{"leaf_name":"GREP","cores":2},"alone":[{"leaf_name":"GREP","tasks":[2,2]},{"leaf_name":"Beyblade","tasks":[1,1,1,1]}]}`))
		},
	})
	var buf bytes.Buffer
	checkRunPreview(&doctorReport{w: &buf}, dataDir)
	out := buf.String()
	for _, want := range []string{"GREP × 1 · 2 cores, Beyblade × 1 · 1 core — 3 of 4 cores; the next GREP task, which needs 2 cores, would wait for them", "GREP 2 at once, 2 cores each; Beyblade 4 at once, 1 core each"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
}
