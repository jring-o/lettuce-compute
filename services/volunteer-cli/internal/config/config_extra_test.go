package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetByPathAllStringFields(t *testing.T) {
	tests := []struct {
		path  string
		value string
		get   func(*Config) string
	}{
		{"data_dir", "/tmp/custom", func(c *Config) string { return c.DataDir }},
		{"key_file", "/tmp/key", func(c *Config) string { return c.KeyFile }},
		{"pubkey_file", "/tmp/pub", func(c *Config) string { return c.PubKeyFile }},
		{"volunteer_id", "vol-123", func(c *Config) string { return c.VolunteerID }},
		{"log_level", "debug", func(c *Config) string { return c.LogLevel }},
		{"scheduling.cron_expression", "0 22 * * *", func(c *Config) string { return c.Scheduling.CronExpression }},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			cfg := Defaults()
			if err := cfg.SetByPath(tt.path, tt.value); err != nil {
				t.Fatalf("SetByPath(%q, %q) error: %v", tt.path, tt.value, err)
			}
			if got := tt.get(cfg); got != tt.value {
				t.Errorf("SetByPath(%q, %q): got %q", tt.path, tt.value, got)
			}
		})
	}
}

func TestSetByPathAllIntFields(t *testing.T) {
	tests := []struct {
		path string
		get  func(*Config) int
	}{
		{"resource_limits.max_cpu_cores", func(c *Config) int { return c.ResourceLimits.MaxCPUCores }},
		{"resource_limits.max_memory_mb", func(c *Config) int { return c.ResourceLimits.MaxMemoryMB }},
		{"resource_limits.max_disk_gb", func(c *Config) int { return c.ResourceLimits.MaxDiskGB }},
		{"resource_limits.max_bandwidth_mbps", func(c *Config) int { return c.ResourceLimits.MaxBandwidthMbps }},
		{"resource_limits.max_gpu_vram_pct", func(c *Config) int { return c.ResourceLimits.MaxGPUVRAMPct }},
		{"scheduling.idle_threshold_mins", func(c *Config) int { return c.Scheduling.IdleThresholdMins }},
		{"max_concurrent_tasks", func(c *Config) int { return c.MaxConcurrentTasks }},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			cfg := Defaults()
			if err := cfg.SetByPath(tt.path, "42"); err != nil {
				t.Fatalf("SetByPath(%q, \"42\") error: %v", tt.path, err)
			}
			if got := tt.get(cfg); got != 42 {
				t.Errorf("SetByPath(%q, \"42\"): got %d", tt.path, got)
			}

			// Invalid integer should fail.
			if err := cfg.SetByPath(tt.path, "xyz"); err == nil {
				t.Errorf("SetByPath(%q, \"xyz\"): expected error for non-integer", tt.path)
			}
		})
	}
}

func TestSetByPathModeUppercasing(t *testing.T) {
	cfg := Defaults()

	// scheduling.mode gets uppercased.
	if err := cfg.SetByPath("scheduling.mode", "when_idle"); err != nil {
		t.Fatalf("SetByPath scheduling.mode error: %v", err)
	}
	if cfg.Scheduling.Mode != "WHEN_IDLE" {
		t.Errorf("scheduling.mode = %q, want WHEN_IDLE", cfg.Scheduling.Mode)
	}

	// leafs.mode gets uppercased.
	if err := cfg.SetByPath("leafs.mode", "specific"); err != nil {
		t.Fatalf("SetByPath leafs.mode error: %v", err)
	}
	if cfg.Leafs.Mode != "SPECIFIC" {
		t.Errorf("leafs.mode = %q, want SPECIFIC", cfg.Leafs.Mode)
	}
}

