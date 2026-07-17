package config

import (
	"strings"
	"testing"
)

// Documented-generator-length sample secrets used across the gate tests. A
// base64 `openssl rand -base64 32` value is 44 chars; a hex `-hex 32` value is
// 64. None contains a placeholder stem.
var (
	goodMachineSecret = strings.Repeat("A", 44)
	goodHexSecret     = strings.Repeat("a", 64)
	goodHumanPassword = "s3cure-operator-pw" // 18 chars, no placeholder stem
)

// bootSecretEnv is a map-backed getenv so the pure gate tests never touch the
// process environment.
func bootSecretEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// checkErr asserts the presence/absence of an error and, when one is expected,
// that it names the given variable.
func checkErr(t *testing.T, err error, wantErr bool, name string) {
	t.Helper()
	if wantErr {
		if err == nil {
			t.Fatalf("expected error naming %s, got nil", name)
		}
		if name != "" && !strings.Contains(err.Error(), name) {
			t.Errorf("error should name %s: %v", name, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateBootSecrets_AdminAPIKey(t *testing.T) {
	tests := []struct {
		name    string
		autogen bool
		value   string
		wantErr bool
	}{
		{"empty is fatal in production", false, "", true},
		{"empty is fatal in dev too", true, "", true},
		{"env-example placeholder", false, "generate-with-openssl-rand-base64-32", true},
		{"dev-compose placeholder", false, "dev-admin-key-not-for-production", true},
		{"too short", false, strings.Repeat("x", 31), true},
		{"documented 44-char length passes", false, goodMachineSecret, false},
		{"placeholder demoted to warning in dev", true, "generate-with-openssl-rand-base64-32", false},
		{"short demoted to warning in dev", true, strings.Repeat("x", 10), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Signing:  SigningConfig{AutoGenerate: tt.autogen},
				Database: DatabaseConfig{Host: "127.0.0.1", Password: goodHumanPassword},
			}
			env := bootSecretEnv(map[string]string{"LETTUCE_ADMIN_API_KEY": tt.value})
			_, err := ValidateBootSecrets(cfg, env)
			checkErr(t, err, tt.wantErr, "LETTUCE_ADMIN_API_KEY")
		})
	}
}

func TestValidateBootSecrets_AdminPassword(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty is OK (bootstrap skips)", "", false},
		{"env-example placeholder", "change-me-to-a-strong-password", true},
		{"too short", "short", true},
		{"valid human password", goodHumanPassword, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Database: DatabaseConfig{Host: "127.0.0.1", Password: goodHumanPassword}}
			env := bootSecretEnv(map[string]string{
				"LETTUCE_ADMIN_API_KEY":  goodMachineSecret,
				"LETTUCE_ADMIN_PASSWORD": tt.value,
			})
			_, err := ValidateBootSecrets(cfg, env)
			checkErr(t, err, tt.wantErr, "LETTUCE_ADMIN_PASSWORD")
		})
	}
}

func TestValidateBootSecrets_DashboardKey(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty is OK", "", false},
		{"placeholder literal", "placeholder", true},
		{"env-example placeholder", "generate-with-openssl-rand-base64-32", true},
		{"too short", strings.Repeat("x", 31), true},
		{"documented 44-char length passes", goodMachineSecret, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Database: DatabaseConfig{Host: "127.0.0.1", Password: goodHumanPassword}}
			env := bootSecretEnv(map[string]string{
				"LETTUCE_ADMIN_API_KEY": goodMachineSecret,
				"DASHBOARD_API_KEY":     tt.value,
			})
			_, err := ValidateBootSecrets(cfg, env)
			checkErr(t, err, tt.wantErr, "DASHBOARD_API_KEY")
		})
	}
}

