package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Config is the top-level configuration for the Lettuce infrastructure server.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Log      LogConfig      `yaml:"log"`
	TLS      TLSConfig      `yaml:"tls"`
	Signing  SigningConfig  `yaml:"signing"`
	Storage  StorageConfig  `yaml:"storage"`
	Head     HeadConfig     `yaml:"head"`
}

// HeadConfig defines the identity for this Lettuce server (head).
// A head is a single infrastructure deployment — one server = one head = many leafs.
type HeadConfig struct {
	Name                    string         `yaml:"name"`
	Description             string         `yaml:"description"`
	URL                     string         `yaml:"url"`
	DefaultLeafWeights      map[string]int `yaml:"default_leaf_weights"`
	MaxInflightPerVolunteer int            `yaml:"max_inflight_per_volunteer"`

	// --- Layer 1: work batching, server-directed retry delay, buffer lease ---

	// MaxBatchPerRequest caps how many assignments a single RequestWorkUnit may
	// return (the client may request fewer via max_assignments). Default 8.
	MaxBatchPerRequest int `yaml:"max_batch_per_request"`
	// MinRetryDelaySeconds is the server-directed retry delay handed out when the
	// head is quiet. Default 30.
	MinRetryDelaySeconds int `yaml:"min_retry_delay_seconds"`
	// MaxRetryDelaySeconds is the retry delay under full load. Must stay strictly
	// below the 30-min StaleVolunteerMonitor threshold. Default 900 (15 min).
	MaxRetryDelaySeconds int `yaml:"max_retry_delay_seconds"`
	// RetryDelayJitterPct is the ±fraction of uniform jitter applied server-side
	// to the computed delay so a fleet does not re-contact in lockstep. Default 0.20.
	RetryDelayJitterPct float64 `yaml:"retry_delay_jitter_pct"`
	// TargetRequestRatePerSec is the per-head RequestWorkUnit rate that maps to
	// load=1 for the rate signal. SIMULATOR-CALIBRATED, not a trusted default.
	// Default 500.
	TargetRequestRatePerSec float64 `yaml:"target_request_rate_per_sec"`
	// LeaseSeconds is how long a buffered (reserved) unit is held for a volunteer
	// before the lease lapses and the unit becomes re-reservable. Must stay below
	// the 30-min stale threshold. Default 900 (15 min).
	LeaseSeconds int `yaml:"lease_seconds"`

	// --- Layer 2: in-process dispatch cache (SINGLE-REPLICA ONLY) ---
	//
	// The dispatch cache serves RequestWorkUnit from an in-memory pool bulk-refilled
	// from Postgres, flushing reservations asynchronously, so the DB is off the
	// hot path. It assumes ONE head process owns dispatch: two replicas would each
	// refill independently and double-hand the same QUEUED unit. Run exactly one
	// head replica until Layer 3 adds shared dispatch ownership.

	// ReadyPoolSize bounds the in-memory pool of pre-fetched, ready-to-assign QUEUED
	// units. Default 2000.
	ReadyPoolSize int `yaml:"ready_pool_size"`
	// RefillBatchSize is how many units one bulk refill (LIMIT N) pulls from
	// Postgres. Default 500.
	RefillBatchSize int `yaml:"refill_batch_size"`
	// DispatchAdmissionCap bounds concurrent CLIENT write-path dispatch-cache DB
	// operations — StartWork / SubmitResult / AbandonWorkUnit gates, the
	// RequestWorkUnit cold-miss identity read, getLeaf, resolveIdentity — so the pool
	// cannot saturate. The background refiller, ticker reservation-flush, and
	// spot-check landing run on the SEPARATE MaintenanceAdmissionCap budget so client
	// writers cannot starve cache restock. Default max(1, MaxConns/2); 0 = derive
	// from the pool.
	DispatchAdmissionCap int `yaml:"dispatch_admission_cap"`
	// MaintenanceAdmissionCap is the reserved admission budget for background restock
	// + landing (refiller, ticker reservation-flush, spot-check flush) so client
	// writes cannot starve them. Default max(1, dispatch_admission_cap/4); 0 = derive.
	MaintenanceAdmissionCap int `yaml:"maintenance_admission_cap"`
	// FlushIntervalMs is the async reservation-flush cadence (milliseconds).
	// Default 100.
	FlushIntervalMs int `yaml:"flush_interval_ms"`
	// FlushBatchSize flushes early once this many pending reservation writes
	// accumulate. Default 200.
	FlushBatchSize int `yaml:"flush_batch_size"`
	// NoDeadlineCeilingSeconds is the synthetic reclaim ceiling applied to NoDeadline
	// leafs so a unit on a vanished volunteer is always reclaimed (heartbeats are
	// gone). This is the DEADLINE, not a lease, so it is NOT bound by the 30-min
	// stale threshold. Default 21600 (6h), operator-tunable.
	NoDeadlineCeilingSeconds int `yaml:"no_deadline_ceiling_seconds"`
}

