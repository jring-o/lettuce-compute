package database

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/lettuce-compute/infrastructure/migrations"
)

// RunMigrations applies all pending migrations to the database.
// Uses golang-migrate with embedded SQL files from the migrations/ directory.
func RunMigrations(databaseURL string) error {
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("creating migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, databaseURL)
	if err != nil {
		return fmt.Errorf("creating migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			slog.Info("migrations: no changes to apply")
			return nil
		}
		version, dirty, _ := m.Version()
		slog.Error("migration failed", "version", version, "dirty", dirty, "error", err)
		return fmt.Errorf("applying migrations: %w", err)
	}

	version, _, _ := m.Version()
	slog.Info("migrations applied successfully", "version", version)
	return nil
}
