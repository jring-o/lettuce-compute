package config

import (
	"strings"
	"testing"
)

// TestDefaultDeadlineSeconds covers the head's default work-unit deadline, the one
// a unit gets when its leaf sets no deadline_seconds: default_deadline_seconds when
// set, else the retired no_deadline_ceiling_seconds name when set, else 6h.
func TestDefaultDeadlineSeconds(t *testing.T) {
	tests := []struct {
		name        string
		cfg         HeadConfig
		want        int
		wantRetired bool
	}{
		{name: "unset is 6h", cfg: HeadConfig{}, want: 21600},
		{name: "the setting", cfg: HeadConfig{DefaultDeadlineSeconds: 7200}, want: 7200},
		{name: "the retired name alone", cfg: HeadConfig{NoDeadlineCeilingSeconds: 3600}, want: 3600, wantRetired: true},
		{name: "the setting wins over the retired name", cfg: HeadConfig{DefaultDeadlineSeconds: 7200, NoDeadlineCeilingSeconds: 3600}, want: 7200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.EffectiveDefaultDeadlineSeconds(); got != tt.want {
				t.Errorf("EffectiveDefaultDeadlineSeconds() = %d, want %d", got, tt.want)
			}
			if got := tt.cfg.UsesRetiredDeadlineCeilingName(); got != tt.wantRetired {
				t.Errorf("UsesRetiredDeadlineCeilingName() = %v, want %v", got, tt.wantRetired)
			}
		})
	}
}

func TestDefaultDeadlineSeconds_Validate(t *testing.T) {
	if err := (HeadConfig{Name: "t", DefaultDeadlineSeconds: -1}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "default_deadline_seconds") {
		t.Errorf("negative default_deadline_seconds: err = %v, want a default_deadline_seconds error", err)
	}
	if err := (HeadConfig{Name: "t", NoDeadlineCeilingSeconds: -1}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "no_deadline_ceiling_seconds") {
		t.Errorf("negative no_deadline_ceiling_seconds: err = %v, want a no_deadline_ceiling_seconds error", err)
	}
}

func TestDefaultDeadlineSeconds_YAMLAndEnv(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		env         map[string]string
		want        int
		wantRetired bool
	}{
		{
			name: "yaml",
			yaml: `head: { name: "t", default_deadline_seconds: 7200 }`,
			want: 7200,
		},
		{
			name:        "yaml, retired name",
			yaml:        `head: { name: "t", no_deadline_ceiling_seconds: 3600 }`,
			want:        3600,
			wantRetired: true,
		},
		{
			name: "env",
			yaml: minimalConfig,
			env:  map[string]string{"LETTUCE_HEAD_DEFAULT_DEADLINE_SECONDS": "5400"},
			want: 5400,
		},
		{
			name:        "env, retired name",
			yaml:        minimalConfig,
			env:         map[string]string{"LETTUCE_HEAD_NO_DEADLINE_CEILING_SECONDS": "3600"},
			want:        3600,
			wantRetired: true,
		},
		{
			name: "env setting over a retired yaml name",
			yaml: `head: { name: "t", no_deadline_ceiling_seconds: 3600 }`,
			env:  map[string]string{"LETTUCE_HEAD_DEFAULT_DEADLINE_SECONDS": "5400"},
			want: 5400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearLettuceEnv(t)
			t.Setenv("LETTUCE_HEAD_DEFAULT_DEADLINE_SECONDS", "")
			t.Setenv("LETTUCE_HEAD_NO_DEADLINE_CEILING_SECONDS", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := Load(writeTestConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got := cfg.Head.EffectiveDefaultDeadlineSeconds(); got != tt.want {
				t.Errorf("EffectiveDefaultDeadlineSeconds() = %d, want %d", got, tt.want)
			}
			if got := cfg.Head.UsesRetiredDeadlineCeilingName(); got != tt.wantRetired {
				t.Errorf("UsesRetiredDeadlineCeilingName() = %v, want %v", got, tt.wantRetired)
			}
		})
	}
}

func TestDefaultDeadlineSeconds_InvalidEnv(t *testing.T) {
	clearLettuceEnv(t)
	t.Setenv("LETTUCE_HEAD_NO_DEADLINE_CEILING_SECONDS", "")
	t.Setenv("LETTUCE_HEAD_DEFAULT_DEADLINE_SECONDS", "six hours")
	if _, err := Load(writeTestConfig(t, minimalConfig)); err == nil ||
		!strings.Contains(err.Error(), "LETTUCE_HEAD_DEFAULT_DEADLINE_SECONDS") {
		t.Errorf("Load() error = %v, want one naming LETTUCE_HEAD_DEFAULT_DEADLINE_SECONDS", err)
	}
}

// The shipped example sets the new name, so a head built from it logs no
// retired-name notice.
func TestLoadExampleConfig_DefaultDeadline(t *testing.T) {
	clearLettuceEnv(t)
	t.Setenv("LETTUCE_HEAD_DEFAULT_DEADLINE_SECONDS", "")
	t.Setenv("LETTUCE_HEAD_NO_DEADLINE_CEILING_SECONDS", "")
	cfg, err := Load("../../lettuce.yaml.example")
	if err != nil {
		t.Fatalf("failed to load example config: %v", err)
	}
	if cfg.Head.DefaultDeadlineSeconds != 21600 || cfg.Head.UsesRetiredDeadlineCeilingName() {
		t.Errorf("example: default_deadline_seconds = %d (retired name used: %v), want 21600 from the new name",
			cfg.Head.DefaultDeadlineSeconds, cfg.Head.UsesRetiredDeadlineCeilingName())
	}
}