// Layer-1 defaults and the stale-volunteer threshold both delays and the lease
// must stay strictly below.
const (
	// defaultMaxBatchPerRequest is a SAFETY CEILING on the per-request batch, not
	// the primary limiter — Layer 2 (#29) makes the client's hours-deficit math the
	// real limiter, so this is raised to 64 to let short-unit leafs fill their
	// work_buffer_hours instead of idling at the old flat cap of 8.
	defaultMaxBatchPerRequest      = 64
	defaultMinRetryDelaySeconds    = 30
	defaultMaxRetryDelaySeconds    = 900
	defaultRetryDelayJitterPct     = 0.20
	defaultTargetRequestRatePerSec = 500.0
	defaultLeaseSeconds            = 900
	// staleVolunteerThresholdSeconds mirrors StaleVolunteerMonitor's 30-min
	// inactivity threshold; retry delay and lease must stay strictly below it so a
	// throttled-but-healthy volunteer is never marked inactive.
	staleVolunteerThresholdSeconds = 1800

	// --- Layer 2 dispatch-cache defaults ---
	defaultReadyPoolSize            = 2000
	defaultRefillBatchSize          = 500
	defaultFlushIntervalMs          = 100
	defaultFlushBatchSize           = 200
	defaultNoDeadlineCeilingSeconds = 21600 // 6h
)

// Validate checks HeadConfig for required fields and valid values.
func (h HeadConfig) Validate() error {
	if h.Name == "" {
		return fmt.Errorf("head.name is required")
	}
	if h.URL != "" {
		u, err := url.Parse(h.URL)
		if err != nil {
			return fmt.Errorf("head.url is invalid: %w", err)
		}
		if u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("head.url must include scheme and host (e.g. https://your-domain.com)")
		}
	}
	for slug, weight := range h.DefaultLeafWeights {
		if weight <= 0 {
			return fmt.Errorf("head.default_leaf_weights[%q] must be > 0, got %d", slug, weight)
		}
	}
	if h.MaxInflightPerVolunteer < 0 {
		return fmt.Errorf("head.max_inflight_per_volunteer must be >= 0, got %d", h.MaxInflightPerVolunteer)
	}
	if h.MaxBatchPerRequest < 0 {
		return fmt.Errorf("head.max_batch_per_request must be >= 0, got %d", h.MaxBatchPerRequest)
	}
	if h.MinRetryDelaySeconds < 0 {
		return fmt.Errorf("head.min_retry_delay_seconds must be >= 0, got %d", h.MinRetryDelaySeconds)
	}
	if h.MaxRetryDelaySeconds < 0 {
		return fmt.Errorf("head.max_retry_delay_seconds must be >= 0, got %d", h.MaxRetryDelaySeconds)
	}
	if h.MaxRetryDelaySeconds >= staleVolunteerThresholdSeconds {
		return fmt.Errorf("head.max_retry_delay_seconds must be < %d (the stale-volunteer threshold), got %d",
			staleVolunteerThresholdSeconds, h.MaxRetryDelaySeconds)
	}
	if h.MinRetryDelaySeconds > 0 && h.MaxRetryDelaySeconds > 0 && h.MinRetryDelaySeconds > h.MaxRetryDelaySeconds {
		return fmt.Errorf("head.min_retry_delay_seconds (%d) must be <= max_retry_delay_seconds (%d)",
			h.MinRetryDelaySeconds, h.MaxRetryDelaySeconds)
	}
	if h.RetryDelayJitterPct < 0 || h.RetryDelayJitterPct >= 1 {
		return fmt.Errorf("head.retry_delay_jitter_pct must be in [0, 1), got %v", h.RetryDelayJitterPct)
	}
	if h.TargetRequestRatePerSec < 0 {
		return fmt.Errorf("head.target_request_rate_per_sec must be >= 0, got %v", h.TargetRequestRatePerSec)
	}
	if h.LeaseSeconds < 0 {
		return fmt.Errorf("head.lease_seconds must be >= 0, got %d", h.LeaseSeconds)
	}
	if h.LeaseSeconds >= staleVolunteerThresholdSeconds {
		return fmt.Errorf("head.lease_seconds must be < %d (the stale-volunteer threshold), got %d",
			staleVolunteerThresholdSeconds, h.LeaseSeconds)
	}
	if h.ReadyPoolSize < 0 {
		return fmt.Errorf("head.ready_pool_size must be >= 0, got %d", h.ReadyPoolSize)
	}
	if h.RefillBatchSize < 0 {
		return fmt.Errorf("head.refill_batch_size must be >= 0, got %d", h.RefillBatchSize)
	}
	if h.DispatchAdmissionCap < 0 {
		return fmt.Errorf("head.dispatch_admission_cap must be >= 0, got %d", h.DispatchAdmissionCap)
	}
	if h.MaintenanceAdmissionCap < 0 {
		return fmt.Errorf("head.maintenance_admission_cap must be >= 0, got %d", h.MaintenanceAdmissionCap)
	}
	if h.FlushIntervalMs < 0 {
		return fmt.Errorf("head.flush_interval_ms must be >= 0, got %d", h.FlushIntervalMs)
	}
	if h.FlushBatchSize < 0 {
		return fmt.Errorf("head.flush_batch_size must be >= 0, got %d", h.FlushBatchSize)
	}
	// NoDeadlineCeilingSeconds is a DEADLINE, not a lease, so it is intentionally
	// NOT bound by the 30-min stale threshold (a 6h reclaim ceiling is valid).
	if h.NoDeadlineCeilingSeconds < 0 {
		return fmt.Errorf("head.no_deadline_ceiling_seconds must be >= 0, got %d", h.NoDeadlineCeilingSeconds)
	}
	return nil
}

