package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Secret-strength floors for the BG-30 boot-time secret gate. A MACHINE secret
// (an API key, a shared-store password) must be long enough that a documented
// `openssl rand` generator produces it; a HUMAN-chosen password (the admin
// login, the DB password) has a lower floor because an operator types it. These
// are exported so the persistence-side bootstrap refusal (internal/bootstrap)
// enforces exactly the numbers this gate does.
const (
	// MinMachineSecretLen is the floor for machine-generated secrets. An
	// `openssl rand -base64 32` value is 44 chars and an `-hex 32` value is 64,
	// so 32 admits any documented generator while rejecting a short hand-typed
	// stand-in.
	MinMachineSecretLen = 32
	// MinHumanPasswordLen is the floor for a human-chosen password (the first-boot
	// admin login, the database password).
	MinHumanPasswordLen = 12
)

// placeholderStems are the case-insensitive substrings that mark a value as a
// committed placeholder rather than a real secret. Substring (not equality)
// matching catches trivially-edited placeholders, and the stems together cover
// every literal KnownPlaceholderSecrets returns.
var placeholderStems = []string{
	"change-me",
	"changeme",
	"generate-with",
	"replace-with",
	"placeholder",
	"not-for-production",
}

// IsPlaceholderSecret reports whether v contains any known placeholder stem
// (case-insensitive). It is the blocklist half of the BG-30 gate; the length
// floors above are the other half.
func IsPlaceholderSecret(v string) bool {
	lower := strings.ToLower(v)
	for _, stem := range placeholderStems {
		if strings.Contains(lower, stem) {
			return true
		}
	}
	return false
}

// KnownPlaceholderSecrets returns the EXACT placeholder literals shipped in the
// repo's .env.example and dev compose. The BG-30b residue sweep
// (bootstrap.SweepPlaceholderCredentials) hashes these to find and neutralize a
// credential that was persisted straight from a placeholder before this gate
// existed. IsPlaceholderSecret is the general (substring) test; this is the
// exact-literal set the sweep needs to compute hashes over.
func KnownPlaceholderSecrets() []string {
	return []string{
		"change-me-to-a-strong-password",
		"generate-with-openssl-rand-base64-32",
		"generate-with-openssl-rand-hex-32",
		"replace-with-bcrypt-hash",
		"placeholder",
		"dev-admin-key-not-for-production",
		"dev-dashboard-key-not-for-production",
		"dev-secret-not-for-production",
	}
}

