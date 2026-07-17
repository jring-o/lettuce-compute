package database

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/lettuce-compute/infrastructure/migrations"
)

// Execution bound for the MIGRATION connection only (never the serving pool).
// Boot-time DDL such as ALTER TABLE takes ACCESS EXCLUSIVE: on a busy table it
// queues behind long-running queries and blocks every query behind it for as
// long as it waits. golang-migrate's x-statement-timeout bounds each migration
// FILE's execution client-side (a context timeout around the file's single
// Exec), so a migration wedged in a lock queue — or simply runaway — aborts
// within the bound and boot exits with an actionable error instead of freezing
// traffic indefinitely.
//
// Deliberately NOT a server-side lock_timeout: that GUC would also govern
// golang-migrate's own pg_advisory_lock serialization call, which the
// non-winning replicas of a multi-replica boot BLOCK on by design while the
// winner applies migrations — a lock_timeout there crash-loops healthy
// replicas whenever a migration outlives it. The waiters' wait is already
// transitively bounded by the winner's per-file bound. Migrations that
// legitimately need longer than this are maintenance-window migrations — see
// guides/migrations.md.
const migrationStatementTimeoutMs = 60000

// migrationSessionURL returns databaseURL with golang-migrate's
// x-statement-timeout applied (the x- parameter is consumed by golang-migrate
// and stripped from the URL before it reaches the database driver). Existing
// query parameters are preserved, and a caller-supplied x-statement-timeout
// wins over the default.
func migrationSessionURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("parsing database URL: %w", err)
	}
	q := u.Query()
	if q.Get("x-statement-timeout") == "" {
		q.Set("x-statement-timeout", strconv.Itoa(migrationStatementTimeoutMs))
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
// The migration session runs with a fail-fast per-file execution bound (see
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
