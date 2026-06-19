package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// parseIP parses s into a net.IP, failing the test on error.
func parseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("parseIP: invalid %q", s)
	}
	return ip
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	// Auto-inject head.name if not present so existing tests pass
	// after head.name became required.
	if !strings.Contains(content, "head:") && !strings.Contains(content, "{{{") {
		if strings.TrimSpace(content) == "{}" {
			content = "head: { name: \"test\" }"
		} else {
			content += "\nhead: { name: \"test\" }\n"
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lettuce.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// minimalConfig is the smallest valid config (head.name is required).
const minimalConfig = `head: { name: "test" }`

func clearLettuceEnv(t *testing.T) {
	t.Helper()
	envVars := []string{
		"LETTUCE_SERVER_HTTP_ADDR", "LETTUCE_SERVER_GRPC_ADDR", "LETTUCE_CORS_ORIGINS",
		"LETTUCE_TRUSTED_PROXIES",
		"LETTUCE_DB_HOST", "LETTUCE_DB_PORT", "LETTUCE_DB_DATABASE",
		"LETTUCE_DB_USER", "LETTUCE_DB_PASSWORD", "LETTUCE_DB_SSL_MODE",
		"LETTUCE_DB_MAX_CONNS", "LETTUCE_DB_MIN_CONNS",
		"LETTUCE_DB_MAX_CONN_LIFETIME", "LETTUCE_DB_MAX_CONN_IDLE_TIME",
		"LETTUCE_LOG_LEVEL", "LETTUCE_LOG_FORMAT",
		"LETTUCE_TLS_CERT_FILE", "LETTUCE_TLS_KEY_FILE", "LETTUCE_TLS_CA_FILE",
		"LETTUCE_SIGNING_PRIVATE_KEY_PATH",
		"LETTUCE_HEAD_NAME", "LETTUCE_HEAD_DESCRIPTION", "LETTUCE_HEAD_URL",
		"LETTUCE_HEAD_INSTANCE_ID", "LETTUCE_REDIS_URL", "LETTUCE_REPLAY_FAIL_MODE",
		"LETTUCE_HEAD_CLAIM_LEASE_SECONDS",
	}
	for _, e := range envVars {
		if orig, ok := os.LookupEnv(e); ok {
			t.Cleanup(func() { os.Setenv(e, orig) })
		} else {
			t.Cleanup(func() { os.Unsetenv(e) })
		}
		os.Unsetenv(e)
	}
}

func TestLoadValidConfig(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, `
server:
  http_addr: ":3000"
  grpc_addr: ":4000"
database:
  host: "db.example.com"
  port: 5433
  database: "mydb"
  user: "myuser"
  password: "secret"
  ssl_mode: "require"
  max_conns: 10
  min_conns: 2
  max_conn_lifetime: "2h"
  max_conn_idle_time: "15m"
log:
  level: "debug"
  format: "text"
head:
  name: "test-server"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.HTTPAddr != ":3000" {
		t.Errorf("HTTPAddr = %q, want %q", cfg.Server.HTTPAddr, ":3000")
	}
	if cfg.Server.GRPCAddr != ":4000" {
		t.Errorf("GRPCAddr = %q, want %q", cfg.Server.GRPCAddr, ":4000")
	}
	if cfg.Database.Host != "db.example.com" {
		t.Errorf("Database.Host = %q, want %q", cfg.Database.Host, "db.example.com")
	}
	if cfg.Database.Port != 5433 {
		t.Errorf("Database.Port = %d, want %d", cfg.Database.Port, 5433)
	}
	if cfg.Database.Password != "secret" {
		t.Errorf("Database.Password = %q, want %q", cfg.Database.Password, "secret")
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "debug")
	}
	if cfg.Log.Format != "text" {
		t.Errorf("Log.Format = %q, want %q", cfg.Log.Format, "text")
	}
}

func TestLoadMissingFile(t *testing.T) {
	clearLettuceEnv(t)
	_, err := Load("/nonexistent/path/lettuce.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, `{{{invalid yaml!!!`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestDefaults(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, minimalConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tests := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"Server.HTTPAddr", cfg.Server.HTTPAddr, ":8080"},
		{"Server.GRPCAddr", cfg.Server.GRPCAddr, ":9090"},
		{"Database.Host", cfg.Database.Host, "localhost"},
		{"Database.Port", cfg.Database.Port, 5432},
		{"Database.Database", cfg.Database.Database, "lettuce"},
		{"Database.User", cfg.Database.User, "lettuce"},
		{"Database.SSLMode", cfg.Database.SSLMode, "prefer"},
		{"Database.MaxConns", cfg.Database.MaxConns, 25},
		{"Database.MinConns", cfg.Database.MinConns, 5},
		{"Database.MaxConnLifetime", cfg.Database.MaxConnLifetime, "1h"},
		{"Database.MaxConnIdleTime", cfg.Database.MaxConnIdleTime, "30m"},
		{"Log.Level", cfg.Log.Level, "info"},
		{"Log.Format", cfg.Log.Format, "json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestEnvOverrides(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, `
database:
  password: "yaml-password"
  port: 5432
log:
  level: "info"
`)
	t.Setenv("LETTUCE_DB_PASSWORD", "env-password")
	t.Setenv("LETTUCE_DB_PORT", "9999")
	t.Setenv("LETTUCE_LOG_LEVEL", "debug")
	t.Setenv("LETTUCE_SERVER_HTTP_ADDR", ":7070")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Database.Password != "env-password" {
		t.Errorf("Password = %q, want %q", cfg.Database.Password, "env-password")
	}
	if cfg.Database.Port != 9999 {
		t.Errorf("Port = %d, want %d", cfg.Database.Port, 9999)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "debug")
	}
	if cfg.Server.HTTPAddr != ":7070" {
		t.Errorf("HTTPAddr = %q, want %q", cfg.Server.HTTPAddr, ":7070")
	}
}

func TestEnvOverrideInvalidPort(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, minimalConfig)
	t.Setenv("LETTUCE_DB_PORT", "abc")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for non-numeric LETTUCE_DB_PORT, got nil")
	}
}

func TestEnvOverrideInvalidMaxConns(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, minimalConfig)
	t.Setenv("LETTUCE_DB_MAX_CONNS", "xyz")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for non-numeric LETTUCE_DB_MAX_CONNS, got nil")
	}
}

func TestEnvOverrideConnLifetime(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, minimalConfig)
	t.Setenv("LETTUCE_DB_MAX_CONN_LIFETIME", "2h")
	t.Setenv("LETTUCE_DB_MAX_CONN_IDLE_TIME", "45m")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Database.MaxConnLifetime != "2h" {
		t.Errorf("MaxConnLifetime = %q, want %q", cfg.Database.MaxConnLifetime, "2h")
	}
	if cfg.Database.MaxConnIdleTime != "45m" {
		t.Errorf("MaxConnIdleTime = %q, want %q", cfg.Database.MaxConnIdleTime, "45m")
	}
}

func TestValidation(t *testing.T) {
	clearLettuceEnv(t)

	tests := []struct {
		name    string
		config  string
		wantErr bool
	}{
		{
			name:    "port zero gets defaulted",
			config:  `database: { port: 0, password: "x" }`,
			wantErr: false,
		},
		{
			name:    "invalid port negative",
			config:  `database: { port: -1, password: "x" }`,
			wantErr: true,
		},
		{
			name:    "invalid port too high",
			config:  `database: { port: 70000, password: "x" }`,
			wantErr: true,
		},
		{
			name:    "max_conns negative",
			config:  `database: { password: "x", max_conns: -1 }`,
			wantErr: true,
		},
		{
			name:    "min_conns greater than max_conns",
			config:  `database: { password: "x", max_conns: 5, min_conns: 10 }`,
			wantErr: true,
		},
		{
			name:    "invalid log level",
			config:  `log: { level: "verbose" }`,
			wantErr: true,
		},
		{
			name:    "invalid log format",
			config:  `log: { format: "xml" }`,
			wantErr: true,
		},
		{
			name:    "invalid ssl mode",
			config:  `database: { password: "x", ssl_mode: "bogus" }`,
			wantErr: true,
		},
		{
			name:    "invalid max_conn_lifetime duration",
			config:  `database: { password: "x", max_conn_lifetime: "not-a-duration" }`,
			wantErr: true,
		},
		{
			name:    "invalid max_conn_idle_time duration",
			config:  `database: { password: "x", max_conn_idle_time: "nope" }`,
			wantErr: true,
		},
		{
			name:    "tls cert without key",
			config:  `tls: { cert_file: "/path/cert.pem" }`,
			wantErr: true,
		},
		{
			name:    "tls key without cert",
			config:  `tls: { key_file: "/path/key.pem" }`,
			wantErr: true,
		},
		{
			name:    "valid tls both set",
			config:  "tls: { cert_file: \"/path/cert.pem\", key_file: \"/path/key.pem\" }\nhead: { name: \"test\" }",
			wantErr: false,
		},
		{
			name:    "valid tls both empty",
			config:  minimalConfig,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearLettuceEnv(t)
			path := writeTestConfig(t, tt.config)
			_, err := Load(path)
			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDatabaseURL(t *testing.T) {
	cfg := DatabaseConfig{
		Host:     "db.example.com",
		Port:     5433,
		Database: "mydb",
		User:     "myuser",
		Password: "s3cret",
		SSLMode:  "require",
	}
	got := cfg.DatabaseURL()
	want := "postgres://myuser:s3cret@db.example.com:5433/mydb?sslmode=require"
	if got != want {
		t.Errorf("DatabaseURL() = %q, want %q", got, want)
	}
}

func TestDatabaseURLSpecialChars(t *testing.T) {
	cfg := DatabaseConfig{
		Host:     "localhost",
		Port:     5432,
		Database: "lettuce",
		User:     "lettuce",
		Password: "p@ss:word/special",
		SSLMode:  "prefer",
	}
	got := cfg.DatabaseURL()
	// Password should be URL-encoded
	if got == "" {
		t.Fatal("DatabaseURL() returned empty string")
	}
	// Verify it contains the encoded password and correct structure
	if got != "postgres://lettuce:p%40ss%3Aword%2Fspecial@localhost:5432/lettuce?sslmode=prefer" {
		t.Errorf("DatabaseURL() = %q, expected proper URL encoding of special characters", got)
	}
}

func TestLoadExampleConfig(t *testing.T) {
	clearLettuceEnv(t)
	cfg, err := Load("../../lettuce.yaml.example")
	if err != nil {
		t.Fatalf("failed to load example config: %v", err)
	}
	if cfg.Server.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want %q", cfg.Server.HTTPAddr, ":8080")
	}
	if cfg.Database.Port != 5432 {
		t.Errorf("Port = %d, want %d", cfg.Database.Port, 5432)
	}
}

func TestHeadConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     HeadConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid config with all fields",
			cfg: HeadConfig{
				Name:               "My Head",
				Description:        "A test head",
				URL:                "https://example.com",
				DefaultLeafWeights: map[string]int{"prime-gap": 10, "protein-fold": 5},
			},
			wantErr: false,
		},
		{
			name: "valid config name only",
			cfg: HeadConfig{
				Name: "Minimal Head",
			},
			wantErr: false,
		},
		{
			name: "valid config with URL no weights",
			cfg: HeadConfig{
				Name: "Head With URL",
				URL:  "https://example.com:8080/path",
			},
			wantErr: false,
		},
		{
			name: "valid config with empty weights map",
			cfg: HeadConfig{
				Name:               "Head Empty Weights",
				DefaultLeafWeights: map[string]int{},
			},
			wantErr: false,
		},
		{
			name: "valid config with nil weights",
			cfg: HeadConfig{
				Name:               "Head Nil Weights",
				DefaultLeafWeights: nil,
			},
			wantErr: false,
		},
		{
			name:    "missing name",
			cfg:     HeadConfig{},
			wantErr: true,
			errMsg:  "head.name is required",
		},
		{
			name: "empty name",
			cfg: HeadConfig{
				Name: "",
				URL:  "https://example.com",
			},
			wantErr: true,
			errMsg:  "head.name is required",
		},
		{
			name: "invalid URL no scheme",
			cfg: HeadConfig{
				Name: "Bad URL Head",
				URL:  "example.com",
			},
			wantErr: true,
			errMsg:  "head.url must include scheme and host",
		},
		{
			name: "invalid URL scheme only",
			cfg: HeadConfig{
				Name: "Scheme Only Head",
				URL:  "https://",
			},
			wantErr: true,
			errMsg:  "head.url must include scheme and host",
		},
		{
			name: "invalid URL no host",
			cfg: HeadConfig{
				Name: "No Host Head",
				URL:  "file:///path/to/file",
			},
			wantErr: true,
			errMsg:  "head.url must include scheme and host",
		},
		{
			name: "no URL passes",
			cfg: HeadConfig{
				Name: "No URL Head",
				URL:  "",
			},
			wantErr: false,
		},
		{
			name: "weight value of zero",
			cfg: HeadConfig{
				Name:               "Zero Weight Head",
				DefaultLeafWeights: map[string]int{"prime-gap": 0},
			},
			wantErr: true,
			errMsg:  "must be > 0, got 0",
		},
		{
			name: "weight value negative",
			cfg: HeadConfig{
				Name:               "Negative Weight Head",
				DefaultLeafWeights: map[string]int{"prime-gap": -1},
			},
			wantErr: true,
			errMsg:  "must be > 0, got -1",
		},
		{
			name: "one valid one invalid weight",
			cfg: HeadConfig{
				Name:               "Mixed Weights Head",
				DefaultLeafWeights: map[string]int{"good-leaf": 5, "bad-leaf": 0},
			},
			wantErr: true,
			errMsg:  "must be > 0, got 0",
		},
		{
			name: "large valid weight",
			cfg: HeadConfig{
				Name:               "Big Weight Head",
				DefaultLeafWeights: map[string]int{"prime-gap": 1000000},
			},
			wantErr: false,
		},
		{
			name: "valid layer-1 dispatch knobs",
			cfg: HeadConfig{
				Name:                    "L1 Head",
				MaxBatchPerRequest:      8,
				MinRetryDelaySeconds:    30,
				MaxRetryDelaySeconds:    900,
				RetryDelayJitterPct:     0.20,
				TargetRequestRatePerSec: 500,
				LeaseSeconds:            900,
			},
			wantErr: false,
		},
		{
			name: "max_retry_delay at stale threshold rejected",
			cfg: HeadConfig{
				Name:                 "Stale Delay Head",
				MaxRetryDelaySeconds: 1800,
			},
			wantErr: true,
			errMsg:  "max_retry_delay_seconds must be < 1800",
		},
		{
			name: "lease above former stale threshold accepted (no upper bound)",
			cfg: HeadConfig{
				Name:         "Long Lease Head",
				LeaseSeconds: 100000,
			},
			wantErr: false,
		},
		{
			name: "min greater than max retry delay rejected",
			cfg: HeadConfig{
				Name:                 "Inverted Delay Head",
				MinRetryDelaySeconds: 600,
				MaxRetryDelaySeconds: 300,
			},
			wantErr: true,
			errMsg:  "min_retry_delay_seconds",
		},
		{
			name: "jitter pct >= 1 rejected",
			cfg: HeadConfig{
				Name:                "Bad Jitter Head",
				RetryDelayJitterPct: 1.0,
			},
			wantErr: true,
			errMsg:  "retry_delay_jitter_pct must be in [0, 1)",
		},
		{
			name: "negative max batch rejected",
			cfg: HeadConfig{
				Name:               "Neg Batch Head",
				MaxBatchPerRequest: -1,
			},
			wantErr: true,
			errMsg:  "max_batch_per_request must be >= 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("HeadConfig.Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errMsg)
				}
				if got := err.Error(); !contains(got, tt.errMsg) {
					t.Errorf("error = %q, want it to contain %q", got, tt.errMsg)
				}
			}
		})
	}
}

func TestHeadEffectiveDispatchDefaults(t *testing.T) {
	// Zero-valued HeadConfig yields the documented dispatch defaults. The
	// max-batch ceiling was raised to 64 in Layer 2 (a safety ceiling, not the
	// limiter) so short-unit leafs can fill their work_buffer_hours.
	var h HeadConfig
	if got := h.EffectiveMaxBatch(); got != 64 {
		t.Errorf("EffectiveMaxBatch() = %d, want 64", got)
	}
	if got := h.EffectiveMinRetryDelaySeconds(); got != 30 {
		t.Errorf("EffectiveMinRetryDelaySeconds() = %d, want 30", got)
	}
	if got := h.EffectiveMaxRetryDelaySeconds(); got != 900 {
		t.Errorf("EffectiveMaxRetryDelaySeconds() = %d, want 900", got)
	}
	if got := h.EffectiveRetryDelayJitterPct(); got != 0.20 {
		t.Errorf("EffectiveRetryDelayJitterPct() = %v, want 0.20", got)
	}
	if got := h.EffectiveTargetRequestRatePerSec(); got != 500 {
		t.Errorf("EffectiveTargetRequestRatePerSec() = %v, want 500", got)
	}
	if got := h.EffectiveLeaseSeconds(); got != 900 {
		t.Errorf("EffectiveLeaseSeconds() = %d, want 900", got)
	}

	// Non-zero values are returned verbatim.
	h2 := HeadConfig{
		MaxBatchPerRequest:      4,
		MinRetryDelaySeconds:    10,
		MaxRetryDelaySeconds:    600,
		RetryDelayJitterPct:     0.1,
		TargetRequestRatePerSec: 250,
		LeaseSeconds:            300,
	}
	if got := h2.EffectiveMaxBatch(); got != 4 {
		t.Errorf("EffectiveMaxBatch() = %d, want 4", got)
	}
	if got := h2.EffectiveLeaseSeconds(); got != 300 {
		t.Errorf("EffectiveLeaseSeconds() = %d, want 300", got)
	}
	if got := h2.EffectiveTargetRequestRatePerSec(); got != 250 {
		t.Errorf("EffectiveTargetRequestRatePerSec() = %v, want 250", got)
	}
}

func TestHeadDispatchEnvOverrides(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, `head: { name: "from-yaml" }`)
	t.Setenv("LETTUCE_HEAD_MAX_BATCH_PER_REQUEST", "5")
	t.Setenv("LETTUCE_HEAD_MIN_RETRY_DELAY_SECONDS", "15")
	t.Setenv("LETTUCE_HEAD_MAX_RETRY_DELAY_SECONDS", "600")
	t.Setenv("LETTUCE_HEAD_RETRY_DELAY_JITTER_PCT", "0.1")
	t.Setenv("LETTUCE_HEAD_TARGET_REQUEST_RATE_PER_SEC", "250")
	t.Setenv("LETTUCE_HEAD_LEASE_SECONDS", "450")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Head.MaxBatchPerRequest != 5 {
		t.Errorf("MaxBatchPerRequest = %d, want 5", cfg.Head.MaxBatchPerRequest)
	}
	if cfg.Head.MinRetryDelaySeconds != 15 {
		t.Errorf("MinRetryDelaySeconds = %d, want 15", cfg.Head.MinRetryDelaySeconds)
	}
	if cfg.Head.MaxRetryDelaySeconds != 600 {
		t.Errorf("MaxRetryDelaySeconds = %d, want 600", cfg.Head.MaxRetryDelaySeconds)
	}
	if cfg.Head.RetryDelayJitterPct != 0.1 {
		t.Errorf("RetryDelayJitterPct = %v, want 0.1", cfg.Head.RetryDelayJitterPct)
	}
	if cfg.Head.TargetRequestRatePerSec != 250 {
		t.Errorf("TargetRequestRatePerSec = %v, want 250", cfg.Head.TargetRequestRatePerSec)
	}
	if cfg.Head.LeaseSeconds != 450 {
		t.Errorf("LeaseSeconds = %d, want 450", cfg.Head.LeaseSeconds)
	}
}

// TestHeadMaintenanceAdmissionCap exercises the FIX-4 maintenance-admission knob:
// the Effective accessor returns 0 to derive (default and negative), a positive
// value verbatim; the env override threads through Load; Validate rejects negative.
func TestHeadMaintenanceAdmissionCap(t *testing.T) {
	var zero HeadConfig
	if got := zero.EffectiveMaintenanceAdmissionCap(); got != 0 {
		t.Errorf("zero MaintenanceAdmissionCap should derive (return 0), got %d", got)
	}
	neg := HeadConfig{MaintenanceAdmissionCap: -1}
	if got := neg.EffectiveMaintenanceAdmissionCap(); got != 0 {
		t.Errorf("negative MaintenanceAdmissionCap should return 0 to derive, got %d", got)
	}
	pos := HeadConfig{MaintenanceAdmissionCap: 7}
	if got := pos.EffectiveMaintenanceAdmissionCap(); got != 7 {
		t.Errorf("explicit MaintenanceAdmissionCap should be returned verbatim, got %d", got)
	}

	// Validate rejects a negative value.
	bad := HeadConfig{Name: "x", MaintenanceAdmissionCap: -5}
	if err := bad.Validate(); err == nil {
		t.Errorf("Validate should reject negative maintenance_admission_cap")
	}

	// Env override threads through Load.
	clearLettuceEnv(t)
	path := writeTestConfig(t, `head: { name: "from-yaml" }`)
	t.Setenv("LETTUCE_HEAD_MAINTENANCE_ADMISSION_CAP", "9")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Head.MaintenanceAdmissionCap != 9 {
		t.Errorf("MaintenanceAdmissionCap = %d, want 9", cfg.Head.MaintenanceAdmissionCap)
	}
}

// TestHeadScaleOutDefaults exercises the Layer 3 Effective accessors on a
// zero-valued HeadConfig: claim lease defaults to 120, replay fail-mode defaults
// to "open" (fail-open), and EffectiveInstanceID generates a fresh UUID each call
// when InstanceID is unset.
func TestHeadScaleOutDefaults(t *testing.T) {
	var h HeadConfig
	if got := h.EffectiveClaimLeaseSeconds(); got != 120 {
		t.Errorf("EffectiveClaimLeaseSeconds() = %d, want 120", got)
	}
	if got := h.EffectiveReplayFailMode(); got != "open" {
		t.Errorf("EffectiveReplayFailMode() = %q, want %q", got, "open")
	}
	if !h.ReplayFailsOpen() {
		t.Error("ReplayFailsOpen() = false, want true by default")
	}

	// Unset InstanceID auto-generates and is non-nil; two calls differ.
	id1 := h.EffectiveInstanceID()
	id2 := h.EffectiveInstanceID()
	if id1 == uuid.Nil {
		t.Error("EffectiveInstanceID() returned the nil UUID for unset InstanceID")
	}
	if id1 == id2 {
		t.Error("EffectiveInstanceID() should generate a fresh UUID each call when InstanceID is unset")
	}

	// A configured InstanceID is parsed and returned verbatim.
	fixed := "11111111-1111-1111-1111-111111111111"
	h2 := HeadConfig{InstanceID: fixed}
	if got := h2.EffectiveInstanceID().String(); got != fixed {
		t.Errorf("EffectiveInstanceID() = %q, want %q", got, fixed)
	}

	// Explicit claim lease + fail mode returned verbatim.
	h3 := HeadConfig{ClaimLeaseSeconds: 60, ReplayFailMode: "closed"}
	if got := h3.EffectiveClaimLeaseSeconds(); got != 60 {
		t.Errorf("EffectiveClaimLeaseSeconds() = %d, want 60", got)
	}
	if got := h3.EffectiveReplayFailMode(); got != "closed" {
		t.Errorf("EffectiveReplayFailMode() = %q, want %q", got, "closed")
	}
	if h3.ReplayFailsOpen() {
		t.Error("ReplayFailsOpen() = true for closed mode, want false")
	}
}

// TestHeadScaleOutValidate covers the Layer 3 Validate rules: instance_id must
// be a UUID; replay_fail_mode must be open|closed; claim_lease_seconds must
// satisfy the flush-cadence floor and stay below lease_seconds.
func TestHeadScaleOutValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     HeadConfig
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid scale-out knobs",
			cfg:     HeadConfig{Name: "x", InstanceID: "22222222-2222-2222-2222-222222222222", ReplayFailMode: "closed", ClaimLeaseSeconds: 120},
			wantErr: false,
		},
		{
			name:    "empty scale-out knobs ok",
			cfg:     HeadConfig{Name: "x"},
			wantErr: false,
		},
		{
			name:    "invalid instance id",
			cfg:     HeadConfig{Name: "x", InstanceID: "not-a-uuid"},
			wantErr: true,
			errMsg:  "head.instance_id must be a valid UUID",
		},
		{
			name:    "invalid replay fail mode",
			cfg:     HeadConfig{Name: "x", ReplayFailMode: "halfopen"},
			wantErr: true,
			errMsg:  "head.replay_fail_mode must be",
		},
		{
			name:    "negative claim lease",
			cfg:     HeadConfig{Name: "x", ClaimLeaseSeconds: -1},
			wantErr: true,
			errMsg:  "claim_lease_seconds must be >= 0",
		},
		{
			name:    "claim lease below 5s floor",
			cfg:     HeadConfig{Name: "x", ClaimLeaseSeconds: 4},
			wantErr: true,
			errMsg:  "is too short",
		},
		{
			name:    "claim lease below 10x flush floor",
			cfg:     HeadConfig{Name: "x", ClaimLeaseSeconds: 5, FlushIntervalMs: 1000},
			wantErr: true,
			errMsg:  "is too short",
		},
		{
			name:    "claim lease not below volunteer lease",
			cfg:     HeadConfig{Name: "x", ClaimLeaseSeconds: 900, LeaseSeconds: 900},
			wantErr: true,
			errMsg:  "must be < lease_seconds",
		},
		{
			name:    "claim lease just below volunteer lease",
			cfg:     HeadConfig{Name: "x", ClaimLeaseSeconds: 120, LeaseSeconds: 300},
			wantErr: false,
		},
		{
			// HARDENING 1: claim_lease_seconds is UNSET (defaults to 120) but a
			// small explicit lease_seconds makes the DEFAULTED claim lease (120)
			// >= the reservation lease. The old guard (raw ClaimLeaseSeconds > 0)
			// skipped this and silently violated the invariant; Validate must now
			// compare EFFECTIVE values and reject it.
			name:    "default claim lease vs small explicit lease rejected",
			cfg:     HeadConfig{Name: "x", LeaseSeconds: 60},
			wantErr: true,
			errMsg:  "must be < lease_seconds",
		},
		{
			// Boundary: default claim lease (120) equal to an explicit lease (120)
			// is still a violation — the claim lease must be STRICTLY below.
			name:    "default claim lease equal to explicit lease rejected",
			cfg:     HeadConfig{Name: "x", LeaseSeconds: 120},
			wantErr: true,
			errMsg:  "must be < lease_seconds",
		},
		{
			// Both unset: defaulted claim lease (120) < defaulted reservation
			// lease (900) — the common production-default path stays valid.
			name:    "both leases unset uses defaults ok",
			cfg:     HeadConfig{Name: "x"},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errMsg)
				}
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tt.errMsg)
				}
			}
		})
	}
}

// TestHeadScaleOutEnvOverrides threads the Layer 3 env knobs through Load.
func TestHeadScaleOutEnvOverrides(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, `head: { name: "from-yaml" }`)
	t.Setenv("LETTUCE_HEAD_INSTANCE_ID", "33333333-3333-3333-3333-333333333333")
	t.Setenv("LETTUCE_REDIS_URL", "redis://redis:6379")
	t.Setenv("LETTUCE_REPLAY_FAIL_MODE", "closed")
	t.Setenv("LETTUCE_HEAD_CLAIM_LEASE_SECONDS", "90")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Head.InstanceID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("InstanceID = %q, unexpected", cfg.Head.InstanceID)
	}
	if cfg.Head.RedisURL != "redis://redis:6379" {
		t.Errorf("RedisURL = %q, unexpected", cfg.Head.RedisURL)
	}
	if cfg.Head.ReplayFailMode != "closed" {
		t.Errorf("ReplayFailMode = %q, want closed", cfg.Head.ReplayFailMode)
	}
	if cfg.Head.ClaimLeaseSeconds != 90 {
		t.Errorf("ClaimLeaseSeconds = %d, want 90", cfg.Head.ClaimLeaseSeconds)
	}
}

func TestHeadEnvOverrides(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, `head: { name: "from-yaml" }`)
	t.Setenv("LETTUCE_HEAD_NAME", "from-env")
	t.Setenv("LETTUCE_HEAD_DESCRIPTION", "env description")
	t.Setenv("LETTUCE_HEAD_URL", "https://example.com")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Head.Name != "from-env" {
		t.Errorf("Head.Name = %q, want %q", cfg.Head.Name, "from-env")
	}
	if cfg.Head.Description != "env description" {
		t.Errorf("Head.Description = %q, want %q", cfg.Head.Description, "env description")
	}
	if cfg.Head.URL != "https://example.com" {
		t.Errorf("Head.URL = %q, want %q", cfg.Head.URL, "https://example.com")
	}
}

func TestTrustedProxiesEnvOverrideAndParse(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, minimalConfig)
	t.Setenv("LETTUCE_TRUSTED_PROXIES", "10.0.0.0/8, 172.16.0.0/12 , 192.168.1.5")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.TrustedProxies != "10.0.0.0/8, 172.16.0.0/12 , 192.168.1.5" {
		t.Errorf("TrustedProxies = %q, unexpected", cfg.Server.TrustedProxies)
	}

	nets, err := cfg.Server.ParsedTrustedProxies()
	if err != nil {
		t.Fatalf("ParsedTrustedProxies error: %v", err)
	}
	if len(nets) != 3 {
		t.Fatalf("expected 3 parsed networks, got %d", len(nets))
	}

	// Bare IP should have become a /32.
	if ones, _ := nets[2].Mask.Size(); ones != 32 {
		t.Errorf("bare IP should be /32, got /%d", ones)
	}

	// Sanity: 10.1.2.3 is contained by the first CIDR.
	if !nets[0].Contains(parseIP(t, "10.1.2.3")) {
		t.Error("10.0.0.0/8 should contain 10.1.2.3")
	}
}

func TestTrustedProxiesEmptyIsNil(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, minimalConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	nets, err := cfg.Server.ParsedTrustedProxies()
	if err != nil {
		t.Fatalf("ParsedTrustedProxies error: %v", err)
	}
	if len(nets) != 0 {
		t.Errorf("empty trusted_proxies should yield no networks, got %d", len(nets))
	}
}

func TestTrustedProxiesInvalidFailsValidation(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, minimalConfig)
	t.Setenv("LETTUCE_TRUSTED_PROXIES", "not-a-cidr")
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error for invalid trusted_proxies, got nil")
	}
}

// contains reports whether s contains substr. Avoids importing strings package.
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
