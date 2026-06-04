package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveEmitsComments verifies the generated config carries explanatory
// comments on the tunable keys and that commented output still round-trips
// through Load (comments are ignored on read).
func TestSaveEmitsComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	c := Defaults()
	if err := c.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	out := string(data)

	// Spot-check a comment from each commented section. The yaml.v3 emitter
	// prepends "# ", so assert on the rendered "# <text>" form.
	wantSubstrings := []string{
		"# How many work units run at once",         // top-level: max_concurrent_tasks
		"# Memory ceiling. A head only sends leafs", // resource_limits.max_memory_mb
		"freeze ALL work when the CPU reaches this", // thermal.cpu_pause_threshold
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("saved config missing comment %q\n--- got ---\n%s", want, out)
		}
	}

	// Round-trip: values survive despite the comments.
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded.ResourceLimits.MaxMemoryMB != c.ResourceLimits.MaxMemoryMB {
		t.Errorf("MaxMemoryMB round-trip = %d, want %d",
			loaded.ResourceLimits.MaxMemoryMB, c.ResourceLimits.MaxMemoryMB)
	}
	if loaded.Thermal.CPUPauseThresholdC != c.Thermal.CPUPauseThresholdC {
		t.Errorf("CPUPauseThresholdC round-trip = %d, want %d",
			loaded.Thermal.CPUPauseThresholdC, c.Thermal.CPUPauseThresholdC)
	}
	if err := loaded.Validate(); err != nil {
		t.Errorf("loaded config failed validation: %v", err)
	}
}

// TestSetGetThermalByPath verifies the thermal.* keys are reachable via the
// dot-path setters/getters that back `lettuce-volunteer config set/get`.
func TestSetGetThermalByPath(t *testing.T) {
	c := Defaults()

	cases := map[string]string{
		"thermal.enabled":               "false",
		"thermal.cpu_pause_threshold":   "90",
		"thermal.cpu_resume_threshold":  "80",
		"thermal.gpu_pause_threshold":   "82",
		"thermal.gpu_resume_threshold":  "72",
		"thermal.poll_interval_seconds": "5",
	}
	for key, val := range cases {
		if err := c.SetByPath(key, val); err != nil {
			t.Fatalf("SetByPath(%q, %q) error: %v", key, val, err)
		}
		got, err := c.GetByPath(key)
		if err != nil {
			t.Fatalf("GetByPath(%q) error: %v", key, err)
		}
		if got != val {
			t.Errorf("GetByPath(%q) = %q, want %q", key, got, val)
		}
	}

	if c.Thermal.Enabled {
		t.Error("thermal.enabled should be false after set")
	}
	if c.Thermal.CPUPauseThresholdC != 90 {
		t.Errorf("CPUPauseThresholdC = %d, want 90", c.Thermal.CPUPauseThresholdC)
	}

	// A non-numeric value is rejected.
	if err := c.SetByPath("thermal.cpu_pause_threshold", "hot"); err == nil {
		t.Error("expected error setting thermal.cpu_pause_threshold to non-integer")
	}
}
