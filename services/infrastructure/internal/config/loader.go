package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads configuration from a YAML file, applies environment variable
// overrides (LETTUCE_ prefix), sets defaults for zero-valued fields, and
// validates the result.
func Load(path string) (*Config, error) {
	cfg, err := loadFromFile(path)
	if err != nil {
		return nil, err
	}
	applyDefaults(cfg)
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, fmt.Errorf("config env override: %w", err)
	}
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	return cfg, nil
}

func loadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.HTTPAddr == "" {
		cfg.Server.HTTPAddr = ":8080"
	}
	if cfg.Server.GRPCAddr == "" {
		cfg.Server.GRPCAddr = ":9090"
	}
	if cfg.Database.Host == "" {
		cfg.Database.Host = "localhost"
	}
	if cfg.Database.Port == 0 {
		cfg.Database.Port = 5432
	}
	if cfg.Database.Database == "" {
		cfg.Database.Database = "lettuce"
	}
	if cfg.Database.User == "" {
		cfg.Database.User = "lettuce"
	}
	if cfg.Database.SSLMode == "" {
		cfg.Database.SSLMode = "prefer"
	}
	if cfg.Database.MaxConns == 0 {
		cfg.Database.MaxConns = 25
	}
	if cfg.Database.MinConns == 0 {
		cfg.Database.MinConns = 5
	}
	if cfg.Database.MaxConnLifetime == "" {
		cfg.Database.MaxConnLifetime = "1h"
	}
	if cfg.Database.MaxConnIdleTime == "" {
		cfg.Database.MaxConnIdleTime = "30m"
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = "json"
	}
	if cfg.Signing.PrivateKeyPath == "" {
		cfg.Signing.PrivateKeyPath = "lettuce-signing.key"
	}
}

