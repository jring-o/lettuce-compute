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

	"github.com/lettuce-compute/volunteer-cli/internal/cron"
	"gopkg.in/yaml.v3"
)

// Config holds all volunteer CLI configuration.
type Config struct {
	DataDir string `yaml:"data_dir"`
	// KeyFile / PubKeyFile optionally pin the identity keypair to explicit paths.
	// Empty (the default) means the keypair lives inside the data dir
	// (<DataDir>/identity.key|.pub — see KeyFilePath/PubKeyFilePath), so a
	// profile stays self-contained under its own --data-dir instead of baking in
	// another profile's absolute paths.
	KeyFile    string `yaml:"key_file,omitempty"`
	PubKeyFile string `yaml:"pubkey_file,omitempty"`
	// HostIDFile is the path of the RETIRED client-generated per-machine host id
	// (default <DataDir>/host.id). Host identity is now HEAD-ISSUED and stored
	// per-head in <DataDir>/host-ids.json (see HostIDsPath); this legacy single-file
	// value is no longer read or written. The field is retained only so an existing
	// config carrying host_id_file still loads (and `config get/set host_id_file`
	// keep working) rather than erroring on an unknown key.
	HostIDFile string `yaml:"host_id_file,omitempty"`

	VolunteerID string `yaml:"volunteer_id,omitempty"`

	ResourceLimits ResourceLimits `yaml:"resource_limits"`

	Scheduling Scheduling `yaml:"scheduling"`

	Leafs LeafFilter `yaml:"leafs"`

	// The retired global runtime keys available_runtimes and allow_native_runtime
	// are deliberately ABSENT from this struct (TB-25 / TQ-22). Runtime enablement
	// is per-head trust (servers[].trusted_runtimes) plus a live engine probe at
	// daemon start; the daemon, registration, and the diagnostics never consult a
	// global list. An old config still carrying either key loads fine — the lenient
	// Unmarshal ignores it and DeprecatedKeyWarnings tells the volunteer it is dead.

	ContainerBackend string `yaml:"container_backend,omitempty"` // "podman", "docker", or ""

	// ContainerCapAdd lists Linux capabilities to re-add to hardened containers
	// (BG-13). Default empty: hardened containers run with CapDrop:ALL. Each entry
	// is an explicit, logged operator choice.
	ContainerCapAdd []string `yaml:"container_cap_add,omitempty"`

	// ContainerGPURelaxUser, when true (default), lets GPU leaves relax the
	// non-root-User / minimal-capability posture that CPU leaves always get,
	// because device passthrough often needs it (BG-13 GPU carve-out). CPU leaves
	// are hardened regardless of this flag.
	ContainerGPURelaxUser bool `yaml:"container_gpu_relax_user"`

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

	// serverAddressRepairs records, per head entry Load repaired, the stored
	// gRPC address an older build wrote in a shape gRPC can never dial and the
	// target it now resolves to (TB-62). Populated by Load, surfaced via
	// ServerAddressRepairs for the start-up log; never written to the file.
	serverAddressRepairs []string

	// logLevelOverride and logFileOverride hold the values of the global
	// --log-level / --log-file flags for the lifetime of one command. They are
	// unexported and untagged so Save can never flush them to disk: a flag is a
	// one-time override, and it used to become the permanent setting because
	// the flag wiring mutated LogLevel/LogFile in place and any later Save —
	// registration on every `start`, `heads trust`, `schedule set` — wrote the
	// whole struct back out (TB-5). Read them through EffectiveLogLevel and
	// LogFilePath, never directly.
	logLevelOverride string
	logFileOverride  string
}

