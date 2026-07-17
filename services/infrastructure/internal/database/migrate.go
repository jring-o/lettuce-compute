package database

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/lettuce-compute/infrastructure/migrations"
)

// Session timeouts for the MIGRATION connection only (never the serving pool).
// Boot-time DDL such as ALTER TABLE takes ACCESS EXCLUSIVE: on a busy table it
// queues behind long-running queries and blocks every query behind it for as
// long as it waits, and a wedged migration also holds golang-migrate's advisory
// lock, stalling the other replicas' boots. With these limits a contended or
// runaway migration fails fast (boot exits with an actionable error and the
// operator retries off-peak) instead of freezing traffic. Migrations that
// legitimately need more than migrationStatementTimeout are maintenance-window
// migrations — see guides/migrations.md.
const (
	// migrationLockTimeout aborts a migration statement that waits longer than
	// this to acquire a database lock (applied server-side via lock_timeout).
	migrationLockTimeout = "5s"
	// migrationStatementTimeoutMs bounds each migration statement's execution
	// (golang-migrate's x-statement-timeout, in milliseconds).
	migrationStatementTimeoutMs = 60000
)

// migrationSessionURL returns databaseURL with the migration-session timeouts
// applied: golang-migrate's x-statement-timeout (stripped from the URL before
// it reaches the database driver) and a lock_timeout passed through the
// standard PostgreSQL `options` startup parameter. Existing query parameters,
// including a pre-set `options`, are preserved.
func migrationSessionURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("parsing database URL: %w", err)
	}
	q := u.Query()
	if q.Get("x-statement-timeout") == "" {
		q.Set("x-statement-timeout", fmt.Sprintf("%d", migrationStatementTimeoutMs))
	}
	lockOpt := "-c lock_timeout=" + migrationLockTimeout
	if opts := q.Get("options"); opts == "" {
		q.Set("options", lockOpt)
	} else if !strings.Contains(opts, "lock_timeout") {
		q.Set("options", opts+" "+lockOpt)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dirtySchemaError renders the actionable operator message for a schema left
// dirty by a previously failed migration. Under compose's restart policy the
// head crash-loops on this error, so the log line the loop repeats must carry
// the recovery procedure, not just the state.
func dirtySchemaError(version int) error {
	return fmt.Errorf(`migrations: the database schema is marked DIRTY at version %d — a previous attempt to apply migration %05d failed partway and the head refuses to run on an indeterminate schema. Boot will keep failing (and the container restart-looping) until an operator repairs it:
 1. Inspect the failed migration (services/infrastructure/migrations/%05d_*.up.sql) and the schema to determine which of its statements applied.
 2. Manually complete the migration's remaining statements, or undo the applied ones.
 3. Clear the dirty flag: UPDATE schema_migrations SET version = %d, dirty = false;  -- if you completed it by hand; use version = %d if you undid it
 4. Restart the head; boot re-applies anything still pending.
Full runbook: guides/migrations.md ("Recovering from a dirty schema")`,
		version, version, version, version, version-1)
}

// RunMigrations applies all pending migrations to the database.
// Uses golang-migrate with embedded SQL files from the migrations/ directory.
// The migration session runs with fail-fast lock/statement timeouts (see
// migrationSessionURL); the serving pool is unaffected.
func RunMigrations(databaseURL string) error {
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("creating migration source: %w", err)
	}

	sessionURL, err := migrationSessionURL(databaseURL)
	if err != nil {
		return err
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, sessionURL)
	if err != nil {
		return fmt.Errorf("creating migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			slog.Info("migrations: no changes to apply")
			return nil
		}
		var dirty migrate.ErrDirty
		if errors.As(err, &dirty) {
			return dirtySchemaError(dirty.Version)
		}
		version, dirtyFlag, _ := m.Version()
		slog.Error("migration failed", "version", version, "dirty", dirtyFlag, "error", err)
		return fmt.Errorf("applying migrations: %w", err)
	}

	version, _, _ := m.Version()
	slog.Info("migrations applied successfully", "version", version)
	return nil
}