// EffectiveMaxInflight returns the max inflight WUs per volunteer,
// defaulting to 10 if not set (0).
func (h HeadConfig) EffectiveMaxInflight() int {
	if h.MaxInflightPerVolunteer <= 0 {
		return 10
	}
	return h.MaxInflightPerVolunteer
}

// EffectiveMaxBatch returns the per-request batch cap, defaulting to 8 if unset.
func (h HeadConfig) EffectiveMaxBatch() int {
	if h.MaxBatchPerRequest <= 0 {
		return defaultMaxBatchPerRequest
	}
	return h.MaxBatchPerRequest
}

// EffectiveMinRetryDelaySeconds returns the quiet-load retry delay, default 30.
func (h HeadConfig) EffectiveMinRetryDelaySeconds() int {
	if h.MinRetryDelaySeconds <= 0 {
		return defaultMinRetryDelaySeconds
	}
	return h.MinRetryDelaySeconds
}

// EffectiveMaxRetryDelaySeconds returns the full-load retry delay, default 900.
func (h HeadConfig) EffectiveMaxRetryDelaySeconds() int {
	if h.MaxRetryDelaySeconds <= 0 {
		return defaultMaxRetryDelaySeconds
	}
	return h.MaxRetryDelaySeconds
}

// EffectiveRetryDelayJitterPct returns the jitter fraction, default 0.20.
// A configured 0 is treated as "use the default"; disable jitter is not a
// supported mode (the anti-synchronization property is load-bearing).
func (h HeadConfig) EffectiveRetryDelayJitterPct() float64 {
	if h.RetryDelayJitterPct <= 0 {
		return defaultRetryDelayJitterPct
	}
	return h.RetryDelayJitterPct
}

// EffectiveTargetRequestRatePerSec returns the rate-signal target, default 500.
func (h HeadConfig) EffectiveTargetRequestRatePerSec() float64 {
	if h.TargetRequestRatePerSec <= 0 {
		return defaultTargetRequestRatePerSec
	}
	return h.TargetRequestRatePerSec
}

// EffectiveLeaseSeconds returns the buffer-lease window, default 900.
func (h HeadConfig) EffectiveLeaseSeconds() int {
	if h.LeaseSeconds <= 0 {
		return defaultLeaseSeconds
	}
	return h.LeaseSeconds
}

// EffectiveReadyPoolSize returns the dispatch-cache ready-pool cap, default 2000.
func (h HeadConfig) EffectiveReadyPoolSize() int {
	if h.ReadyPoolSize <= 0 {
		return defaultReadyPoolSize
	}
	return h.ReadyPoolSize
}

// EffectiveRefillBatchSize returns the bulk-refill LIMIT, default 500.
func (h HeadConfig) EffectiveRefillBatchSize() int {
	if h.RefillBatchSize <= 0 {
		return defaultRefillBatchSize
	}
	return h.RefillBatchSize
}

// EffectiveDispatchAdmissionCap returns the configured admission cap, or 0 to let
// the caller derive it from the DB pool (max(1, MaxConns/2)).
func (h HeadConfig) EffectiveDispatchAdmissionCap() int {
	if h.DispatchAdmissionCap < 0 {
		return 0
	}
	return h.DispatchAdmissionCap
}

// EffectiveMaintenanceAdmissionCap returns the configured reserved maintenance
// admission budget, or 0 to let the caller derive it (max(1, admissionCap/4)).
func (h HeadConfig) EffectiveMaintenanceAdmissionCap() int {
	if h.MaintenanceAdmissionCap < 0 {
		return 0
	}
	return h.MaintenanceAdmissionCap
}

