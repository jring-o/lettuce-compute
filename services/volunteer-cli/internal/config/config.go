package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all volunteer CLI configuration.
type Config struct {
	DataDir    string `yaml:"data_dir"`
	KeyFile    string `yaml:"key_file"`
	PubKeyFile string `yaml:"pubkey_file"`

	VolunteerID string `yaml:"volunteer_id,omitempty"`

	ResourceLimits ResourceLimits `yaml:"resource_limits"`

	Scheduling Scheduling `yaml:"scheduling"`

	Leafs LeafFilter `yaml:"leafs"`

	AvailableRuntimes []string `yaml:"available_runtimes"`

	ContainerBackend string `yaml:"container_backend,omitempty"` // "podman", "docker", or ""

	GPUOverrides []GPUOverride `yaml:"gpu_overrides,omitempty"`

	Thermal ThermalConfig `yaml:"thermal"`

	Notifications NotificationConfig `yaml:"notifications"`

	Servers []ServerConfig `yaml:"servers,omitempty"`

	MaxConcurrentTasks int    `yaml:"max_concurrent_tasks"`
	WorkBufferSize     int    `yaml:"work_buffer_size"`
	LogLevel           string `yaml:"log_level"`
	ResultCacheMaxMB   int    `yaml:"result_cache_max_mb"` // max MB for viz result cache (default 500)
}

// ThermalConfig controls thermal monitoring thresholds.
type ThermalConfig struct {
	Enabled             bool `yaml:"enabled" json:"enabled"`                             // default true
	CPUPauseThresholdC  int  `yaml:"cpu_pause_threshold" json:"cpu_pause_threshold"`     // default 85
	CPUResumeThresholdC int  `yaml:"cpu_resume_threshold" json:"cpu_resume_threshold"`   // default 75
	GPUPauseThresholdC  int  `yaml:"gpu_pause_threshold" json:"gpu_pause_threshold"`     // default 80
	GPUResumeThresholdC int  `yaml:"gpu_resume_threshold" json:"gpu_resume_threshold"`   // default 70
	PollIntervalSeconds int  `yaml:"poll_interval_seconds" json:"poll_interval_seconds"` // default 10
}

// NotificationConfig controls notification preferences.
type NotificationConfig struct {
	CreditMilestones         bool `yaml:"credit_milestones" json:"credit_milestones"`
	CreditMilestoneThreshold int  `yaml:"credit_milestone_threshold" json:"credit_milestone_threshold"`
	WorkUnitCompleted        bool `yaml:"work_unit_completed" json:"work_unit_completed"`
	Errors                   bool `yaml:"errors" json:"errors"`
	Updates                  bool `yaml:"updates" json:"updates"`
}

// ResourceLimits defines compute resource caps.
type ResourceLimits struct {
	MaxCPUCores      int `yaml:"max_cpu_cores" json:"max_cpu_cores"`
	MaxMemoryMB      int `yaml:"max_memory_mb" json:"max_memory_mb"`
	MaxDiskGB        int `yaml:"max_disk_gb" json:"max_disk_gb"`
	MaxBandwidthMbps int `yaml:"max_bandwidth_mbps" json:"max_bandwidth_mbps"`
	MaxGPUVRAMPct    int `yaml:"max_gpu_vram_pct" json:"max_gpu_vram_pct"` // 0-100, default 50. 0 = disable GPU tasks
}

// GPUOverride allows per-GPU configuration.
type GPUOverride struct {
	Index      int  `yaml:"index"`        // GPU index (0-based)
	MaxVRAMPct int  `yaml:"max_vram_pct"` // override global default for this GPU
	Disabled   bool `yaml:"disabled"`     // skip this GPU entirely
}

// ScheduleRange represents an active time window for scheduled mode.
// The desktop app's visual schedule builder writes these; the CLI writes cron expressions.
// Both are valid representations for SCHEDULED mode.
type ScheduleRange struct {
	Days      []int `yaml:"days" json:"days"`             // 0=Mon, 6=Sun
	StartHour int   `yaml:"start_hour" json:"start_hour"` // 0-23
	EndHour   int   `yaml:"end_hour" json:"end_hour"`     // 0-23, can wrap (22 → 6 means 22:00-06:00)
}

// Scheduling controls when the volunteer runs.
type Scheduling struct {
	Mode              string          `yaml:"mode" json:"mode"`
	IdleThresholdMins int             `yaml:"idle_threshold_mins" json:"idle_threshold_mins"`
	CronExpression    string          `yaml:"cron_expression,omitempty" json:"cron_expression,omitempty"`
	ScheduleRanges    []ScheduleRange `yaml:"schedule_ranges,omitempty" json:"schedule_ranges,omitempty"`
}

