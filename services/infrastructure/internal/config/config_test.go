package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