// TestValidateBootSecrets_DBPassword exercises the host-aware DB-password rule:
// a placeholder is always a violation; an empty/short password is a violation
// ONLY when the host is not confined to a private network. Hosts are IP literals
// (or localhost) so the classification is hermetic — no DNS.
func TestValidateBootSecrets_DBPassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		host     string
		wantErr  bool
	}{
		{"placeholder errors on private host", "change-me-to-a-strong-password", "127.0.0.1", true},
		{"placeholder errors on public host", "change-me-to-a-strong-password", "203.0.113.7", true},
		{"empty + loopback is silent", "", "127.0.0.1", false},
		{"empty + localhost is silent", "", "localhost", false},
		{"empty + rfc1918 is silent", "", "10.1.2.3", false},
		{"empty + public literal errors", "", "203.0.113.7", true},
		{"short + public literal errors", "short", "203.0.113.7", true},
		{"short + private is silent", "short", "10.1.2.3", false},
		{"strong password on public host is OK", goodHumanPassword, "203.0.113.7", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Database: DatabaseConfig{Host: tt.host, Password: tt.password}}
			env := bootSecretEnv(map[string]string{"LETTUCE_ADMIN_API_KEY": goodMachineSecret})
			_, err := ValidateBootSecrets(cfg, env)
			checkErr(t, err, tt.wantErr, "LETTUCE_DB_PASSWORD")
		})
	}
}

func TestValidateBootSecrets_Redis(t *testing.T) {
	tests := []struct {
		name     string
		redisURL string
		wantErr  bool
		wantWarn bool
	}{
		{"empty is OK", "", false, false},
		{"no password warns, does not error", "redis://redis:6379", false, true},
		{"placeholder password errors", "redis://:generate-with-openssl-rand-hex-32@redis:6379", true, false},
		{"short password errors", "redis://:pw@redis:6379", true, false},
		{"documented 64-char hex password passes", "redis://:" + goodHexSecret + "@redis:6379", false, false},
		{"unparseable url is silently skipped", "postgres://not-a-redis-url:5432", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Database: DatabaseConfig{Host: "127.0.0.1", Password: goodHumanPassword},
				Head:     HeadConfig{RedisURL: tt.redisURL},
			}
			env := bootSecretEnv(map[string]string{"LETTUCE_ADMIN_API_KEY": goodMachineSecret})
			warnings, err := ValidateBootSecrets(cfg, env)
			checkErr(t, err, tt.wantErr, "LETTUCE_REDIS_URL")
			gotWarn := false
			for _, w := range warnings {
				if strings.Contains(w, "LETTUCE_REDIS_URL") {
					gotWarn = true
				}
			}
			if gotWarn != tt.wantWarn {
				t.Errorf("redis warning present = %v, want %v (warnings=%v)", gotWarn, tt.wantWarn, warnings)
			}
		})
	}
}

// TestValidateBootSecrets_DevPostureDemotesToWarnings pins the posture behavior:
// under autogen every production-fatal violation becomes a warning that names the
// production consequence, and Load still succeeds — EXCEPT an empty admin key,
// which stays fatal in both postures.
func TestValidateBootSecrets_DevPostureDemotesToWarnings(t *testing.T) {
	cfg := &Config{
		Signing:  SigningConfig{AutoGenerate: true},
		Database: DatabaseConfig{Host: "203.0.113.7", Password: "change-me-to-a-strong-password"},
		Head:     HeadConfig{RedisURL: "redis://:pw@redis:6379"},
	}
	env := bootSecretEnv(map[string]string{
		"LETTUCE_ADMIN_API_KEY":  "generate-with-openssl-rand-base64-32",
		"LETTUCE_ADMIN_PASSWORD": "change-me-to-a-strong-password",
		"DASHBOARD_API_KEY":      "placeholder",
	})
	warnings, err := ValidateBootSecrets(cfg, env)
	if err != nil {
		t.Fatalf("dev posture must not error on placeholders, got: %v", err)
	}
	// Five demoted violations (admin key, admin password, dashboard key, DB
	// password, redis password), each naming the production consequence.
	if len(warnings) != 5 {
		t.Fatalf("expected 5 demotion warnings, got %d: %v", len(warnings), warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w, "would refuse to boot") {
			t.Errorf("demoted warning should name the production consequence: %q", w)
		}
	}

	// Empty admin key is still fatal under autogen.
	devEmpty := &Config{
		Signing:  SigningConfig{AutoGenerate: true},
		Database: DatabaseConfig{Host: "127.0.0.1", Password: goodHumanPassword},
	}
	if _, err := ValidateBootSecrets(devEmpty, bootSecretEnv(map[string]string{"LETTUCE_ADMIN_API_KEY": ""})); err == nil {
		t.Fatal("empty LETTUCE_ADMIN_API_KEY must be fatal even under autogen")
	}
}