// LeafFilter controls which leafs the volunteer accepts.
type LeafFilter struct {
	Mode       string   `yaml:"mode" json:"mode"`
	LeafIDs    []string `yaml:"leaf_ids,omitempty" json:"leaf_ids,omitempty"`
	BlockedIDs []string `yaml:"blocked_ids,omitempty" json:"blocked_ids,omitempty"`
}

// ServerConfig holds connection details for an infrastructure server.
type ServerConfig struct {
	GRPCAddress     string          `yaml:"grpc_address" json:"grpc_address"`
	HTTPAddress     string          `yaml:"http_address,omitempty" json:"http_address,omitempty"`
	LeafID          string          `yaml:"leaf_id,omitempty" json:"leaf_id,omitempty"`
	Name            string          `yaml:"name" json:"name"`
	Insecure        bool            `yaml:"insecure,omitempty" json:"insecure,omitempty"`                     // default false — use TLS
	CACertPath      string          `yaml:"ca_cert,omitempty" json:"ca_cert,omitempty"`                       // optional CA certificate for server verification
	CertPath        string          `yaml:"cert,omitempty" json:"cert,omitempty"`                             // optional client cert for mTLS
	KeyPath         string          `yaml:"key,omitempty" json:"key,omitempty"`                               // optional client key for mTLS
	Weight          int             `yaml:"weight,omitempty" json:"weight,omitempty"`                         // head-level weight, default 100
	LeafPreferences LeafPreferences `yaml:"leaf_preferences,omitempty" json:"leaf_preferences,omitempty"`     // per-leaf config
}

// DisplayName returns the server's Name, falling back to GRPCAddress if Name is empty.
func (s ServerConfig) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.GRPCAddress
}

// LeafPreferences controls which leafs a volunteer computes on a given server.
type LeafPreferences struct {
	Mode     string         `yaml:"mode" json:"mode"`                                // "ALL" (use defaults), "SPECIFIC", "BLOCKLIST"
	Weights  map[string]int `yaml:"weights,omitempty" json:"weights,omitempty"`       // slug -> weight overrides
	Enabled  []string       `yaml:"enabled,omitempty" json:"enabled,omitempty"`       // for SPECIFIC mode
	Disabled []string       `yaml:"disabled,omitempty" json:"disabled,omitempty"`     // for BLOCKLIST mode
}

// defaultDataDir returns the default data directory (~/.lettuce/).
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".lettuce")
}

// Defaults returns a Config with all default values.
func Defaults() *Config {
	dataDir := defaultDataDir()
	numCPU := runtime.NumCPU()
	defaultCores := numCPU / 2
	if defaultCores < 1 {
		defaultCores = 1
	}

	return &Config{
		DataDir:    dataDir,
		KeyFile:    filepath.Join(dataDir, "identity.key"),
		PubKeyFile: filepath.Join(dataDir, "identity.pub"),
		ResourceLimits: ResourceLimits{
			MaxCPUCores:      defaultCores,
			MaxMemoryMB:      2048,
			MaxDiskGB:        10,
			MaxBandwidthMbps: 0,
			MaxGPUVRAMPct:    50,
		},
		Scheduling: Scheduling{
			Mode:              "ALWAYS",
			IdleThresholdMins: 5,
		},
		Leafs: LeafFilter{
			Mode: "ALL",
		},
		AvailableRuntimes:  []string{"NATIVE", "WASM"},
		Notifications: NotificationConfig{
			CreditMilestones:         true,
			CreditMilestoneThreshold: 100,
			WorkUnitCompleted:        false,
			Errors:                   true,
			Updates:                  true,
		},
		Thermal: ThermalConfig{
			Enabled:             true,
			CPUPauseThresholdC:  85,
			CPUResumeThresholdC: 75,
			GPUPauseThresholdC:  80,
			GPUResumeThresholdC: 70,
			PollIntervalSeconds: 10,
		},
		MaxConcurrentTasks: 1,
		LogLevel:           "info",
		ResultCacheMaxMB:   500,
	}
}

// Load reads and parses a YAML config file. Returns defaults if the file doesn't exist.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Defaults(), nil
		}
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := Defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}
	return cfg, nil
}

// Save writes the config to a YAML file, creating parent directories if needed.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}
	return nil
}

