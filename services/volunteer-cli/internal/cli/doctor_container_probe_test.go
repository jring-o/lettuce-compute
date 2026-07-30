package cli

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TB-25 regression: doctor's container verdict must come from a live engine
// probe — the same source the daemon uses — never from a config record. The
// retired available_runtimes key used to short-circuit checkContainer before
// any probe ran, so doctor printed "not enabled (no CONTAINER in
// available_runtimes)" on a machine that was executing a container work unit
// at that very moment, and could not distinguish "no engine" from "engine
// present, socket broken" whenever the key was stale.
func TestCheckContainer_ProbeDerivedVerdict(t *testing.T) {
	prevCfg := cfg
	prevDetect := detectContainerBackendDoctor
	t.Cleanup(func() {
		cfg = prevCfg
		detectContainerBackendDoctor = prevDetect
	})
	cfg = config.Defaults()

	// Engine probe finds nothing on this machine.
	detectContainerBackendDoctor = func(_ string, _ runtime.ContainerBackend) runtime.BackendInfo {
		return runtime.BackendInfo{Backend: runtime.BackendNone}
	}

	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	if usable := checkContainer(rep, logger); usable {
		t.Fatal("checkContainer = usable with no engine detected")
	}
	out := buf.String()
	if strings.Contains(out, "available_runtimes") {
		t.Errorf("container verdict still keyed on the retired available_runtimes config record:\n%s", out)
	}
	if !strings.Contains(out, "no container engine found") {
		t.Errorf("expected the probe-derived 'no container engine found' verdict, got:\n%s", out)
	}
}
