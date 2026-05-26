package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()

	if cfg.ResourceLimits.MaxCPUCores < 1 {
		t.Error("default MaxCPUCores should be >= 1")
	}
	if cfg.ResourceLimits.MaxMemoryMB != 2048 {
		t.Errorf("default MaxMemoryMB = %d, want 2048", cfg.ResourceLimits.MaxMemoryMB)
	}
	if cfg.ResourceLimits.MaxDiskGB != 10 {
		t.Errorf("default MaxDiskGB = %d, want 10", cfg.ResourceLimits.MaxDiskGB)
	}
	if cfg.Scheduling.Mode != "ALWAYS" {
		t.Errorf("default scheduling mode = %q, want ALWAYS", cfg.Scheduling.Mode)
	}
	if cfg.Leafs.Mode != "ALL" {
		t.Errorf("default leaf mode = %q, want ALL", cfg.Leafs.Mode)
	}
	if cfg.MaxConcurrentTasks != 1 {
		t.Errorf("default MaxConcurrentTasks = %d, want 1", cfg.MaxConcurrentTasks)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("default LogLevel = %q, want info", cfg.LogLevel)
	}
	if len(cfg.AvailableRuntimes) != 2 || cfg.AvailableRuntimes[0] != "NATIVE" || cfg.AvailableRuntimes[1] != "WASM" {
		t.Errorf("default runtimes = %v, want [NATIVE WASM]", cfg.AvailableRuntimes)
	}
	if cfg.ResourceLimits.MaxGPUVRAMPct != 50 {
		t.Errorf("default MaxGPUVRAMPct = %d, want 50", cfg.ResourceLimits.MaxGPUVRAMPct)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	original := Defaults()
	original.ResourceLimits.MaxCPUCores = 8
	original.ResourceLimits.MaxMemoryMB = 4096
	original.Scheduling.Mode = "WHEN_IDLE"
	original.Scheduling.IdleThresholdMins = 10
	original.Leafs.Mode = "SPECIFIC"
	original.Leafs.LeafIDs = []string{"proj-1", "proj-2"}
	original.Servers = []ServerConfig{
		{GRPCAddress: "localhost:9090", HTTPAddress: "http://localhost:8080", Name: "test"},
	}

	if err := original.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if loaded.ResourceLimits.MaxCPUCores != 8 {
		t.Errorf("loaded MaxCPUCores = %d, want 8", loaded.ResourceLimits.MaxCPUCores)
	}
	if loaded.ResourceLimits.MaxMemoryMB != 4096 {
		t.Errorf("loaded MaxMemoryMB = %d, want 4096", loaded.ResourceLimits.MaxMemoryMB)
	}
	if loaded.Scheduling.Mode != "WHEN_IDLE" {
		t.Errorf("loaded scheduling mode = %q, want WHEN_IDLE", loaded.Scheduling.Mode)
	}
	if loaded.Scheduling.IdleThresholdMins != 10 {
		t.Errorf("loaded idle threshold = %d, want 10", loaded.Scheduling.IdleThresholdMins)
	}
	if loaded.Leafs.Mode != "SPECIFIC" {
		t.Errorf("loaded leaf mode = %q, want SPECIFIC", loaded.Leafs.Mode)
	}
	if len(loaded.Leafs.LeafIDs) != 2 {
		t.Errorf("loaded leaf IDs count = %d, want 2", len(loaded.Leafs.LeafIDs))
	}
	if len(loaded.Servers) != 1 {
		t.Errorf("loaded servers count = %d, want 1", len(loaded.Servers))
	}
}

func TestSaveLoadRoundTrip_GPUOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config-gpu.yaml")

	original := Defaults()
	original.ResourceLimits.MaxGPUVRAMPct = 75
	original.GPUOverrides = []GPUOverride{
		{Index: 0, MaxVRAMPct: 90},
		{Index: 2, Disabled: true},
	}

	if err := original.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if loaded.ResourceLimits.MaxGPUVRAMPct != 75 {
		t.Errorf("loaded MaxGPUVRAMPct = %d, want 75", loaded.ResourceLimits.MaxGPUVRAMPct)
	}
	if len(loaded.GPUOverrides) != 2 {
		t.Fatalf("loaded GPUOverrides count = %d, want 2", len(loaded.GPUOverrides))
	}
	if loaded.GPUOverrides[0].Index != 0 || loaded.GPUOverrides[0].MaxVRAMPct != 90 {
		t.Errorf("GPU override 0: got index=%d vram=%d, want index=0 vram=90",
			loaded.GPUOverrides[0].Index, loaded.GPUOverrides[0].MaxVRAMPct)
	}
	if loaded.GPUOverrides[1].Index != 2 || !loaded.GPUOverrides[1].Disabled {
		t.Errorf("GPU override 1: got index=%d disabled=%v, want index=2 disabled=true",
			loaded.GPUOverrides[1].Index, loaded.GPUOverrides[1].Disabled)
	}
}