// ThermalConfig controls thermal monitoring thresholds.
type ThermalConfig struct {
	Enabled             bool `yaml:"enabled" json:"enabled"`                             // default true
	CPUPauseThresholdC  int  `yaml:"cpu_pause_threshold" json:"cpu_pause_threshold"`     // default 85
	CPUResumeThresholdC int  `yaml:"cpu_resume_threshold" json:"cpu_resume_threshold"`   // default 75
	GPUPauseThresholdC  int  `yaml:"gpu_pause_threshold" json:"gpu_pause_threshold"`     // default 80
	GPUResumeThresholdC int  `yaml:"gpu_resume_threshold" json:"gpu_resume_threshold"`   // default 70
	PollIntervalSeconds int  `yaml:"poll_interval_seconds" json:"poll_interval_seconds"` // default 10

	// MaxThrottleMinutes bounds one continuous throttle. 0 uses the default (30);
	// a negative value waits indefinitely, which is the pre-TB-17 behavior and is
	// a livelock whenever the heat is not this client's to clear.
	MaxThrottleMinutes int `yaml:"max_throttle_minutes" json:"max_throttle_minutes"` // default 30
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
	MaxPids          int `yaml:"max_pids" json:"max_pids"`                 // max PIDs per container (BG-13 fork-bomb cap); <=0 = built-in default
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

// NeverRuns reports WHY this scheduling configuration can never become active,
// or nil when it can. It mirrors the SCHEDULED branch of the resource
// scheduler's ShouldRun exactly — ranges take precedence, else the cron
// expression must parse — so it is a statement about runtime behavior rather
// than a stricter opinion about the file.
//
// It exists separately from Validate because the read paths (`start`, `doctor`)
// must flag only a genuinely dead schedule: a config that is merely untidy
// still runs today, and turning that into a refusal would lock working
// volunteers out over something cosmetic. The write paths use Validate.
func (s *Scheduling) NeverRuns() error {
	if s.Mode != "SCHEDULED" || len(s.ScheduleRanges) > 0 {
		return nil
	}
	if s.CronExpression == "" {
		return errors.New("scheduling.mode is SCHEDULED but neither a time window nor a cron expression is configured")
	}
	if err := cron.Validate(s.CronExpression); err != nil {
		return fmt.Errorf("scheduling.cron_expression %q is not a valid cron expression: %w", s.CronExpression, err)
	}
	return nil
}

// Validate checks the scheduling block on its own, so callers that need the
// scheduling rules without validating a whole config can reuse them.
//
// The cron expression is PARSED, not merely checked for non-emptiness. An
// unparseable expression used to be accepted everywhere it could be written and
// then failed silently on every 10-second scheduler poll, so the volunteer was
// configured, appeared healthy, and never ran (TB-3).
func (s *Scheduling) Validate() error {
	validModes := map[string]bool{"ALWAYS": true, "WHEN_IDLE": true, "SCHEDULED": true}
	if !validModes[s.Mode] {
		return fmt.Errorf("scheduling.mode must be ALWAYS, WHEN_IDLE, or SCHEDULED, got %q", s.Mode)
	}
	if s.Mode == "WHEN_IDLE" && s.IdleThresholdMins < 1 {
		return fmt.Errorf("scheduling.idle_threshold_mins must be >= 1 when mode is WHEN_IDLE")
	}
	if s.Mode == "SCHEDULED" && s.CronExpression == "" && len(s.ScheduleRanges) == 0 {
		return fmt.Errorf("scheduling.cron_expression or schedule_ranges is required when mode is SCHEDULED")
	}
	// Checked whenever present, not only when it is the active representation:
	// schedule ranges take precedence over cron, so a stored bad cron would
	// otherwise lie in wait until the ranges were removed.
	if s.CronExpression != "" {
		if err := cron.Validate(s.CronExpression); err != nil {
			return fmt.Errorf("scheduling.cron_expression %q is not a valid cron expression: %w — "+
				"if you meant a daily window use `lettuce-volunteer schedule set --from 20:00 --to 06:00`",
				s.CronExpression, err)
		}
	}
	for i, r := range s.ScheduleRanges {
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
	return nil
}

// LeafFilter controls which leafs the volunteer accepts.
type LeafFilter struct {
	Mode       string   `yaml:"mode" json:"mode"`
	LeafIDs    []string `yaml:"leaf_ids,omitempty" json:"leaf_ids,omitempty"`
	BlockedIDs []string `yaml:"blocked_ids,omitempty" json:"blocked_ids,omitempty"`
}

// ServerConfig holds connection details for an infrastructure server.
type ServerConfig struct {
	GRPCAddress string `yaml:"grpc_address" json:"grpc_address"`
	HTTPAddress string `yaml:"http_address,omitempty" json:"http_address,omitempty"`
	// LeafID is the RETIRED single-leaf pin of the old one-entry-per-leaf model.
	// `attach <leaf-id>` used to APPEND a whole duplicate server entry carrying
	// only this field — the daemon then collapsed the duplicate at startup and
	// silently discarded the pin (PB-16). The field is retained only so an
	// existing config still loads; Load migrates it into the same-address head
	// entry's PinnedLeafIDs and it is never written again.
	LeafID string `yaml:"leaf_id,omitempty" json:"-"`
	// PinnedLeafIDs are leaf IDs explicitly attached on THIS head
	// (`attach <leaf-id>` / `attach --server … --leaf <id>`). A pin makes the
	// daemon request work for that leaf BY ID even when the head's public
	// catalog does not list it — UNLISTED/PRIVATE leafs are absent from
	// GetHeadInfo by design and are reachable only this way. Pins are exempt
	// from the slug-based leaf_preferences filters (an explicit attach is the
	// stronger signal, and an unlisted leaf has no slug to filter on anyway).
	PinnedLeafIDs []string `yaml:"pinned_leaf_ids,omitempty" json:"pinned_leaf_ids,omitempty"`
	Name          string   `yaml:"name" json:"name"`
	Insecure        bool            `yaml:"insecure,omitempty" json:"insecure,omitempty"`                 // default false — use TLS
	CACertPath      string          `yaml:"ca_cert,omitempty" json:"ca_cert,omitempty"`                   // optional CA certificate for server verification
	CertPath        string          `yaml:"cert,omitempty" json:"cert,omitempty"`                         // optional client cert for mTLS
	KeyPath         string          `yaml:"key,omitempty" json:"key,omitempty"`                           // optional client key for mTLS
	Weight          int             `yaml:"weight,omitempty" json:"weight,omitempty"`                     // head-level weight, default 100
	LeafPreferences LeafPreferences `yaml:"leaf_preferences,omitempty" json:"leaf_preferences,omitempty"` // per-leaf config

	// TrustedRuntimes records how far this volunteer's trust in THIS head extends —
	// which runtime kinds the head may run on this machine, UPPERCASE
	// ("WASM"/"CONTAINER"/"NATIVE"). A head is a single operator's trust domain:
	// attaching to it IS the trust decision, and this field is what that decision chose
	// (see the attach/init consent prompt). WASM is always safe (a sealed sandbox) and
	// is implicitly trusted even when absent from this list — see EffectiveTrustedRuntimes.
	// CONTAINER and NATIVE are explicit opt-ins.
	//
	// nil vs empty is load-bearing (PB-28): nil (key absent from the file) marks a
	// config that predates per-head trust, and Load pins it to the explicit
	// WASM-only empty list — trust in a head's CONTAINER/NATIVE code is a consent
	// decision, and a file that never recorded one gets the safe answer, not a
	// guess from the retired global keys (TB-25). A present-but-EMPTY list
	// ("trusted_runtimes: []") is an explicit "none": the volunteer deliberately
	// granted this head WASM only, and the migration must never re-seed over that
	// choice. The yaml tag therefore has no omitempty — every entry written by
	// this version records its trust decision explicitly, empty included.
	TrustedRuntimes []string `yaml:"trusted_runtimes" json:"trusted_runtimes"`
}

// DisplayName returns the server's Name, falling back to GRPCAddress if Name is empty.
func (s ServerConfig) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.GRPCAddress
}

// EffectiveTrustedRuntimes returns the UPPERCASE runtime kinds this head is trusted to
// run on this machine, always including WASM (the sandbox is safe without trusting the
// operator). Runtime capability (does the machine actually have a container backend,
// etc.) is a SEPARATE gate applied when the registry is built — this method answers only
// "does the volunteer trust this head to run X", never "can this machine run X".
func (s ServerConfig) EffectiveTrustedRuntimes() []string {
	out := []string{"WASM"}
	for _, r := range s.TrustedRuntimes {
		u := strings.ToUpper(strings.TrimSpace(r))
		if u == "" || u == "WASM" {
			continue
		}
		out = append(out, u)
	}
	return out
}

// TrustsRuntime reports whether this head is trusted to run the given runtime kind
// (case-insensitive) on this machine. WASM is always trusted.
func (s ServerConfig) TrustsRuntime(runtime string) bool {
	want := strings.ToUpper(strings.TrimSpace(runtime))
	if want == "" {
		return false
	}
	for _, r := range s.EffectiveTrustedRuntimes() {
		if r == want {
			return true
		}
	}
	return false
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
		// KeyFile/PubKeyFile/HostIDFile stay empty: identity paths are derived
		// from DataDir at use time (KeyFilePath/PubKeyFilePath), so a profile
		// created under --data-dir never points at the default profile's files,
		// and the retired host_id_file is no longer written at all.
		DataDir: dataDir,
		ResourceLimits: ResourceLimits{
			MaxCPUCores:      defaultCores,
			MaxMemoryMB:      2048,
			MaxDiskGB:        10,
			MaxBandwidthMbps: 0,
			MaxGPUVRAMPct:    50,
			MaxPids:          512,
		},
		Scheduling: Scheduling{
			Mode:              "ALWAYS",
			IdleThresholdMins: 5,
		},
		Leafs: LeafFilter{
			Mode: "ALL",
		},
		ContainerGPURelaxUser: true,
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
			MaxThrottleMinutes:  30,
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

// KeyFilePath returns the resolved private-key path: the explicit KeyFile when
// set, otherwise <DataDir>/identity.key.
func (c *Config) KeyFilePath() string {
	if c.KeyFile != "" {
		return c.KeyFile
	}
	return filepath.Join(c.DataDir, "identity.key")
}

// PubKeyFilePath returns the resolved public-key path: the explicit PubKeyFile
// when set, otherwise <DataDir>/identity.pub.
func (c *Config) PubKeyFilePath() string {
	if c.PubKeyFile != "" {
		return c.PubKeyFile
	}
	return filepath.Join(c.DataDir, "identity.pub")
}

// SetLogOverrides records the --log-level / --log-file values for this run.
// Either may be empty to leave the configured value in force. The values are
// held outside the serialized struct, so they change what this process logs
// without ever being written back by Save.
func (c *Config) SetLogOverrides(level, file string) {
	c.logLevelOverride = level
	c.logFileOverride = file
}

// EffectiveLogLevel returns the level this process should log at: the
// --log-level override when one was given, otherwise the configured log_level.
func (c *Config) EffectiveLogLevel() string {
	if c.logLevelOverride != "" {
		return c.logLevelOverride
	}
	return c.LogLevel
}

// LogFilePath returns the resolved log file path: the --log-file override when
// one was given, else the explicit LogFile, otherwise
// <DataDir>/logs/volunteer.log.
func (c *Config) LogFilePath() string {
	if c.logFileOverride != "" {
		return c.logFileOverride
	}
	if c.LogFile != "" {
		return c.LogFile
	}
	return filepath.Join(c.DataDir, "logs", "volunteer.log")
}

// HostIDPath returns the resolved path of the RETIRED single-file host id. It is kept
// only for reference/diagnostics; nothing consults this file anymore (see HostIDsPath
// for the current head-issued, per-head store).
func (c *Config) HostIDPath() string {
	if c.HostIDFile != "" {
		return c.HostIDFile
	}
	return filepath.Join(c.DataDir, "host.id")
}

// HostIDsPath returns the path of the per-head host-id store (<DataDir>/host-ids.json),
// a JSON object mapping each head's gRPC address to the host id that head issued this
// machine. Head identity is minted server-side (BG-25); the client persists and echoes
// exactly what each head returns.
func (c *Config) HostIDsPath() string {
	return filepath.Join(c.DataDir, "host-ids.json")
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
	// Address repair first, so the entry-shape merge below keys on the address
	// a head is actually dialled at and a scheme-form duplicate of a clean entry
	// collapses into it (TB-62); then the merge (one entry per head, pins
	// merged), then trust: the merge keeps the head-level entry's fields, so
	// the trust migration seeds at most one entry per head from a coherent
	// starting point.
	repaired := cfg.repairServerAddresses()
	cfg.migrateServerEntries(repaired)
	cfg.migrateServerRuntimeTrust()
	return cfg, nil
}

// repairServerAddresses rewrites every server entry whose stored gRPC address
// is a shape gRPC's resolver can never dial — an http(s):// URL, or a host
// carrying a path — into the target ParseHeadAddress derives from it (TB-62).
// desktop-v2.0.0 and `init --server` before v0.12.0 stored the typed
// "https://host" verbatim, and PR #197 normalised only the three STORE paths,
// so such an entry survived every update, failed "name resolver error:
// produced zero addresses" on every start and showed as a down head until it
// was detached and re-added. Repairing at load makes the update itself the
// fix: gRPC and HTTP targets come from the parsed address, an http:// scheme
// marks the head insecure, and the name is replaced only when it is the
// scheme-derived junk those builds wrote ("https", or the URL itself); a name
// the volunteer chose is kept. Trust, pins, weight and TLS files are untouched.
//
// Only entries that could never have dialled are touched: a well-formed
// host:port is left byte-for-byte as stored, because the gRPC address is the
// key of the per-head host-id store (identity.HostIDStore) and a working
// entry must keep its head-issued id. A repaired entry never reached its
// head, so no id exists under the old key.
//
// The repair is idempotent and persisted by the next Save (registration with
// the head, or any settings write). It returns, per entry, whether that entry
// was repaired, for migrateServerEntries to prefer a clean duplicate's fields.
func (c *Config) repairServerAddresses() []bool {
	repaired := make([]bool, len(c.Servers))
	for i := range c.Servers {
		s := &c.Servers[i]
		stored := strings.TrimSpace(s.GRPCAddress)
		if !needsHeadAddressRepair(stored) {
			continue
		}
		addr, err := ParseHeadAddress(stored)
		if err != nil {
			// Not a URL either; leave it for start to report the dial failure.
			continue
		}
		oldName := s.Name
		s.GRPCAddress = addr.GRPCAddress()
		s.HTTPAddress = addr.HTTPAddress()
		if addr.Insecure {
			s.Insecure = true
		}
		if isSchemeDerivedName(s.Name) {
			s.Name = addr.Host
		}
		repaired[i] = true
		c.serverAddressRepairs = append(c.serverAddressRepairs, fmt.Sprintf(
			"head %q: the stored address %q was written by an older build as a URL, which could never be dialled; it now reads %s (HTTP %s, name %q) and is saved back at the next config write",
			oldName, stored, s.GRPCAddress, s.HTTPAddress, s.Name))
	}
	return repaired
}

// needsHeadAddressRepair reports whether a stored gRPC target is a shape the
// dial can never succeed on: a URL with a scheme ("https://host") or a host
// with a path ("host/leafs/x"). Everything else — host:port, a bare host, an
// IPv6 literal — is left exactly as stored (see repairServerAddresses).
func needsHeadAddressRepair(grpcAddr string) bool {
	return strings.Contains(grpcAddr, "/")
}

// isSchemeDerivedName reports whether a head's stored name is the junk the
// pre-v0.12.0 store paths derived from a URL input — "https" (init took the
// text before the last colon), the URL itself (the management attach used the
// raw address as the default name) — or empty. Such a name is replaced by the
// host; any other name was the volunteer's choice and is kept.
func isSchemeDerivedName(name string) bool {
	n := strings.TrimSpace(name)
	return n == "" || strings.Contains(n, "/") || strings.EqualFold(n, "http") || strings.EqualFold(n, "https")
}

// ServerAddressRepairs returns one line per server entry Load repaired from a
// never-dialable stored address (TB-62), naming the old and new targets, for
// the start-up log. Returns nil when every entry was well-formed.
func (c *Config) ServerAddressRepairs() []string {
	return c.serverAddressRepairs
}

// migrateServerEntries normalizes the servers list to ONE entry per gRPC
// address with explicit leaf pins (PB-16). Two legacy shapes are migrated:
//
//   - the retired single-leaf `leaf_id` field becomes a PinnedLeafIDs entry;
//   - whole duplicate entries for the same address — the old attach flow
//     APPENDED `{grpc_address, http_address, leaf_id, name}` for every
//     `attach <leaf-id>` — are merged into the head-level entry: its
//     connection fields (TLS, weight, preferences) win, the pins of every
//     duplicate are unioned, and TrustedRuntimes is merged trust-aware (see
//     mergeTrustedRuntimes) so an explicit decision — the empty "none"
//     included — survives no matter which entry it rode on (PB-28). Pre-fix,
//     the daemon collapsed such duplicates at startup and silently DISCARDED
//     the pin, which made unlisted leafs permanently unreachable for CLI
//     volunteers.
//
// A third shape joins them with TB-62: an entry repairServerAddresses just
// rewrote from a never-dialable URL now shares its address with a clean entry
// the volunteer added later (the "add it again" workaround, without the
// detach). They merge like the leaf-pin case — the clean entry's connection
// fields win, because the repaired one never reached the head — with pins
// unioned and trust merged. repaired says, per entry of c.Servers, which ones
// were rewritten; nil means none.
//
// The migration is idempotent and pinned by the next Save (leaf_id is never
// written again).
func (c *Config) migrateServerEntries(repaired []bool) {
	if len(c.Servers) == 0 {
		return
	}
	merged := make([]ServerConfig, 0, len(c.Servers))
	// leafOnly tracks whether a merged entry came from a bare leaf-pin append
	// (it carried only address/leaf/name), so a later HEAD-LEVEL entry for the
	// same address can take over the connection fields. wasRepaired tracks
	// the same for an address-repaired entry (TB-62): a later clean entry for
	// the address takes over.
	leafOnly := make([]bool, 0, len(c.Servers))
	wasRepaired := make([]bool, 0, len(c.Servers))
	indexByAddr := make(map[string]int, len(c.Servers))

	for idx, s := range c.Servers {
		wasLeafEntry := s.LeafID != ""
		if wasLeafEntry {
			s.PinnedLeafIDs = appendUniqueString(s.PinnedLeafIDs, s.LeafID)
			s.LeafID = ""
		}
		thisRepaired := idx < len(repaired) && repaired[idx]
		i, seen := indexByAddr[s.GRPCAddress]
		if !seen {
			indexByAddr[s.GRPCAddress] = len(merged)
			merged = append(merged, s)
			leafOnly = append(leafOnly, wasLeafEntry)
			wasRepaired = append(wasRepaired, thisRepaired)
			continue
		}
		takeOver := (leafOnly[i] && !wasLeafEntry) || (wasRepaired[i] && !wasLeafEntry && !thisRepaired)
		if takeOver {
			// The kept entry was a bare leaf pin, or a repaired URL entry that
			// never dialled, and this one is the real head-level entry: adopt
			// its connection fields, keep the union of pins and the merged
			// trust.
			pins := merged[i].PinnedLeafIDs
			trust := merged[i].TrustedRuntimes
			merged[i] = s
			merged[i].TrustedRuntimes = mergeTrustedRuntimes(s.TrustedRuntimes, trust)
			for _, p := range pins {
				merged[i].PinnedLeafIDs = appendUniqueString(merged[i].PinnedLeafIDs, p)
			}
			leafOnly[i] = false
			wasRepaired[i] = thisRepaired
			continue
		}
		merged[i].TrustedRuntimes = mergeTrustedRuntimes(merged[i].TrustedRuntimes, s.TrustedRuntimes)
		for _, p := range s.PinnedLeafIDs {
			merged[i].PinnedLeafIDs = appendUniqueString(merged[i].PinnedLeafIDs, p)
		}
	}
	c.Servers = merged
}

// mergeTrustedRuntimes combines the per-head trust of two entries being merged
// for the same address (PB-28): an explicit decision (non-nil, the empty
// "none" included) must never be lost to a legacy nil, and when both entries
// carry an explicit decision the merge keeps the intersection — the most
// restrictive reading, so merging duplicates can only ever narrow trust,
// never widen it. Returns nil only when both inputs are nil (a genuinely
// legacy head, left for migrateServerRuntimeTrust to seed).
func mergeTrustedRuntimes(a, b []string) []string {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := make([]string, 0, len(a))
	for _, r := range a {
		if containsFold(b, strings.TrimSpace(r)) {
			out = append(out, r)
		}
	}
	return out
}

// appendUniqueString appends s to list unless already present (or empty).
func appendUniqueString(list []string, s string) []string {
	if s == "" {
		return list
	}
	for _, e := range list {
		if e == s {
			return list
		}
	}
	return append(list, s)
}

// migrateServerRuntimeTrust pins per-head TrustedRuntimes for any server entry
// that never recorded a trust decision (a NIL list — the key absent from the
// file) to the explicit WASM-only empty list. Trusting a head to run CONTAINER
// or NATIVE code is a consent decision made at attach (or via `heads trust`);
// an entry with no recorded decision gets the safe default rather than a value
// inferred from the retired global keys available_runtimes /
// allow_native_runtime, which used to silently grant CONTAINER trust to every
// entry written without one (TB-25, and the DA-3 consent gap). WASM needs no
// entry — it is implicitly trusted always (EffectiveTrustedRuntimes).
//
// A present-but-empty list is an explicit "WASM only" the volunteer chose at
// attach or via `heads trust none` and is left untouched (PB-28). The pinned
// result is always non-nil so one load settles the question and a later save
// records it explicitly.
func (c *Config) migrateServerRuntimeTrust() {
	for i := range c.Servers {
		if c.Servers[i].TrustedRuntimes != nil {
			continue // an explicit per-head choice, including the empty "WASM only"
		}
		c.Servers[i].TrustedRuntimes = []string{}
	}
}

// containsFold reports whether list contains want, case-insensitively and ignoring
// surrounding whitespace.
func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), want) {
			return true
		}
	}
	return false
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
	// The two retired global runtime keys (TB-25 / TQ-22). Which runtimes actually
	// run was never governed by them on current builds — per-head trust plus a live
	// engine probe decide — so the keys sat in config.yaml reading as authoritative
	// while contradicting the daemon. Name the real mechanism in the advisory.
	"available_runtimes":   `retired: which runtimes run is decided per head by servers[].trusted_runtimes (see "lettuce-volunteer heads trust") plus a live engine check at daemon start. Delete the key.`,
	"allow_native_runtime": `retired: NATIVE is enabled per head via servers[].trusted_runtimes (see "lettuce-volunteer heads trust"). Delete the key.`,
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
	"container_cap_add":     "Linux capabilities to re-add to hardened containers. Default none (containers drop all capabilities).",
	"container_gpu_relax_user": "Let GPU leaves relax the non-root/minimal-capability container posture when device access needs it. CPU leaves stay fully hardened.",
	"resource_limits":      "Per-task resource ceilings. A head only sends leafs whose requirements fit under these - too low and you silently get no work.",
	"scheduling":           "When the volunteer runs.",
	"thermal":              "Hardware overheating protection. Temperatures in degrees C, NOT workload limits: ALL work freezes above the pause threshold and resumes below the resume threshold.",
}

