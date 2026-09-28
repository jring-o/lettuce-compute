package cli

import (
	"bytes"
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