func TestLoadNonExistent(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load() should return defaults for nonexistent file, got error: %v", err)
	}
	if cfg.Scheduling.Mode != "ALWAYS" {
		t.Error("expected defaults when file doesn't exist")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid defaults",
			modify:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "zero cpu cores",
			modify:  func(c *Config) { c.ResourceLimits.MaxCPUCores = 0 },
			wantErr: true,
		},
		{
			name:    "negative memory",
			modify:  func(c *Config) { c.ResourceLimits.MaxMemoryMB = -1 },
			wantErr: true,
		},
		{
			name:    "zero disk",
			modify:  func(c *Config) { c.ResourceLimits.MaxDiskGB = 0 },
			wantErr: true,
		},
		{
			name:    "negative bandwidth",
			modify:  func(c *Config) { c.ResourceLimits.MaxBandwidthMbps = -1 },
			wantErr: true,
		},
		{
			name:    "invalid scheduling mode",
			modify:  func(c *Config) { c.Scheduling.Mode = "INVALID" },
			wantErr: true,
		},
		{
			name: "idle mode without threshold",
			modify: func(c *Config) {
				c.Scheduling.Mode = "WHEN_IDLE"
				c.Scheduling.IdleThresholdMins = 0
			},
			wantErr: true,
		},
		{
			name: "scheduled mode without cron",
			modify: func(c *Config) {
				c.Scheduling.Mode = "SCHEDULED"
				c.Scheduling.CronExpression = ""
			},
			wantErr: true,
		},
		{
			name:    "invalid leaf mode",
			modify:  func(c *Config) { c.Leafs.Mode = "INVALID" },
			wantErr: true,
		},
		{
			name:    "zero concurrent tasks",
			modify:  func(c *Config) { c.MaxConcurrentTasks = 0 },
			wantErr: true,
		},
		{
			name:    "invalid log level",
			modify:  func(c *Config) { c.LogLevel = "trace" },
			wantErr: true,
		},
		{
			name: "valid scheduled mode",
			modify: func(c *Config) {
				c.Scheduling.Mode = "SCHEDULED"
				c.Scheduling.CronExpression = "0 22 * * *"
			},
			wantErr: false,
		},
		{
			name:    "gpu vram pct negative",
			modify:  func(c *Config) { c.ResourceLimits.MaxGPUVRAMPct = -1 },
			wantErr: true,
		},
		{
			name:    "gpu vram pct over 100",
			modify:  func(c *Config) { c.ResourceLimits.MaxGPUVRAMPct = 101 },
			wantErr: true,
		},
		{
			name:    "gpu vram pct zero valid",
			modify:  func(c *Config) { c.ResourceLimits.MaxGPUVRAMPct = 0 },
			wantErr: false,
		},
		{
			name:    "gpu vram pct 100 valid",
			modify:  func(c *Config) { c.ResourceLimits.MaxGPUVRAMPct = 100 },
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			tt.modify(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSetByPath(t *testing.T) {
	cfg := Defaults()

	if err := cfg.SetByPath("resource_limits.max_cpu_cores", "4"); err != nil {
		t.Fatalf("SetByPath() error: %v", err)
	}
	if cfg.ResourceLimits.MaxCPUCores != 4 {
		t.Errorf("MaxCPUCores = %d, want 4", cfg.ResourceLimits.MaxCPUCores)
	}

	if err := cfg.SetByPath("scheduling.mode", "when_idle"); err != nil {
		t.Fatalf("SetByPath() error: %v", err)
	}
	if cfg.Scheduling.Mode != "WHEN_IDLE" {
		t.Errorf("Mode = %q, want WHEN_IDLE", cfg.Scheduling.Mode)
	}

	if err := cfg.SetByPath("log_level", "debug"); err != nil {
		t.Fatalf("SetByPath() error: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}

	// Invalid path.
	if err := cfg.SetByPath("nonexistent.path", "value"); err == nil {
		t.Error("expected error for unknown path")
	}

	// Invalid int.
	if err := cfg.SetByPath("resource_limits.max_cpu_cores", "notanumber"); err == nil {
		t.Error("expected error for non-integer value")
	}
}

func TestGetByPath(t *testing.T) {
	cfg := Defaults()
	cfg.ResourceLimits.MaxCPUCores = 4

	val, err := cfg.GetByPath("resource_limits.max_cpu_cores")
	if err != nil {
		t.Fatalf("GetByPath() error: %v", err)
	}
	if val != "4" {
		t.Errorf("GetByPath() = %q, want 4", val)
	}

	val, err = cfg.GetByPath("scheduling.mode")
	if err != nil {
		t.Fatalf("GetByPath() error: %v", err)
	}
	if val != "ALWAYS" {
		t.Errorf("GetByPath() = %q, want ALWAYS", val)
	}

	// Unknown path.
	_, err = cfg.GetByPath("nonexistent")
	if err == nil {
		t.Error("expected error for unknown path")
	}
}

func TestSaveLoadRoundTrip_TLSFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config-tls.yaml")

	original := Defaults()
	original.Servers = []ServerConfig{
		{
			GRPCAddress: "example.com:9090",
			HTTPAddress: "https://example.com:8080",
			Name:        "secure-server",
			Insecure:    false,
			CACertPath:  "/path/to/ca.crt",
			CertPath:    "/path/to/client.crt",
			KeyPath:     "/path/to/client.key",
		},
		{
			GRPCAddress: "localhost:9090",
			HTTPAddress: "http://localhost:8080",
			Name:        "dev-server",
			Insecure:    true,
		},
	}

	if err := original.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(loaded.Servers) != 2 {
		t.Fatalf("loaded servers count = %d, want 2", len(loaded.Servers))
	}

	// Check TLS-enabled server.
	s := loaded.Servers[0]
	if s.Insecure {
		t.Error("server 0: Insecure should be false")
	}
	if s.CACertPath != "/path/to/ca.crt" {
		t.Errorf("server 0: CACertPath = %q, want /path/to/ca.crt", s.CACertPath)
	}
	if s.CertPath != "/path/to/client.crt" {
		t.Errorf("server 0: CertPath = %q, want /path/to/client.crt", s.CertPath)
	}
	if s.KeyPath != "/path/to/client.key" {
		t.Errorf("server 0: KeyPath = %q, want /path/to/client.key", s.KeyPath)
	}

	// Check insecure server.
	s2 := loaded.Servers[1]
	if !s2.Insecure {
		t.Error("server 1: Insecure should be true")
	}
	if s2.CACertPath != "" {
		t.Errorf("server 1: CACertPath should be empty, got %q", s2.CACertPath)
	}
}

func TestSaveCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "config.yaml")

	cfg := Defaults()
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not created: %v", err)
	}
}