var resourceLimitsComments = map[string]string{
	"max_cpu_cores":      "Max CPU cores a single work unit may use.",
	"max_memory_mb":      "Memory ceiling. A head only sends leafs whose per-unit memory fits under this; set it too low and you match no work.",
	"max_disk_gb":        "Disk capacity you offer: a head only sends leafs whose declared disk need fits under this, and Lettuce keeps its own footprint (work folders + container images) within it. A download needs only the LEAF's declared disk free (plus a 2 GB floor), never this whole number.",
	"max_bandwidth_mbps": "Bandwidth cap in Mbps. 0 = unlimited.",
	"max_gpu_vram_pct":   "Max percent of each GPU's VRAM a task may use. A head compares a leaf's VRAM requirement against this share of your card, not the card itself, so at the default 50% a 6 GB card offers 3072 MB. 0 disables GPU work entirely.",
	"max_pids":           "Max simultaneous processes/threads inside a container (fork-bomb cap). 0 uses the built-in default.",
}

var thermalComments = map[string]string{
	"enabled":               "Master switch for thermal protection.",
	"max_throttle_minutes":  "How long work may stay frozen on one continuous overheat before the client resumes and re-checks. Stops a sensor that never cools (often not the CPU) from freezing you indefinitely. Negative means wait forever.",
	"cpu_pause_threshold":   "degrees C - freeze ALL work when the CPU reaches this.",
	"cpu_resume_threshold":  "degrees C - resume once the CPU cools below this (must be < cpu_pause_threshold).",
	"gpu_pause_threshold":   "degrees C - freeze ALL work when the GPU reaches this.",
	"gpu_resume_threshold":  "degrees C - resume once the GPU cools below this (must be < gpu_pause_threshold).",
	"poll_interval_seconds": "How often temperatures are sampled, in seconds.",
}

