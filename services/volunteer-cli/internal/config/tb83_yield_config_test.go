package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TB-83: the yield block — off by default, reachable by `config set`/`get`,
// validated only when on, and self-documenting in the saved file.

func TestTB83_YieldDefaultsOff(t *testing.T) {
	c := Defaults()
	if c.Yield.Enabled {
		t.Error("yield must default OFF: nothing changes for a volunteer who has not turned it on")
	}
	if c.Yield.CPUPausePct != 25 || c.Yield.CPUResumePct != 15 || c.Yield.WindowSeconds != 30 || c.Yield.PollIntervalSeconds != 5 {
		t.Errorf("yield defaults = %+v, want 25 / 15 / 30 s / 5 s", c.Yield)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestTB83_YieldValidation(t *testing.T) {
	cases := []struct {
		name string
		y    YieldConfig
		ok   bool
	}{
		{"off ignores nonsense", YieldConfig{Enabled: false, CPUPausePct: 0, CPUResumePct: 500}, true},
		{"defaults on", YieldConfig{Enabled: true, CPUPausePct: 25, CPUResumePct: 15, WindowSeconds: 30, PollIntervalSeconds: 5}, true},
		{"resume >= pause", YieldConfig{Enabled: true, CPUPausePct: 20, CPUResumePct: 20, WindowSeconds: 30, PollIntervalSeconds: 5}, false},
		{"pause over 100", YieldConfig{Enabled: true, CPUPausePct: 101, CPUResumePct: 15, WindowSeconds: 30, PollIntervalSeconds: 5}, false},
		{"pause at 0", YieldConfig{Enabled: true, CPUPausePct: 0, CPUResumePct: 0, WindowSeconds: 30, PollIntervalSeconds: 5}, false},
		{"window too short", YieldConfig{Enabled: true, CPUPausePct: 25, CPUResumePct: 15, WindowSeconds: 2, PollIntervalSeconds: 1}, false},
		{"poll longer than window", YieldConfig{Enabled: true, CPUPausePct: 25, CPUResumePct: 15, WindowSeconds: 10, PollIntervalSeconds: 20}, false},
		{"poll at 0", YieldConfig{Enabled: true, CPUPausePct: 25, CPUResumePct: 15, WindowSeconds: 30, PollIntervalSeconds: 0}, false},
	}
	for _, tc := range cases {
		c := Defaults()
		c.Yield = tc.y
		err := c.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected a validation error", tc.name)
		}
		if !tc.ok && err != nil && !strings.HasPrefix(err.Error(), "yield.") {
			t.Errorf("%s: error %q must name the yield key", tc.name, err)
		}
	}
}

func TestTB83_YieldCommentsSavedAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := Defaults()
	c.Yield.Enabled = true
	c.Yield.CPUPausePct = 35
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{"# Yield to other programs", "# Master switch. Off by default", "cpu_pause_pct: 35"} {
		if !strings.Contains(out, want) {
			t.Errorf("saved config lacks %q\n--- got ---\n%s", want, out)
		}
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Yield.Enabled || loaded.Yield.CPUPausePct != 35 || loaded.Yield.CPUResumePct != 15 {
		t.Errorf("round-trip yield = %+v", loaded.Yield)
	}
	if warnings := loaded.DeprecatedKeyWarnings(); len(warnings) != 0 {
		t.Errorf("a saved yield block must not read as unknown keys: %v", warnings)
	}
}