func applyEnvOverrides(cfg *Config) error {
	if v := os.Getenv("LETTUCE_SERVER_HTTP_ADDR"); v != "" {
		cfg.Server.HTTPAddr = v
	}
	if v := os.Getenv("LETTUCE_SERVER_GRPC_ADDR"); v != "" {
		cfg.Server.GRPCAddr = v
	}
	if v := os.Getenv("LETTUCE_CORS_ORIGINS"); v != "" {
		cfg.Server.CORSOrigins = v
	}
	if v := os.Getenv("LETTUCE_TRUSTED_PROXIES"); v != "" {
		cfg.Server.TrustedProxies = v
	}
	if v := os.Getenv("LETTUCE_DB_HOST"); v != "" {
		cfg.Database.Host = v
	}
	if v := os.Getenv("LETTUCE_DB_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_DB_PORT must be an integer: %w", err)
		}
		cfg.Database.Port = port
	}
	if v := os.Getenv("LETTUCE_DB_DATABASE"); v != "" {
		cfg.Database.Database = v
	}
	if v := os.Getenv("LETTUCE_DB_USER"); v != "" {
		cfg.Database.User = v
	}
	if v := os.Getenv("LETTUCE_DB_PASSWORD"); v != "" {
		cfg.Database.Password = v
	}
	if v := os.Getenv("LETTUCE_DB_SSL_MODE"); v != "" {
		cfg.Database.SSLMode = v
	}
	if v := os.Getenv("LETTUCE_DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_DB_MAX_CONNS must be an integer: %w", err)
		}
		cfg.Database.MaxConns = n
	}
	if v := os.Getenv("LETTUCE_DB_MIN_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_DB_MIN_CONNS must be an integer: %w", err)
		}
		cfg.Database.MinConns = n
	}
	if v := os.Getenv("LETTUCE_DB_MAX_CONN_LIFETIME"); v != "" {
		cfg.Database.MaxConnLifetime = v
	}
	if v := os.Getenv("LETTUCE_DB_MAX_CONN_IDLE_TIME"); v != "" {
		cfg.Database.MaxConnIdleTime = v
	}
	if v := os.Getenv("LETTUCE_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("LETTUCE_LOG_FORMAT"); v != "" {
		cfg.Log.Format = v
	}
	if v := os.Getenv("LETTUCE_TLS_CERT_FILE"); v != "" {
		cfg.TLS.CertFile = v
	}
	if v := os.Getenv("LETTUCE_TLS_KEY_FILE"); v != "" {
		cfg.TLS.KeyFile = v
	}
	if v := os.Getenv("LETTUCE_TLS_CA_FILE"); v != "" {
		cfg.TLS.CAFile = v
	}
	if v := os.Getenv("LETTUCE_SIGNING_PRIVATE_KEY_PATH"); v != "" {
		cfg.Signing.PrivateKeyPath = v
	}
	if v := os.Getenv("LETTUCE_SIGNING_KEY_AUTOGEN"); v != "" {
		autogen, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_SIGNING_KEY_AUTOGEN must be a boolean (true/false): %w", err)
		}
		cfg.Signing.AutoGenerate = autogen
	}
	if v := os.Getenv("LETTUCE_HEAD_NAME"); v != "" {
		cfg.Head.Name = v
	}
	if v := os.Getenv("LETTUCE_HEAD_DESCRIPTION"); v != "" {
		cfg.Head.Description = v
	}
	if v := os.Getenv("LETTUCE_HEAD_URL"); v != "" {
		cfg.Head.URL = v
	}
	if v := os.Getenv("LETTUCE_HEAD_INSTANCE_ID"); v != "" {
		cfg.Head.InstanceID = v
	}
	if v := os.Getenv("LETTUCE_REDIS_URL"); v != "" {
		cfg.Head.RedisURL = v
	}
	if v := os.Getenv("LETTUCE_REPLAY_FAIL_MODE"); v != "" {
		cfg.Head.ReplayFailMode = v
	}
	if v := os.Getenv("LETTUCE_HEAD_CLAIM_LEASE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_CLAIM_LEASE_SECONDS must be an integer: %w", err)
		}
		cfg.Head.ClaimLeaseSeconds = n
	}
	if v := os.Getenv("LETTUCE_HEAD_MAX_INFLIGHT_PER_VOLUNTEER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_MAX_INFLIGHT_PER_VOLUNTEER must be an integer: %w", err)
		}
		cfg.Head.MaxInflightPerVolunteer = n
	}
	if v := os.Getenv("LETTUCE_HEAD_MAX_BATCH_PER_REQUEST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_MAX_BATCH_PER_REQUEST must be an integer: %w", err)
		}
		cfg.Head.MaxBatchPerRequest = n
	}
	if v := os.Getenv("LETTUCE_HEAD_MIN_RETRY_DELAY_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_MIN_RETRY_DELAY_SECONDS must be an integer: %w", err)
		}
		cfg.Head.MinRetryDelaySeconds = n
	}
	if v := os.Getenv("LETTUCE_HEAD_MAX_RETRY_DELAY_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_MAX_RETRY_DELAY_SECONDS must be an integer: %w", err)
		}
		cfg.Head.MaxRetryDelaySeconds = n
	}
	if v := os.Getenv("LETTUCE_HEAD_RETRY_DELAY_JITTER_PCT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_RETRY_DELAY_JITTER_PCT must be a float: %w", err)
		}
		cfg.Head.RetryDelayJitterPct = f
	}
	if v := os.Getenv("LETTUCE_HEAD_TARGET_REQUEST_RATE_PER_SEC"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_TARGET_REQUEST_RATE_PER_SEC must be a float: %w", err)
		}
		cfg.Head.TargetRequestRatePerSec = f
	}
	if v := os.Getenv("LETTUCE_HEAD_LEASE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_LEASE_SECONDS must be an integer: %w", err)
		}
		cfg.Head.LeaseSeconds = n
	}
	if v := os.Getenv("LETTUCE_HEAD_READY_POOL_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_READY_POOL_SIZE must be an integer: %w", err)
		}
		cfg.Head.ReadyPoolSize = n
	}
	if v := os.Getenv("LETTUCE_HEAD_REFILL_BATCH_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_REFILL_BATCH_SIZE must be an integer: %w", err)
		}
		cfg.Head.RefillBatchSize = n
	}
	if v := os.Getenv("LETTUCE_HEAD_DISPATCH_ADMISSION_CAP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_DISPATCH_ADMISSION_CAP must be an integer: %w", err)
		}
		cfg.Head.DispatchAdmissionCap = n
	}
	if v := os.Getenv("LETTUCE_HEAD_MAINTENANCE_ADMISSION_CAP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_MAINTENANCE_ADMISSION_CAP must be an integer: %w", err)
		}
		cfg.Head.MaintenanceAdmissionCap = n
	}
	if v := os.Getenv("LETTUCE_HEAD_FLUSH_INTERVAL_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_FLUSH_INTERVAL_MS must be an integer: %w", err)
		}
		cfg.Head.FlushIntervalMs = n
	}
	if v := os.Getenv("LETTUCE_HEAD_FLUSH_BATCH_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_FLUSH_BATCH_SIZE must be an integer: %w", err)
		}
		cfg.Head.FlushBatchSize = n
	}
	if v := os.Getenv("LETTUCE_HEAD_NO_DEADLINE_CEILING_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LETTUCE_HEAD_NO_DEADLINE_CEILING_SECONDS must be an integer: %w", err)
		}
		cfg.Head.NoDeadlineCeilingSeconds = n
	}
	return nil
}