func TestDefaults_NotificationConfig(t *testing.T) {
	cfg := Defaults()

	if !cfg.Notifications.CreditMilestones {
		t.Error("default CreditMilestones should be true")
	}
	if cfg.Notifications.CreditMilestoneThreshold != 100 {
		t.Errorf("default CreditMilestoneThreshold = %d, want 100", cfg.Notifications.CreditMilestoneThreshold)
	}
	if cfg.Notifications.WorkUnitCompleted {
		t.Error("default WorkUnitCompleted should be false")
	}
	if !cfg.Notifications.Errors {
		t.Error("default Errors should be true")
	}
	if !cfg.Notifications.Updates {
		t.Error("default Updates should be true")
	}
}

func TestValidate_ServerWeight_Zero_OK(t *testing.T) {
	cfg := Defaults()
	cfg.Servers = []ServerConfig{{GRPCAddress: "localhost:9090", Name: "test", Weight: 0}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("weight 0 should be valid (defaults to 100): %v", err)
	}
}

func TestValidate_ServerWeight_Negative_Error(t *testing.T) {
	cfg := Defaults()
	cfg.Servers = []ServerConfig{{GRPCAddress: "localhost:9090", Name: "test", Weight: -1}}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for negative weight")
	}
}

func TestValidate_LeafPreferences_InvalidMode(t *testing.T) {
	cfg := Defaults()
	cfg.Servers = []ServerConfig{{
		GRPCAddress:     "localhost:9090",
		Name:            "test",
		LeafPreferences: LeafPreferences{Mode: "INVALID"},
	}}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid leaf preferences mode")
	}
}