// Validate checks that all config values are valid.
func (c *Config) Validate() error {
	if c.ResourceLimits.MaxCPUCores < 1 {
		return fmt.Errorf("resource_limits.max_cpu_cores must be >= 1, got %d", c.ResourceLimits.MaxCPUCores)
	}
	if c.ResourceLimits.MaxMemoryMB < 1 {
		return fmt.Errorf("resource_limits.max_memory_mb must be >= 1, got %d", c.ResourceLimits.MaxMemoryMB)
	}
	if c.ResourceLimits.MaxDiskGB < 1 {
		return fmt.Errorf("resource_limits.max_disk_gb must be >= 1, got %d", c.ResourceLimits.MaxDiskGB)
	}
	if c.ResourceLimits.MaxBandwidthMbps < 0 {
		return fmt.Errorf("resource_limits.max_bandwidth_mbps must be >= 0, got %d", c.ResourceLimits.MaxBandwidthMbps)
	}
	if c.ResourceLimits.MaxGPUVRAMPct < 0 || c.ResourceLimits.MaxGPUVRAMPct > 100 {
		return fmt.Errorf("resource_limits.max_gpu_vram_pct must be 0-100, got %d", c.ResourceLimits.MaxGPUVRAMPct)
	}

	validModes := map[string]bool{"ALWAYS": true, "WHEN_IDLE": true, "SCHEDULED": true}
	if !validModes[c.Scheduling.Mode] {
		return fmt.Errorf("scheduling.mode must be ALWAYS, WHEN_IDLE, or SCHEDULED, got %q", c.Scheduling.Mode)
	}
	if c.Scheduling.Mode == "WHEN_IDLE" && c.Scheduling.IdleThresholdMins < 1 {
		return fmt.Errorf("scheduling.idle_threshold_mins must be >= 1 when mode is WHEN_IDLE")
	}
	if c.Scheduling.Mode == "SCHEDULED" && c.Scheduling.CronExpression == "" && len(c.Scheduling.ScheduleRanges) == 0 {
		return fmt.Errorf("scheduling.cron_expression or schedule_ranges is required when mode is SCHEDULED")
	}
	for i, r := range c.Scheduling.ScheduleRanges {
		if r.StartHour < 0 || r.StartHour > 23 {
			return fmt.Errorf("scheduling.schedule_ranges[%d].start_hour must be 0-23, got %d", i, r.StartHour)
		}
		if r.EndHour < 0 || r.EndHour > 23 {
			return fmt.Errorf("scheduling.schedule_ranges[%d].end_hour must be 0-23, got %d", i, r.EndHour)
		}
		for _, d := range r.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("scheduling.schedule_ranges[%d] has invalid day %d (must be 0-6)", i, d)
			}
		}
	}

	validLeafModes := map[string]bool{"ALL": true, "SPECIFIC": true, "BLOCKLIST": true}
	if !validLeafModes[c.Leafs.Mode] {
		return fmt.Errorf("leafs.mode must be ALL, SPECIFIC, or BLOCKLIST, got %q", c.Leafs.Mode)
	}

	// Server-level validation: weight and leaf preferences.
	for i, srv := range c.Servers {
		if srv.Weight < 0 {
			return fmt.Errorf("servers[%d].weight must be >= 0, got %d", i, srv.Weight)
		}
		lp := srv.LeafPreferences
		if lp.Mode != "" {
			validLeafModes := map[string]bool{"ALL": true, "SPECIFIC": true, "BLOCKLIST": true}
			if !validLeafModes[lp.Mode] {
				return fmt.Errorf("servers[%d].leaf_preferences.mode must be ALL, SPECIFIC, or BLOCKLIST, got %q", i, lp.Mode)
			}
			if lp.Mode == "SPECIFIC" && len(lp.Enabled) == 0 {
				return fmt.Errorf("servers[%d].leaf_preferences: SPECIFIC mode requires at least one enabled leaf", i)
			}
		}
		for slug, w := range lp.Weights {
			if w <= 0 {
				return fmt.Errorf("servers[%d].leaf_preferences.weights[%q] must be > 0, got %d", i, slug, w)
			}
		}
	}

	if c.MaxConcurrentTasks < 1 {
		return fmt.Errorf("max_concurrent_tasks must be >= 1, got %d", c.MaxConcurrentTasks)
	}
	if c.WorkBufferSize < 0 {
		return fmt.Errorf("work_buffer_size must be >= 0 (0 = auto), got %d", c.WorkBufferSize)
	}

	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[c.LogLevel] {
		return fmt.Errorf("log_level must be debug, info, warn, or error, got %q", c.LogLevel)
	}

	// Container backend validation.
	validBackends := map[string]bool{"": true, "podman": true, "docker": true}
	if !validBackends[c.ContainerBackend] {
		return fmt.Errorf("container_backend must be podman, docker, or empty, got %q", c.ContainerBackend)
	}

	// Thermal config validation.
	if c.Thermal.Enabled {
		if c.Thermal.CPUPauseThresholdC <= c.Thermal.CPUResumeThresholdC {
			return fmt.Errorf("thermal.cpu_pause_threshold (%d) must be > cpu_resume_threshold (%d)",
				c.Thermal.CPUPauseThresholdC, c.Thermal.CPUResumeThresholdC)
		}
		if c.Thermal.GPUPauseThresholdC <= c.Thermal.GPUResumeThresholdC {
			return fmt.Errorf("thermal.gpu_pause_threshold (%d) must be > gpu_resume_threshold (%d)",
				c.Thermal.GPUPauseThresholdC, c.Thermal.GPUResumeThresholdC)
		}
		for _, threshold := range []struct {
			name  string
			value int
		}{
			{"cpu_pause_threshold", c.Thermal.CPUPauseThresholdC},
			{"cpu_resume_threshold", c.Thermal.CPUResumeThresholdC},
			{"gpu_pause_threshold", c.Thermal.GPUPauseThresholdC},
			{"gpu_resume_threshold", c.Thermal.GPUResumeThresholdC},
		} {
			if threshold.value < 30 || threshold.value > 105 {
				return fmt.Errorf("thermal.%s must be 30-105, got %d", threshold.name, threshold.value)
			}
		}
		if c.Thermal.PollIntervalSeconds < 1 || c.Thermal.PollIntervalSeconds > 300 {
			return fmt.Errorf("thermal.poll_interval_seconds must be 1-300, got %d", c.Thermal.PollIntervalSeconds)
		}
	}

	return nil
}

