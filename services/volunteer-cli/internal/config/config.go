package config

import (
	"bytes"
	"errors"
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
	// HostIDFile holds this MACHINE's stable host key (default <DataDir>/host.id). The
	// keypair is the ACCOUNT — run the same key everywhere — and the host key
	// distinguishes this machine under it so the head meters in-flight work and the
	// work-send floor per machine while credit pools per account (TODO #19). Empty =>
	// <DataDir>/host.id.
	HostIDFile string `yaml:"host_id_file,omitempty"`

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

	MaxConcurrentTasks int     `yaml:"max_concurrent_tasks"`
	WorkBufferHours    float64 `yaml:"work_buffer_hours"` // hours of work to keep buffered per slot (default 2.0; 0 = a small unit-count fallback)
	LogLevel           string  `yaml:"log_level"`
	ResultCacheMaxMB   int     `yaml:"result_cache_max_mb"` // max MB for viz result cache (default 500)

	// Logging output. By default logs are written to both stderr and a
	// size-rotated JSON file under <DataDir>/logs/ so problems remain
	// debuggable after the fact with no manual stderr redirection.
	LogFile       string `yaml:"log_file,omitempty"` // log file path; empty = <DataDir>/logs/volunteer.log
	LogToFile     bool   `yaml:"log_to_file"`        // write logs to the rotating file (default true)
	LogToStderr   bool   `yaml:"log_to_stderr"`      // write logs to stderr (default true)
	LogMaxSizeMB  int    `yaml:"log_max_size_mb"`    // rotate after the file reaches this size (default 10)
	LogMaxBackups int    `yaml:"log_max_backups"`    // number of rotated files to retain (default 5)
	LogMaxAgeDays int    `yaml:"log_max_age_days"`   // max age of rotated files in days (default 0 = no limit)

	// deprecatedKeyWarnings holds advisories about keys present in the on-disk
	// config file that this version does not recognize (e.g. left over from an
	// older release whose syntax has since changed). It is populated by Load and
	// surfaced via DeprecatedKeyWarnings; it is never read from or written to the
	// file (no yaml tag, unexported), so an unknown key is reported, not applied.
	deprecatedKeyWarnings []string
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
// The desktop app's visual schedule builder and the CLI's `schedule set` command
// both write these; a cron expression is the third, equivalent representation for
// SCHEDULED mode (ranges take precedence over cron when both are present).
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
	Insecure        bool            `yaml:"insecure,omitempty" json:"insecure,omitempty"`                 // default false — use TLS
	CACertPath      string          `yaml:"ca_cert,omitempty" json:"ca_cert,omitempty"`                   // optional CA certificate for server verification
	CertPath        string          `yaml:"cert,omitempty" json:"cert,omitempty"`                         // optional client cert for mTLS
	KeyPath         string          `yaml:"key,omitempty" json:"key,omitempty"`                           // optional client key for mTLS
	Weight          int             `yaml:"weight,omitempty" json:"weight,omitempty"`                     // head-level weight, default 100
	LeafPreferences LeafPreferences `yaml:"leaf_preferences,omitempty" json:"leaf_preferences,omitempty"` // per-leaf config
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
	Mode     string         `yaml:"mode" json:"mode"`                             // "ALL" (use defaults), "SPECIFIC", "BLOCKLIST"
	Weights  map[string]int `yaml:"weights,omitempty" json:"weights,omitempty"`   // slug -> weight overrides
	Enabled  []string       `yaml:"enabled,omitempty" json:"enabled,omitempty"`   // for SPECIFIC mode
	Disabled []string       `yaml:"disabled,omitempty" json:"disabled,omitempty"` // for BLOCKLIST mode
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
		HostIDFile: filepath.Join(dataDir, "host.id"),
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
		AvailableRuntimes: []string{"NATIVE", "WASM"},
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
		WorkBufferHours:    2.0,
		LogLevel:           "info",
		LogToFile:          true,
		LogToStderr:        true,
		LogMaxSizeMB:       10,
		LogMaxBackups:      5,
		LogMaxAgeDays:      0,
		ResultCacheMaxMB:   500,
	}
}