// ValidateBootSecrets is the BG-30 boot-time secret gate. It refuses to let a
// PRODUCTION head (one that has NOT opted into dev signing-key autogeneration)
// boot on a committed placeholder, empty, or too-short value for any secret the
// head itself reads. In a DEV head (LETTUCE_SIGNING_KEY_AUTOGEN=true) every
// violation that would be fatal in production is DEMOTED to a warning, so a
// laptop/CI head still boots on the shipped placeholders — with ONE exception:
// an EMPTY LETTUCE_ADMIN_API_KEY is fatal in BOTH postures (it preserves the
// pre-existing main.go refusal, and the dev compose sets a non-empty key).
//
// All violations are collected and joined so an operator fixes everything in one
// pass; every message names the variable and its exact generator. getenv is
// injected (os.Getenv in production, a map in tests) so the gate is a pure
// function of its inputs. NEXTAUTH_SECRET and REGISTRY_PASS_HASH are dashboard-
// and registry-side, not read by the head, and are deliberately NOT checked here.
func ValidateBootSecrets(cfg *Config, getenv func(string) string) (warnings []string, err error) {
	production := !cfg.Signing.AutoGenerate

	var violations []error
	// violation records a production-fatal problem. In a dev head it is demoted to
	// a warning that names the production consequence, so the same misconfiguration
	// is visible without blocking a dev boot.
	violation := func(msg string) {
		if production {
			violations = append(violations, errors.New(msg))
			return
		}
		warnings = append(warnings, msg+": a production head (no LETTUCE_SIGNING_KEY_AUTOGEN) would refuse to boot")
	}

	// (a) LETTUCE_ADMIN_API_KEY — any non-empty value becomes a full-ADMIN bearer
	// token (internal/server/auth), so it must be a real machine secret. EMPTY is
	// fatal in BOTH postures (preserves the original main.go refusal).
	switch adminKey := getenv("LETTUCE_ADMIN_API_KEY"); {
	case adminKey == "":
		violations = append(violations, errors.New(
			"LETTUCE_ADMIN_API_KEY is required. Generate one with: openssl rand -base64 32"))
	case IsPlaceholderSecret(adminKey) || len(adminKey) < MinMachineSecretLen:
		violation(fmt.Sprintf(
			"LETTUCE_ADMIN_API_KEY is a placeholder or shorter than %d chars — it grants full admin access. Generate one with: openssl rand -base64 32",
			MinMachineSecretLen))
	}

	// (b) LETTUCE_ADMIN_PASSWORD — bootstrap's first-boot admin login. Empty is
	// legitimately OK (bootstrap skips admin creation); a set value must be a real
	// human password.
	if adminPass := getenv("LETTUCE_ADMIN_PASSWORD"); adminPass != "" &&
		(IsPlaceholderSecret(adminPass) || len(adminPass) < MinHumanPasswordLen) {
		violation(fmt.Sprintf(
			"LETTUCE_ADMIN_PASSWORD is a placeholder or shorter than %d chars for the first-boot admin login — choose a strong password",
			MinHumanPasswordLen))
	}

	// (c) DASHBOARD_API_KEY — the key the dashboard authenticates with; bootstrap
	// persists it. Empty is OK (bootstrap skips). The shipped dev value
	// "placeholder" is caught by the blocklist.
	if dashKey := getenv("DASHBOARD_API_KEY"); dashKey != "" &&
		(IsPlaceholderSecret(dashKey) || len(dashKey) < MinMachineSecretLen) {
		violation(fmt.Sprintf(
			"DASHBOARD_API_KEY is a placeholder or shorter than %d chars. Generate one with: openssl rand -base64 32",
			MinMachineSecretLen))
	}

	// (d) DB password (LETTUCE_DB_PASSWORD -> cfg.Database.Password). A committed
	// placeholder is always wrong. An empty/short password is a violation ONLY when
	// the DB host is not confined to a private network — the same host
	// classification the sslmode-downgrade warning uses (classifyDBHost) — because
	// a weak password that never crosses a public link is the bundled compose
	// topology (Postgres on the private bridge network), not a hazard.
	switch dbPass := cfg.Database.Password; {
	case IsPlaceholderSecret(dbPass):
		violation("LETTUCE_DB_PASSWORD is a placeholder. Generate one with: openssl rand -base64 32")
	case len(dbPass) < MinHumanPasswordLen && !hostConfinedToPrivate(cfg.Database.Host):
		violation(fmt.Sprintf(
			"LETTUCE_DB_PASSWORD is empty or shorter than %d chars and database host %q is not confined to a private network. Generate one with: openssl rand -base64 32",
			MinHumanPasswordLen, cfg.Database.Host))
	}

	// (e) Redis (LETTUCE_REDIS_URL -> cfg.Head.RedisURL) backs the shared
	// replay/rate-limit store. Empty is OK (single-replica in-process behavior). A
	// MISSING password is WARN-only in BOTH postures (the store may sit on a
	// private network); a placeholder/short password is a violation. A parse error
	// is ignored on purpose — the redis connect path fails loudly later with a
	// precise message, and the secret gate does not duplicate it.
	if redisURL := cfg.Head.RedisURL; redisURL != "" {
		if opt, perr := redis.ParseURL(redisURL); perr == nil {
			switch pw := opt.Password; {
			case pw == "":
				warnings = append(warnings,
					"LETTUCE_REDIS_URL has no password — the shared replay/rate-limit store is unauthenticated on the network it listens on")
			case IsPlaceholderSecret(pw) || len(pw) < MinMachineSecretLen:
				violation(fmt.Sprintf(
					"LETTUCE_REDIS_URL password is a placeholder or shorter than %d chars. Generate one with: openssl rand -hex 32",
					MinMachineSecretLen))
			}
		}
	}

	return warnings, errors.Join(violations...)
}