var schedulingComments = map[string]string{
	"mode":                "ALWAYS, WHEN_IDLE (only when the machine is idle), or SCHEDULED (time windows).",
	"idle_threshold_mins": "WHEN_IDLE only: minutes of inactivity before work starts.",
	"cron_expression":     "SCHEDULED only: a valid 5-field cron expression (minute hour day-of-month month day-of-week). Prefer schedule_ranges - see `lettuce-volunteer schedule set`.",
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

	if err := c.Scheduling.Validate(); err != nil {
		return err
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
			// SPECIFIC with no enabled leaf is a valid, deliberate state: "stay
			// attached to this head but take none of its work" — what the desktop
			// app writes when the last box on a head is unchecked and what `leafs
			// disable` writes for the last enabled leaf. Every reader of the
			// per-head preference selects exactly the listed slugs, so an empty
			// list selects nothing; it is never read as "all" (TB-65).
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
		if c.Thermal.MaxThrottleMinutes > 1440 {
			return fmt.Errorf("thermal.max_throttle_minutes must be <= 1440 (24h), got %d", c.Thermal.MaxThrottleMinutes)
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
	case "thermal.max_throttle_minutes":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer for %s: %w", dotPath, err)
		}
		c.Thermal.MaxThrottleMinutes = v
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
	case "thermal.max_throttle_minutes":
		return strconv.Itoa(c.Thermal.MaxThrottleMinutes), nil
	default:
		return "", fmt.Errorf("unknown config path: %s", dotPath)
	}
}