// LogFilePath returns the resolved log file path: the explicit LogFile when
// set, otherwise <DataDir>/logs/volunteer.log.
func (c *Config) LogFilePath() string {
	if c.LogFile != "" {
		return c.LogFile
	}
	return filepath.Join(c.DataDir, "logs", "volunteer.log")
}

// HostIDPath returns the resolved host-id file path: the explicit HostIDFile when set,
// otherwise <DataDir>/host.id.
func (c *Config) HostIDPath() string {
	if c.HostIDFile != "" {
		return c.HostIDFile
	}
	return filepath.Join(c.DataDir, "host.id")
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
	// Lenient Unmarshal above silently ignores keys this version no longer knows,
	// so an upgraded config can leave a stale setting that quietly does nothing
	// (issue #51). Re-scan strictly to collect those keys and surface them as
	// non-fatal advisories — the config still loads with the recognized keys.
	cfg.deprecatedKeyWarnings = detectUnknownKeys(data)
	return cfg, nil
}

// DeprecatedKeyWarnings returns non-fatal advisories about keys found in the
// loaded config file that this version does not recognize. Returns nil when the
// file used only known keys (or no file was loaded).
func (c *Config) DeprecatedKeyWarnings() []string {
	return c.deprecatedKeyWarnings
}

// deprecatedKeyHints maps a known-renamed/removed key name to a short hint about
// its current replacement, so the advisory can point the user at the new key.
// Unmapped unknown keys still get a generic "unrecognized / being ignored"
// warning. Extend this as keys are renamed across releases.
//
// Entries are keyed by the bare key name (the last path segment), matching how the
// strict decoder reports an unknown field.
var deprecatedKeyHints = map[string]string{
	// Renamed AND re-semanticized: the old key sized the client work buffer as a
	// unit COUNT; the current key sizes it in HOURS. The value cannot be carried
	// over safely, so point the user at the new key rather than copying the number.
	"work_buffer_size": `renamed to "work_buffer_hours", which now sizes the buffer in HOURS of work per task (not a unit count) — set work_buffer_hours to the number of hours you want buffered.`,
}

// detectUnknownKeys re-decodes the raw config bytes with strict field checking
// and returns one advisory per key that does not map to the current schema. The
// strict decode is used only to enumerate unknown keys; the authoritative values
// come from the lenient Unmarshal in Load, so an unknown key never breaks loading.
func detectUnknownKeys(data []byte) []string {
	var probe Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&probe); err != nil {
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			var warnings []string
			for _, msg := range typeErr.Errors {
				// KnownFields reports an unknown key as
				// "line N: field X not found in type T".
				if strings.Contains(msg, "not found in type") {
					warnings = append(warnings, formatUnknownKeyWarning(msg))
				}
			}
			return warnings
		}
		// A non-type error means malformed YAML, which the lenient Unmarshal in
		// Load already rejected; nothing to add here.
	}
	return nil
}

// formatUnknownKeyWarning turns a strict-decode "field X not found in type T"
// message into a user-facing advisory, appending a replacement hint when the key
// is a known rename.
func formatUnknownKeyWarning(msg string) string {
	field := msg
	if i := strings.Index(msg, "field "); i >= 0 {
		rest := msg[i+len("field "):]
		if j := strings.Index(rest, " not found"); j >= 0 {
			field = strings.TrimSpace(rest[:j])
		}
	}
	line := ""
	if strings.HasPrefix(msg, "line ") {
		if j := strings.Index(msg, ":"); j >= 0 {
			line = msg[:j] // e.g. "line 12"
		}
	}
	warning := fmt.Sprintf("unrecognized config key %q", field)
	if line != "" {
		warning += " (" + line + ")"
	}
	warning += " is being ignored; it may be deprecated or renamed in this version."
	if hint := deprecatedKeyHints[field]; hint != "" {
		warning += " " + hint
	}
	return warning
}

// Save writes the config to a YAML file, creating parent directories if needed.
// The file is emitted with short explanatory comments on the keys volunteers most
// often tune (see marshalCommented).
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	data, err := c.marshalCommented()
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}
	return nil
}