// TestValidateBootSecrets_EnvExamplePlaceholdersRejected walks every exact
// placeholder shipped in .env.example, routed to the head-side variable that
// carries it, and asserts a production boot fails naming that variable.
func TestValidateBootSecrets_EnvExamplePlaceholdersRejected(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(cfg *Config, env map[string]string)
		wantName string
	}{
		{"LETTUCE_ADMIN_API_KEY", func(_ *Config, env map[string]string) {
			env["LETTUCE_ADMIN_API_KEY"] = "generate-with-openssl-rand-base64-32"
		}, "LETTUCE_ADMIN_API_KEY"},
		{"LETTUCE_ADMIN_PASSWORD", func(_ *Config, env map[string]string) {
			env["LETTUCE_ADMIN_PASSWORD"] = "change-me-to-a-strong-password"
		}, "LETTUCE_ADMIN_PASSWORD"},
		{"DASHBOARD_API_KEY", func(_ *Config, env map[string]string) {
			env["DASHBOARD_API_KEY"] = "generate-with-openssl-rand-base64-32"
		}, "DASHBOARD_API_KEY"},
		{"LETTUCE_DB_PASSWORD", func(cfg *Config, _ map[string]string) {
			cfg.Database.Password = "change-me-to-a-strong-password"
		}, "LETTUCE_DB_PASSWORD"},
		{"LETTUCE_REDIS_URL", func(cfg *Config, _ map[string]string) {
			cfg.Head.RedisURL = "redis://:generate-with-openssl-rand-hex-32@redis:6379"
		}, "LETTUCE_REDIS_URL"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Database: DatabaseConfig{Host: "127.0.0.1", Password: goodHumanPassword}}
			env := map[string]string{"LETTUCE_ADMIN_API_KEY": goodMachineSecret}
			tt.mutate(cfg, env)
			_, err := ValidateBootSecrets(cfg, bootSecretEnv(env))
			if err == nil {
				t.Fatalf("expected production boot failure for %s", tt.wantName)
			}
			if !strings.Contains(err.Error(), tt.wantName) {
				t.Errorf("error should name %s: %v", tt.wantName, err)
			}
		})
	}
}

// TestValidateBootSecrets_CollectsAllViolations verifies the gate joins every
// violation into one error (errors.Join) so an operator fixes them in one pass.
func TestValidateBootSecrets_CollectsAllViolations(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{Host: "203.0.113.7", Password: "change-me-to-a-strong-password"},
		Head:     HeadConfig{RedisURL: "redis://:pw@redis:6379"},
	}
	env := bootSecretEnv(map[string]string{
		"LETTUCE_ADMIN_API_KEY":  "short",
		"LETTUCE_ADMIN_PASSWORD": "short",
		"DASHBOARD_API_KEY":      "short",
	})
	_, err := ValidateBootSecrets(cfg, env)
	if err == nil {
		t.Fatal("expected a joined error for multiple weak secrets")
	}
	for _, name := range []string{
		"LETTUCE_ADMIN_API_KEY", "LETTUCE_ADMIN_PASSWORD", "DASHBOARD_API_KEY",
		"LETTUCE_DB_PASSWORD", "LETTUCE_REDIS_URL",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("joined error should name %s: %v", name, err)
		}
	}
}

// TestLoad_BootSecretGate is the wiring test THROUGH config.Load: a placeholder
// admin key fails a production Load with the pinned "boot secret validation:"
// prefix, and the same env under autogen succeeds (the violation is demoted).
func TestLoad_BootSecretGate(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, minimalConfig)
	t.Setenv("LETTUCE_ADMIN_API_KEY", "generate-with-openssl-rand-base64-32")

	t.Setenv("LETTUCE_SIGNING_KEY_AUTOGEN", "false")
	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to fail on a placeholder admin key in production posture")
	} else if !strings.Contains(err.Error(), "boot secret validation") {
		t.Errorf("Load error should carry the pinned prefix: %v", err)
	}

	t.Setenv("LETTUCE_SIGNING_KEY_AUTOGEN", "true")
	if _, err := Load(path); err != nil {
		t.Fatalf("expected Load to succeed under autogen, got: %v", err)
	}
}