func TestValidate_LeafPreferences_SpecificEmpty(t *testing.T) {
	cfg := Defaults()
	cfg.Servers = []ServerConfig{{
		GRPCAddress:     "localhost:9090",
		Name:            "test",
		LeafPreferences: LeafPreferences{Mode: "SPECIFIC", Enabled: []string{}},
	}}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for SPECIFIC mode with empty enabled list")
	}
}

func TestValidate_LeafWeights_ZeroValue_Error(t *testing.T) {
	cfg := Defaults()
	cfg.Servers = []ServerConfig{{
		GRPCAddress: "localhost:9090",
		Name:        "test",
		LeafPreferences: LeafPreferences{
			Mode:    "ALL",
			Weights: map[string]int{"leaf-1": 0},
		},
	}}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for zero weight in leaf preferences")
	}
}

func TestSaveLoadRoundTrip_LeafPreferences(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config-leaf.yaml")

	original := Defaults()
	original.Servers = []ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "test-server",
			Weight:      200,
			LeafPreferences: LeafPreferences{
				Mode:     "SPECIFIC",
				Weights:  map[string]int{"leaf-a": 300, "leaf-b": 100},
				Enabled:  []string{"leaf-a", "leaf-b"},
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

	if len(loaded.Servers) != 1 {
		t.Fatalf("loaded servers count = %d, want 1", len(loaded.Servers))
	}

	srv := loaded.Servers[0]
	if srv.Weight != 200 {
		t.Errorf("Weight = %d, want 200", srv.Weight)
	}
	if srv.LeafPreferences.Mode != "SPECIFIC" {
		t.Errorf("LeafPreferences.Mode = %q, want SPECIFIC", srv.LeafPreferences.Mode)
	}
	if len(srv.LeafPreferences.Enabled) != 2 {
		t.Errorf("Enabled count = %d, want 2", len(srv.LeafPreferences.Enabled))
	}
	if srv.LeafPreferences.Weights["leaf-a"] != 300 {
		t.Errorf("Weights[leaf-a] = %d, want 300", srv.LeafPreferences.Weights["leaf-a"])
	}
}

func TestDefaults_ResultCacheMaxMB(t *testing.T) {
	cfg := Defaults()
	if cfg.ResultCacheMaxMB != 500 {
		t.Errorf("default ResultCacheMaxMB = %d, want 500", cfg.ResultCacheMaxMB)
	}
}

func TestSaveLoadRoundTrip_ResultCacheMaxMB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config-result-cache.yaml")

	original := Defaults()
	original.ResultCacheMaxMB = 1024

	if err := original.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if loaded.ResultCacheMaxMB != 1024 {
		t.Errorf("loaded ResultCacheMaxMB = %d, want 1024", loaded.ResultCacheMaxMB)
	}
}

func TestSaveLoadRoundTrip_NotificationConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config-notify.yaml")

	original := Defaults()
	original.Notifications.CreditMilestones = false
	original.Notifications.CreditMilestoneThreshold = 500
	original.Notifications.WorkUnitCompleted = true
	original.Notifications.Errors = false
	original.Notifications.Updates = false

	if err := original.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if loaded.Notifications.CreditMilestones {
		t.Error("loaded CreditMilestones should be false")
	}
	if loaded.Notifications.CreditMilestoneThreshold != 500 {
		t.Errorf("loaded CreditMilestoneThreshold = %d, want 500", loaded.Notifications.CreditMilestoneThreshold)
	}
	if !loaded.Notifications.WorkUnitCompleted {
		t.Error("loaded WorkUnitCompleted should be true")
	}
	if loaded.Notifications.Errors {
		t.Error("loaded Errors should be false")
	}
	if loaded.Notifications.Updates {
		t.Error("loaded Updates should be false")
	}
}