// marshalCommented renders the config as YAML with one-line explanatory comments
// on the keys volunteers most often tune. A plain struct marshal carries no
// comments, so they are regenerated on every Save and always match the current
// schema. Comment text is stored bare: the yaml.v3 emitter prepends "# " to each
// comment line itself.
func (c *Config) marshalCommented() ([]byte, error) {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		root := doc.Content[0]
		applyKeyComments(root, topLevelConfigComments)
		applyKeyComments(childMappingNode(root, "resource_limits"), resourceLimitsComments)
		applyKeyComments(childMappingNode(root, "thermal"), thermalComments)
		applyKeyComments(childMappingNode(root, "scheduling"), schedulingComments)
	}
	return yaml.Marshal(&doc)
}

// childMappingNode returns the value node mapped to key within mapping m, or nil
// if m is not a mapping or the key is absent.
func childMappingNode(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// applyKeyComments sets a head comment on each present key listed in comments,
// leaving any existing comment untouched.
func applyKeyComments(m *yaml.Node, comments map[string]string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		key := m.Content[i]
		if cmt, ok := comments[key.Value]; ok && key.HeadComment == "" {
			key.HeadComment = cmt
		}
	}
}

// Comment maps keyed by YAML field name. Edited alongside the struct so the
// generated config stays self-documenting.
var topLevelConfigComments = map[string]string{
	"max_concurrent_tasks": "How many work units run at once - THIS is the workload throttle (the thermal thresholds are not). The buffer target scales with it.",
	"work_buffer_hours":    "Hours of work to keep buffered per concurrent task. Larger = fewer, bigger requests; 0 = a small fixed unit count.",
	"available_runtimes":   "Runtimes this volunteer will run. NATIVE and WASM are always available; CONTAINER also needs Docker or Podman.",
	"resource_limits":      "Per-task resource ceilings. A head only sends leafs whose requirements fit under these - too low and you silently get no work.",
	"scheduling":           "When the volunteer runs.",
	"thermal":              "Hardware overheating protection. Temperatures in degrees C, NOT workload limits: ALL work freezes above the pause threshold and resumes below the resume threshold.",
}

var resourceLimitsComments = map[string]string{
	"max_cpu_cores":      "Max CPU cores a single work unit may use.",
	"max_memory_mb":      "Memory ceiling. A head only sends leafs whose per-unit memory fits under this; set it too low and you match no work.",
	"max_disk_gb":        "Disk under the data dir the volunteer may use. Work is not fetched unless at least this much is free.",
	"max_bandwidth_mbps": "Bandwidth cap in Mbps. 0 = unlimited.",
	"max_gpu_vram_pct":   "Max percent of each GPU's VRAM a task may use. 0 disables GPU work entirely.",
}

var thermalComments = map[string]string{
	"enabled":               "Master switch for thermal protection.",
	"cpu_pause_threshold":   "degrees C - freeze ALL work when the CPU reaches this.",
	"cpu_resume_threshold":  "degrees C - resume once the CPU cools below this (must be < cpu_pause_threshold).",
	"gpu_pause_threshold":   "degrees C - freeze ALL work when the GPU reaches this.",
	"gpu_resume_threshold":  "degrees C - resume once the GPU cools below this (must be < gpu_pause_threshold).",
	"poll_interval_seconds": "How often temperatures are sampled, in seconds.",
}