// SetByPath sets a config value by dot-delimited path (e.g., "resource_limits.max_cpu_cores").
func (c *Config) SetByPath(dotPath string, value string) error {
	switch dotPath {
	case "data_dir":
		c.DataDir = value
	case "key_file":
		c.KeyFile = value
	case "pubkey_file":
		c.PubKeyFile = value
	case "volunteer_id":
		c.VolunteerID = value
	case "log_level":
		c.LogLevel = value
	case "max_concurrent_tasks":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.MaxConcurrentTasks = v
	case "work_buffer_size":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.WorkBufferSize = v
	case "resource_limits.max_cpu_cores":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.ResourceLimits.MaxCPUCores = v
	case "resource_limits.max_memory_mb":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.ResourceLimits.MaxMemoryMB = v
	case "resource_limits.max_disk_gb":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.ResourceLimits.MaxDiskGB = v
	case "resource_limits.max_bandwidth_mbps":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.ResourceLimits.MaxBandwidthMbps = v
	case "resource_limits.max_gpu_vram_pct":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.ResourceLimits.MaxGPUVRAMPct = v
	case "scheduling.mode":
		c.Scheduling.Mode = strings.ToUpper(value)
	case "scheduling.idle_threshold_mins":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.Scheduling.IdleThresholdMins = v
	case "scheduling.cron_expression":
		c.Scheduling.CronExpression = value
	case "leafs.mode":
		c.Leafs.Mode = strings.ToUpper(value)
	case "container_backend":
		c.ContainerBackend = value
	default:
		return fmt.Errorf("unknown config path: %s", dotPath)
	}
	return nil
}

// GetByPath gets a config value by dot-delimited path.
func (c *Config) GetByPath(dotPath string) (string, error) {
	switch dotPath {
	case "data_dir":
		return c.DataDir, nil
	case "key_file":
		return c.KeyFile, nil
	case "pubkey_file":
		return c.PubKeyFile, nil
	case "volunteer_id":
		return c.VolunteerID, nil
	case "log_level":
		return c.LogLevel, nil
	case "max_concurrent_tasks":
		return strconv.Itoa(c.MaxConcurrentTasks), nil
	case "work_buffer_size":
		return strconv.Itoa(c.WorkBufferSize), nil
	case "resource_limits.max_cpu_cores":
		return strconv.Itoa(c.ResourceLimits.MaxCPUCores), nil
	case "resource_limits.max_memory_mb":
		return strconv.Itoa(c.ResourceLimits.MaxMemoryMB), nil
	case "resource_limits.max_disk_gb":
		return strconv.Itoa(c.ResourceLimits.MaxDiskGB), nil
	case "resource_limits.max_bandwidth_mbps":
		return strconv.Itoa(c.ResourceLimits.MaxBandwidthMbps), nil
	case "resource_limits.max_gpu_vram_pct":
		return strconv.Itoa(c.ResourceLimits.MaxGPUVRAMPct), nil
	case "scheduling.mode":
		return c.Scheduling.Mode, nil
	case "scheduling.idle_threshold_mins":
		return strconv.Itoa(c.Scheduling.IdleThresholdMins), nil
	case "scheduling.cron_expression":
		return c.Scheduling.CronExpression, nil
	case "leafs.mode":
		return c.Leafs.Mode, nil
	case "container_backend":
		return c.ContainerBackend, nil
	default:
		return "", fmt.Errorf("unknown config path: %s", dotPath)
	}
}
