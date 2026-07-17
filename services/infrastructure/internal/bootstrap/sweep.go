package bootstrap

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/config"
)

// SweepPlaceholderCredentials is the BG-30b residue sweep: on a PRODUCTION head
// it finds and neutralizes any credential PERSISTED from a known placeholder
// before the BG-30 boot gate existed (the config gate screens env, but cannot
// reach a row already committed to the database). It runs two independent passes.
//
// Pass (a) revokes API keys whose key_hash equals the hash of a known placeholder
// value (revoked_at set), logging a warning that names the BG-30 rotation to
// perform. Pass (b) treats an ADMIN user whose password_hash bcrypt-matches a
// known placeholder as a refuse-to-start condition — the row is NEVER rewritten
// (rotation is an operator action), the head simply refuses to boot until it is
// rotated.
//
// On a dev head (production=false) it is a no-op. Wired once at boot, right after
// bootstrap provisioning, under the same posture flag.
func SweepPlaceholderCredentials(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, production bool) error {
	if !production {
		logger.Debug("dev head (LETTUCE_SIGNING_KEY_AUTOGEN); skipping BG-30b placeholder-credential sweep")
		return nil
	}

	placeholders := config.KnownPlaceholderSecrets()

	// (a) Revoke API keys whose stored hash is the hash of a known placeholder. One
	// UPDATE over the SHA-256 hashes (apikey.HashKey), matching the way the keys are
	// stored, so no plaintext is ever handled.
	hashes := make([][]byte, 0, len(placeholders))
	for _, p := range placeholders {
		hashes = append(hashes, apikey.HashKey(p))
	}
	tag, err := pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = NOW() WHERE revoked_at IS NULL AND key_hash = ANY($1)`,
		hashes)
	if err != nil {
		return fmt.Errorf("BG-30b: revoking placeholder API keys: %w", err)
	}
	if n := tag.RowsAffected(); n > 0 {
		logger.Warn("BG-30b: revoked API key(s) whose value is a known placeholder — mint a real DASHBOARD_API_KEY and restart",
			"revoked", n)
	}

	// (b) Refuse to start if any ADMIN user still carries a known-placeholder
	// password. bcrypt hashes are salted, so a value match needs a per-row compare;
	// read every admin row first, then compare, so the connection is free during the
	// (CPU-bound) bcrypt work. The row is NEVER rewritten. password_hash is nullable
	// (OAuth-only accounts satisfy chk_users_auth_method with a NULL hash), and a
	// passwordless admin has no placeholder password to refuse — filter NULLs in SQL
	// so the scan below never sees one.
	rows, err := pool.Query(ctx, `SELECT id, email, password_hash FROM users WHERE role = 'ADMIN' AND password_hash IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("BG-30b: scanning admin users: %w", err)
	}
	type adminRow struct {
		id    string
		email string
		hash  string
	}
	var admins []adminRow
	for rows.Next() {
		var a adminRow
		if err := rows.Scan(&a.id, &a.email, &a.hash); err != nil {
			rows.Close()
			return fmt.Errorf("BG-30b: reading admin user: %w", err)
		}
		admins = append(admins, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("BG-30b: iterating admin users: %w", err)
	}

	for _, a := range admins {
		for _, p := range placeholders {
			if bcrypt.CompareHashAndPassword([]byte(a.hash), []byte(p)) == nil {
				return fmt.Errorf("refusing to start: admin user %q still has a known placeholder password — "+
					"rotate it and restart; see guides/head-setup.md (Troubleshooting)", a.email)
			}
		}
	}
	return nil
}