func TestGetByPathAllFields(t *testing.T) {
	cfg := Defaults()
	cfg.DataDir = "/d"
	cfg.KeyFile = "/k"
	cfg.PubKeyFile = "/p"
	cfg.VolunteerID = "vol-1"
	cfg.LogLevel = "warn"
	cfg.MaxConcurrentTasks = 3
	cfg.ResourceLimits.MaxCPUCores = 4
	cfg.ResourceLimits.MaxMemoryMB = 1024
	cfg.ResourceLimits.MaxDiskGB = 5
	cfg.ResourceLimits.MaxBandwidthMbps = 100
	cfg.ResourceLimits.MaxGPUVRAMPct = 75
	cfg.Scheduling.Mode = "SCHEDULED"
	cfg.Scheduling.IdleThresholdMins = 15
	cfg.Scheduling.CronExpression = "0 * * * *"
	cfg.Leafs.Mode = "BLOCKLIST"

	expected := map[string]string{
		"data_dir":                       "/d",
		"key_file":                       "/k",
		"pubkey_file":                    "/p",
		"volunteer_id":                   "vol-1",
		"log_level":                      "warn",
		"max_concurrent_tasks":           "3",
		"resource_limits.max_cpu_cores":  "4",
		"resource_limits.max_memory_mb":  "1024",
		"resource_limits.max_disk_gb":    "5",
		"resource_limits.max_bandwidth_mbps": "100",
		"resource_limits.max_gpu_vram_pct":   "75",
		"scheduling.mode":               "SCHEDULED",
		"scheduling.idle_threshold_mins": "15",
		"scheduling.cron_expression":     "0 * * * *",
		"leafs.mode":                    "BLOCKLIST",
	}

	for path, want := range expected {
		t.Run(path, func(t *testing.T) {
			got, err := cfg.GetByPath(path)
			if err != nil {
				t.Fatalf("GetByPath(%q) error: %v", path, err)
			}
			if got != want {
				t.Errorf("GetByPath(%q) = %q, want %q", path, got, want)
			}
		})
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")

	if err := os.WriteFile(path, []byte("not: [valid: yaml\n"), 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Error("Load() should fail for invalid YAML")
	}
}

func TestValidateValidWhenIdle(t *testing.T) {
	cfg := Defaults()
	cfg.Scheduling.Mode = "WHEN_IDLE"
	cfg.Scheduling.IdleThresholdMins = 5

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() should pass for valid WHEN_IDLE config: %v", err)
	}
}

func TestValidateLeafModeBlocklist(t *testing.T) {
	cfg := Defaults()
	cfg.Leafs.Mode = "BLOCKLIST"
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() should pass for BLOCKLIST mode: %v", err)
	}
}

func TestDisplayName_WithName(t *testing.T) {
	srv := ServerConfig{Name: "my-server", GRPCAddress: "localhost:9090"}
	if got := srv.DisplayName(); got != "my-server" {
		t.Errorf("DisplayName() = %q, want my-server", got)
	}
}

func TestDisplayName_FallbackToGRPCAddress(t *testing.T) {
	srv := ServerConfig{GRPCAddress: "example.com:9090"}
	if got := srv.DisplayName(); got != "example.com:9090" {
		t.Errorf("DisplayName() = %q, want example.com:9090", got)
	}
}

func TestDisplayName_EmptyBoth(t *testing.T) {
	srv := ServerConfig{}
	if got := srv.DisplayName(); got != "" {
		t.Errorf("DisplayName() = %q, want empty string", got)
	}
}

func TestValidate_LeafPreferences_ValidBLOCKLIST(t *testing.T) {
	cfg := Defaults()
	cfg.Servers = []ServerConfig{{
		GRPCAddress: "localhost:9090",
		Name:        "test",
		LeafPreferences: LeafPreferences{
			Mode:     "BLOCKLIST",
			Disabled: []string{"leaf-a"},
		},
	}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("BLOCKLIST mode with disabled list should be valid: %v", err)
	}
}

func TestValidate_LeafPreferences_NegativeWeight(t *testing.T) {
	cfg := Defaults()
	cfg.Servers = []ServerConfig{{
		GRPCAddress: "localhost:9090",
		Name:        "test",
		LeafPreferences: LeafPreferences{
			Weights: map[string]int{"leaf-a": -5},
		},
	}}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for negative leaf weight")
	}
}

func TestValidate_LeafPreferences_ValidALL(t *testing.T) {
	cfg := Defaults()
	cfg.Servers = []ServerConfig{{
		GRPCAddress: "localhost:9090",
		Name:        "test",
		LeafPreferences: LeafPreferences{
			Mode:    "ALL",
			Weights: map[string]int{"leaf-a": 200},
		},
	}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("ALL mode with weights should be valid: %v", err)
	}
}

func TestSaveLoadRoundTrip_LeafPreferences_BLOCKLIST(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config-blocklist.yaml")

	original := Defaults()
	original.Servers = []ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "test-server",
			Weight:      150,
			LeafPreferences: LeafPreferences{
				Mode:     "BLOCKLIST",
				Disabled: []string{"leaf-bad"},
				Weights:  map[string]int{"leaf-good": 200},
			},
		},
	}

	if err := original.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	srv := loaded.Servers[0]
	if srv.LeafPreferences.Mode != "BLOCKLIST" {
		t.Errorf("Mode = %q, want BLOCKLIST", srv.LeafPreferences.Mode)
	}
	if len(srv.LeafPreferences.Disabled) != 1 || srv.LeafPreferences.Disabled[0] != "leaf-bad" {
		t.Errorf("Disabled = %v, want [leaf-bad]", srv.LeafPreferences.Disabled)
	}
	if srv.LeafPreferences.Weights["leaf-good"] != 200 {
		t.Errorf("Weights[leaf-good] = %d, want 200", srv.LeafPreferences.Weights["leaf-good"])
	}
}