var schedulingComments = map[string]string{
	"mode":                "ALWAYS, WHEN_IDLE (only when the machine is idle), or SCHEDULED (time windows).",
	"idle_threshold_mins": "WHEN_IDLE only: minutes of inactivity before work starts.",
	"cron_expression":     "SCHEDULED only: cron expression for active windows.",
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
	if c.WorkBufferHours < 0 {
		return fmt.Errorf("work_buffer_hours must be >= 0 (0 = small unit-count fallback), got %g", c.WorkBufferHours)
	}

	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[c.LogLevel] {
		return fmt.Errorf("log_level must be debug, info, warn, or error, got %q", c.LogLevel)
	}

	if c.LogMaxSizeMB < 0 {
		return fmt.Errorf("log_max_size_mb must be >= 0, got %d", c.LogMaxSizeMB)
	}
	if c.LogMaxBackups < 0 {
		return fmt.Errorf("log_max_backups must be >= 0, got %d", c.LogMaxBackups)
	}
	if c.LogMaxAgeDays < 0 {
		return fmt.Errorf("log_max_age_days must be >= 0, got %d", c.LogMaxAgeDays)
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

// LeafConfigWarnings returns non-fatal advisories about the leaf-filtering
// configuration. The volunteer has two independent leaf filters — the global
// `leafs:` filter (by leaf ID) and each server's `leaf_preferences:` (by slug).
// Both are honored, but configuring both restrictively at once is a common
// source of confusion (especially after upgrading an older config), so surface
// the overlap rather than silently applying both. Returns nil when there is
// nothing worth flagging.
func (c *Config) LeafConfigWarnings() []string {
	var warnings []string
	globalRestrictive := c.Leafs.Mode == "SPECIFIC" || c.Leafs.Mode == "BLOCKLIST"
	for _, srv := range c.Servers {
		m := srv.LeafPreferences.Mode
		if (m == "SPECIFIC" || m == "BLOCKLIST") && globalRestrictive {
			warnings = append(warnings, fmt.Sprintf(
				"server %q sets leaf_preferences.mode=%s while the global leafs.mode=%s is also restrictive; "+
					"both filters apply (global by leaf ID, per-server by slug). If a leaf you expect is missing, check both.",
				srv.DisplayName(), m, c.Leafs.Mode))
		}
	}
	return warnings
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
	case "host_id_file":
		c.HostIDFile = value
	case "volunteer_id":
		c.VolunteerID = value
	case "log_level":
		c.LogLevel = value
	case "log_file":
		c.LogFile = value
	case "log_to_file":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean for %s: %w", dotPath, err)
		}
		c.LogToFile = v
	case "log_to_stderr":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean for %s: %w", dotPath, err)
		}
		c.LogToStderr = v
	case "log_max_size_mb":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.LogMaxSizeMB = v
	case "log_max_backups":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.LogMaxBackups = v
	case "log_max_age_days":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.LogMaxAgeDays = v
	case "max_concurrent_tasks":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.MaxConcurrentTasks = v
	case "work_buffer_hours":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("invalid number for %s: %w", dotPath, err)
		}
		c.WorkBufferHours = v
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
	case "thermal.enabled":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean for %s: %w", dotPath, err)
		}
		c.Thermal.Enabled = v
	case "thermal.cpu_pause_threshold":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.Thermal.CPUPauseThresholdC = v
	case "thermal.cpu_resume_threshold":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.Thermal.CPUResumeThresholdC = v
	case "thermal.gpu_pause_threshold":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.Thermal.GPUPauseThresholdC = v
	case "thermal.gpu_resume_threshold":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.Thermal.GPUResumeThresholdC = v
	case "thermal.poll_interval_seconds":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.Thermal.PollIntervalSeconds = v
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
	case "host_id_file":
		return c.HostIDFile, nil
	case "volunteer_id":
		return c.VolunteerID, nil
	case "log_level":
		return c.LogLevel, nil
	case "log_file":
		return c.LogFile, nil
	case "log_to_file":
		return strconv.FormatBool(c.LogToFile), nil
	case "log_to_stderr":
		return strconv.FormatBool(c.LogToStderr), nil
	case "log_max_size_mb":
		return strconv.Itoa(c.LogMaxSizeMB), nil
	case "log_max_backups":
		return strconv.Itoa(c.LogMaxBackups), nil
	case "log_max_age_days":
		return strconv.Itoa(c.LogMaxAgeDays), nil
	case "max_concurrent_tasks":
		return strconv.Itoa(c.MaxConcurrentTasks), nil
	case "work_buffer_hours":
		return strconv.FormatFloat(c.WorkBufferHours, 'g', -1, 64), nil
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
	case "thermal.enabled":
		return strconv.FormatBool(c.Thermal.Enabled), nil
	case "thermal.cpu_pause_threshold":
		return strconv.Itoa(c.Thermal.CPUPauseThresholdC), nil
	case "thermal.cpu_resume_threshold":
		return strconv.Itoa(c.Thermal.CPUResumeThresholdC), nil
	case "thermal.gpu_pause_threshold":
		return strconv.Itoa(c.Thermal.GPUPauseThresholdC), nil
	case "thermal.gpu_resume_threshold":
		return strconv.Itoa(c.Thermal.GPUResumeThresholdC), nil
	case "thermal.poll_interval_seconds":
		return strconv.Itoa(c.Thermal.PollIntervalSeconds), nil
	default:
		return "", fmt.Errorf("unknown config path: %s", dotPath)
	}
}