// EffectiveFlushIntervalMs returns the async flush cadence in ms, default 100.
func (h HeadConfig) EffectiveFlushIntervalMs() int {
	if h.FlushIntervalMs <= 0 {
		return defaultFlushIntervalMs
	}
	return h.FlushIntervalMs
}

// EffectiveFlushBatchSize returns the early-flush threshold, default 200.
func (h HeadConfig) EffectiveFlushBatchSize() int {
	if h.FlushBatchSize <= 0 {
		return defaultFlushBatchSize
	}
	return h.FlushBatchSize
}

// EffectiveNoDeadlineCeilingSeconds returns the synthetic reclaim ceiling for
// NoDeadline leafs, default 21600 (6h).
func (h HeadConfig) EffectiveNoDeadlineCeilingSeconds() int {
	if h.NoDeadlineCeilingSeconds <= 0 {
		return defaultNoDeadlineCeilingSeconds
	}
	return h.NoDeadlineCeilingSeconds
}

// StorageConfig defines local filesystem storage settings.
type StorageConfig struct {
	CheckpointDir string `yaml:"checkpoint_dir"`
}

// SigningConfig defines the Ed25519 signing key used for credit attestations.
type SigningConfig struct {
	PrivateKeyPath string `yaml:"private_key_path"`
	// AutoGenerate, when true, lets the server generate and persist a new
	// ephemeral signing key if the configured key file is missing. This is a
	// development-only convenience: in production the key is the platform's
	// external trust anchor and must be pre-generated. Defaults to false
	// (fail closed). Override via LETTUCE_SIGNING_KEY_AUTOGEN=true.
	AutoGenerate bool `yaml:"autogenerate"`
}

// ServerConfig defines listen addresses for HTTP and gRPC servers.
type ServerConfig struct {
	HTTPAddr    string `yaml:"http_addr"`
	GRPCAddr    string `yaml:"grpc_addr"`
	CORSOrigins string `yaml:"cors_origins"`
	// TrustedProxies is a comma-separated list of CIDRs and/or bare IPs of
	// reverse proxies whose X-Forwarded-For / X-Real-IP headers may be trusted
	// for client-IP extraction. Bare IPs are treated as /32 (IPv4) or /128
	// (IPv6). EMPTY by default: when empty, forwarding headers are never trusted
	// and the direct peer (RemoteAddr) is always used. Override via
	// LETTUCE_TRUSTED_PROXIES.
	TrustedProxies string `yaml:"trusted_proxies"`
}

// ParsedTrustedProxies parses the TrustedProxies string into a slice of
// *net.IPNet. Comma-separated entries may be CIDRs (e.g. "10.0.0.0/8") or bare
// IPs (e.g. "172.18.0.5"), where a bare IP is treated as a /32 (IPv4) or /128
// (IPv6) network. Empty or whitespace-only entries are skipped. Returns an
// error on the first malformed entry. An empty input yields a nil slice
// (the secure default: no header trust).
func (s ServerConfig) ParsedTrustedProxies() ([]*net.IPNet, error) {
	return ParseTrustedProxies(s.TrustedProxies)
}

// ParseTrustedProxies parses a comma-separated list of CIDRs and/or bare IPs
// into *net.IPNet networks. See ServerConfig.ParsedTrustedProxies for semantics.
func ParseTrustedProxies(raw string) ([]*net.IPNet, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Try CIDR first.
		if _, ipNet, err := net.ParseCIDR(entry); err == nil {
			nets = append(nets, ipNet)
			continue
		}
		// Fall back to a bare IP → /32 or /128.
		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, fmt.Errorf("trusted_proxies: invalid CIDR or IP %q", entry)
		}
		mask := net.CIDRMask(32, 32)
		if ip.To4() == nil {
			mask = net.CIDRMask(128, 128)
		}
		nets = append(nets, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
	}
	return nets, nil
}

// DatabaseConfig defines PostgreSQL connection parameters.
type DatabaseConfig struct {
	Host            string `yaml:"host"`
	Port            int    `yaml:"port"`
	Database        string `yaml:"database"`
	User            string `yaml:"user"`
	Password        string `yaml:"password"`
	SSLMode         string `yaml:"ssl_mode"`
	MaxConns        int    `yaml:"max_conns"`
	MinConns        int    `yaml:"min_conns"`
	MaxConnLifetime string `yaml:"max_conn_lifetime"`
	MaxConnIdleTime string `yaml:"max_conn_idle_time"`
}

// DatabaseURL returns a pgx-compatible connection string.
func (d DatabaseConfig) DatabaseURL() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(d.User, d.Password),
		Host:   fmt.Sprintf("%s:%d", d.Host, d.Port),
		Path:   d.Database,
	}
	q := u.Query()
	q.Set("sslmode", d.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// LogConfig defines logging behavior.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// TLSConfig defines TLS certificate paths.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	CAFile   string `yaml:"ca_file"`
}