func validate(cfg *Config) error {
	if cfg.Database.Port < 1 || cfg.Database.Port > 65535 {
		return fmt.Errorf("database.port must be between 1 and 65535, got %d", cfg.Database.Port)
	}
	if cfg.Database.MaxConns < 1 {
		return fmt.Errorf("database.max_conns must be > 0, got %d", cfg.Database.MaxConns)
	}
	if cfg.Database.MinConns < 0 || cfg.Database.MinConns > cfg.Database.MaxConns {
		return fmt.Errorf("database.min_conns must be >= 0 and <= max_conns (%d), got %d", cfg.Database.MaxConns, cfg.Database.MinConns)
	}
	if _, err := time.ParseDuration(cfg.Database.MaxConnLifetime); err != nil {
		return fmt.Errorf("database.max_conn_lifetime must be a valid duration: %w", err)
	}
	if _, err := time.ParseDuration(cfg.Database.MaxConnIdleTime); err != nil {
		return fmt.Errorf("database.max_conn_idle_time must be a valid duration: %w", err)
	}

	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[cfg.Log.Level] {
		return fmt.Errorf("log.level must be one of debug, info, warn, error; got %q", cfg.Log.Level)
	}

	validLogFormats := map[string]bool{"json": true, "text": true}
	if !validLogFormats[cfg.Log.Format] {
		return fmt.Errorf("log.format must be one of json, text; got %q", cfg.Log.Format)
	}

	validSSLModes := map[string]bool{
		"disable": true, "allow": true, "prefer": true,
		"require": true, "verify-ca": true, "verify-full": true,
	}
	if !validSSLModes[cfg.Database.SSLMode] {
		return fmt.Errorf("database.ssl_mode must be one of disable, allow, prefer, require, verify-ca, verify-full; got %q", cfg.Database.SSLMode)
	}

	if (cfg.TLS.CertFile != "") != (cfg.TLS.KeyFile != "") {
		return fmt.Errorf("tls.cert_file and tls.key_file must both be set or both be empty")
	}

	// Fail fast on a malformed trusted_proxies list rather than silently
	// disabling proxy trust at runtime.
	if _, err := cfg.Server.ParsedTrustedProxies(); err != nil {
		return fmt.Errorf("server.%w", err)
	}

	if err := cfg.Head.Validate(); err != nil {
		return err
	}

	return nil
}
